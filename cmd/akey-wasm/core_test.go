package main

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/xxddccaa/akey/internal/spec"
	"github.com/xxddccaa/akey/internal/vault"
)

func call(t *testing.T, fn string, args ...string) map[string]any {
	t.Helper()
	out, err := handlers[fn](args)
	if err != nil {
		t.Fatalf("%s: %v", fn, err)
	}
	return out
}

func example(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../testdata/master.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDecodeEncodeRoundTrip(t *testing.T) {
	src := example(t)
	js := asJSON(t, call(t, "decode", src)["json"])
	// provider order follows the file
	order := []string{"relay", "ds", "gateway", "openrouter", "claudeproxy", "hybrid"}
	last := -1
	for _, id := range order {
		i := strings.Index(js, `"`+id+`":{`)
		if i < 0 || i < last {
			t.Fatalf("provider %s out of order in %s", id, js)
		}
		last = i
	}
	if strings.Contains(js, `"wire":"responses"`) && strings.Count(js, `"wire"`) != 2 {
		t.Fatalf("decode must not fill defaults: %s", js)
	}
	y := call(t, "encode", js)["yaml"].(string)
	a, err := spec.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	b, err := spec.Parse([]byte(y))
	if err != nil {
		t.Fatalf("encoded yaml invalid: %v\n%s", err, y)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("round trip changed the config:\n%s", y)
	}
	if strings.Index(y, "relay:") > strings.Index(y, "hybrid:") {
		t.Fatalf("encode lost provider order:\n%s", y)
	}
	if !strings.Contains(y, "reasoning_efforts: [low, medium, high, xhigh, max]") {
		t.Fatalf("short lists should use flow style:\n%s", y)
	}
	if strings.Index(y, "  codex:") > strings.Index(y, "  pi:") {
		t.Fatalf("tools not in canonical order:\n%s", y)
	}
}

func TestEncodeRejectsUnknownFields(t *testing.T) {
	_, err := handlers["encode"]([]string{`{"providers":{"x":{"bogus":1}}}`})
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("want unknown-field error, got %v", err)
	}
}

func TestValidateSplitsErrors(t *testing.T) {
	out := call(t, "validate", "version: 1\nmodels:\n  - id: a\n    provider: nope\ntools:\n  codex: {}\n")
	errs := out["errors"].([]string)
	if len(errs) != 2 || out["valid"].(bool) {
		t.Fatalf("want 2 errors, got %q", errs)
	}
	for _, e := range errs {
		if strings.HasPrefix(e, "-") || strings.Contains(e, "invalid master config") {
			t.Fatalf("unsplit error item %q", e)
		}
	}
	out = call(t, "validate", "providers:\n  x:\n    bogus: 1\n")
	if errs := out["errors"].([]string); len(errs) != 1 || !strings.Contains(errs[0], "bogus") {
		t.Fatalf("unknown field: %q", errs)
	}
	if errs := call(t, "validate", example(t))["errors"].([]string); len(errs) != 0 {
		t.Fatalf("example should be valid: %q", errs)
	}
}

func TestRenderRedacts(t *testing.T) {
	files := call(t, "render", example(t), "codex")["files"].([]map[string]any)
	if len(files) == 0 {
		t.Fatal("no files")
	}
	all := call(t, "render", example(t), "")["files"].([]map[string]any)
	for _, f := range append(files, all...) {
		c := f["content"].(string)
		if strings.Contains(c, "sk-test-relay") || strings.Contains(c, "sk-test-hybrid") {
			t.Fatalf("%s leaks a key:\n%s", f["path"], c)
		}
	}
	if !strings.Contains(files[0]["content"].(string)+files[len(files)-1]["content"].(string), "sk-t…(redacted)") {
		t.Fatalf("expected masked key in codex output")
	}
	if files[0]["mode"] != "0600" && files[0]["mode"] != "0644" {
		t.Fatalf("mode = %v", files[0]["mode"])
	}
}

func TestCompat(t *testing.T) {
	out := call(t, "compat", example(t))
	codex := out["codex"].([]string)
	for _, id := range codex {
		if id == "openrouter-qwen-coder" || id == "hybrid-claude-opus" {
			t.Fatalf("codex catalog has incompatible %s", id)
		}
	}
	pi := out["pi"].([]string)
	if len(pi) != 4 {
		t.Fatalf("pi catalog limited to tools.pi.models: %v", pi)
	}
	if c := out["compatible"].(map[string][]string)["pi"]; len(c) <= len(pi) {
		t.Fatalf("compatible[pi] should ignore tools.pi.models: %v", c)
	}
	if a := out["providersApi"].(map[string][]string)["hybrid"]; len(a) != 2 {
		t.Fatalf("providersApi hybrid = %v", a)
	}
}

func TestSnippets(t *testing.T) {
	y := call(t, "toYAML", `{"b":1,"a":{"x":"line1\nline2","n":"123"}}`)["yaml"].(string)
	if !strings.HasPrefix(y, "b: 1\na:") || !strings.Contains(y, "|") || !strings.Contains(y, `"123"`) {
		t.Fatalf("toYAML:\n%s", y)
	}
	j := asJSON(t, call(t, "fromYAML", y)["json"])
	if j != `{"b":1,"a":{"x":"line1\nline2","n":"123"}}` {
		t.Fatalf("fromYAML = %s", j)
	}
	if _, err := handlers["fromYAML"]([]string{"a: [1"}); err == nil {
		t.Fatal("want parse error")
	}
}

func TestVaultRoundTrip(t *testing.T) {
	p, _ := vault.DefaultParams()
	p.MemoryKiB, p.Time, p.Threads = 8*1024, 1, 1
	pj, _ := json.Marshal(p)
	defer wipe()
	if _, err := handlers["open"]([]string{"{}"}); err == nil {
		t.Fatal("open while locked should fail")
	}
	d := call(t, "derive", "correct horse battery", string(pj))
	k, _ := vault.Derive("correct horse battery", p)
	if d["id"] != k.ID || d["writeToken"] != k.WriteToken {
		t.Fatal("derive mismatch with vault.Derive")
	}
	if _, err := handlers["seal"]([]string{"tools: {codex: {}}"}); err == nil {
		t.Fatal("seal must validate")
	}
	src := example(t)
	env := call(t, "seal", src)["envelope"].(string)
	pt, err := vault.Open(k, []byte(env))
	if err != nil || string(pt) != src {
		t.Fatalf("vault.Open of wasm seal: %v", err)
	}
	cli, _ := vault.Seal(k, []byte("version: 1\n"))
	if got := call(t, "open", string(cli))["yaml"]; got != "version: 1\n" {
		t.Fatalf("open = %q", got)
	}
	other, _ := vault.Derive("other", p)
	bad, _ := vault.Seal(other, []byte("x"))
	if _, err := handlers["open"]([]string{string(bad)}); err == nil || !strings.Contains(err.Error(), "解密失败") {
		t.Fatalf("want Chinese decrypt error, got %v", err)
	}
	call(t, "lock")
	if call(t, "unlocked")["unlocked"].(bool) {
		t.Fatal("still unlocked after lock")
	}
}
