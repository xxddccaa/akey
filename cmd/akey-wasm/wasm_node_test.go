//go:build !(js && wasm)

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/xxddccaa/akey/internal/vault"
)

// TestWasmUnderNode builds akey.wasm, runs it under Node and checks that it
// interoperates with the Go vault (same id/token, each side opens the other's
// envelopes). Skipped when node is not installed or with -short.
func TestWasmUnderNode(t *testing.T) {
	if testing.Short() {
		t.Skip("-short")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	wasmExec := ""
	for _, d := range []string{"lib/wasm", "misc/wasm"} {
		p := filepath.Join(runtime.GOROOT(), d, "wasm_exec.js")
		if _, err := os.Stat(p); err == nil {
			wasmExec = p
			break
		}
	}
	if wasmExec == "" {
		t.Skip("wasm_exec.js not found in GOROOT")
	}
	dir := t.TempDir()
	wasm := filepath.Join(dir, "akey.wasm")
	build := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w -X main.version=test", "-o", wasm, ".")
	build.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm", "GOTOOLCHAIN=local")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build wasm: %v\n%s", err, out)
	}

	params, err := vault.DefaultParams()
	if err != nil {
		t.Fatal(err)
	}
	params.MemoryKiB, params.Time, params.Threads = 8*1024, 1, 2 // fast but real Argon2id
	const pass = "test passphrase 123"
	keys, err := vault.Derive(pass, params)
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile("../../testdata/master.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cliEnv, err := vault.Seal(keys, []byte("version: 1 # sealed by the Go CLI\n"))
	if err != nil {
		t.Fatal(err)
	}
	job, _ := json.Marshal(map[string]any{"params": params, "passphrase": pass, "envelope": string(cliEnv), "yaml": string(src)})
	jobPath := filepath.Join(dir, "job.json")
	if err := os.WriteFile(jobPath, job, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, "testdata/wasm_node.mjs", wasm, wasmExec, jobPath)
	cmd.Stderr = os.Stderr
	raw, err := cmd.Output()
	if err != nil {
		t.Fatalf("node: %v\n%.2000s", err, raw)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatalf("node output: %v\n%.2000s", err, raw)
	}
	out := map[string]map[string]any{}
	for k, v := range all {
		var m map[string]any
		if json.Unmarshal(v, &m) == nil {
			out[k] = m
		}
	}
	ok := func(name string) map[string]any {
		t.Helper()
		r := out[name]
		if r == nil || r["ok"] != true {
			t.Fatalf("%s: %v", name, r)
		}
		return r
	}
	notOK := func(name, want string) {
		t.Helper()
		r := out[name]
		if r == nil || r["ok"] != false || !strings.Contains(r["error"].(string), want) {
			t.Fatalf("%s: want error containing %q, got %v", name, want, r)
		}
	}
	if v := ok("version")["version"]; v != "test" {
		t.Fatalf("version = %v", v)
	}
	notOK("badArg", "字符串")
	notOK("lockedOpen", "已锁定")
	d := ok("derive")
	if d["id"] != keys.ID || d["writeToken"] != keys.WriteToken {
		t.Fatalf("wasm derive differs from Go: %v vs %s", d, keys.ID)
	}
	if _, leaked := d["enc"]; leaked {
		t.Fatal("encryption key crossed into JS")
	}
	if ok("unlocked")["unlocked"] != true {
		t.Fatal("not unlocked after derive")
	}
	if y := ok("open")["yaml"]; y != "version: 1 # sealed by the Go CLI\n" {
		t.Fatalf("wasm open of Go envelope = %q", y)
	}
	notOK("badEnvelope", "解密失败")
	env := ok("seal")["envelope"].(string)
	pt, err := vault.Open(keys, []byte(env))
	if err != nil || string(pt) != string(src) {
		t.Fatalf("Go open of wasm envelope: %v", err)
	}
	notOK("sealInvalid", "default_model")
	if errs := ok("validate")["errors"].([]any); len(errs) != 1 {
		t.Fatalf("validate errors = %v", errs)
	}
	if !strings.Contains(ok("encode")["yaml"].(string), "relay-gpt-6.1-sol") {
		t.Fatal("encode lost models")
	}
	if !strings.Contains(ok("template")["yaml"].(string), "providers:") {
		t.Fatal("template")
	}
	if len(ok("compat")["codex"].([]any)) == 0 {
		t.Fatal("compat codex empty")
	}
	files := ok("render")["files"].([]any)
	for _, f := range files {
		if c := f.(map[string]any)["content"].(string); strings.Contains(c, "sk-test-relay") {
			t.Fatal("render preview leaks key")
		}
	}
	notOK("renderMissing", "unknown tool")
	ok("lock")
	if out["afterLock"]["unlocked"] != false {
		t.Fatal("still unlocked after lock()")
	}
	t.Logf("wasm Argon2id (8 MiB, t=1): %s ms", all["deriveMs"])
}
