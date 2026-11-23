package render

import (
	"github.com/xxddccaa/akey/internal/spec"
)

// renderCodex writes config.toml for OpenAI Codex CLI.
//
//	model / model_provider          default model (upstream name) and its provider id
//	model_reasoning_effort          effective effort for the tool, if any
//	model_context_window, model_auto_compact_token_limit   from the default model
//	[model_providers.<id>]          one per provider used by codex's models
//
// Merge order: generated -> provider overrides (into each provider block) ->
// default model's overrides (into the root, since codex has no per-model
// catalog) -> tools.codex.extra (root).
func renderCodex(m *spec.Master, t *spec.Tool, def *spec.Model) ([]File, error) {
	root := codexRoot(m, spec.ToolCodex, def, false)
	if ov := def.Overrides[spec.ToolCodex]; ov != nil {
		deepMerge(root, ov)
	}
	deepMerge(root, t.Extra)
	b, err := encodeTOML(root)
	if err != nil {
		return nil, err
	}
	return []File{{RelPath: "config.toml", Content: b}}, nil
}

// codexRoot builds the part shared by codex and xdcode (xd = xdcode fork).
func codexRoot(m *spec.Master, tool string, def *spec.Model, xd bool) map[string]any {
	root := map[string]any{
		"model":          def.Model,
		"model_provider": def.Provider,
	}
	if e := m.Effort(tool); e != "" {
		root["model_reasoning_effort"] = e
	}
	if def.ContextWindow > 0 {
		root["model_context_window"] = def.ContextWindow
	}
	if def.AutoCompact > 0 {
		root["model_auto_compact_token_limit"] = def.AutoCompact
	}
	// Note: codex has no config key for max output tokens, so MaxOutput is
	// not rendered here.

	provs := map[string]any{}
	for _, pid := range m.ProvidersFor(tool) {
		p := m.Providers[pid]
		blk := map[string]any{
			"name":                p.Name,
			"base_url":            p.OpenAI.BaseURL,
			"wire_api":            spec.WireResponses,
			"supports_websockets": false,
		}
		if p.Key != "" {
			blk["experimental_bearer_token"] = p.Key
		}
		if xd {
			blk["requires_hosted_auth"] = false
		}
		provs[pid] = deepMerge(blk, p.Overrides[tool])
	}
	root["model_providers"] = provs
	return root
}
