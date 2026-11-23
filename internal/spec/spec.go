// Package spec defines master.yaml: the single source of truth that is
// encrypted, stored on the server, and rendered into each agent's config.
package spec

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Tool names. Order is the canonical render order.
const (
	ToolCodex    = "codex"
	ToolXdcode   = "xdcode"
	ToolClaude   = "claude"
	ToolOpencode = "opencode"
	ToolPi       = "pi"
)

var ToolNames = []string{ToolCodex, ToolXdcode, ToolClaude, ToolOpencode, ToolPi}

// API families a provider endpoint / model can speak.
const (
	APIOpenAI    = "openai"
	APIAnthropic = "anthropic"
)

// OpenAI wire protocols.
const (
	WireResponses = "responses"
	WireChat      = "chat"
)

// Anthropic auth styles.
const (
	AuthAPIKey = "api_key" // x-api-key header      -> ANTHROPIC_API_KEY
	AuthBearer = "bearer"  // Authorization: Bearer -> ANTHROPIC_AUTH_TOKEN
)

type Master struct {
	Version   int                  `yaml:"version"`
	Providers map[string]*Provider `yaml:"providers"`
	Models    []*Model             `yaml:"models"`
	Tools     map[string]*Tool     `yaml:"tools"`
}

type Provider struct {
	Name      string             `yaml:"name,omitempty"`
	Key       string             `yaml:"key,omitempty"`
	OpenAI    *OpenAIEndpoint    `yaml:"openai,omitempty"`
	Anthropic *AnthropicEndpoint `yaml:"anthropic,omitempty"`
	// Overrides are deep-merged into this provider's generated block for a
	// given tool, keyed by tool name, using that tool's native key names.
	// e.g. overrides: {xdcode: {supports_standalone_web_search: false}}
	Overrides map[string]map[string]any `yaml:"overrides,omitempty"`
}

type OpenAIEndpoint struct {
	BaseURL string `yaml:"base_url"`
	Wire    string `yaml:"wire,omitempty"` // responses (default) | chat
}

type AnthropicEndpoint struct {
	BaseURL string `yaml:"base_url"`
	Auth    string `yaml:"auth,omitempty"` // api_key (default) | bearer
}

type Model struct {
	ID       string `yaml:"id"`              // unique across the file
	Provider string `yaml:"provider"`        // key into Master.Providers
	Model    string `yaml:"model,omitempty"` // upstream model name; defaults to ID
	Name     string `yaml:"name,omitempty"`  // display name; defaults to Model
	// API selects which provider endpoint this model is served on.
	// Defaults to the provider's only endpoint, or openai if it has both.
	API              string   `yaml:"api,omitempty"`
	ContextWindow    int      `yaml:"context_window,omitempty"`
	AutoCompact      int      `yaml:"auto_compact,omitempty"`
	MaxOutput        int      `yaml:"max_output,omitempty"`
	ReasoningEfforts []string `yaml:"reasoning_efforts,omitempty"`
	DefaultEffort    string   `yaml:"default_effort,omitempty"`
	Input            []string `yaml:"input,omitempty"`         // text, image
	Compatibility    string   `yaml:"compatibility,omitempty"` // xdcode only; default "responses"
	// Tools restricts which tools get this model. Empty = every compatible tool.
	Tools []string `yaml:"tools,omitempty"`
	// Overrides are deep-merged into this model's generated entry per tool.
	Overrides map[string]map[string]any `yaml:"overrides,omitempty"`
}

type Tool struct {
	// Enabled defaults to true when the tool section exists.
	Enabled       *bool  `yaml:"enabled,omitempty"`
	DefaultModel  string `yaml:"default_model,omitempty"`  // Model.ID
	DefaultEffort string `yaml:"default_effort,omitempty"` // overrides the model's default_effort
	// Models limits the catalog for this tool. Empty = every compatible model.
	Models []string `yaml:"models,omitempty"`
	// Install is a shell command run by `akey pull --install-missing` when
	// the tool's binary is not on PATH.
	Install string `yaml:"install,omitempty"`
	// Extra is deep-merged into the tool's main generated document, using the
	// tool's native keys (TOML for codex/xdcode, JSON for the rest; for pi it
	// is merged into settings.json).
	Extra map[string]any `yaml:"extra,omitempty"`
	// Files are extra files written into the tool's config dir, e.g.
	// {"AGENTS.md": "..."} for codex. Paths are relative, slash-separated.
	Files map[string]string `yaml:"files,omitempty"`
}

// ---------------------------------------------------------------------------

func Load(path string) (*Master, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse decodes YAML strictly (unknown fields are errors) and validates.
func Parse(b []byte) (*Master, error) {
	m, err := Decode(b)
	if err != nil {
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Master) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Validate checks references and fills defaults in place.
func (m *Master) Validate() error {
	if m.Version == 0 {
		m.Version = 1
	}
	if m.Version != 1 {
		return fmt.Errorf("unsupported master config version %d", m.Version)
	}
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, fmt.Sprintf(f, a...)) }

	for id, p := range m.Providers {
		if p == nil {
			add("provider %q is empty", id)
			continue
		}
		if p.Name == "" {
			p.Name = id
		}
		if p.OpenAI == nil && p.Anthropic == nil {
			add("provider %q needs an openai or anthropic endpoint", id)
		}
		if p.OpenAI != nil {
			if p.OpenAI.BaseURL == "" {
				add("provider %q: openai.base_url is required", id)
			}
			if p.OpenAI.Wire == "" {
				p.OpenAI.Wire = WireResponses
			}
			if p.OpenAI.Wire != WireResponses && p.OpenAI.Wire != WireChat {
				add("provider %q: openai.wire must be responses or chat", id)
			}
		}
		if p.Anthropic != nil {
			if p.Anthropic.BaseURL == "" {
				add("provider %q: anthropic.base_url is required", id)
			}
			if p.Anthropic.Auth == "" {
				p.Anthropic.Auth = AuthAPIKey
			}
			if p.Anthropic.Auth != AuthAPIKey && p.Anthropic.Auth != AuthBearer {
				add("provider %q: anthropic.auth must be api_key or bearer", id)
			}
		}
		for t := range p.Overrides {
			if !IsTool(t) {
				add("provider %q: overrides for unknown tool %q", id, t)
			}
		}
	}

	seen := map[string]bool{}
	for i, md := range m.Models {
		if md == nil || md.ID == "" {
			add("models[%d]: id is required", i)
			continue
		}
		if seen[md.ID] {
			add("model id %q is duplicated", md.ID)
		}
		seen[md.ID] = true
		if md.Model == "" {
			md.Model = md.ID
		}
		if md.Name == "" {
			md.Name = md.Model
		}
		p := m.Providers[md.Provider]
		if p == nil {
			add("model %q: unknown provider %q", md.ID, md.Provider)
			continue
		}
		if md.API == "" {
			if p.OpenAI != nil {
				md.API = APIOpenAI
			} else {
				md.API = APIAnthropic
			}
		}
		switch md.API {
		case APIOpenAI:
			if p.OpenAI == nil {
				add("model %q: provider %q has no openai endpoint", md.ID, md.Provider)
			}
		case APIAnthropic:
			if p.Anthropic == nil {
				add("model %q: provider %q has no anthropic endpoint", md.ID, md.Provider)
			}
		default:
			add("model %q: api must be openai or anthropic", md.ID)
		}
		for _, t := range md.Tools {
			if !IsTool(t) {
				add("model %q: unknown tool %q", md.ID, t)
			}
		}
		for t := range md.Overrides {
			if !IsTool(t) {
				add("model %q: overrides for unknown tool %q", md.ID, t)
			}
		}
	}

	for name, t := range m.Tools {
		if !IsTool(name) {
			add("unknown tool %q (known: %s)", name, strings.Join(ToolNames, ", "))
			continue
		}
		if t == nil {
			t = &Tool{}
			m.Tools[name] = t
		}
		for _, id := range t.Models {
			if m.ModelByID(id) == nil {
				add("tools.%s.models: unknown model %q", name, id)
			}
		}
		if t.DefaultModel != "" {
			md := m.ModelByID(t.DefaultModel)
			switch {
			case md == nil:
				add("tools.%s.default_model: unknown model %q", name, t.DefaultModel)
			case !m.ModelAllowedFor(md, name):
				add("tools.%s.default_model: model %q is not usable by %s (api %s / tool filters)", name, md.ID, name, md.API)
			}
		}
		if !m.ToolEnabled(name) {
			continue
		}
		if t.DefaultModel == "" {
			add("tools.%s.default_model is required", name)
		}
	}

	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("invalid master config:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers used by renderers.

func IsTool(name string) bool {
	for _, t := range ToolNames {
		if t == name {
			return true
		}
	}
	return false
}

func (m *Master) ModelByID(id string) *Model {
	for _, md := range m.Models {
		if md != nil && md.ID == id {
			return md
		}
	}
	return nil
}

// ToolEnabled reports whether a tool section exists and is not disabled.
func (m *Master) ToolEnabled(name string) bool {
	t := m.Tools[name]
	if t == nil {
		return false
	}
	return t.Enabled == nil || *t.Enabled
}

// EnabledTools returns enabled tool names in canonical order.
func (m *Master) EnabledTools() []string {
	var out []string
	for _, n := range ToolNames {
		if m.ToolEnabled(n) {
			out = append(out, n)
		}
	}
	return out
}

// ToolSupports reports whether a tool can talk to a given API/wire.
//   - codex, xdcode: openai responses only
//   - claude: anthropic only
//   - opencode, pi: openai (responses or chat) and anthropic
func ToolSupports(tool, api, wire string) bool {
	switch tool {
	case ToolCodex, ToolXdcode:
		return api == APIOpenAI && wire == WireResponses
	case ToolClaude:
		return api == APIAnthropic
	case ToolOpencode, ToolPi:
		return api == APIOpenAI || api == APIAnthropic
	}
	return false
}

// Wire returns the effective wire for a model ("" for anthropic models).
func (m *Master) Wire(md *Model) string {
	if md.API == APIOpenAI {
		if p := m.Providers[md.Provider]; p != nil && p.OpenAI != nil {
			return p.OpenAI.Wire
		}
	}
	return ""
}

// ModelAllowedFor applies API compatibility plus the model's Tools filter
// (but not the tool's Models list).
func (m *Master) ModelAllowedFor(md *Model, tool string) bool {
	if !ToolSupports(tool, md.API, m.Wire(md)) {
		return false
	}
	if len(md.Tools) == 0 {
		return true
	}
	for _, t := range md.Tools {
		if t == tool {
			return true
		}
	}
	return false
}

// ModelsFor returns the catalog for a tool, in file order: compatible models,
// filtered by the tool's Models list if set. The default model is always
// included.
func (m *Master) ModelsFor(tool string) []*Model {
	t := m.Tools[tool]
	var allow map[string]bool
	if t != nil && len(t.Models) > 0 {
		allow = map[string]bool{}
		for _, id := range t.Models {
			allow[id] = true
		}
		if t.DefaultModel != "" {
			allow[t.DefaultModel] = true
		}
	}
	var out []*Model
	for _, md := range m.Models {
		if md == nil || !m.ModelAllowedFor(md, tool) {
			continue
		}
		if allow != nil && !allow[md.ID] {
			continue
		}
		out = append(out, md)
	}
	return out
}

// ProvidersFor returns provider IDs (sorted) used by ModelsFor(tool).
func (m *Master) ProvidersFor(tool string) []string {
	set := map[string]bool{}
	for _, md := range m.ModelsFor(tool) {
		set[md.Provider] = true
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Effort returns the effective default reasoning effort for a tool.
func (m *Master) Effort(tool string) string {
	t := m.Tools[tool]
	if t != nil && t.DefaultEffort != "" {
		return t.DefaultEffort
	}
	if t != nil {
		if md := m.ModelByID(t.DefaultModel); md != nil {
			return md.DefaultEffort
		}
	}
	return ""
}

// Secrets returns all provider keys, for redaction in logs/dry-run output.
func (m *Master) Secrets() []string {
	var out []string
	for _, p := range m.Providers {
		if p != nil && p.Key != "" {
			out = append(out, p.Key)
		}
	}
	return out
}
