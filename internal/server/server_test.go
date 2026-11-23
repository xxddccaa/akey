package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/xxddccaa/akey/internal/vault"
)

const (
	testID  = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testTok = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	badTok  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	env1    = `{"v":1,"alg":"xchacha20poly1305","nonce":"AA==","ct":"AA=="}`
)

func newTest(t *testing.T, cfg Config) (*server, *httptest.Server) {
	t.Helper()
	if cfg.DataDir == "" {
		cfg.DataDir = t.TempDir()
	}
	s, err := newServer(cfg, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(s.handler())
	if cfg.InsecureHTTP {
		ts.Start()
	} else {
		ts.TLS = &tls.Config{Certificates: []tls.Certificate{*s.tlsCert}}
		ts.StartTLS()
	}
	t.Cleanup(ts.Close)
	return s, ts
}

func do(t *testing.T, ts *httptest.Server, method, path, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func put(t *testing.T, s *server, ts *httptest.Server, id, tok, admin, body string) *http.Response {
	h := map[string]string{"X-Akey-Write-Token": tok}
	if admin != "" {
		h["Authorization"] = "Bearer " + admin
	}
	return do(t, ts, "PUT", "/v1/blob/"+id, body, h)
}

func readBody(t *testing.T, r *http.Response) string {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParamsAndPersistence(t *testing.T) {
	dir := t.TempDir()
	s, ts := newTest(t, Config{DataDir: dir, Hosts: []string{"203.0.113.7"}})
	r := do(t, ts, "GET", "/v1/params", "", nil)
	var p vault.Params
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil || r.StatusCode != 200 {
		t.Fatalf("%d %v", r.StatusCode, err)
	}
	if p.Validate() != nil || p.MemoryKiB != 65536 {
		t.Fatalf("bad params %+v", p)
	}
	if !s.adminNew || len(s.adminToken) != 64 {
		t.Fatal("admin token not generated")
	}
	if !strings.HasPrefix(s.pin, "sha256//") || len(s.certSHA) != 64 || strings.ToUpper(s.certSHA) != s.certSHA {
		t.Fatalf("pin %q cert %q", s.pin, s.certSHA)
	}
	if s.tlsCert.Leaf.VerifyHostname("203.0.113.7") != nil || s.tlsCert.Leaf.VerifyHostname("localhost") != nil ||
		s.tlsCert.Leaf.VerifyHostname("127.0.0.1") != nil {
		t.Fatal("SANs missing")
	}
	if d := s.tlsCert.Leaf.NotAfter.Sub(time.Now()); d < 9*365*24*time.Hour {
		t.Fatalf("validity %v", d)
	}
	for _, f := range []string{"params.json", "admin_token", "tls/cert.pem", "tls/key.pem"} {
		st, err := os.Stat(filepath.Join(dir, f))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", f, err, st.Mode())
		}
	}
	// Restart with different hosts: same params, token, and pin.
	s2, err := newServer(Config{DataDir: dir, Hosts: []string{"198.51.100.1"}}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if s2.adminNew || s2.adminToken != s.adminToken || s2.pin != s.pin || !bytes.Equal(s2.params, s.params) {
		t.Fatal("state not persisted")
	}
}

func TestCreateUpdateGet(t *testing.T) {
	s, ts := newTest(t, Config{})
	// unknown id
	if r := do(t, ts, "GET", "/v1/blob/"+testID, "", nil); r.StatusCode != 404 {
		t.Fatal(r.StatusCode)
	}
	// create needs admin token
	if r := put(t, s, ts, testID, testTok, "", env1); r.StatusCode != 401 {
		t.Fatal(r.StatusCode)
	}
	if r := put(t, s, ts, testID, testTok, "wrong-admin-token-xxxxxxxx", env1); r.StatusCode != 403 {
		t.Fatal(r.StatusCode)
	}
	if r := put(t, s, ts, testID, "", s.adminToken, env1); r.StatusCode != 401 {
		t.Fatal(r.StatusCode)
	}
	r := put(t, s, ts, testID, testTok, s.adminToken, env1)
	if r.StatusCode != 204 || r.Header.Get("X-Akey-Version") == "" {
		t.Fatal(r.StatusCode)
	}
	v1 := r.Header.Get("X-Akey-Version")
	r = do(t, ts, "GET", "/v1/blob/"+testID, "", nil)
	if r.StatusCode != 200 || readBody(t, r) != env1 || r.Header.Get("X-Akey-Version") != v1 {
		t.Fatalf("get: %d %s", r.StatusCode, r.Header.Get("X-Akey-Version"))
	}
	// update: wrong token 403 (even with admin token), right token 204 without admin
	if r := put(t, s, ts, testID, badTok, s.adminToken, `{"v":1}`); r.StatusCode != 403 {
		t.Fatal(r.StatusCode)
	}
	if r := put(t, s, ts, testID, testTok, "", `{"v":1,"x":2}`); r.StatusCode != 204 {
		t.Fatal(r.StatusCode)
	}
	r = do(t, ts, "GET", "/v1/blob/"+testID, "", nil)
	if b := readBody(t, r); b != `{"v":1,"x":2}` || r.Header.Get("X-Akey-Version") <= v1 {
		t.Fatalf("after update: %s %s", b, r.Header.Get("X-Akey-Version"))
	}
	// stored hash, perms
	d := filepath.Join(s.blobDir, testID)
	h, _ := os.ReadFile(filepath.Join(d, "write_token.sha256"))
	if strings.TrimSpace(string(h)) != vault.HashWriteToken(testTok) {
		t.Fatal("hash not stored")
	}
	if st, _ := os.Stat(d); st.Mode().Perm() != 0o700 {
		t.Fatal(st.Mode())
	}
	if st, _ := os.Stat(filepath.Join(d, "current")); st.Mode().Perm() != 0o600 {
		t.Fatal(st.Mode())
	}
	// not JSON
	if r := put(t, s, ts, testID, testTok, "", "garbage"); r.StatusCode != 400 {
		t.Fatal(r.StatusCode)
	}
}

func TestValidation(t *testing.T) {
	s, ts := newTest(t, Config{})
	for _, id := range []string{"abc", strings.ToUpper(testID), testID + "0", strings.Repeat("g", 64)} {
		if r := do(t, ts, "GET", "/v1/blob/"+id, "", nil); r.StatusCode != 400 {
			t.Fatalf("%s: %d", id, r.StatusCode)
		}
		if r := put(t, s, ts, id, testTok, s.adminToken, env1); r.StatusCode != 400 {
			t.Fatalf("put %s: %d", id, r.StatusCode)
		}
	}
	if r := put(t, s, ts, testID, "short", s.adminToken, env1); r.StatusCode != 400 {
		t.Fatal(r.StatusCode)
	}
	big := `{"x":"` + strings.Repeat("a", maxBody) + `"}`
	if r := put(t, s, ts, testID, testTok, s.adminToken, big); r.StatusCode != 413 {
		t.Fatal(r.StatusCode)
	}
	// chunked (no Content-Length) oversize
	req, _ := http.NewRequest("PUT", ts.URL+"/v1/blob/"+testID, io.MultiReader(strings.NewReader(big)))
	req.ContentLength = -1
	req.Header.Set("X-Akey-Write-Token", testTok)
	req.Header.Set("Authorization", "Bearer "+s.adminToken)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Fatal(resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(s.blobDir, testID)); !os.IsNotExist(err) {
		t.Fatal("rejected upload created blob dir")
	}
	if r := do(t, ts, "POST", "/v1/blob/"+testID, env1, nil); r.StatusCode != 405 {
		t.Fatal(r.StatusCode)
	}
}

func TestRateLimit(t *testing.T) {
	s, ts := newTest(t, Config{})
	now := time.Now()
	s.lim.now = func() time.Time { return now }
	for i := 0; i < 5; i++ {
		if r := do(t, ts, "GET", "/v1/blob/"+testID, "", nil); r.StatusCode != 404 {
			t.Fatal(r.StatusCode)
		}
	}
	for i := 0; i < 5; i++ {
		if r := put(t, s, ts, testID, testTok, "bad-admin-token-xxxxxxxxx", env1); r.StatusCode != 403 {
			t.Fatal(r.StatusCode)
		}
	}
	r := do(t, ts, "GET", "/v1/blob/"+testID, "", nil)
	if r.StatusCode != 429 || r.Header.Get("Retry-After") == "" {
		t.Fatal(r.StatusCode)
	}
	if r := put(t, s, ts, testID, testTok, s.adminToken, env1); r.StatusCode != 429 {
		t.Fatal(r.StatusCode)
	}
	if r := do(t, ts, "GET", "/v1/params", "", nil); r.StatusCode != 200 {
		t.Fatal("params should not be limited")
	}
	now = now.Add(blockFor + time.Second)
	if r := do(t, ts, "GET", "/v1/blob/"+testID, "", nil); r.StatusCode != 404 {
		t.Fatalf("block should expire: %d", r.StatusCode)
	}
	// Misses spread beyond the window do not accumulate.
	for i := 0; i < 20; i++ {
		now = now.Add(missWindow / 5)
		if r := do(t, ts, "GET", "/v1/blob/"+testID, "", nil); r.StatusCode != 404 {
			t.Fatalf("i=%d %d", i, r.StatusCode)
		}
	}
}

func TestVersionRetention(t *testing.T) {
	s, ts := newTest(t, Config{})
	for i := 0; i < 11; i++ {
		admin := ""
		if i == 0 {
			admin = s.adminToken
		}
		if r := put(t, s, ts, testID, testTok, admin, fmt.Sprintf(`{"n":%d}`, i)); r.StatusCode != 204 {
			t.Fatal(i, r.StatusCode)
		}
	}
	vers := listVersions(filepath.Join(s.blobDir, testID))
	if len(vers) != keepVersions {
		t.Fatalf("%d versions", len(vers))
	}
	newest, _ := os.ReadFile(filepath.Join(s.blobDir, testID, fmt.Sprintf("v-%d", vers[len(vers)-1])))
	oldest, _ := os.ReadFile(filepath.Join(s.blobDir, testID, fmt.Sprintf("v-%d", vers[0])))
	if string(newest) != `{"n":10}` || string(oldest) != `{"n":1}` {
		t.Fatalf("newest %s oldest %s", newest, oldest)
	}
	ents, _ := os.ReadDir(filepath.Join(s.blobDir, testID))
	if len(ents) != keepVersions+2 { // + current + write_token.sha256 (no temp files left)
		t.Fatalf("%d entries", len(ents))
	}
}

func TestInstallTemplates(t *testing.T) {
	s, ts := newTest(t, Config{})
	host := strings.TrimPrefix(ts.URL, "https://")
	r := do(t, ts, "GET", "/i", "", nil)
	sh := readBody(t, r)
	if !strings.Contains(sh, "SERVER='https://"+host+"'") || !strings.Contains(sh, "PIN='"+s.pin+"'") {
		t.Fatalf("install.sh:\n%s", sh)
	}
	if strings.Contains(sh, "{{") {
		t.Fatal("unrendered template")
	}
	ps := readBody(t, do(t, ts, "GET", "/i.ps1", "", nil))
	if !strings.Contains(ps, "$Server = 'https://"+host+"'") || !strings.Contains(ps, "-eq '"+s.certSHA+"'") ||
		!strings.Contains(ps, "$Pin = '"+s.pin+"'") {
		t.Fatalf("install.ps1:\n%s", ps)
	}
	// Hostile Host header falls back to the configured host.
	req, _ := http.NewRequest("GET", ts.URL+"/i", nil)
	req.Host = "x';rm -rf ~;'"
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(b), "rm -rf ~") || !strings.Contains(string(b), "SERVER='https://127.0.0.1'") {
		t.Fatalf("host injection:\n%s", b)
	}
	// Rendered script passes sh -n / shellcheck.
	f := filepath.Join(t.TempDir(), "i.sh")
	os.WriteFile(f, []byte(sh), 0o600)
	if out, err := exec.Command("sh", "-n", f).CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v %s", err, out)
	}
	if _, err := exec.LookPath("shellcheck"); err == nil {
		if out, err := exec.Command("shellcheck", "-s", "sh", f).CombinedOutput(); err != nil {
			t.Fatalf("shellcheck: %v %s", err, out)
		}
	}
	// Insecure mode: empty pin, http scheme.
	_, ts2 := newTest(t, Config{InsecureHTTP: true})
	sh2 := readBody(t, do(t, ts2, "GET", "/i", "", nil))
	if !strings.Contains(sh2, "SERVER='http://") || !strings.Contains(sh2, "PIN=''") {
		t.Fatalf("insecure install.sh:\n%s", sh2)
	}
}

func TestDownloadWhitelist(t *testing.T) {
	dist := t.TempDir()
	os.WriteFile(filepath.Join(dist, "akey-linux-amd64"), []byte("BIN"), 0o644)
	os.WriteFile(filepath.Join(dist, "SHA256SUMS"), []byte("x  akey-linux-amd64\n"), 0o644)
	os.WriteFile(filepath.Join(dist, "secret.txt"), []byte("nope"), 0o644)
	_, ts := newTest(t, Config{DistDir: dist})
	if r := do(t, ts, "GET", "/dl/akey-linux-amd64", "", nil); r.StatusCode != 200 || readBody(t, r) != "BIN" {
		t.Fatal(r.StatusCode)
	}
	if r := do(t, ts, "GET", "/dl/SHA256SUMS", "", nil); r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	for _, p := range []string{"secret.txt", "akey-darwin-arm64", "akey-windows-amd64", "akey-linux-386", "..%2fsecret.txt"} {
		if r := do(t, ts, "GET", "/dl/"+p, "", nil); r.StatusCode != 404 {
			t.Fatalf("%s: %d", p, r.StatusCode)
		}
	}
	_, ts2 := newTest(t, Config{})
	if r := do(t, ts2, "GET", "/dl/akey-linux-amd64", "", nil); r.StatusCode != 404 {
		t.Fatal(r.StatusCode)
	}
}

func TestBanner(t *testing.T) {
	s, err := newServer(Config{DataDir: t.TempDir(), Hosts: []string{"203.0.113.7", "vps.example"}}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	s.banner(&b, &net.TCPAddr{IP: net.IPv4zero, Port: 8787})
	out := b.String()
	for _, want := range []string{
		"curl -fsSk --pinnedpubkey '" + s.pin + "' https://203.0.113.7:8787/i | sh",
		"curl -fsSk --pinnedpubkey '" + s.pin + "' https://vps.example:8787/i | sh",
		"-eq '" + s.certSHA + "'};iex (New-Object Net.WebClient).DownloadString('https://203.0.113.7:8787/i.ps1')",
		s.adminToken,
		"panel: https://203.0.113.7:8787/",
		"panel: https://vps.example:8787/",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("banner missing %q:\n%s", want, out)
		}
	}
	t.Log("\n" + out)
	s2, _ := newServer(Config{DataDir: s.cfg.DataDir, InsecureHTTP: true}, t.Logf)
	b.Reset()
	s2.banner(&b, &net.TCPAddr{IP: net.IPv4zero, Port: 8787})
	if strings.Contains(b.String(), s.adminToken) || !strings.Contains(b.String(), "curl -fsSL http://127.0.0.1:8787/i | sh") ||
		!strings.Contains(b.String(), "WARNING") {
		t.Fatalf("insecure banner:\n%s", b.String())
	}
	t.Log("\n" + b.String())
}

// TestRealRun serves on a real TLS listener and checks curl's pin handling.
func TestRealRun(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	dist := t.TempDir()
	fake := "#!/bin/sh\necho \"FAKE-AKEY $*\"\n"
	name := "akey-" + runtime.GOOS + "-" + runtime.GOARCH
	os.WriteFile(filepath.Join(dist, name), []byte(fake), 0o644)
	sum := sha256.Sum256([]byte(fake))
	os.WriteFile(filepath.Join(dist, "SHA256SUMS"), []byte(hex.EncodeToString(sum[:])+"  "+name+"\n"), 0o644)
	s, err := newServer(Config{DataDir: t.TempDir(), DistDir: dist}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.serve(ln)
	t.Cleanup(func() { ln.Close() })
	base := "https://" + ln.Addr().String()

	out, err := exec.Command("curl", "-fsSk", "--pinnedpubkey", s.pin, base+"/v1/params").CombinedOutput()
	if err != nil || !strings.Contains(string(out), `"memory_kib":65536`) {
		t.Fatalf("pinned curl: %v %s", err, out)
	}
	out, err = exec.Command("curl", "-fsSk", "--pinnedpubkey", s.pin, base+"/dl/"+name).CombinedOutput()
	if err != nil || string(out) != fake {
		t.Fatalf("dl: %v %s", err, out)
	}
	// Full installer: curl | sh -s -- --only codex, with a fake akey binary.
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		home := t.TempDir()
		cmd := exec.Command("sh", "-c", "curl -fsSk --pinnedpubkey '"+s.pin+"' "+base+"/i | sh -s -- --only codex")
		cmd.Env = append(os.Environ(), "HOME="+home, "AKEY_BIN_DIR="+filepath.Join(home, "bin"))
		out, err := cmd.CombinedOutput()
		want := "FAKE-AKEY pull --server " + base + " --pin " + s.pin + " --only codex"
		if err != nil || !strings.Contains(string(out), want) {
			t.Fatalf("installer: %v\n%s", err, out)
		}
		if st, err := os.Stat(filepath.Join(home, "bin", "akey")); err != nil || st.Mode().Perm() != 0o755 {
			t.Fatalf("installed binary: %v", err)
		}
		t.Logf("installer output:\n%s", out)
		// Tampered checksum must abort before running anything.
		os.WriteFile(filepath.Join(dist, "SHA256SUMS"), []byte(strings.Repeat("0", 64)+"  "+name+"\n"), 0o644)
		cmd = exec.Command("sh", "-c", "curl -fsSk --pinnedpubkey '"+s.pin+"' "+base+"/i | sh")
		cmd.Env = append(os.Environ(), "HOME="+home, "AKEY_BIN_DIR="+filepath.Join(home, "bin2"))
		if out, err := cmd.CombinedOutput(); err == nil || strings.Contains(string(out), "FAKE-AKEY") || !strings.Contains(string(out), "checksum mismatch") {
			t.Fatalf("bad checksum not rejected: %v\n%s", err, out)
		}
	}
	wrong := "sha256//AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	if out, err := exec.Command("curl", "-fsSk", "--pinnedpubkey", wrong, base+"/v1/params").CombinedOutput(); err == nil {
		t.Fatalf("wrong pin accepted: %s", out)
	}
	if out, err := exec.Command("curl", "-fsS", base+"/v1/params").CombinedOutput(); err == nil {
		t.Fatalf("unverified self-signed accepted: %s", out)
	}
}

func TestInfoEndpoint(t *testing.T) {
	s, ts := newTest(t, Config{Version: "v9.8.7"})
	host := strings.TrimPrefix(ts.URL, "https://")
	r := do(t, ts, "GET", "/v1/info", "", nil)
	var info infoResponse
	if err := json.NewDecoder(r.Body).Decode(&info); err != nil || r.StatusCode != 200 {
		t.Fatalf("%d %v", r.StatusCode, err)
	}
	if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatal(r.Header)
	}
	if info.Version != "v9.8.7" || !info.TLS || info.Pin != s.pin || info.CertSHA256 != s.certSHA {
		t.Fatalf("%+v", info)
	}
	if info.Install.SH != "curl -fsSk --pinnedpubkey '"+s.pin+"' https://"+host+"/i | sh" ||
		!strings.Contains(info.Install.PS1, "-eq '"+s.certSHA+"'") ||
		!strings.HasSuffix(info.Install.PS1, "DownloadString('https://"+host+"/i.ps1')") {
		t.Fatalf("%+v", info.Install)
	}
	// Hostile Host header -> configured fallback.
	req, _ := http.NewRequest("GET", ts.URL+"/v1/info", nil)
	req.Host = "x';rm -rf ~;'"
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(b), "rm -rf") || !strings.Contains(string(b), "https://127.0.0.1/i") {
		t.Fatalf("host injection: %s", b)
	}
	// Insecure mode + default version.
	_, ts2 := newTest(t, Config{InsecureHTTP: true})
	var info2 infoResponse
	json.NewDecoder(do(t, ts2, "GET", "/v1/info", "", nil).Body).Decode(&info2)
	host2 := strings.TrimPrefix(ts2.URL, "http://")
	if info2.TLS || info2.Pin != "" || info2.CertSHA256 != "" || info2.Version != "dev" ||
		info2.Install.SH != "curl -fsSL http://"+host2+"/i | sh" || strings.Contains(info2.Install.PS1, "ServerCertificateValidationCallback") {
		t.Fatalf("%+v", info2)
	}
}

func TestVersionsEndpoints(t *testing.T) {
	s, ts := newTest(t, Config{})
	now := time.Now()
	s.lim.now = func() time.Time { return now }
	bodies := []string{`{"n":0}`, `{"n":11}`, `{"n":222}`}
	var vers []string
	for i, b := range bodies {
		admin := ""
		if i == 0 {
			admin = s.adminToken
		}
		r := put(t, s, ts, testID, testTok, admin, b)
		if r.StatusCode != 204 {
			t.Fatal(r.StatusCode)
		}
		vers = append(vers, r.Header.Get("X-Akey-Version"))
	}
	tok := map[string]string{"X-Akey-Write-Token": testTok}
	r := do(t, ts, "GET", "/v1/blob/"+testID+"/versions", "", tok)
	var list struct {
		Versions []struct {
			Version int64 `json:"version"`
			Size    int64 `json:"size"`
			Current bool  `json:"current"`
		} `json:"versions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&list); err != nil || r.StatusCode != 200 || len(list.Versions) != 3 {
		t.Fatalf("%d %v %+v", r.StatusCode, err, list)
	}
	for i, v := range list.Versions {
		want := len(bodies) - 1 - i // newest first
		if strconv.FormatInt(v.Version, 10) != vers[want] || v.Size != int64(len(bodies[want])) || v.Current != (i == 0) {
			t.Fatalf("entry %d: %+v (want %s)", i, v, vers[want])
		}
	}
	r = do(t, ts, "GET", "/v1/blob/"+testID+"/versions/"+vers[0], "", tok)
	if r.StatusCode != 200 || readBody(t, r) != bodies[0] || r.Header.Get("X-Akey-Version") != vers[0] ||
		r.Header.Get("Content-Type") != "application/json" {
		t.Fatal(r.StatusCode, r.Header)
	}
	for _, c := range []struct {
		path string
		hdr  map[string]string
		want int
	}{
		{"/versions/1", tok, 404},
		{"/versions/abc", tok, 400},
		{"/versions/-1", tok, 400},
		{"/versions/0" + vers[0], tok, 400},
		{"/versions", map[string]string{"X-Akey-Write-Token": "short"}, 400},
	} {
		if r := do(t, ts, "GET", "/v1/blob/"+testID+c.path, "", c.hdr); r.StatusCode != c.want {
			t.Fatalf("%s: %d want %d", c.path, r.StatusCode, c.want)
		}
	}
	if r := do(t, ts, "GET", "/v1/blob/abc/versions", "", tok); r.StatusCode != 400 {
		t.Fatal(r.StatusCode)
	}
	// Misses: missing token 401, wrong token 403, unknown id 404 -> 10 of them block the IP.
	other := strings.Repeat("c", 64)
	for i := 0; i < 3; i++ {
		if r := do(t, ts, "GET", "/v1/blob/"+testID+"/versions", "", nil); r.StatusCode != 401 {
			t.Fatal(r.StatusCode)
		}
		if r := do(t, ts, "GET", "/v1/blob/"+testID+"/versions/"+vers[0], "", map[string]string{"X-Akey-Write-Token": badTok}); r.StatusCode != 403 {
			t.Fatal(r.StatusCode)
		}
		if r := do(t, ts, "GET", "/v1/blob/"+other+"/versions", "", tok); r.StatusCode != 404 {
			t.Fatal(r.StatusCode)
		}
	}
	if r := do(t, ts, "GET", "/v1/blob/"+testID+"/versions", "", map[string]string{"X-Akey-Write-Token": badTok}); r.StatusCode != 403 {
		t.Fatal(r.StatusCode)
	}
	if r := do(t, ts, "GET", "/v1/blob/"+testID+"/versions", "", tok); r.StatusCode != 429 {
		t.Fatalf("not rate limited: %d", r.StatusCode)
	}
}

// fakeUpstream records the last request and serves model lists.
type fakeUpstream struct {
	srv  *httptest.Server
	last *http.Request
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	f := &fakeUpstream{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.last = r.Clone(r.Context())
		switch {
		case r.URL.Path == "/openai/v1/models":
			if r.Header.Get("Authorization") != "Bearer sk-test-openai" {
				http.Error(w, `{"error":"bad key sk-test-openai"}`, 401)
				return
			}
			io.WriteString(w, `{"object":"list","data":[{"id":"gpt-b","object":"model"},{"id":"gpt-a"},{"id":"gpt-a"},{"id":""}]}`)
		case r.URL.Path == "/router/models":
			io.WriteString(w, `{"data":[{"id":"z/m","name":"Zed M","context_length":200000},{"id":"a/m","name":"A M","context_length":"8192"}]}`)
		case r.URL.Path == "/v1/models":
			if r.Header.Get("anthropic-version") != "2023-06-01" || r.URL.Query().Get("limit") != "1000" {
				http.Error(w, "bad anthropic request", 400)
				return
			}
			if r.Header.Get("X-Api-Key") != "sk-test-ant" && r.Header.Get("Authorization") != "Bearer sk-test-ant" {
				http.Error(w, "unauthorized", 401)
				return
			}
			io.WriteString(w, `{"data":[{"type":"model","id":"claude-z","display_name":"Claude Z"},{"type":"model","id":"claude-a","display_name":"Claude A"}],"has_more":false}`)
		case r.URL.Path == "/big/models":
			w.Write([]byte(`{"data":[{"id":"`))
			w.Write(bytes.Repeat([]byte("a"), probeMaxResp))
			w.Write([]byte(`"}]}`))
		case r.URL.Path == "/html/models":
			io.WriteString(w, "<html>not json</html>")
		case r.URL.Path == "/bare/models":
			io.WriteString(w, `[{"id":"m2"},{"id":"m1"}]`)
		case r.URL.Path == "/loop/models":
			http.Redirect(w, r, "/loop/models", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func TestProbe(t *testing.T) {
	s, ts := newTest(t, Config{})
	now := time.Now()
	s.lim.now = func() time.Time { return now }
	up := newFakeUpstream(t)
	if r := put(t, s, ts, testID, testTok, s.adminToken, env1); r.StatusCode != 204 {
		t.Fatal(r.StatusCode)
	}
	owner := map[string]string{"X-Akey-Id": testID, "X-Akey-Write-Token": testTok}
	admin := map[string]string{"Authorization": "Bearer " + s.adminToken}
	probeBody := func(base, api, key, auth string) string {
		b, _ := json.Marshal(probeRequest{BaseURL: base, API: api, Key: key, Auth: auth})
		return string(b)
	}
	type model = probeModel
	get := func(hdr map[string]string, body string) (int, []model, string) {
		r := do(t, ts, "POST", "/v1/probe", body, hdr)
		raw := readBody(t, r)
		if strings.Contains(raw, "sk-test") {
			t.Fatalf("key echoed: %s", raw)
		}
		var out struct{ Models []model }
		if r.StatusCode == 200 {
			if err := json.Unmarshal([]byte(raw), &out); err != nil {
				t.Fatal(err)
			}
		}
		return r.StatusCode, out.Models, raw
	}

	code, ms, raw := get(owner, probeBody(up.srv.URL+"/openai/v1/", "openai", "sk-test-openai", ""))
	if code != 200 || fmt.Sprint(ms) != "[{gpt-a  0} {gpt-b  0}]" {
		t.Fatalf("openai: %d %v %s", code, ms, raw)
	}
	code, ms, raw = get(admin, probeBody(up.srv.URL+"/router", "openai", "", ""))
	if code != 200 || fmt.Sprint(ms) != "[{a/m A M 8192} {z/m Zed M 200000}]" || up.last.Header.Get("Authorization") != "" {
		t.Fatalf("openrouter: %d %v %s", code, ms, raw)
	}
	for _, base := range []string{up.srv.URL, up.srv.URL + "/", up.srv.URL + "/v1", up.srv.URL + "/v1/"} {
		code, ms, raw = get(owner, probeBody(base, "anthropic", "sk-test-ant", ""))
		if code != 200 || fmt.Sprint(ms) != "[{claude-a Claude A 0} {claude-z Claude Z 0}]" || up.last.Header.Get("Authorization") != "" {
			t.Fatalf("anthropic %s: %d %v %s", base, code, ms, raw)
		}
	}
	code, _, raw = get(owner, probeBody(up.srv.URL, "anthropic", "sk-test-ant", "bearer"))
	if code != 200 || up.last.Header.Get("X-Api-Key") != "" {
		t.Fatalf("anthropic bearer: %d %s", code, raw)
	}
	code, ms, _ = get(owner, probeBody(up.srv.URL+"/bare", "openai", "", ""))
	if code != 200 || fmt.Sprint(ms) != "[{m1  0} {m2  0}]" {
		t.Fatalf("bare: %d %v", code, ms)
	}
	// Upstream failures -> 502 with status, never the key.
	for _, c := range []struct{ body, want string }{
		{probeBody(up.srv.URL+"/openai/v1", "openai", "sk-test-wrong", ""), "HTTP 401"},
		{probeBody(up.srv.URL, "anthropic", "sk-test-wrong", "api_key"), "HTTP 401"},
		{probeBody(up.srv.URL+"/big", "openai", "", ""), "larger than"},
		{probeBody(up.srv.URL+"/html", "openai", "", ""), "not a model list"},
		{probeBody(up.srv.URL+"/nope", "openai", "", ""), "HTTP 404"},
		{probeBody(up.srv.URL+"/loop", "openai", "sk-test-x", ""), "redirects"},
		{probeBody("http://169.254.169.254/latest", "openai", "sk-test-x", ""), "refusing"},
		{probeBody("http://[fe80::1]:80/v1", "anthropic", "sk-test-x", ""), "refusing"},
		{probeBody("http://100.100.100.200/", "openai", "sk-test-x", ""), "refusing"},
		{probeBody("http://127.0.0.1:1", "openai", "sk-test-x", ""), "upstream request failed"},
	} {
		code, _, raw := get(owner, c.body)
		if code != 502 || !strings.Contains(raw, c.want) {
			t.Fatalf("%s: %d %s", c.body, code, raw)
		}
	}
	// Bad requests -> 400.
	for _, body := range []string{
		"not json",
		probeBody("ftp://example.com", "openai", "k", ""),
		probeBody("file:///etc/passwd", "openai", "k", ""),
		probeBody("example.com/v1", "openai", "k", ""),
		probeBody("http://user:pw@example.com", "openai", "k", ""),
		probeBody(up.srv.URL+"?x=1", "openai", "k", ""),
		probeBody(up.srv.URL, "gemini", "k", ""),
		probeBody(up.srv.URL, "anthropic", "k", "basic"),
		probeBody(up.srv.URL, "openai", "sk-test-a\r\nX: y", ""),
	} {
		if code, _, raw := get(owner, body); code != 400 {
			t.Fatalf("%s: %d %s", body, code, raw)
		}
	}
	big := `{"key":"` + strings.Repeat("a", probeMaxBody) + `"}`
	if code, _, _ := get(owner, big); code != 413 {
		t.Fatal(code)
	}
	// Auth failures count as misses: 10 -> blocked.
	body := probeBody(up.srv.URL+"/bare", "openai", "", "")
	bad := []map[string]string{
		nil,
		{"Authorization": "Bearer wrong-admin-token-xxxxxxxx"},
		{"X-Akey-Id": testID, "X-Akey-Write-Token": badTok},
		{"X-Akey-Id": strings.Repeat("c", 64), "X-Akey-Write-Token": testTok},
		{"X-Akey-Id": "nope", "X-Akey-Write-Token": testTok},
	}
	for i := 0; i < 2; i++ {
		for j, h := range bad {
			want := 403
			if h == nil {
				want = 401
			}
			if code, _, raw := get(h, body); code != want {
				t.Fatalf("bad[%d]: %d %s", j, code, raw)
			}
		}
	}
	if code, _, _ := get(admin, body); code != 429 {
		t.Fatalf("not rate limited: %d", code)
	}
}

func TestProbeRedirectStripsKey(t *testing.T) {
	var got http.Header
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		io.WriteString(w, `{"data":[{"id":"m"}]}`)
	}))
	defer final.Close()
	// "localhost" vs "127.0.0.1": a different host for the redirect.
	other := strings.Replace(final.URL, "127.0.0.1", "localhost", 1)
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other+"/v1/models", http.StatusTemporaryRedirect)
	}))
	defer first.Close()
	s, ts := newTest(t, Config{})
	for _, api := range []string{"openai", "anthropic"} {
		b, _ := json.Marshal(probeRequest{BaseURL: first.URL + "/v1", API: api, Key: "sk-test-secret"})
		r := do(t, ts, "POST", "/v1/probe", string(b), map[string]string{"Authorization": "Bearer " + s.adminToken})
		if raw := readBody(t, r); r.StatusCode != 200 || !strings.Contains(raw, `"id":"m"`) {
			t.Fatalf("%s: %d %s", api, r.StatusCode, raw)
		}
		if got.Get("Authorization") != "" || got.Get("X-Api-Key") != "" {
			t.Fatalf("%s: key forwarded across hosts: %v", api, got)
		}
	}
}

func TestPanel(t *testing.T) {
	wasm := bytes.Repeat([]byte("\x00asm wasm-bytes "), 4096)
	js := strings.Repeat("console.log('akey');\n", 200)
	s, ts := newTest(t, Config{})
	s.web = fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><title>akey panel</title>")},
		"akey.wasm":     {Data: wasm},
		"app.js":        {Data: []byte(js)},
		"style.css":     {Data: []byte("body{}")},
		"test/smoke.js": {Data: []byte("secret test")},
	}
	checkPanelHeaders := func(r *http.Response) {
		t.Helper()
		h := r.Header
		if h.Get("Content-Security-Policy") != panelCSP || h.Get("X-Frame-Options") != "DENY" ||
			h.Get("Referrer-Policy") != "no-referrer" || h.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("headers: %v", h)
		}
	}
	r := do(t, ts, "GET", "/", "", nil)
	if r.StatusCode != 200 || readBody(t, r) != "<!doctype html><title>akey panel</title>" ||
		r.Header.Get("Content-Type") != "text/html; charset=utf-8" || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatal(r.StatusCode, r.Header)
	}
	checkPanelHeaders(r)

	// Explicit Accept-Encoding disables the client's transparent decompression.
	r = do(t, ts, "GET", "/assets/akey.wasm", "", map[string]string{"Accept-Encoding": "gzip, deflate"})
	if r.StatusCode != 200 || r.Header.Get("Content-Type") != "application/wasm" || r.Header.Get("Content-Encoding") != "gzip" ||
		r.Header.Get("Vary") != "Accept-Encoding" || r.Header.Get("Cache-Control") != "no-cache" {
		t.Fatal(r.StatusCode, r.Header)
	}
	checkPanelHeaders(r)
	zr, err := gzip.NewReader(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(zr); !bytes.Equal(b, wasm) {
		t.Fatal("gzip body mismatch")
	}
	etag := r.Header.Get("ETag")
	if r := do(t, ts, "GET", "/assets/akey.wasm", "", map[string]string{"Accept-Encoding": "gzip", "If-None-Match": etag}); r.StatusCode != 304 {
		t.Fatal(r.StatusCode)
	}
	// Identity: different ETag, raw bytes.
	r = do(t, ts, "GET", "/assets/akey.wasm", "", map[string]string{"Accept-Encoding": "identity"})
	if b := readBody(t, r); !bytes.Equal([]byte(b), wasm) || r.Header.Get("Content-Encoding") != "" || r.Header.Get("ETag") == etag {
		t.Fatal(r.Header)
	}
	if r := do(t, ts, "GET", "/assets/akey.wasm", "", map[string]string{"Accept-Encoding": "gzip;q=0"}); r.Header.Get("Content-Encoding") != "" {
		t.Fatal("gzip;q=0 ignored")
	}
	r = do(t, ts, "GET", "/assets/app.js", "", nil) // transparent gzip
	if b := readBody(t, r); b != js || r.Header.Get("Content-Type") != "text/javascript; charset=utf-8" {
		t.Fatal(r.Header)
	}
	r = do(t, ts, "GET", "/assets/style.css", "", map[string]string{"Accept-Encoding": "gzip"})
	if b := readBody(t, r); b != "body{}" || r.Header.Get("Content-Type") != "text/css; charset=utf-8" {
		t.Fatal(r.Header, b) // tiny file: served uncompressed
	}
	for _, p := range []string{"/assets/test/smoke.js", "/assets/test", "/assets/missing.js", "/assets/..%2fserver.go",
		"/assets/", "/index.html", "/app.js", "/web/index.html"} {
		if r := do(t, ts, "GET", p, "", nil); r.StatusCode != 404 {
			t.Fatalf("%s: %d", p, r.StatusCode)
		}
	}
	if r := do(t, ts, "POST", "/", "", nil); r.StatusCode != 405 {
		t.Fatal(r.StatusCode)
	}
	if r := do(t, ts, "HEAD", "/assets/app.js", "", nil); r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}

	// No akey.wasm -> "not built" page at /, other files still served.
	s2, ts2 := newTest(t, Config{})
	s2.web = fstest.MapFS{"index.html": {Data: []byte("x")}, "app.js": {Data: []byte("y")}}
	r = do(t, ts2, "GET", "/", "", nil)
	if b := readBody(t, r); r.StatusCode != 200 || !strings.Contains(b, "make wasm") || !strings.Contains(b, "面板未构建") {
		t.Fatal(r.StatusCode, b)
	}
	checkPanelHeaders(r)
	if r := do(t, ts2, "GET", "/assets/app.js", "", nil); r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}

	// The real embedded web/ dir: / is the panel or the not-built page.
	_, ts3 := newTest(t, Config{})
	r = do(t, ts3, "GET", "/", "", nil)
	if b := readBody(t, r); r.StatusCode != 200 || !strings.Contains(strings.ToLower(b), "<!doctype html>") {
		t.Fatal(r.StatusCode, b)
	}
	checkPanelHeaders(r)
}

func TestPrintInfo(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "data")
	if err := PrintInfo(Config{DataDir: empty}, io.Discard); !IsNotInitialised(err) {
		t.Fatalf("want not-initialised error, got %v", err)
	}
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Fatal("PrintInfo created the data dir")
	}
	os.MkdirAll(empty, 0o755)
	if err := PrintInfo(Config{DataDir: empty}, io.Discard); !IsNotInitialised(err) || !strings.Contains(err.Error(), "akey serve") {
		t.Fatalf("got %v", err)
	}
	if ents, _ := os.ReadDir(empty); len(ents) != 0 {
		t.Fatalf("PrintInfo wrote %d entries", len(ents))
	}

	dir := t.TempDir()
	s, err := newServer(Config{DataDir: dir}, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := PrintInfo(Config{DataDir: dir, Hosts: []string{"203.0.113.9,vps.example"}, Listen: "0.0.0.0:9443"}, &b); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"admin token: " + s.adminToken,
		"panel: https://203.0.113.9:9443/",
		"panel: https://vps.example:9443/",
		"curl -fsSk --pinnedpubkey '" + s.pin + "' https://203.0.113.9:9443/i | sh",
		"cert sha256: " + s.certSHA,
		"does not start a server",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	t.Log("\n" + out)
	b.Reset()
	if err := PrintInfo(Config{DataDir: dir, InsecureHTTP: true, Listen: "8080"}, &b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "panel: http://127.0.0.1:8080/") || !strings.Contains(b.String(), s.adminToken) {
		t.Fatal(b.String())
	}
	if err := PrintInfo(Config{DataDir: dir, Listen: "nope:x:y"}, io.Discard); err == nil {
		t.Fatal("bad listen accepted")
	}
	// TLS mode on a dir that never had TLS.
	plain := t.TempDir()
	if _, err := newServer(Config{DataDir: plain, InsecureHTTP: true}, t.Logf); err != nil {
		t.Fatal(err)
	}
	if err := PrintInfo(Config{DataDir: plain}, io.Discard); !IsNotInitialised(err) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(filepath.Join(plain, "tls")); !os.IsNotExist(err) {
		t.Fatal("PrintInfo created tls/")
	}
}

func TestProbeDialControl(t *testing.T) {
	for addr, ok := range map[string]bool{
		"169.254.169.254:80": false, "[fe80::1]:443": false, "100.100.100.200:80": false, "[fd00:ec2::254]:80": false,
		"0.0.0.0:80": false, "224.0.0.1:80": false,
		"127.0.0.1:11434": true, "10.0.0.5:8000": true, "[::1]:80": true, "8.8.8.8:443": true,
	} {
		if err := probeDialControl("tcp", addr, nil); (err == nil) != ok {
			t.Errorf("%s: %v", addr, err)
		}
	}
}
