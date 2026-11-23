package render

import (
	"github.com/xxddccaa/akey/internal/spec"
)

// npm packages opencode loads per API family / wire.
const (
	npmOpenAI       = "@ai-sdk/openai"            // openai responses
	npmOpenAICompat = "@ai-sdk/openai-compatible" // openai chat completions
	npmAnthropic    = "@ai-sdk/anthropic"
)

// knownEfforts is the effort vocabulary opencode may generate variants for.
// Variants for efforts a model does not list are disabled, so opencode's
// variant picker shows exactly the master's reasoning_efforts.
var knownEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// renderOpencode writes opencode.jsonc:
//
//	$schema, model: "<pid>/<upstream model>"
//	provider.<pid>: npm, name, options.baseURL/apiKey,
//	                models.<upstream model>: name, limit, modalities,
//	                reasoning, options (default effort), variants (one per effort)
//
// A provider with both endpoints becomes "<id>" (openai) and "<id>-anthropic".
// Anthropic baseURL gets "/v1" appended (the AI SDK expects it); bearer auth
// adds an explicit Authorization header. Effort options use the AI SDK
// provider options: reasoningEffort (+ reasoningSummary/include for the
// Responses API) for openai, {thinking: adaptive, effort} for anthropic.
//
// Merge order: model overrides.opencode into models.<model>, provider
// overrides.opencode into provider.<pid> (both halves of a split provider),
// then tools.opencode.extra into the root.
func renderOpencode(m *spec.Master, t *spec.Tool, def *spec.Model) ([]File, error) {
	models := m.ModelsFor(spec.ToolOpencode)
	if err := checkSplitCollisions(m, models); err != nil {
		return nil, err
	}
	providers := map[string]any{}
	owner := map[string]*spec.Provider{}
	for _, md := range models {
		p := m.Providers[md.Provider]
		pid := splitProviderID(m, md)
		blk, ok := providers[pid].(map[string]any)
		if !ok {
			blk = opencodeProvider(m, md)
			providers[pid] = blk
			owner[pid] = p
		}
		catalog := child(blk, "models")
		if _, dup := catalog[md.Model]; dup {
			continue // same upstream model listed twice: first entry wins
		}
		catalog[md.Model] = deepMerge(opencodeModel(m, def, md), md.Overrides[spec.ToolOpencode])
	}
	for pid, blk := range providers {
		deepMerge(blk.(map[string]any), owner[pid].Overrides[spec.ToolOpencode])
	}

	root := map[string]any{
		"$schema":  "https://opencode.ai/config.json",
		"model":    splitProviderID(m, def) + "/" + def.Model,
		"provider": providers,
	}
	deepMerge(root, t.Extra)
	b, err := encodeJSON(root, true)
	if err != nil {
		return nil, err
	}
	return []File{{RelPath: "opencode.jsonc", Content: b}}, nil
}

func opencodeProvider(m *spec.Master, md *spec.Model) map[string]any {
	p := m.Providers[md.Provider]
	opts := map[string]any{}
	blk := map[string]any{"name": p.Name, "options": opts}
	if md.API == spec.APIAnthropic {
		blk["npm"] = npmAnthropic
		opts["baseURL"] = anthropicRoot(p.Anthropic.BaseURL) + "/v1"
		if p.OpenAI != nil {
			blk["name"] = p.Name + " (Anthropic)"
		}
		if p.Key != "" && p.Anthropic.Auth == spec.AuthBearer {
			opts["headers"] = map[string]any{"Authorization": "Bearer " + p.Key}
		}
	} else {
		blk["npm"] = npmOpenAI
		if p.OpenAI.Wire == spec.WireChat {
			blk["npm"] = npmOpenAICompat
		}
		opts["baseURL"] = p.OpenAI.BaseURL
	}
	if p.Key != "" {
		opts["apiKey"] = p.Key
	}
	return blk
}

func opencodeModel(m *spec.Master, def, md *spec.Model) map[string]any {
	e := map[string]any{"name": md.Name}
	if md.ContextWindow > 0 || md.MaxOutput > 0 {
		// opencode requires both; 0 means "unknown, use opencode's default".
		e["limit"] = map[string]any{"context": md.ContextWindow, "output": md.MaxOutput}
	}
	if in := filterInput(md.Input, "text", "image", "audio", "video", "pdf"); len(in) > 0 {
		e["modalities"] = map[string]any{"input": in, "output": []any{"text"}}
		if hasString(md.Input, "image") || hasString(md.Input, "pdf") {
			e["attachment"] = true
		}
	}
	effort := modelEffort(m, spec.ToolOpencode, def, md)
	if len(md.ReasoningEfforts) == 0 && effort == "" {
		return e
	}
	e["reasoning"] = true
	wire := m.Wire(md)
	if effort != "" {
		e["options"] = opencodeEffort(md.API, wire, effort)
	}
	if len(md.ReasoningEfforts) > 0 {
		variants := map[string]any{}
		for _, k := range knownEfforts {
			variants[k] = map[string]any{"disabled": true}
		}
		for _, k := range md.ReasoningEfforts {
			variants[k] = opencodeEffort(md.API, wire, k)
		}
		e["variants"] = variants
	}
	return e
}

// opencodeEffort returns AI SDK provider options selecting one effort. They
// mirror what opencode itself generates for known models, so variants it
// would auto-create and the ones added here look the same.
func opencodeEffort(api, wire, effort string) map[string]any {
	switch {
	case api == spec.APIAnthropic && effort == "none":
		return map[string]any{"thinking": map[string]any{"type": "disabled"}}
	case api == spec.APIAnthropic:
		return map[string]any{"thinking": map[string]any{"type": "adaptive"}, "effort": effort}
	case wire == spec.WireResponses:
		return map[string]any{
			"reasoningEffort":  effort,
			"reasoningSummary": "auto",
			"include":          []any{"reasoning.encrypted_content"},
		}
	}
	return map[string]any{"reasoningEffort": effort}
}
