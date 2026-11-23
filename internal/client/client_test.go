package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xxddccaa/akey/internal/render"
	"github.com/xxddccaa/akey/internal/vault"
)

// fakeServer implements the CONTRACT.md HTTP protocol in memory.
type fakeServer struct {
	mu        sync.Mutex
	params    vault.Params
	admin     string
	blobs     map[string][]byte
	versions  map[string]int
	tokens    map[string]string // id -> sha256(write token)
	failGet   int               // number of 404s served
	rateAfter int               // 404s before answering 429 (0 = never)
	puts      []http.Header
}

var reID = regexp.MustCompile(`^[0-9a-f]{64}$`)

func newFakeServer(t *testing.T) *fakeServer {
	salt := bytes.Repeat([]byte{7}, 32)
	return &fakeServer{
		params: vault.Params{Version: 1, Salt: salt, Time: 1, MemoryKiB: 8 * 1024, Threads: 1},
		admin:  "admin-test-token",
		blobs:  map[string][]byte{}, versions: map[string]int{}, tokens: map[string]string{},
	}
}

func hashTok(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/v1/params" && r.Method == http.MethodGet {
		json.NewEncoder(w).Encode(f.params)
		return
	}
	id, ok := strings.CutPrefix(r.URL.Path, "/v1/blob/")
	if !ok || !reID.MatchString(id) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodGet:
		b, ok := f.blobs[id]
		if !ok {
			f.failGet++
			if f.rateAfter > 0 && f.failGet > f.rateAfter {
				w.Header().Set("Retry-After", "60")
				http.Error(w, "slow down", http.StatusTooManyRequests)
				return
			}
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-Akey-Version", strconv.FormatInt(1_790_000_000_000_000_000+int64(f.versions[id]), 10))
		w.Write(b)
	case http.MethodPut:
		f.puts = append(f.puts, r.Header.Clone())
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
		if len(body) > 1<<20 {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		wt := r.Header.Get("X-Akey-Write-Token")
		if wt == "" {
			http.Error(w, "no write token", http.StatusUnauthorized)
			return
		}
		if h, exists := f.tokens[id]; exists {
			if h != hashTok(wt) {
				http.Error(w, "bad write token", http.StatusForbidden)
				return
			}
		} else {
			switch r.Header.Get("Authorization") {
			case "Bearer " + f.admin:
			case "":
				http.Error(w, "admin token required", http.StatusUnauthorized)
				return
			default:
				http.Error(w, "bad admin token", http.StatusForbidden)
				return
			}
			f.tokens[id] = hashTok(wt)
		}
		f.blobs[id] = body
		f.versions[id]++
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method", http.StatusMethodNotAllowed)
	}
}

func isolate(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("AKEY_CLIENT_CONFIG", filepath.Join(d, "client.json"))
	t.Setenv("AKEY_ADMIN_TOKEN", "")
	askAdminFn = func() (string, error) { return "", errNoTTY }
	t.Cleanup(func() { askAdminFn = askAdmin })
	var out bytes.Buffer
	Stdout, Stderr = &out, &out
	t.Cleanup(func() { Stdout, Stderr = os.Stdout, os.Stderr })
	return d
}

func TestPinVerification(t *testing.T) {
	isolate(t)
	srv := httptest.NewTLSServer(newFakeServer(t))
	defer srv.Close()
	pin := PinFromCert(srv.Certificate())
	ctx := context.Background()

	a, err := newAPI(Config{Server: srv.URL, Pin: pin})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.params(ctx); err != nil {
		t.Fatalf("correct pin failed: %v", err)
	}

	wrong := "sha256//" + strings.Repeat("A", 43) + "="
	a, _ = newAPI(Config{Server: srv.URL, Pin: wrong})
	if _, err := a.params(ctx); err == nil || !strings.Contains(err.Error(), "pin mismatch") {
		t.Fatalf("wrong pin should fail closed, got %v", err)
	}

	a, _ = newAPI(Config{Server: srv.URL})
	if _, err := a.params(ctx); err == nil {
		t.Fatal("self-signed cert without pin must fail normal verification")
	}

	if _, err := newAPI(Config{Server: "http://127.0.0.1:1", Pin: pin}); err == nil {
		t.Fatal("pin with http:// must be rejected")
	}
	if _, err := newAPI(Config{Server: srv.URL, Pin: "md5//x"}); err == nil {
		t.Fatal("malformed pin must be rejected")
	}
}

func TestAPIErrors(t *testing.T) {
	isolate(t)
	fs := newFakeServer(t)
	fs.rateAfter = 1
	srv := httptest.NewServer(fs)
	defer srv.Close()
	a, _ := newAPI(Config{Server: srv.URL})
	ctx := context.Background()
	id := strings.Repeat("a", 64)

	if _, _, err := a.getBlob(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if !strings.Contains(ErrNotFound.Error(), "wrong passphrase") {
		t.Fatal("404 message must mention wrong passphrase")
	}
	if _, _, err := a.getBlob(ctx, id); !errors.Is(err, ErrRateLimited) || !strings.Contains(err.Error(), "1m0s") {
		t.Fatalf("want ErrRateLimited with retry-after, got %v", err)
	}

	// PUT without admin token -> 401 -> ask -> retry.
	asked := 0
	ask := func() (string, error) { asked++; return "admin-test-token", nil }
	if err := putWithAdminRetry(ctx, a, id, []byte(`{}`), "wt", "", ask); err != nil {
		t.Fatalf("put with retry: %v", err)
	}
	if asked != 1 || len(fs.puts) != 2 {
		t.Fatalf("asked=%d puts=%d", asked, len(fs.puts))
	}
	if fs.puts[1].Get("Authorization") != "Bearer admin-test-token" || fs.puts[1].Get("X-Akey-Write-Token") != "wt" {
		t.Fatalf("bad headers: %v", fs.puts[1])
	}
	env, ver, err := a.getBlob(ctx, id)
	if err != nil || string(env) != "{}" || ver != "1790000000000000001" {
		t.Fatalf("get after put: %q %s %v", env, ver, err)
	}
	// Existing id: write token alone is enough; a wrong one is 403.
	if err := putWithAdminRetry(ctx, a, id, []byte(`{"v":2}`), "wt", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := putWithAdminRetry(ctx, a, id, []byte(`{}`), "other", "", nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("want ErrForbidden, got %v", err)
	}
	// New id with a wrong admin token: no prompt, clear error.
	id2 := strings.Repeat("b", 64)
	err = putWithAdminRetry(ctx, a, id2, []byte(`{}`), "wt", "nope", func() (string, error) { t.Fatal("must not ask"); return "", nil })
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("got %v", err)
	}
}

func TestResolveConfig(t *testing.T) {
	isolate(t)
	if _, err := resolveConfig("", "", false); err == nil {
		t.Fatal("no server should error")
	}
	if err := SaveConfig(Config{Server: "https://1.2.3.4:8787", Pin: "sha256//abc"}); err != nil {
		t.Fatal(err)
	}
	p, _ := ConfigPath()
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("client.json mode %v", st.Mode())
	}
	c, _ := resolveConfig("", "", false)
	if c.Server != "https://1.2.3.4:8787" || c.Pin != "sha256//abc" {
		t.Fatalf("saved: %+v", c)
	}
	c, _ = resolveConfig("1.2.3.4:8787/", "", false)
	if c.Server != "https://1.2.3.4:8787" || c.Pin != "sha256//abc" {
		t.Fatalf("same server keeps pin: %+v", c)
	}
	c, _ = resolveConfig("https://5.6.7.8:8787", "", false)
	if c.Pin != "" {
		t.Fatalf("other server must drop saved pin: %+v", c)
	}
	c, _ = resolveConfig("", "sha256//new", true)
	if c.Pin != "sha256//new" {
		t.Fatalf("flag pin: %+v", c)
	}
	// install.sh in insecure mode: --server http://... --pin ''
	c, err := resolveConfig("http://1.2.3.4:8787", "", true)
	if err != nil || c.Pin != "" || c.Server != "http://1.2.3.4:8787" {
		t.Fatalf("explicit empty pin: %+v %v", c, err)
	}
	if _, err := newAPI(c); err != nil {
		t.Fatalf("http + empty pin must work: %v", err)
	}
	// explicit empty pin on the saved https server -> normal CA verification
	c, _ = resolveConfig("https://1.2.3.4:8787", "", true)
	if c.Pin != "" {
		t.Fatalf("explicit empty pin must clear saved pin: %+v", c)
	}
}

func TestApplyPlanBackupsAndModes(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	toolDir := func(tool string) (string, error) { return filepath.Join(dir, tool), nil }
	files := []render.File{
		{Tool: "codex", RelPath: "config.toml", Content: []byte("a = 1\n"), Mode: 0o600},
		{Tool: "pi", RelPath: "sub/models.json", Content: []byte("{}\n"), Mode: 0o644},
	}
	var out bytes.Buffer
	plan, err := planFiles(files, toolDir)
	if err != nil {
		t.Fatal(err)
	}
	if plan[0].Status != StatusNew || plan[1].Status != StatusNew {
		t.Fatalf("statuses %v %v", plan[0].Status, plan[1].Status)
	}
	if _, err := applyPlan(plan, &out); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "codex", "config.toml")
	if st, _ := os.Stat(target); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	if st, _ := os.Stat(filepath.Join(dir, "pi", "sub")); st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", st.Mode())
	}
	if st, _ := os.Stat(filepath.Join(dir, "pi", "sub", "models.json")); st.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", st.Mode())
	}

	// Unchanged: no write, no backup.
	plan, _ = planFiles(files, toolDir)
	if plan[0].Status != StatusUnchanged {
		t.Fatal("expected unchanged")
	}
	res, _ := applyPlan(plan, &out)
	if res.Unchanged != 2 || len(listBackups(target)) != 0 {
		t.Fatalf("res %+v backups %v", res, listBackups(target))
	}

	// 7 changes -> only 5 newest backups kept; latest content wins.
	base := time.Date(2026, 11, 23, 10, 0, 0, 0, time.Local)
	defer func() { now = time.Now }()
	for i := 0; i < 7; i++ {
		now = func() time.Time { return base.Add(time.Duration(i) * time.Second) }
		files[0].Content = []byte("a = " + strconv.Itoa(i+2) + "\n")
		plan, _ = planFiles(files[:1], toolDir)
		if plan[0].Status != StatusChanged {
			t.Fatal("expected changed")
		}
		if _, err := applyPlan(plan, &out); err != nil {
			t.Fatal(err)
		}
	}
	bs := listBackups(target)
	if len(bs) != KeepBackups {
		t.Fatalf("backups: %v", bs)
	}
	if !strings.HasSuffix(bs[len(bs)-1], ".akey-bak-20261123-100006") {
		t.Fatalf("newest backup name %s", bs[len(bs)-1])
	}
	if b, _ := os.ReadFile(bs[len(bs)-1]); string(b) != "a = 7\n" {
		t.Fatalf("backup content %q", b)
	}
	if st, _ := os.Stat(bs[0]); st.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode %v", st.Mode())
	}
	if b, _ := os.ReadFile(target); string(b) != "a = 8\n" {
		t.Fatalf("content %q", b)
	}
	// Same-second collision gets a suffix instead of overwriting.
	files[0].Content = []byte("a = 9\n")
	plan, _ = planFiles(files[:1], toolDir)
	applyPlan(plan, &out)
	if bs := listBackups(target); !strings.HasSuffix(bs[len(bs)-1], "-100006-1") {
		t.Fatalf("collision: %v", bs)
	}
	// No temp files left behind.
	ents, _ := os.ReadDir(filepath.Join(dir, "codex"))
	for _, e := range ents {
		if strings.Contains(e.Name(), "akey-tmp") {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}

	// Symlinked target is written through, link preserved.
	real := filepath.Join(dir, "dotfiles", "settings.json")
	os.MkdirAll(filepath.Dir(real), 0o700)
	os.WriteFile(real, []byte("old"), 0o600)
	os.MkdirAll(filepath.Join(dir, "claude"), 0o700)
	os.Symlink(real, filepath.Join(dir, "claude", "settings.json"))
	plan, _ = planFiles([]render.File{{Tool: "claude", RelPath: "settings.json", Content: []byte("new"), Mode: 0o600}}, toolDir)
	applyPlan(plan, &out)
	if fi, _ := os.Lstat(filepath.Join(dir, "claude", "settings.json")); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced")
	}
	if b, _ := os.ReadFile(real); string(b) != "new" {
		t.Fatalf("symlink target %q", b)
	}

	// Unsafe paths are refused.
	if _, err := planFiles([]render.File{{Tool: "codex", RelPath: "../evil"}}, toolDir); err == nil {
		t.Fatal("expected unsafe path error")
	}
}

func TestRedactAndDiff(t *testing.T) {
	secret := "sk-test-abcdefghijkl1234"
	old := "model = \"a\"\nexperimental_bearer_token = \"sk-test-OLDOLDOLDOLD\"\nmax_tokens = 128000\nx = 1\n"
	nw := "model = \"b\"\nexperimental_bearer_token = \"" + secret + "\"\nmax_tokens = 128000\nx = 1\n"
	d := lineDiff(redact(old, []string{secret}), redact(nw, []string{secret}), 1)
	if strings.Contains(d, "sk-test") || strings.Contains(d, "OLDOLD") {
		t.Fatalf("secret leaked:\n%s", d)
	}
	if !strings.Contains(d, "- model = \"a\"") || !strings.Contains(d, "+ model = \"b\"") {
		t.Fatalf("diff:\n%s", d)
	}
	if strings.Contains(d, "x = 1") {
		t.Fatalf("context too wide:\n%s", d)
	}
	j := redact(`{"env": {"ANTHROPIC_API_KEY": "abc-123-secret", "IS_SANDBOX": "1"}, "maxTokens": 4096, "apiKey": "{env:FOO}"}`, nil)
	if strings.Contains(j, "abc-123-secret") || !strings.Contains(j, `"maxTokens": 4096`) || !strings.Contains(j, `"IS_SANDBOX": "1"`) {
		t.Fatalf("json redact: %s", j)
	}
	if redact("TOKEN=plainvalue\n", nil) != "TOKEN="+redacted+"\n" {
		t.Fatalf("env redact: %q", redact("TOKEN=plainvalue\n", nil))
	}
	if lineDiff("a\n", "a\n", 2) != "" {
		t.Fatal("equal should be empty")
	}
}

func TestParseOnly(t *testing.T) {
	got, err := ParseOnly("codex, Claude,codex")
	if err != nil || len(got) != 2 || got[1] != "claude" {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := ParseOnly("codex,vim"); err == nil {
		t.Fatal("expected error")
	}
	if got, _ := ParseOnly(""); got != nil {
		t.Fatal("empty should be nil")
	}
}

const testMaster = `version: 1
providers:
  fake:
    name: Fake
    key: sk-test-roundtrip-0000000000
    openai: {base_url: "https://api.example.invalid/v1"}
models:
  - id: fake-model
    provider: fake
    model: gpt-test
    context_window: 100000
tools:
  codex: {default_model: fake-model}
  xdcode: {default_model: fake-model}
`

// TestRoundTrip: push via the real vault to a fake server, then pull into
// temp tool dirs. Needs a working vault and render.
func TestRoundTrip(t *testing.T) {
	if _, err := vault.DefaultParams(); err != nil {
		t.Skip("vault not implemented yet")
	}
	d := isolate(t)
	fs := newFakeServer(t)
	srv := httptest.NewTLSServer(fs)
	defer srv.Close()
	pin := PinFromCert(srv.Certificate())
	t.Setenv("CODEX_HOME", filepath.Join(d, "codex"))
	t.Setenv("XDCODE_HOME", filepath.Join(d, "xdcode"))
	t.Setenv("AKEY_PASSPHRASE", "correct horse battery staple")
	ctx := context.Background()
	opts := Options{Server: srv.URL, Pin: pin}

	// Pull before anything exists -> clear not-found error.
	if err := Pull(ctx, opts); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	mf := filepath.Join(d, "master.yaml")
	os.WriteFile(mf, []byte(testMaster), 0o600)
	if err := Push(ctx, mf, opts); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("push without admin token: want 401, got %v", err)
	}
	opts.AdminToken = fs.admin
	if err := Push(ctx, mf, opts); err != nil {
		t.Fatalf("push: %v", err)
	}
	// Server never sees plaintext.
	for _, b := range fs.blobs {
		if bytes.Contains(b, []byte("sk-test-roundtrip")) {
			t.Fatal("plaintext key on server")
		}
	}
	// Client config saved; second pull needs no flags.
	if c, _ := LoadConfig(); c.Server != srv.URL || c.Pin != pin {
		t.Fatalf("client config %+v", c)
	}

	var out bytes.Buffer
	Stdout = &out
	if err := Pull(ctx, Options{DryRun: true}); err != nil {
		t.Fatalf("dry-run pull: %v", err)
	}
	if strings.Contains(out.String(), "sk-test-roundtrip") {
		t.Fatalf("dry run leaked secret:\n%s", out.String())
	}
	if ents, _ := os.ReadDir(filepath.Join(d, "codex")); len(ents) != 0 {
		t.Fatal("dry run wrote files")
	}
	if err := Pull(ctx, Options{Only: []string{"codex"}}); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if ents, _ := os.ReadDir(filepath.Join(d, "codex")); len(ents) == 0 {
		t.Fatal("pull wrote nothing for codex")
	}
	if ents, _ := os.ReadDir(filepath.Join(d, "xdcode")); len(ents) != 0 {
		t.Fatal("--only codex wrote xdcode")
	}

	// Wrong passphrase -> not found.
	t.Setenv("AKEY_PASSPHRASE", "wrong passphrase entirely")
	if err := Pull(ctx, Options{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong passphrase: %v", err)
	}
}

func TestRenderLocal(t *testing.T) {
	d := isolate(t)
	mf := filepath.Join(d, "master.yaml")
	os.WriteFile(mf, []byte(testMaster), 0o600)
	out := filepath.Join(d, "out")
	err := RenderLocal(mf, nil, out)
	if err != nil && strings.Contains(err.Error(), "not implemented") {
		t.Skip("render not implemented yet")
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"codex", "xdcode"} {
		if ents, _ := os.ReadDir(filepath.Join(out, tool)); len(ents) == 0 {
			t.Fatalf("nothing rendered for %s", tool)
		}
	}
}
