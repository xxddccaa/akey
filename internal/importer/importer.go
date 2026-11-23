// Package importer builds a draft master config from the agent configs
// already present on this machine. It only ever reads them.
package importer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/xxddccaa/akey/internal/paths"
	"github.com/xxddccaa/akey/internal/spec"
)

// Import reads ~/.xdcode, ~/.codex, ~/.claude, opencode and pi configs
// (honoring each tool's dir env var) and returns a draft master plus
// human-readable warnings. Secrets end up in the returned master; never
// print it unredacted.
func Import() (*spec.Master, []string, error) {
	b := &builder{
		m:        &spec.Master{Version: 1, Providers: map[string]*spec.Provider{}, Tools: map[string]*spec.Tool{}},
		defaults: map[string]string{},
		found:    map[string]bool{},
	}
	b.importTOMLTool(spec.ToolXdcode)
	b.importTOMLTool(spec.ToolCodex)
	b.importClaude()
	b.importOpencode()
	b.importPi()
	if len(b.m.Providers) == 0 && len(b.found) == 0 {
		return nil, b.warns, errors.New("no existing agent configs found to import")
	}
	b.finish()
	return b.m, b.warns, nil
}

type builder struct {
	m        *spec.Master
	warns    []string
	defaults map[string]string // tool -> model ID chosen from that tool's own config
	found    map[string]bool   // tools with a config file on this machine
}

func (b *builder) warn(f string, a ...any) { b.warns = append(b.warns, fmt.Sprintf(f, a...)) }

// ---------------------------------------------------------------------------
// providers & models

func normURL(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return strings.ToLower(s)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	return u.String()
}

func hostOf(s string) string {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

var reUnsafeID = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func sanitizeID(s string) string {
	s = reUnsafeID.ReplaceAllString(s, "-")
	return strings.Trim(s, "-.")
}

func (b *builder) sortedProviderIDs() []string {
	ids := make([]string, 0, len(b.m.Providers))
	for id := range b.m.Providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (b *builder) uniqueProvID(src, tool string) string {
	id := sanitizeID(src)
	if id == "" {
		id = tool
	}
	if b.m.Providers[id] == nil {
		return id
	}
	cand := id + "-" + tool
	for i := 2; b.m.Providers[cand] != nil; i++ {
		cand = fmt.Sprintf("%s-%s%d", id, tool, i)
	}
	return cand
}

func mergeOverrides(p *spec.Provider, tool string, ov map[string]any) {
	if len(ov) == 0 {
		return
	}
	if p.Overrides == nil {
		p.Overrides = map[string]map[string]any{}
	}
	if p.Overrides[tool] == nil {
		p.Overrides[tool] = ov
	}
}

// openaiProvider finds or creates a provider with this openai endpoint.
// Providers are deduped by (base_url, key).
func (b *builder) openaiProvider(tool, src, name, base, key, wire string, ov map[string]any) string {
	if wire != spec.WireChat {
		wire = spec.WireResponses
	}
	for _, id := range b.sortedProviderIDs() {
		p := b.m.Providers[id]
		if p.OpenAI != nil && normURL(p.OpenAI.BaseURL) == normURL(base) && p.Key == key {
			if p.OpenAI.Wire != wire {
				b.warn("%s: provider %q uses wire %q but %q (same endpoint) uses %q; kept %q", tool, src, wire, id, p.OpenAI.Wire, p.OpenAI.Wire)
			}
			mergeOverrides(p, tool, ov)
			return id
		}
	}
	for _, id := range b.sortedProviderIDs() {
		p := b.m.Providers[id]
		if p.OpenAI == nil && p.Anthropic != nil && p.Key == key && hostOf(p.Anthropic.BaseURL) == hostOf(base) {
			p.OpenAI = &spec.OpenAIEndpoint{BaseURL: base, Wire: wire}
			mergeOverrides(p, tool, ov)
			return id
		}
	}
	id := b.uniqueProvID(src, tool)
	p := &spec.Provider{Name: name, Key: key, OpenAI: &spec.OpenAIEndpoint{BaseURL: base, Wire: wire}}
	mergeOverrides(p, tool, ov)
	b.m.Providers[id] = p
	return id
}

// anthropicProvider finds or creates a provider with this anthropic
// endpoint, merging into an existing provider with the same key and host.
func (b *builder) anthropicProvider(tool, src, name, base, key, auth string) string {
	if auth != spec.AuthBearer {
		auth = spec.AuthAPIKey
	}
	for _, id := range b.sortedProviderIDs() {
		p := b.m.Providers[id]
		if p.Anthropic != nil && normURL(p.Anthropic.BaseURL) == normURL(base) && p.Key == key {
			return id
		}
	}
	for _, id := range b.sortedProviderIDs() {
		p := b.m.Providers[id]
		if p.Anthropic == nil && p.OpenAI != nil && p.Key == key && hostOf(p.OpenAI.BaseURL) == hostOf(base) {
			p.Anthropic = &spec.AnthropicEndpoint{BaseURL: base, Auth: auth}
			return id
		}
	}
	id := b.uniqueProvID(src, tool)
	b.m.Providers[id] = &spec.Provider{Name: name, Key: key, Anthropic: &spec.AnthropicEndpoint{BaseURL: base, Auth: auth}}
	return id
}

func (b *builder) modelByID(id string) *spec.Model { return b.m.ModelByID(id) }

// findModel looks up a model by provider + (catalog id or upstream name) + api.
func (b *builder) findModel(prov, name, api string) *spec.Model {
	for _, md := range b.m.Models {
		if md.Provider == prov && md.ID == name && (api == "" || md.API == api) {
			return md
		}
	}
	for _, md := range b.m.Models {
		if md.Provider == prov && md.Model == name && (api == "" || md.API == api) {
			return md
		}
	}
	return nil
}

func (b *builder) uniqueModelID(want string) string {
	want = sanitizeID(want)
	if want == "" {
		want = "model"
	}
	id := want
	for i := 2; b.modelByID(id) != nil; i++ {
		id = fmt.Sprintf("%s-%d", want, i)
	}
	return id
}

// ensureModel returns an existing model or appends a new one named
// "<provider>-<model>". API is always set here and simplified in finish.
func (b *builder) ensureModel(prov, upstream, api string, fill func(*spec.Model)) *spec.Model {
	if md := b.findModel(prov, upstream, api); md != nil {
		if fill != nil {
			fillMissing(md, fill)
		}
		return md
	}
	base := strings.ToLower(prov)
	up := sanitizeID(strings.ToLower(upstream))
	want := up
	if !strings.HasPrefix(up, base+"-") {
		want = base + "-" + up
	}
	md := &spec.Model{ID: b.uniqueModelID(want), Provider: prov, Model: upstream, API: api}
	if fill != nil {
		fill(md)
	}
	b.m.Models = append(b.m.Models, md)
	return md
}

// fillMissing applies fill to a scratch copy and copies over only fields
// the existing model lacks.
func fillMissing(md *spec.Model, fill func(*spec.Model)) {
	var t spec.Model
	fill(&t)
	if md.ContextWindow == 0 {
		md.ContextWindow = t.ContextWindow
	}
	if md.AutoCompact == 0 {
		md.AutoCompact = t.AutoCompact
	}
	if md.MaxOutput == 0 {
		md.MaxOutput = t.MaxOutput
	}
	if md.DefaultEffort == "" {
		md.DefaultEffort = t.DefaultEffort
	}
	if md.Name == "" {
		md.Name = t.Name
	}
	if len(md.Input) == 0 {
		md.Input = t.Input
	}
}

func (b *builder) tool(name string) *spec.Tool {
	t := b.m.Tools[name]
	if t == nil {
		t = &spec.Tool{}
		b.m.Tools[name] = t
	}
	return t
}

// ---------------------------------------------------------------------------
// codex & xdcode (config.toml)

// generated by akey (or machine-local) top-level keys for codex/xdcode.
var tomlDrop = []string{
	"model", "model_provider", "model_reasoning_effort",
	"model_context_window", "model_auto_compact_token_limit", "model_max_output_tokens",
	"model_providers", "models", "projects", "notice",
}

var providerGenerated = map[string]bool{
	"name": true, "base_url": true, "experimental_bearer_token": true, "env_key": true, "wire_api": true,
}

func (b *builder) importTOMLTool(tool string) {
	dir, err := paths.ToolDir(tool)
	if err != nil {
		b.warn("%s: %v", tool, err)
		return
	}
	path := filepath.Join(dir, "config.toml")
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		b.warn("%s: %v", tool, err)
		return
	}
	b.found[tool] = true
	var doc map[string]any
	if err := toml.Unmarshal(raw, &doc); err != nil {
		b.warn("%s: cannot parse %s: %v", tool, path, err)
		return
	}
	dotenv := map[string]string{}
	if tool == spec.ToolCodex {
		dotenv = readDotEnv(filepath.Join(dir, ".env"))
	}

	provIDs := map[string]string{}
	provs, _ := doc["model_providers"].(map[string]any)
	for _, src := range sortedKeys(provs) {
		pm, _ := provs[src].(map[string]any)
		base := str(pm["base_url"])
		if base == "" {
			b.warn("%s: provider %q has no base_url; skipped", tool, src)
			continue
		}
		key := str(pm["experimental_bearer_token"])
		if key == "" {
			if ek := str(pm["env_key"]); ek != "" {
				key = os.Getenv(ek)
				if key == "" {
					key = dotenv[ek]
				}
				if key == "" {
					b.warn("%s: provider %q: env var %s is not set (nor in %s/.env); key left empty", tool, src, ek, dir)
				}
			}
		}
		ov := map[string]any{}
		for k, v := range pm {
			if !providerGenerated[k] {
				ov[k] = v
			}
		}
		wire := spec.WireResponses
		if w := str(pm["wire_api"]); w == "chat" {
			wire = spec.WireChat
		}
		provIDs[src] = b.openaiProvider(tool, src, str(pm["name"]), base, key, wire, ov)
	}

	if tool == spec.ToolXdcode {
		b.importXdcodeCatalog(doc, provIDs)
	}

	t := b.tool(tool)
	if name := str(doc["model"]); name != "" {
		src := str(doc["model_provider"])
		if src == "" {
			src = "openai"
		}
		prov := provIDs[src]
		if prov == "" {
			b.warn("%s: default model %q uses provider %q, which is built in or undefined; set tools.%s.default_model yourself", tool, name, src, tool)
		} else {
			md := b.findModel(prov, name, spec.APIOpenAI)
			fill := func(md *spec.Model) {
				md.ContextWindow = num(doc["model_context_window"])
				md.AutoCompact = num(doc["model_auto_compact_token_limit"])
				md.MaxOutput = num(doc["model_max_output_tokens"])
				md.DefaultEffort = str(doc["model_reasoning_effort"])
			}
			if md == nil {
				md = b.ensureModel(prov, name, spec.APIOpenAI, fill)
			} else {
				fillMissing(md, fill)
			}
			t.DefaultModel = md.ID
			b.defaults[tool] = md.ID
			if e := str(doc["model_reasoning_effort"]); e != "" && e != md.DefaultEffort {
				t.DefaultEffort = e
			}
		}
	}

	extra := map[string]any{}
	for k, v := range doc {
		extra[k] = v
	}
	for _, k := range tomlDrop {
		delete(extra, k)
	}
	if tui, ok := extra["tui"].(map[string]any); ok {
		delete(tui, "model_availability_nux")
		delete(tui, "screen_reader_detection_done")
		if len(tui) == 0 {
			delete(extra, "tui")
		}
	}
	if len(extra) > 0 {
		t.Extra = extra
	}
	b.importFiles(tool, dir, "AGENTS.md")
}

var xdcodeModelKnown = map[string]bool{
	"id": true, "name": true, "provider": true, "model": true, "context_window": true,
	"auto_compact_token_limit": true, "reasoning_efforts": true, "default_reasoning_effort": true,
	"compatibility": true, "max_output_tokens": true,
}

func (b *builder) importXdcodeCatalog(doc map[string]any, provIDs map[string]string) {
	list, _ := doc["models"].([]any)
	dropped := 0
	for i, v := range list {
		mm, _ := v.(map[string]any)
		id, src := str(mm["id"]), str(mm["provider"])
		prov := provIDs[src]
		if id == "" || prov == "" {
			b.warn("xdcode: models[%d] (%q) references unknown provider %q; skipped", i, id, src)
			continue
		}
		upstream := str(mm["model"])
		if upstream == "" {
			upstream = id
		}
		if ex := b.modelByID(id); ex != nil {
			if ex.Provider == prov && ex.Model == upstream {
				continue
			}
			id = b.uniqueModelID(id)
		}
		md := &spec.Model{
			ID: sanitizeID(id), Provider: prov, Model: upstream, Name: str(mm["name"]), API: spec.APIOpenAI,
			ContextWindow:    num(mm["context_window"]),
			AutoCompact:      num(mm["auto_compact_token_limit"]),
			MaxOutput:        num(mm["max_output_tokens"]),
			ReasoningEfforts: strs(mm["reasoning_efforts"]),
			DefaultEffort:    str(mm["default_reasoning_effort"]),
			Compatibility:    str(mm["compatibility"]),
		}
		if md.ID != id {
			b.warn("xdcode: model id %q renamed to %q", id, md.ID)
		}
		ov := map[string]any{}
		for k, val := range mm {
			if xdcodeModelKnown[k] {
				continue
			}
			if k == "reasoning_discovery" { // cached probe results
				dropped++
				continue
			}
			ov[k] = val
		}
		if len(ov) > 0 {
			md.Overrides = map[string]map[string]any{spec.ToolXdcode: ov}
		}
		b.m.Models = append(b.m.Models, md)
	}
	if dropped > 0 {
		b.warn("xdcode: dropped %d cached reasoning_discovery record(s); xdcode re-probes when needed", dropped)
	}
}

// ---------------------------------------------------------------------------
// claude (settings.json)

var claudeGeneratedEnv = []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}

func (b *builder) importClaude() {
	dir, err := paths.ToolDir(spec.ToolClaude)
	if err != nil {
		b.warn("claude: %v", err)
		return
	}
	path := filepath.Join(dir, "settings.json")
	doc, ok := b.readJSON(spec.ToolClaude, path, false)
	if !ok {
		return
	}
	b.found[spec.ToolClaude] = true
	t := b.tool(spec.ToolClaude)
	env, _ := doc["env"].(map[string]any)
	base := str(env["ANTHROPIC_BASE_URL"])
	key, auth := str(env["ANTHROPIC_API_KEY"]), spec.AuthAPIKey
	if tok := str(env["ANTHROPIC_AUTH_TOKEN"]); tok != "" {
		if key != "" && key != tok {
			b.warn("claude: both ANTHROPIC_API_KEY and ANTHROPIC_AUTH_TOKEN are set; using ANTHROPIC_API_KEY")
		} else {
			key, auth = tok, spec.AuthBearer
		}
	}
	model := str(doc["model"])
	usedEnvModel := false
	if model == "" {
		if model = str(env["ANTHROPIC_MODEL"]); model != "" {
			usedEnvModel = true
		}
	}

	extra := map[string]any{}
	for k, v := range doc {
		extra[k] = v
	}
	delete(extra, "model")
	if env != nil {
		rest := map[string]any{}
		for k, v := range env {
			rest[k] = v
		}
		for _, k := range claudeGeneratedEnv {
			delete(rest, k)
		}
		if usedEnvModel {
			delete(rest, "ANTHROPIC_MODEL")
		}
		if len(rest) > 0 {
			extra["env"] = rest
		} else {
			delete(extra, "env")
		}
	}
	if len(extra) > 0 {
		t.Extra = extra
	}
	b.importFiles(spec.ToolClaude, dir, "CLAUDE.md")

	if base == "" && key == "" {
		b.warn("claude: settings.json has no ANTHROPIC_BASE_URL/API key (OAuth login?); tools.claude is disabled until you add a provider with an anthropic endpoint")
		f := false
		t.Enabled = &f
		return
	}
	if base == "" {
		base = "https://api.anthropic.com"
	}
	src := "anthropic"
	if h := hostOf(base); h != "" && h != "api.anthropic.com" {
		src = domainLabel(h)
	}
	prov := b.anthropicProvider(spec.ToolClaude, src, "", base, key, auth)
	if model == "" {
		model = "claude-sonnet-4-5"
		b.warn("claude: no model in settings.json; using %q, adjust tools.claude.default_model", model)
	}
	md := b.ensureModel(prov, model, spec.APIAnthropic, nil)
	t.DefaultModel = md.ID
	b.defaults[spec.ToolClaude] = md.ID
}

// domainLabel picks a readable id from a host: "mp.z01.example.com" -> "example".
func domainLabel(host string) string {
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	parts := strings.Split(host, ".")
	if len(parts) >= 2 && !isIPv4(host) {
		return sanitizeID(parts[len(parts)-2])
	}
	return sanitizeID(strings.ReplaceAll(host, ".", "-"))
}

var reIPv4 = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)

func isIPv4(s string) bool { return reIPv4.MatchString(s) }

// ---------------------------------------------------------------------------
// opencode (opencode.json / opencode.jsonc)

func (b *builder) importOpencode() {
	dir, err := paths.ToolDir(spec.ToolOpencode)
	if err != nil {
		b.warn("opencode: %v", err)
		return
	}
	doc := map[string]any{}
	foundAny := false
	for _, name := range []string{"opencode.json", "opencode.jsonc"} {
		d, ok := b.readJSON(spec.ToolOpencode, filepath.Join(dir, name), true)
		if !ok {
			continue
		}
		foundAny = true
		for k, v := range d {
			doc[k] = v
		}
	}
	if !foundAny {
		return
	}
	b.found[spec.ToolOpencode] = true
	t := b.tool(spec.ToolOpencode)

	provIDs := map[string]string{}
	apis := map[string]string{}
	provs, _ := doc["provider"].(map[string]any)
	for _, src := range sortedKeys(provs) {
		pm, _ := provs[src].(map[string]any)
		opts, _ := pm["options"].(map[string]any)
		npm := str(pm["npm"])
		base := str(opts["baseURL"])
		key := b.resolveOpencodeValue(str(opts["apiKey"]))
		api, wire := spec.APIOpenAI, spec.WireResponses
		switch {
		case npm == "@ai-sdk/anthropic" || (npm == "" && src == "anthropic"):
			api = spec.APIAnthropic
			if base == "" {
				base = "https://api.anthropic.com"
			}
		case npm == "@ai-sdk/openai-compatible":
			wire = spec.WireChat
		case npm == "@ai-sdk/openai" || (npm == "" && src == "openai"):
			if base == "" {
				base = "https://api.openai.com/v1"
			}
		default:
			b.warn("opencode: provider %q uses %q, which akey does not model; skipped (keep it via tools.opencode.extra)", src, npm)
			continue
		}
		if base == "" {
			b.warn("opencode: provider %q has no options.baseURL; skipped", src)
			continue
		}
		ov := map[string]any{}
		for k, v := range pm {
			if k != "npm" && k != "name" && k != "options" && k != "models" {
				ov[k] = v
			}
		}
		restOpts := map[string]any{}
		for k, v := range opts {
			if k != "baseURL" && k != "apiKey" {
				restOpts[k] = v
			}
		}
		if len(restOpts) > 0 {
			ov["options"] = restOpts
		}
		var prov string
		if api == spec.APIAnthropic {
			prov = b.anthropicProvider(spec.ToolOpencode, src, str(pm["name"]), base, key, spec.AuthAPIKey)
			if len(ov) > 0 {
				mergeOverrides(b.m.Providers[prov], spec.ToolOpencode, ov)
			}
		} else {
			prov = b.openaiProvider(spec.ToolOpencode, src, str(pm["name"]), base, key, wire, ov)
		}
		provIDs[src], apis[src] = prov, api
		models, _ := pm["models"].(map[string]any)
		for _, mid := range sortedKeys(models) {
			mm, _ := models[mid].(map[string]any)
			upstream := str(mm["id"])
			if upstream == "" {
				upstream = mid
			}
			b.ensureModel(prov, upstream, api, func(md *spec.Model) {
				md.Name = str(mm["name"])
				if lim, ok := mm["limit"].(map[string]any); ok {
					md.ContextWindow = num(lim["context"])
					md.MaxOutput = num(lim["output"])
				}
				if mod, ok := mm["modalities"].(map[string]any); ok {
					md.Input = strs(mod["input"])
				}
			})
		}
	}
	if ref := str(doc["model"]); ref != "" {
		src, mid, ok := strings.Cut(ref, "/")
		if prov := provIDs[src]; ok && prov != "" {
			md := b.ensureModel(prov, mid, apis[src], nil)
			t.DefaultModel = md.ID
			b.defaults[spec.ToolOpencode] = md.ID
		} else {
			b.warn("opencode: default model %q uses a provider akey did not import; pick tools.opencode.default_model yourself", ref)
		}
	}
	extra := map[string]any{}
	for k, v := range doc {
		if k != "provider" && k != "model" && k != "$schema" {
			extra[k] = v
		}
	}
	if len(extra) > 0 {
		t.Extra = extra
	}
	b.importFiles(spec.ToolOpencode, dir, "AGENTS.md")
}

var reEnvRef = regexp.MustCompile(`^\{env:([A-Za-z_][A-Za-z0-9_]*)\}$`)

func (b *builder) resolveOpencodeValue(v string) string {
	if m := reEnvRef.FindStringSubmatch(v); m != nil {
		if val := os.Getenv(m[1]); val != "" {
			return val
		}
		b.warn("opencode: env var %s is not set; key left empty", m[1])
		return ""
	}
	if strings.HasPrefix(v, "{file:") && strings.HasSuffix(v, "}") {
		p := strings.TrimSuffix(strings.TrimPrefix(v, "{file:"), "}")
		if strings.HasPrefix(p, "~/") {
			if h, err := os.UserHomeDir(); err == nil {
				p = filepath.Join(h, p[2:])
			}
		}
		if raw, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(raw))
		}
		b.warn("opencode: cannot read key file %s; key left empty", p)
		return ""
	}
	return v
}

// ---------------------------------------------------------------------------
// pi (~/.pi/agent/models.json + settings.json)

var piDropSettings = []string{"defaultProvider", "defaultModel", "defaultThinkingLevel", "lastChangelogVersion"}

var reEnvName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

func (b *builder) importPi() {
	dir, err := paths.ToolDir(spec.ToolPi)
	if err != nil {
		b.warn("pi: %v", err)
		return
	}
	models, okM := b.readJSON(spec.ToolPi, filepath.Join(dir, "models.json"), true)
	settings, okS := b.readJSON(spec.ToolPi, filepath.Join(dir, "settings.json"), true)
	if !okM && !okS {
		return
	}
	b.found[spec.ToolPi] = true
	t := b.tool(spec.ToolPi)
	provIDs := map[string]string{}
	apis := map[string]string{}
	provs, _ := models["providers"].(map[string]any)
	for _, src := range sortedKeys(provs) {
		pm, _ := provs[src].(map[string]any)
		base := str(pm["baseUrl"])
		key := str(pm["apiKey"])
		if strings.HasPrefix(key, "!") {
			b.warn("pi: provider %q gets its key from a command; key left empty", src)
			key = ""
		} else if reEnvName.MatchString(key) {
			if v := os.Getenv(key); v != "" {
				key = v
			} else {
				b.warn("pi: provider %q: env var %s is not set; key left empty", src, key)
				key = ""
			}
		}
		if base == "" {
			b.warn("pi: provider %q has no baseUrl (built-in provider?); skipped", src)
			continue
		}
		var prov, api string
		switch str(pm["api"]) {
		case "openai-responses", "":
			api = spec.APIOpenAI
			prov = b.openaiProvider(spec.ToolPi, src, str(pm["name"]), base, key, spec.WireResponses, nil)
		case "openai-completions":
			api = spec.APIOpenAI
			prov = b.openaiProvider(spec.ToolPi, src, str(pm["name"]), base, key, spec.WireChat, nil)
		case "anthropic-messages":
			api = spec.APIAnthropic
			prov = b.anthropicProvider(spec.ToolPi, src, str(pm["name"]), base, key, spec.AuthAPIKey)
		default:
			b.warn("pi: provider %q uses api %q, which akey does not model; skipped", src, str(pm["api"]))
			continue
		}
		provIDs[src], apis[src] = prov, api
		list, _ := pm["models"].([]any)
		for _, v := range list {
			mm, _ := v.(map[string]any)
			id := str(mm["id"])
			if id == "" {
				continue
			}
			b.ensureModel(prov, id, api, func(md *spec.Model) {
				md.Name = str(mm["name"])
				md.ContextWindow = num(mm["contextWindow"])
				md.MaxOutput = num(mm["maxTokens"])
				md.Input = strs(mm["input"])
			})
		}
	}
	if okS {
		if src, mid := str(settings["defaultProvider"]), str(settings["defaultModel"]); mid != "" {
			if prov := provIDs[src]; prov != "" {
				md := b.ensureModel(prov, mid, apis[src], nil)
				t.DefaultModel = md.ID
				b.defaults[spec.ToolPi] = md.ID
			} else {
				b.warn("pi: default model %s/%s uses a provider akey did not import", src, mid)
			}
		}
		if e := str(settings["defaultThinkingLevel"]); e != "" {
			t.DefaultEffort = e
		}
		extra := map[string]any{}
		for k, v := range settings {
			extra[k] = v
		}
		for _, k := range piDropSettings {
			delete(extra, k)
		}
		if len(extra) > 0 {
			t.Extra = extra
		}
	}
	b.importFiles(spec.ToolPi, dir, "AGENTS.md")
}

// ---------------------------------------------------------------------------
// finishing

func (b *builder) finish() {
	// Every tool gets a section so one master configures all five tools.
	fallbackOrder := []string{spec.ToolXdcode, spec.ToolCodex, spec.ToolOpencode, spec.ToolPi, spec.ToolClaude}
	for _, tool := range spec.ToolNames {
		t := b.tool(tool)
		if t.DefaultModel != "" || (t.Enabled != nil && !*t.Enabled) {
			continue
		}
		var pick string
		for _, from := range fallbackOrder {
			if md := b.modelByID(b.defaults[from]); md != nil && b.m.ModelAllowedFor(md, tool) {
				pick = md.ID
				break
			}
		}
		if pick == "" {
			for _, md := range b.m.Models {
				if b.m.ModelAllowedFor(md, tool) {
					pick = md.ID
					break
				}
			}
		}
		where := "its config has no default model"
		if !b.found[tool] {
			where = "no local config found"
		}
		if pick == "" {
			f := false
			t.Enabled = &f
			b.warn("%s: %s and no compatible model exists; section disabled", tool, where)
			continue
		}
		t.DefaultModel = pick
		b.warn("%s: %s; default_model set to %q (delete the section or set enabled: false if you do not use %s)", tool, where, pick, tool)
	}

	used := map[string]bool{}
	for _, md := range b.m.Models {
		used[md.Provider] = true
	}
	for _, id := range b.sortedProviderIDs() {
		p := b.m.Providers[id]
		if !used[id] {
			b.warn("provider %q has no models; add models for it or delete it", id)
		}
		if p.Key == "" {
			b.warn("provider %q has no key", id)
		}
		if p.Name == id {
			p.Name = ""
		}
	}
	// Drop fields equal to what spec.Validate would infer, for a tidy file.
	for _, md := range b.m.Models {
		p := b.m.Providers[md.Provider]
		if p != nil && ((md.API == spec.APIOpenAI && p.OpenAI != nil) || (md.API == spec.APIAnthropic && p.OpenAI == nil)) {
			md.API = ""
		}
		if md.Name == md.Model || (md.Model == "" && md.Name == md.ID) {
			md.Name = ""
		}
		if md.Model == md.ID {
			md.Model = ""
		}
	}
	for _, p := range b.m.Providers {
		if p.OpenAI != nil && p.OpenAI.Wire == spec.WireResponses {
			p.OpenAI.Wire = ""
		}
		if p.Anthropic != nil && p.Anthropic.Auth == spec.AuthAPIKey {
			p.Anthropic.Auth = ""
		}
	}

	// Validate a copy so the returned draft stays tidy.
	if out, err := b.m.Marshal(); err != nil {
		b.warn("cannot serialize draft: %v", err)
	} else if _, err := spec.Parse(out); err != nil {
		b.warn("draft does not validate yet, fix before pushing: %v", err)
	}
}

// ---------------------------------------------------------------------------
// helpers

func (b *builder) importFiles(tool, dir string, names ...string) {
	for _, n := range names {
		p := filepath.Join(dir, n)
		st, err := os.Stat(p)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		if st.Size() > 256<<10 {
			b.warn("%s: %s is larger than 256 KiB; not imported", tool, p)
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		t := b.tool(tool)
		if t.Files == nil {
			t.Files = map[string]string{}
		}
		t.Files[n] = string(raw)
	}
}

// readJSON reads a JSON (optionally JSONC) object file.
func (b *builder) readJSON(tool, path string, jsonc bool) (map[string]any, bool) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		b.warn("%s: %v", tool, err)
		return nil, false
	}
	if jsonc {
		raw = stripJSONC(raw)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		b.warn("%s: cannot parse %s: %v", tool, path, err)
		return nil, false
	}
	return normJSON(doc).(map[string]any), true
}

// normJSON converts json.Number into int64/float64 so YAML output is clean.
func normJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = normJSON(e)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = normJSON(e)
		}
		return x
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		f, _ := x.Float64()
		return f
	}
	return v
}

// stripJSONC removes // and /* */ comments and trailing commas.
func stripJSONC(b []byte) []byte {
	out := make([]byte, 0, len(b))
	inStr, esc := false, false
	for i := 0; i < len(b); i++ {
		c := b[i]
		if inStr {
			out = append(out, c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch {
		case c == '"':
			inStr = true
			out = append(out, c)
		case c == '/' && i+1 < len(b) && b[i+1] == '/':
			for i < len(b) && b[i] != '\n' {
				i++
			}
			if i < len(b) {
				out = append(out, '\n')
			}
		case c == '/' && i+1 < len(b) && b[i+1] == '*':
			end := bytes.Index(b[i+2:], []byte("*/"))
			if end < 0 {
				i = len(b)
			} else {
				i += end + 3
			}
		default:
			out = append(out, c)
		}
	}
	// trailing commas
	res := make([]byte, 0, len(out))
	inStr, esc = false, false
	for i := 0; i < len(out); i++ {
		c := out[i]
		if inStr {
			res = append(res, c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
		}
		if c == ',' {
			j := i + 1
			for j < len(out) && (out[j] == ' ' || out[j] == '\t' || out[j] == '\n' || out[j] == '\r') {
				j++
			}
			if j < len(out) && (out[j] == '}' || out[j] == ']') {
				continue
			}
		}
		res = append(res, c)
	}
	return res
}

func readDotEnv(path string) map[string]string {
	out := map[string]string{}
	raw, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		out[k] = v
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func str(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func num(v any) int {
	switch x := v.(type) {
	case int64:
		return int(x)
	case int:
		return x
	case float64:
		return int(x)
	}
	return 0
}

func strs(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
