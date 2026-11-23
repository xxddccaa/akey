// Package server implements `akey serve`.
// OWNER: server agent.
//
// Data dir layout (dirs 0700, files 0600, all writes atomic):
//
//	params.json                      public KDF params (created once)
//	admin_token                      bearer token required to create new ids (created once)
//	tls/cert.pem, tls/key.pem        self-signed ECDSA P-256 (created once)
//	blobs/<id>/current               latest envelope
//	blobs/<id>/v-<unixnano>          version history (newest 10 kept)
//	blobs/<id>/write_token.sha256    hex(sha256(write token))
//
// The server never sees plaintext configs: the web panel (web/, served at /)
// derives keys and encrypts in the browser via WASM, exactly like the CLI.
package server

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/xxddccaa/akey/internal/vault"
)

type Config struct {
	Listen       string   // e.g. ":8787"
	DataDir      string   // holds params.json, admin token, tls/, blobs/
	DistDir      string   // directory of client binaries served under /dl/
	Hosts        []string // public IPs/hostnames for the cert SAN and printed install commands
	InsecureHTTP bool     // plain HTTP, no TLS (LAN only)
	Version      string   // reported by GET /v1/info
}

const (
	maxBody      = 1 << 20
	keepVersions = 10
	missLimit    = 10
	missWindow   = 10 * time.Minute
	blockFor     = 30 * time.Minute
)

var (
	idRe    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	tokenRe = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hostRe  = regexp.MustCompile(`^[A-Za-z0-9.\-:\[\]]{1,255}$`)
	distRe  = regexp.MustCompile(`^(akey-(linux|darwin)-(amd64|arm64)|akey-windows-(amd64|arm64)\.exe|SHA256SUMS)$`)
)

//go:embed assets/install.sh assets/install.ps1
var assets embed.FS

var tmplFuncs = template.FuncMap{
	// sq escapes for inclusion inside a POSIX sh single-quoted string.
	"sq": func(s string) string { return strings.ReplaceAll(s, `'`, `'\''`) },
	// psq escapes for inclusion inside a PowerShell single-quoted string.
	"psq": func(s string) string { return strings.ReplaceAll(s, `'`, `''`) },
}

type server struct {
	cfg        Config
	blobDir    string
	params     []byte // JSON
	adminPath  string
	adminToken string
	adminNew   bool // token generated during this start
	adminHash  [32]byte
	tlsCert    *tls.Certificate
	pin        string // "sha256//<b64>" or "" in insecure mode
	certSHA    string // upper hex sha256(cert DER) or ""
	fallback   string // host:port used when r.Host is unusable
	sh, ps     *template.Template
	lim        *limiter
	mu         sync.RWMutex
	logf       func(format string, args ...any)
	readOnly   bool         // `akey info`: never create state
	probeHTTP  *http.Client // outbound client for POST /v1/probe
	web        fs.FS        // panel files (flat); defaults to the embedded web/
	panelOnce  sync.Once
	panel      *panelSet
}

// Run starts the server and blocks until it fails.
func Run(cfg Config) error {
	s, err := newServer(cfg, log.New(os.Stderr, "akey: ", log.LstdFlags).Printf)
	if err != nil {
		return err
	}
	listen := cfg.Listen
	if listen == "" {
		listen = ":8787"
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	s.banner(os.Stdout, ln.Addr())
	go s.panelFiles() // pre-compress the panel so the first visitor doesn't wait
	return s.serve(ln)
}

func newServer(cfg Config, logf func(string, ...any)) (*server, error) {
	return newServerMode(cfg, logf, false)
}

// newServerMode loads (and unless readOnly, creates) the data dir state.
func newServerMode(cfg Config, logf func(string, ...any), readOnly bool) (*server, error) {
	if cfg.DataDir == "" {
		return nil, errors.New("server: data dir is required")
	}
	cfg.Hosts = normalizeHosts(cfg.Hosts)
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	s := &server{cfg: cfg, lim: newLimiter(), logf: logf, readOnly: readOnly, probeHTTP: newProbeClient()}
	if sub, err := fs.Sub(webFS, "web"); err == nil {
		s.web = sub
	}
	if !readOnly {
		for _, d := range []string{cfg.DataDir, filepath.Join(cfg.DataDir, "blobs")} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				return nil, err
			}
			_ = os.Chmod(d, 0o700)
		}
	}
	s.blobDir = filepath.Join(cfg.DataDir, "blobs")
	if err := s.loadParams(); err != nil {
		return nil, err
	}
	if err := s.loadAdminToken(); err != nil {
		return nil, err
	}
	if !cfg.InsecureHTTP {
		if err := s.loadCert(); err != nil {
			return nil, err
		}
	}
	var err error
	if s.sh, err = template.New("install.sh").Funcs(tmplFuncs).ParseFS(assets, "assets/install.sh"); err != nil {
		return nil, err
	}
	if s.ps, err = template.New("install.ps1").Funcs(tmplFuncs).ParseFS(assets, "assets/install.ps1"); err != nil {
		return nil, err
	}
	s.fallback = "127.0.0.1"
	if len(cfg.Hosts) > 0 {
		s.fallback = cfg.Hosts[0]
	}
	return s, nil
}

func (s *server) serve(ln net.Listener) error {
	if p, ok := ln.Addr().(*net.TCPAddr); ok {
		s.fallback = net.JoinHostPort(strings.Trim(s.fallback, "[]"), strconv.Itoa(p.Port))
	}
	hs := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          log.New(quietTLS{os.Stderr}, "akey: ", log.LstdFlags),
	}
	if s.cfg.InsecureHTTP {
		return hs.Serve(ln)
	}
	hs.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{*s.tlsCert}}
	return hs.ServeTLS(ln, "", "")
}

// quietTLS drops the handshake-error noise from scanners and wrong-pin clients.
type quietTLS struct{ w io.Writer }

func (q quietTLS) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("TLS handshake error")) {
		return len(p), nil
	}
	return q.w.Write(p)
}

func normalizeHosts(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, h := range in {
		for _, part := range strings.Split(h, ",") {
			part = strings.TrimSpace(part)
			if hh, _, err := net.SplitHostPort(part); err == nil {
				part = hh
			}
			part = strings.Trim(part, "[]")
			if part == "" || seen[part] {
				continue
			}
			seen[part] = true
			out = append(out, part)
		}
	}
	return out
}

// ---------- persistent state ----------

func (s *server) loadParams() error {
	path := filepath.Join(s.cfg.DataDir, "params.json")
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) && s.readOnly {
		return notInitialised(s.cfg.DataDir, path)
	}
	if errors.Is(err, fs.ErrNotExist) {
		p, err := vault.DefaultParams()
		if err != nil {
			return err
		}
		if b, err = json.Marshal(p); err != nil {
			return err
		}
		if err := writeFileAtomic(path, append(b, '\n')); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	var p vault.Params
	if err := json.Unmarshal(b, &p); err != nil {
		return fmt.Errorf("server: %s: %w", path, err)
	}
	if err := p.Validate(); err != nil {
		return fmt.Errorf("server: %s: %w", path, err)
	}
	s.params, _ = json.Marshal(p)
	return nil
}

func (s *server) loadAdminToken() error {
	s.adminPath = filepath.Join(s.cfg.DataDir, "admin_token")
	b, err := os.ReadFile(s.adminPath)
	if errors.Is(err, fs.ErrNotExist) && s.readOnly {
		return notInitialised(s.cfg.DataDir, s.adminPath)
	}
	if errors.Is(err, fs.ErrNotExist) {
		tok, err := randHex(32)
		if err != nil {
			return err
		}
		if err := writeFileAtomic(s.adminPath, []byte(tok+"\n")); err != nil {
			return err
		}
		b, s.adminNew = []byte(tok), true
	} else if err != nil {
		return err
	}
	s.adminToken = strings.TrimSpace(string(b))
	if len(s.adminToken) < 16 {
		return fmt.Errorf("server: %s: admin token too short", s.adminPath)
	}
	s.adminHash = sha256.Sum256([]byte(s.adminToken))
	return nil
}

func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------- HTTP ----------

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/params", s.getParams)
	mux.HandleFunc("GET /v1/info", s.getInfo)
	mux.HandleFunc("GET /v1/blob/{id}", s.getBlob)
	mux.HandleFunc("PUT /v1/blob/{id}", s.putBlob)
	mux.HandleFunc("GET /v1/blob/{id}/versions", s.listBlobVersions)
	mux.HandleFunc("GET /v1/blob/{id}/versions/{v}", s.getBlobVersion)
	mux.HandleFunc("POST /v1/probe", s.probe)
	mux.HandleFunc("GET /i", s.install(func() *template.Template { return s.sh }))
	mux.HandleFunc("GET /i.ps1", s.install(func() *template.Template { return s.ps }))
	mux.HandleFunc("GET /dl/{file}", s.download)
	mux.HandleFunc("GET /{$}", s.panelIndex)
	mux.HandleFunc("GET /assets/{file}", s.panelAsset)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		mux.ServeHTTP(w, r)
	})
}

func (s *server) getParams(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(s.params)
}

func (s *server) getBlob(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.tooMany(w, ip) {
		return
	}
	id := r.PathValue("id")
	if !idRe.MatchString(id) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	dir := filepath.Join(s.blobDir, id)
	s.mu.RLock()
	data, err := os.ReadFile(filepath.Join(dir, "current"))
	vers := listVersions(dir)
	s.mu.RUnlock()
	if errors.Is(err, fs.ErrNotExist) {
		s.miss(ip)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.logf("read blob: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var ver int64
	if len(vers) > 0 {
		ver = vers[len(vers)-1]
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Akey-Version", strconv.FormatInt(ver, 10))
	w.Write(data)
}

// authorize checks the PUT credentials against the current state of dir.
// Must be called with s.mu held (read or write).
func (s *server) authorize(r *http.Request, dir, token string) (exists bool, status int, msg string) {
	stored, err := os.ReadFile(filepath.Join(dir, "write_token.sha256"))
	switch {
	case err == nil:
		want := []byte(strings.TrimSpace(string(stored)))
		got := []byte(vault.HashWriteToken(token))
		if subtle.ConstantTimeCompare(want, got) != 1 {
			return true, http.StatusForbidden, "write token mismatch"
		}
		return true, 0, ""
	case !errors.Is(err, fs.ErrNotExist):
		s.logf("read write token: %v", err)
		return false, http.StatusInternalServerError, "internal error"
	}
	bearer, ok := bearerToken(r)
	if !ok {
		return false, http.StatusUnauthorized, "admin token required to create a new id"
	}
	if !s.adminOK(bearer) {
		return false, http.StatusForbidden, "invalid admin token"
	}
	return false, 0, ""
}

// bearerToken returns the token of an "Authorization: Bearer <token>" header.
func bearerToken(r *http.Request) (string, bool) {
	auth := r.Header.Get("Authorization")
	const pfx = "Bearer "
	if len(auth) <= len(pfx) || !strings.EqualFold(auth[:len(pfx)], pfx) {
		return "", false
	}
	return strings.TrimSpace(auth[len(pfx):]), true
}

// adminOK compares tok with the admin token in constant time.
func (s *server) adminOK(tok string) bool {
	got := sha256.Sum256([]byte(strings.TrimSpace(tok)))
	return subtle.ConstantTimeCompare(got[:], s.adminHash[:]) == 1
}

// checkWriteToken verifies token against dir's stored hash: 0 when it
// matches, 404 for an unknown id, 403 on mismatch. Caller holds s.mu.
func (s *server) checkWriteToken(dir, token string) (status int, msg string) {
	stored, err := os.ReadFile(filepath.Join(dir, "write_token.sha256"))
	if errors.Is(err, fs.ErrNotExist) {
		return http.StatusNotFound, "not found"
	}
	if err != nil {
		s.logf("read write token: %v", err)
		return http.StatusInternalServerError, "internal error"
	}
	want := []byte(strings.TrimSpace(string(stored)))
	got := []byte(vault.HashWriteToken(token))
	if subtle.ConstantTimeCompare(want, got) != 1 {
		return http.StatusForbidden, "write token mismatch"
	}
	return 0, ""
}

// versionsAuth is the common prelude of the history endpoints: rate limit,
// id + write token validation. On failure it has written the response.
func (s *server) versionsAuth(w http.ResponseWriter, r *http.Request) (dir, token, ip string, ok bool) {
	ip = clientIP(r)
	if s.tooMany(w, ip) {
		return "", "", ip, false
	}
	id := r.PathValue("id")
	if !idRe.MatchString(id) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return "", "", ip, false
	}
	token = r.Header.Get("X-Akey-Write-Token")
	if token == "" {
		s.miss(ip)
		http.Error(w, "missing X-Akey-Write-Token", http.StatusUnauthorized)
		return "", "", ip, false
	}
	if !tokenRe.MatchString(token) {
		http.Error(w, "malformed X-Akey-Write-Token (want 64 lowercase hex)", http.StatusBadRequest)
		return "", "", ip, false
	}
	return filepath.Join(s.blobDir, id), token, ip, true
}

type versionInfo struct {
	Version int64 `json:"version"`
	Size    int64 `json:"size"`
	Current bool  `json:"current"`
}

// listBlobVersions: GET /v1/blob/{id}/versions -> {"versions":[...]} newest first.
func (s *server) listBlobVersions(w http.ResponseWriter, r *http.Request) {
	dir, token, ip, ok := s.versionsAuth(w, r)
	if !ok {
		return
	}
	s.mu.RLock()
	status, msg := s.checkWriteToken(dir, token)
	var out []versionInfo
	if status == 0 {
		vers := listVersions(dir)
		out = make([]versionInfo, 0, len(vers))
		for i := len(vers) - 1; i >= 0; i-- {
			st, err := os.Stat(filepath.Join(dir, "v-"+strconv.FormatInt(vers[i], 10)))
			if err != nil {
				continue
			}
			out = append(out, versionInfo{Version: vers[i], Size: st.Size(), Current: i == len(vers)-1})
		}
	}
	s.mu.RUnlock()
	if status != 0 {
		if status != http.StatusInternalServerError {
			s.miss(ip)
		}
		http.Error(w, msg, status)
		return
	}
	writeJSON(w, map[string]any{"versions": out})
}

// getBlobVersion: GET /v1/blob/{id}/versions/{v} -> that envelope.
func (s *server) getBlobVersion(w http.ResponseWriter, r *http.Request) {
	dir, token, ip, ok := s.versionsAuth(w, r)
	if !ok {
		return
	}
	vs := r.PathValue("v")
	v, err := strconv.ParseInt(vs, 10, 64)
	if err != nil || v <= 0 || strconv.FormatInt(v, 10) != vs {
		http.Error(w, "invalid version", http.StatusBadRequest)
		return
	}
	s.mu.RLock()
	status, msg := s.checkWriteToken(dir, token)
	var data []byte
	if status == 0 {
		data, err = os.ReadFile(filepath.Join(dir, "v-"+vs))
	}
	s.mu.RUnlock()
	switch {
	case status == http.StatusInternalServerError:
		http.Error(w, msg, status)
		return
	case status != 0:
		s.miss(ip)
		http.Error(w, msg, status)
		return
	case errors.Is(err, fs.ErrNotExist):
		http.Error(w, "version not found", http.StatusNotFound)
		return
	case err != nil:
		s.logf("read version: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Akey-Version", vs)
	w.Write(data)
}

func writeJSON(w http.ResponseWriter, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(append(b, '\n'))
}

func (s *server) putBlob(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.tooMany(w, ip) {
		return
	}
	id := r.PathValue("id")
	if !idRe.MatchString(id) {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	token := r.Header.Get("X-Akey-Write-Token")
	if token == "" {
		s.miss(ip)
		http.Error(w, "missing X-Akey-Write-Token", http.StatusUnauthorized)
		return
	}
	if !tokenRe.MatchString(token) {
		http.Error(w, "malformed X-Akey-Write-Token (want 64 lowercase hex)", http.StatusBadRequest)
		return
	}
	if r.ContentLength > maxBody {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	dir := filepath.Join(s.blobDir, id)
	fail := func(status int, msg string) {
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			s.miss(ip)
		}
		http.Error(w, msg, status)
	}

	// Check credentials before reading the body, without holding the
	// write lock while a slow client uploads.
	s.mu.RLock()
	existed, status, msg := s.authorize(r, dir, token)
	s.mu.RUnlock()
	if status != 0 {
		fail(status, msg)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "read body", http.StatusBadRequest)
		}
		return
	}
	if len(body) == 0 || !json.Valid(body) {
		http.Error(w, "body must be a JSON envelope", http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	exists, status, msg := s.authorize(r, dir, token) // state may have changed
	if status != 0 {
		fail(status, msg)
		return
	}
	ver, err := s.store(dir, body, exists, token)
	if err != nil {
		s.logf("store blob %s: %v", id[:8], err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if !existed && !exists {
		s.logf("created id %s… from %s", id[:8], ip)
	}
	w.Header().Set("X-Akey-Version", strconv.FormatInt(ver, 10))
	w.WriteHeader(http.StatusNoContent)
}

// store writes a new version. Caller holds s.mu.
func (s *server) store(dir string, body []byte, exists bool, token string) (int64, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}
	vers := listVersions(dir)
	ver := time.Now().UnixNano()
	if n := len(vers); n > 0 && vers[n-1] >= ver {
		ver = vers[n-1] + 1
	}
	if err := writeFileAtomic(filepath.Join(dir, "v-"+strconv.FormatInt(ver, 10)), body); err != nil {
		return 0, err
	}
	if err := writeFileAtomic(filepath.Join(dir, "current"), body); err != nil {
		return 0, err
	}
	if !exists {
		if err := writeFileAtomic(filepath.Join(dir, "write_token.sha256"), []byte(vault.HashWriteToken(token)+"\n")); err != nil {
			return 0, err
		}
	}
	vers = append(vers, ver)
	for len(vers) > keepVersions {
		if err := os.Remove(filepath.Join(dir, "v-"+strconv.FormatInt(vers[0], 10))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			s.logf("prune: %v", err)
		}
		vers = vers[1:]
	}
	return ver, nil
}

// listVersions returns the v-<n> numbers in dir, ascending.
func listVersions(dir string) []int64 {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []int64
	for _, e := range ents {
		if n, ok := strings.CutPrefix(e.Name(), "v-"); ok {
			if v, err := strconv.ParseInt(n, 10, 64); err == nil {
				out = append(out, v)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

type installData struct{ Server, Pin, CertSHA256 string }

// baseURL is scheme://host[:port] as the client reached us; hostile Host
// headers fall back to the configured host.
func (s *server) baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if !hostRe.MatchString(host) {
		host = s.fallback
	}
	return scheme + "://" + host
}

func (s *server) install(t func() *template.Template) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		if err := t().Execute(&buf, installData{s.baseURL(r), s.pin, s.certSHA}); err != nil {
			s.logf("render installer: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(buf.Bytes())
	}
}

func (s *server) download(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	if s.cfg.DistDir == "" || !distRe.MatchString(name) {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(s.cfg.DistDir, name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	ct := "application/octet-stream"
	if name == "SHA256SUMS" {
		ct = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	http.ServeContent(w, r, name, st.ModTime(), f)
}

// ---------- rate limiting ----------

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	// IPv6 clients usually control a whole /64.
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

func (s *server) tooMany(w http.ResponseWriter, ip string) bool {
	left, blocked := s.lim.blocked(ip)
	if !blocked {
		return false
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(left.Seconds())+1))
	http.Error(w, "too many failed attempts, try again later", http.StatusTooManyRequests)
	return true
}

func (s *server) miss(ip string) {
	if s.lim.miss(ip) {
		s.logf("rate limit: blocking %s for %s after %d failed requests", ip, blockFor, missLimit)
	}
}

type ipState struct {
	misses []time.Time
	until  time.Time
}

type limiter struct {
	mu        sync.Mutex
	m         map[string]*ipState
	now       func() time.Time
	lastSweep time.Time
}

func newLimiter() *limiter { return &limiter{m: map[string]*ipState{}, now: time.Now} }

func (l *limiter) blocked(ip string) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)
	st := l.m[ip]
	if st == nil || !now.Before(st.until) {
		return 0, false
	}
	return st.until.Sub(now), true
}

// miss records a failed attempt; returns true if it triggered a block.
func (l *limiter) miss(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	st := l.m[ip]
	if st == nil {
		st = &ipState{}
		l.m[ip] = st
	}
	keep := st.misses[:0]
	for _, t := range st.misses {
		if now.Sub(t) < missWindow {
			keep = append(keep, t)
		}
	}
	st.misses = append(keep, now)
	if len(st.misses) >= missLimit {
		st.until = now.Add(blockFor)
		st.misses = nil
		return true
	}
	return false
}

func (l *limiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	for ip, st := range l.m {
		if now.Before(st.until) {
			continue
		}
		if n := len(st.misses); n == 0 || now.Sub(st.misses[n-1]) >= missWindow {
			delete(l.m, ip)
		}
	}
}
