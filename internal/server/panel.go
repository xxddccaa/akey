package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// The browser panel: static files in web/ (flat: index.html, app.js,
// worker.js, style.css, akey.wasm, wasm_exec.js, ...). Only regular files
// directly inside web/ are served; subdirectories (e.g. tests) never are.
//
//go:embed web
var webFS embed.FS

const panelCSP = "default-src 'none'; script-src 'self' 'wasm-unsafe-eval'; worker-src 'self'; " +
	"connect-src 'self'; style-src 'self'; img-src 'self' data:; base-uri 'none'; " +
	"form-action 'none'; frame-ancestors 'none'"

type webAsset struct {
	ctype      string
	raw, gz    []byte // gz is nil when compression doesn't help
	etag, etgz string
}

type panelSet struct {
	files map[string]*webAsset
	built bool // akey.wasm and index.html present
}

var notBuiltPage = []byte(`<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>akey - 面板未构建</title>
</head>
<body>
<h1>akey 面板未构建</h1>
<p>这个 akey 程序编译时没有包含网页面板（缺少 <code>akey.wasm</code>）。
在源码目录先运行 <code>make wasm</code>（<code>make build</code> / <code>make dist</code> 会自动执行），再重新编译；
或者直接使用 GitHub Releases 发布的二进制。</p>
<p>The web panel is not built into this binary (<code>akey.wasm</code> is missing).
Run <code>make wasm</code> and rebuild, or use a release binary.</p>
<p>命令行一键安装仍可用 / CLI installers still work: <code>/i</code> (sh), <code>/i.ps1</code> (PowerShell).</p>
</body>
</html>
`)

var compressible = map[string]bool{".html": true, ".js": true, ".mjs": true, ".css": true, ".wasm": true,
	".json": true, ".svg": true, ".txt": true, ".map": true}

func assetType(name string) string {
	switch ext := strings.ToLower(path.Ext(name)); ext {
	case ".html":
		return "text/html; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".wasm":
		return "application/wasm"
	case ".json", ".map":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	case ".txt":
		return "text/plain; charset=utf-8"
	default:
		if t := mime.TypeByExtension(ext); t != "" {
			return t
		}
		return "application/octet-stream"
	}
}

func newAsset(name string, raw []byte) *webAsset {
	sum := sha256.Sum256(raw)
	tag := hex.EncodeToString(sum[:12])
	a := &webAsset{ctype: assetType(name), raw: raw, etag: `"` + tag + `"`, etgz: `"` + tag + `-gz"`}
	if compressible[strings.ToLower(path.Ext(name))] && len(raw) > 256 {
		var b bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&b, gzip.BestCompression)
		zw.Write(raw)
		zw.Close()
		if b.Len() < len(raw)*9/10 {
			a.gz = b.Bytes()
		}
	}
	return a
}

func loadPanel(fsys fs.FS) *panelSet {
	p := &panelSet{files: map[string]*webAsset{}}
	if fsys == nil {
		return p
	}
	ents, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return p
	}
	for _, e := range ents {
		name := e.Name()
		if !e.Type().IsRegular() || strings.HasPrefix(name, ".") {
			continue
		}
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			continue
		}
		p.files[name] = newAsset(name, raw)
	}
	p.built = p.files["akey.wasm"] != nil && p.files["index.html"] != nil
	return p
}

// panelFiles loads and compresses the panel once (lazily: `akey info` and
// API-only use never pay for compressing the wasm).
func (s *server) panelFiles() *panelSet {
	s.panelOnce.Do(func() { s.panel = loadPanel(s.web) })
	return s.panel
}

func setPanelHeaders(h http.Header) {
	h.Set("Content-Security-Policy", panelCSP)
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
}

// panelIndex: GET / -> index.html (no-store so upgrades show immediately),
// or a short "panel not built" page when akey.wasm wasn't embedded.
func (s *server) panelIndex(w http.ResponseWriter, r *http.Request) {
	p := s.panelFiles()
	if !p.built {
		setPanelHeaders(w.Header())
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Length", strconv.Itoa(len(notBuiltPage)))
		w.Write(notBuiltPage)
		return
	}
	serveAsset(w, r, p.files["index.html"], false)
}

// panelAsset: GET /assets/{file} -> web/{file}. Revalidated on every use
// (no-cache + content-hash ETag), so a new release is never mixed with
// stale cached files, while unchanged files cost a 304.
func (s *server) panelAsset(w http.ResponseWriter, r *http.Request) {
	a := s.panelFiles().files[r.PathValue("file")]
	if a == nil {
		http.NotFound(w, r)
		return
	}
	serveAsset(w, r, a, true)
}

func serveAsset(w http.ResponseWriter, r *http.Request, a *webAsset, revalidate bool) {
	h := w.Header()
	setPanelHeaders(h)
	h.Set("Content-Type", a.ctype)
	h.Add("Vary", "Accept-Encoding")
	body, etag := a.raw, a.etag
	if a.gz != nil && acceptsGzip(r.Header.Get("Accept-Encoding")) {
		body, etag = a.gz, a.etgz
		h.Set("Content-Encoding", "gzip")
	}
	if revalidate {
		h.Set("Cache-Control", "no-cache")
		h.Set("ETag", etag)
		if etagMatch(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.Write(body)
}

func acceptsGzip(ae string) bool {
	for _, part := range strings.Split(ae, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		for _, prm := range strings.Split(params, ";") {
			k, v, _ := strings.Cut(strings.TrimSpace(prm), "=")
			if strings.EqualFold(k, "q") {
				if q, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && q == 0 {
					return false
				}
			}
		}
		return true
	}
	return false
}

func etagMatch(inm, etag string) bool {
	if inm == "" {
		return false
	}
	for _, t := range strings.Split(inm, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == "*" || t == etag {
			return true
		}
	}
	return false
}
