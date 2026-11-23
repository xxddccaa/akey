package importer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xxddccaa/akey/internal/spec"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) string {
	d := t.TempDir()
	t.Setenv("HOME", d)
	t.Setenv("CODEX_HOME", filepath.Join(d, "codex"))
	t.Setenv("XDCODE_HOME", filepath.Join(d, "xdcode"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(d, "claude"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(d, "xdg"))
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(d, "pi"))
	return d
}

const xdcodeTOML = `model = "gpt-test-1"
model_provider = "Alpha"
model_reasoning_effort = "xhigh"
model_context_window = 1000000
approval_policy = "never"

[model_providers.Alpha]
name = "Alpha"
base_url = "https://alpha.example.invalid/v1"
experimental_bearer_token = "sk-test-alpha-000000"
wire_api = "responses"
supports_websockets = false

[model_providers.beta]
name = "Beta"
base_url = "https://beta.example.invalid/v1/"
experimental_bearer_token = "sk-test-beta-000000"

[[models]]
id = "alpha-gpt"
name = "GPT Test (Alpha)"
provider = "Alpha"
model = "gpt-test-1"
context_window = 1000000
auto_compact_token_limit = 850000
reasoning_efforts = ["low", "high"]
default_reasoning_effort = "high"
compatibility = "responses"

[models.reasoning_discovery]
source = "verified_probes"

[[models]]
id = "beta-x"
provider = "beta"
model = "x-1"

[features]
multi_agent = true

[tui]
status_line = ["model-name"]
screen_reader_detection_done = true

[tui.model_availability_nux]
"x" = 1

[projects."/tmp/x"]
trust_level = "trusted"
`

const codexTOML = `model = "gpt-5-test"
model_provider = "beta2"
model_context_window = 272000
model_max_output_tokens = 128000
personality = "pragmatic"

[model_providers.beta2]
name = "beta"
base_url = "https://beta.example.invalid/v1"
env_key = "BETA_KEY"
wire_api = "responses"

[model_providers.alpha]
name = "alpha other key"
base_url = "https://alpha.example.invalid/v1"
env_key = "ALPHA_OTHER"

[model_providers.ghost]
base_url = "https://ghost.example.invalid/v1"
env_key = "AKEY_TEST_UNSET_VAR"

[notice]
x = true
`

const claudeJSON = `{
  "env": {
    "ANTHROPIC_BASE_URL": "https://alpha.example.invalid",
    "ANTHROPIC_API_KEY": "sk-test-alpha-000000",
    "IS_SANDBOX": "1"
  },
  "model": "claude-test",
  "permissions": {"defaultMode": "bypassPermissions"},
  "cleanupPeriodDays": 30
}`

const opencodeJSONC = `{
  // comment
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "gamma": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Gamma",
      "options": {"baseURL": "https://gamma.example.invalid/v1", "apiKey": "{env:AKEY_TEST_GAMMA}", "timeout": 5},
      "models": {"g-1": {"name": "G One", "limit": {"context": 64000, "output": 8000}}},
    },
  },
  "model": "gamma/g-1",
  "theme": "dark", /* block */
}`

const piModels = `{"providers": {"delta": {"baseUrl": "https://delta.example.invalid", "api": "anthropic-messages", "apiKey": "sk-test-delta-0000",
  "models": [{"id": "d-1", "name": "D1", "contextWindow": 200000, "maxTokens": 32000, "input": ["text", "image"]}]}}}`

const piSettings = `{"defaultProvider": "delta", "defaultModel": "d-1", "defaultThinkingLevel": "high", "theme": "light", "lastChangelogVersion": "1"}`

func TestImportAll(t *testing.T) {
	d := setup(t)
	t.Setenv("ALPHA_OTHER", "sk-test-alpha-OTHER")
	t.Setenv("AKEY_TEST_GAMMA", "sk-test-gamma-0000")
	write(t, filepath.Join(d, "xdcode", "config.toml"), xdcodeTOML)
	write(t, filepath.Join(d, "xdcode", "AGENTS.md"), "be nice\n")
	write(t, filepath.Join(d, "codex", "config.toml"), codexTOML)
	write(t, filepath.Join(d, "codex", ".env"), "BETA_KEY=\"sk-test-beta-000000\"\n# c\n")
	write(t, filepath.Join(d, "claude", "settings.json"), claudeJSON)
	write(t, filepath.Join(d, "xdg", "opencode", "opencode.jsonc"), opencodeJSONC)
	write(t, filepath.Join(d, "pi", "models.json"), piModels)
	write(t, filepath.Join(d, "pi", "settings.json"), piSettings)

	m, warns, err := Import()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("warnings:\n  %s", strings.Join(warns, "\n  "))

	// codex beta2 (key via .env) dedupes into xdcode's beta (trailing slash ignored).
	if m.Providers["beta2"] != nil {
		t.Error("beta2 should dedupe into beta")
	}
	// codex "alpha" has a different key -> separate provider.
	if p := m.Providers["alpha"]; p == nil || p.Key != "sk-test-alpha-OTHER" {
		t.Errorf("alpha (other key) missing: %+v", p)
	}
	// claude endpoint merged into Alpha (same key + host).
	if p := m.Providers["Alpha"]; p == nil || p.Anthropic == nil || p.Anthropic.BaseURL != "https://alpha.example.invalid" {
		t.Errorf("anthropic not merged into Alpha: %+v", p)
	}
	if p := m.Providers["Alpha"]; p.Overrides["xdcode"]["supports_websockets"] != false {
		t.Errorf("xdcode provider overrides lost: %+v", p.Overrides)
	}
	if p := m.Providers["gamma"]; p == nil || p.OpenAI.Wire != spec.WireChat || p.Key != "sk-test-gamma-0000" {
		t.Errorf("gamma: %+v", p)
	}
	if p := m.Providers["delta"]; p == nil || p.Anthropic == nil || p.OpenAI != nil {
		t.Errorf("delta: %+v", p)
	}
	if p := m.Providers["ghost"]; p == nil || p.Key != "" {
		t.Errorf("ghost: %+v", p)
	}

	md := m.ModelByID("alpha-gpt")
	if md == nil || md.AutoCompact != 850000 || md.DefaultEffort != "high" || len(md.ReasoningEfforts) != 2 || md.Compatibility != "responses" {
		t.Fatalf("alpha-gpt: %+v", md)
	}
	if md.Overrides != nil {
		t.Errorf("reasoning_discovery should be dropped: %+v", md.Overrides)
	}

	tools := m.Tools
	if tools["xdcode"].DefaultModel != "alpha-gpt" || tools["xdcode"].DefaultEffort != "xhigh" {
		t.Errorf("xdcode tool: %+v", tools["xdcode"])
	}
	cx := m.ModelByID(tools["codex"].DefaultModel)
	if cx == nil || cx.Provider != "beta" || cx.Model != "gpt-5-test" || cx.ContextWindow != 272000 || cx.MaxOutput != 128000 {
		t.Errorf("codex default: %+v", cx)
	}
	cl := m.ModelByID(tools["claude"].DefaultModel)
	if cl == nil || cl.Provider != "Alpha" || cl.API != spec.APIAnthropic || cl.Model != "claude-test" {
		t.Errorf("claude default: %+v", cl)
	}
	if oc := m.ModelByID(tools["opencode"].DefaultModel); oc == nil || oc.Provider != "gamma" || oc.ContextWindow != 64000 || oc.MaxOutput != 8000 {
		t.Errorf("opencode default: %+v", oc)
	}
	if pi := m.ModelByID(tools["pi"].DefaultModel); pi == nil || pi.Provider != "delta" || pi.ContextWindow != 200000 || len(pi.Input) != 2 {
		t.Errorf("pi default: %+v", pi)
	}
	if tools["pi"].DefaultEffort != "high" {
		t.Errorf("pi effort: %+v", tools["pi"])
	}

	// extras: generated & machine-local keys removed.
	xe := tools["xdcode"].Extra
	for _, k := range []string{"model", "model_provider", "model_providers", "models", "projects", "model_context_window", "model_reasoning_effort"} {
		if _, ok := xe[k]; ok {
			t.Errorf("xdcode extra still has %s", k)
		}
	}
	tui := xe["tui"].(map[string]any)
	if _, ok := tui["model_availability_nux"]; ok || tui["screen_reader_detection_done"] != nil || tui["status_line"] == nil {
		t.Errorf("xdcode tui: %+v", tui)
	}
	if xe["approval_policy"] != "never" {
		t.Errorf("xdcode extra lost approval_policy: %+v", xe)
	}
	if _, ok := tools["codex"].Extra["notice"]; ok {
		t.Error("codex notice not dropped")
	}
	ce := tools["claude"].Extra
	env := ce["env"].(map[string]any)
	if _, ok := env["ANTHROPIC_API_KEY"]; ok || env["IS_SANDBOX"] != "1" || ce["model"] != nil || ce["cleanupPeriodDays"] != int64(30) {
		t.Errorf("claude extra: %+v", ce)
	}
	if oe := tools["opencode"].Extra; oe["theme"] != "dark" || oe["provider"] != nil || oe["$schema"] != nil {
		t.Errorf("opencode extra: %+v", oe)
	}
	if pe := tools["pi"].Extra; pe["theme"] != "light" || pe["defaultModel"] != nil || pe["lastChangelogVersion"] != nil {
		t.Errorf("pi extra: %+v", pe)
	}
	if tools["xdcode"].Files["AGENTS.md"] != "be nice\n" {
		t.Error("AGENTS.md not imported")
	}

	// The draft must round-trip through spec.Parse.
	out, err := Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spec.Parse(out); err != nil {
		t.Fatalf("draft does not validate: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "reasoning_efforts: [low, high]") {
		t.Errorf("expected flow-style lists:\n%s", out)
	}
	// Source files untouched.
	if b, _ := os.ReadFile(filepath.Join(d, "xdcode", "config.toml")); string(b) != xdcodeTOML {
		t.Error("importer modified a source file")
	}
}

func TestImportOnlyClaudeOAuth(t *testing.T) {
	d := setup(t)
	write(t, filepath.Join(d, "claude", "settings.json"), `{"permissions": {"defaultMode": "plan"}}`)
	m, _, err := Import()
	if err != nil {
		t.Fatal(err)
	}
	if m.ToolEnabled("claude") {
		t.Error("claude without endpoint should be disabled")
	}
}

func TestImportNothing(t *testing.T) {
	setup(t)
	if _, _, err := Import(); err == nil {
		t.Fatal("expected error with no configs")
	}
}

func TestStripJSONC(t *testing.T) {
	in := `{"a": "x // not a comment", /* c */ "b": [1, 2,], // tail
"c": "\"/*q*/\"",}`
	got := string(stripJSONC([]byte(in)))
	want := `{"a": "x // not a comment",  "b": [1, 2],` + " \n" + `"c": "\"/*q*/\""}`
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}
