package spec

import (
	"reflect"
	"strings"
	"testing"
)

const base = `
providers:
  oa:
    key: sk-test-oa
    openai: {base_url: https://oa.example.com/v1}
  chat:
    name: Chat Co
    key: sk-test-chat
    openai: {base_url: https://chat.example.com/v1, wire: chat}
  an:
    key: sk-test-an
    anthropic: {base_url: https://an.example.com}
  both:
    key: sk-test-both
    openai: {base_url: https://both.example.com/v1}
    anthropic: {base_url: https://both.example.com, auth: bearer}
models:
  - {id: m-oa, provider: oa, model: gpt-x, default_effort: high}
  - {id: m-chat, provider: chat}
  - {id: m-an, provider: an, model: claude-x, name: Claude X}
  - {id: m-both-oa, provider: both}
  - {id: m-both-an, provider: both, api: anthropic, default_effort: xhigh}
  - {id: m-only-pi, provider: oa, tools: [pi]}
tools:
  codex: {default_model: m-oa}
  claude: {default_model: m-both-an}
  opencode: {default_model: m-chat}
  pi: {default_model: m-an, default_effort: low, models: [m-chat]}
  xdcode: {enabled: false}
`

func mustParse(t *testing.T, s string) *Master {
	t.Helper()
	m, err := Parse([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDefaults(t *testing.T) {
	m := mustParse(t, base)
	if m.Version != 1 {
		t.Errorf("version default = %d", m.Version)
	}
	if p := m.Providers["oa"]; p.Name != "oa" || p.OpenAI.Wire != WireResponses {
		t.Errorf("provider defaults: name=%q wire=%q", p.Name, p.OpenAI.Wire)
	}
	if p := m.Providers["chat"]; p.Name != "Chat Co" || p.OpenAI.Wire != WireChat {
		t.Errorf("explicit values overwritten: %+v", p)
	}
	if a := m.Providers["an"].Anthropic; a.Auth != AuthAPIKey {
		t.Errorf("anthropic auth default = %q", a.Auth)
	}
	if a := m.Providers["both"].Anthropic; a.Auth != AuthBearer {
		t.Errorf("anthropic auth = %q", a.Auth)
	}
	cases := map[string][3]string{ // id -> model, name, api
		"m-oa":      {"gpt-x", "gpt-x", APIOpenAI},
		"m-chat":    {"m-chat", "m-chat", APIOpenAI},
		"m-an":      {"claude-x", "Claude X", APIAnthropic},
		"m-both-oa": {"m-both-oa", "m-both-oa", APIOpenAI},
		"m-both-an": {"m-both-an", "m-both-an", APIAnthropic},
	}
	for id, want := range cases {
		md := m.ModelByID(id)
		if got := [3]string{md.Model, md.Name, md.API}; got != want {
			t.Errorf("%s: got %v want %v", id, got, want)
		}
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name, yaml, want string
	}{
		{"version", "version: 2\n", "unsupported master config version 2"},
		{"unknown field", "providers: {a: {nope: 1}}\n", "field nope not found"},
		{"no endpoint", "providers: {a: {key: k}}\n", `provider "a" needs an openai or anthropic endpoint`},
		{"empty provider", "providers: {a: }\n", `provider "a" is empty`},
		{"base url", "providers: {a: {openai: {wire: chat}}}\n", "openai.base_url is required"},
		{"anthropic base url", "providers: {a: {anthropic: {}}}\n", "anthropic.base_url is required"},
		{"bad wire", "providers: {a: {openai: {base_url: u, wire: grpc}}}\n", "openai.wire must be responses or chat"},
		{"bad auth", "providers: {a: {anthropic: {base_url: u, auth: basic}}}\n", "anthropic.auth must be api_key or bearer"},
		{"provider override tool", "providers: {a: {openai: {base_url: u}, overrides: {vim: {}}}}\n", `overrides for unknown tool "vim"`},
		{"model id", "models: [{provider: a}]\n", "models[0]: id is required"},
		{"dup model", "providers: {a: {openai: {base_url: u}}}\nmodels: [{id: x, provider: a}, {id: x, provider: a}]\n", `model id "x" is duplicated`},
		{"unknown provider", "models: [{id: x, provider: nope}]\n", `model "x": unknown provider "nope"`},
		{"api mismatch", "providers: {a: {openai: {base_url: u}}}\nmodels: [{id: x, provider: a, api: anthropic}]\n", `provider "a" has no anthropic endpoint`},
		{"api mismatch openai", "providers: {a: {anthropic: {base_url: u}}}\nmodels: [{id: x, provider: a, api: openai}]\n", `provider "a" has no openai endpoint`},
		{"bad api", "providers: {a: {openai: {base_url: u}}}\nmodels: [{id: x, provider: a, api: grpc}]\n", "api must be openai or anthropic"},
		{"model tool filter", "providers: {a: {openai: {base_url: u}}}\nmodels: [{id: x, provider: a, tools: [vim]}]\n", `model "x": unknown tool "vim"`},
		{"model override tool", "providers: {a: {openai: {base_url: u}}}\nmodels: [{id: x, provider: a, overrides: {vim: {}}}]\n", `model "x": overrides for unknown tool "vim"`},
		{"unknown tool", "tools: {vim: {}}\n", `unknown tool "vim"`},
		{"default required", "tools: {codex: {}}\n", "tools.codex.default_model is required"},
		{"default unknown", "tools: {codex: {default_model: x}}\n", `tools.codex.default_model: unknown model "x"`},
		{"tool models unknown", "tools: {codex: {enabled: false, models: [x]}}\n", `tools.codex.models: unknown model "x"`},
		{"default incompatible", "providers: {a: {anthropic: {base_url: u}}}\nmodels: [{id: x, provider: a}]\ntools: {codex: {default_model: x}}\n", `model "x" is not usable by codex`},
		{"default chat for codex", "providers: {a: {openai: {base_url: u, wire: chat}}}\nmodels: [{id: x, provider: a}]\ntools: {xdcode: {default_model: x}}\n", `model "x" is not usable by xdcode`},
		{"default filtered by model tools", "providers: {a: {openai: {base_url: u}}}\nmodels: [{id: x, provider: a, tools: [pi]}]\ntools: {codex: {default_model: x}}\n", `model "x" is not usable by codex`},
	}
	for _, c := range cases {
		_, err := Parse([]byte(c.yaml))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want error containing %q", c.name, err, c.want)
		}
	}
}

func TestErrorsAreCollectedAndSorted(t *testing.T) {
	_, err := Parse([]byte("providers: {b: {key: k}, a: {key: k}}\n"))
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	ia, ib := strings.Index(msg, `provider "a"`), strings.Index(msg, `provider "b"`)
	if ia < 0 || ib < 0 || ia > ib {
		t.Errorf("want both errors, sorted:\n%s", msg)
	}
}

func TestDisabledToolNeedsNoDefault(t *testing.T) {
	m := mustParse(t, base)
	if m.ToolEnabled(ToolXdcode) {
		t.Error("xdcode should be disabled")
	}
	if got, want := m.EnabledTools(), []string{ToolCodex, ToolClaude, ToolOpencode, ToolPi}; !reflect.DeepEqual(got, want) {
		t.Errorf("EnabledTools = %v, want %v", got, want)
	}
	// An empty tool section (`codex:`) is still enabled.
	if _, err := Parse([]byte("tools:\n  codex:\n")); err == nil || !strings.Contains(err.Error(), "default_model is required") {
		t.Errorf("empty tool section should be enabled and need default_model, got %v", err)
	}
}

func ids(ms []*Model) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

func TestToolSupports(t *testing.T) {
	cases := []struct {
		tool, api, wire string
		want            bool
	}{
		{ToolCodex, APIOpenAI, WireResponses, true},
		{ToolCodex, APIOpenAI, WireChat, false},
		{ToolCodex, APIAnthropic, "", false},
		{ToolXdcode, APIOpenAI, WireResponses, true},
		{ToolClaude, APIAnthropic, "", true},
		{ToolClaude, APIOpenAI, WireResponses, false},
		{ToolOpencode, APIOpenAI, WireChat, true},
		{ToolOpencode, APIAnthropic, "", true},
		{ToolPi, APIOpenAI, WireResponses, true},
		{"vim", APIOpenAI, WireResponses, false},
	}
	for _, c := range cases {
		if got := ToolSupports(c.tool, c.api, c.wire); got != c.want {
			t.Errorf("ToolSupports(%s,%s,%s) = %v", c.tool, c.api, c.wire, got)
		}
	}
}

func TestModelsAndProvidersFor(t *testing.T) {
	m := mustParse(t, base)
	cases := map[string]struct{ models, providers []string }{
		ToolCodex:    {[]string{"m-oa", "m-both-oa"}, []string{"both", "oa"}},
		ToolXdcode:   {[]string{"m-oa", "m-both-oa"}, []string{"both", "oa"}},
		ToolClaude:   {[]string{"m-an", "m-both-an"}, []string{"an", "both"}},
		ToolOpencode: {[]string{"m-oa", "m-chat", "m-an", "m-both-oa", "m-both-an"}, []string{"an", "both", "chat", "oa"}},
		// pi: models list [m-chat] + default m-an always included.
		ToolPi: {[]string{"m-chat", "m-an"}, []string{"an", "chat"}},
	}
	for tool, want := range cases {
		if got := ids(m.ModelsFor(tool)); !reflect.DeepEqual(got, want.models) {
			t.Errorf("ModelsFor(%s) = %v, want %v", tool, got, want.models)
		}
		if got := m.ProvidersFor(tool); !reflect.DeepEqual(got, want.providers) {
			t.Errorf("ProvidersFor(%s) = %v, want %v", tool, got, want.providers)
		}
	}
	if m.ModelAllowedFor(m.ModelByID("m-only-pi"), ToolOpencode) {
		t.Error("m-only-pi is restricted to pi")
	}
	if !m.ModelAllowedFor(m.ModelByID("m-only-pi"), ToolPi) {
		t.Error("m-only-pi should be allowed for pi")
	}
}

func TestWireEffortLookup(t *testing.T) {
	m := mustParse(t, base)
	if w := m.Wire(m.ModelByID("m-chat")); w != WireChat {
		t.Errorf("Wire(m-chat) = %q", w)
	}
	if w := m.Wire(m.ModelByID("m-both-an")); w != "" {
		t.Errorf("Wire(anthropic) = %q", w)
	}
	for tool, want := range map[string]string{
		ToolCodex:    "high",  // model default
		ToolClaude:   "xhigh", // model default
		ToolOpencode: "",      // none set
		ToolPi:       "low",   // tool overrides model
		"vim":        "",
	} {
		if got := m.Effort(tool); got != want {
			t.Errorf("Effort(%s) = %q, want %q", tool, got, want)
		}
	}
	if m.ModelByID("nope") != nil {
		t.Error("ModelByID(nope) should be nil")
	}
	if !IsTool(ToolPi) || IsTool("vim") {
		t.Error("IsTool")
	}
}

func TestSecrets(t *testing.T) {
	m := mustParse(t, base)
	m.Providers["nokey"] = &Provider{OpenAI: &OpenAIEndpoint{BaseURL: "u"}}
	got := m.Secrets()
	want := map[string]bool{"sk-test-oa": true, "sk-test-chat": true, "sk-test-an": true, "sk-test-both": true}
	if len(got) != len(want) {
		t.Fatalf("Secrets = %v", got)
	}
	for _, s := range got {
		if !want[s] {
			t.Errorf("unexpected secret %q", s)
		}
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	m := mustParse(t, base)
	b, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	m2, err := Parse(b)
	if err != nil {
		t.Fatalf("re-parse: %v\n%s", err, b)
	}
	if !reflect.DeepEqual(m, m2) {
		t.Errorf("round trip changed master:\n%s", b)
	}
}

func TestExampleParses(t *testing.T) {
	m, err := Load("../../testdata/master.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := m.EnabledTools(); !reflect.DeepEqual(got, ToolNames) {
		t.Errorf("example should enable every tool, got %v", got)
	}
	for _, s := range m.Secrets() {
		if !strings.HasPrefix(s, "sk-test-") {
			t.Errorf("example contains a non-test key %q", s)
		}
	}
}

func TestLoadMissing(t *testing.T) {
	if _, err := Load("/nonexistent/master.yaml"); err == nil {
		t.Error("expected error")
	}
}
