package server

import (
	"bytes"
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
	"strings"
	"testing"
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
