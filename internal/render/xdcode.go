package render

import (
	"github.com/xxddccaa/akey/internal/spec"
)

// renderXdcode writes config.toml for xdcode (the user's codex fork). Same as
// codex, plus requires_hosted_auth = false on every provider and a [[models]]
// catalog, one entry per model usable by xdcode:
//
//	id, name, provider, model, context_window, auto_compact_token_limit,
//	reasoning_efforts, default_reasoning_effort, compatibility
//
// compatibility defaults to "responses". Each model's overrides.xdcode is
// merged into its catalog entry; tools.xdcode.extra is merged into the root.
func renderXdcode(m *spec.Master, t *spec.Tool, def *spec.Model) ([]File, error) {
	root := codexRoot(m, spec.ToolXdcode, def, true)

	var catalog []any
	for _, md := range m.ModelsFor(spec.ToolXdcode) {
		e := map[string]any{
			"id":            md.ID,
			"name":          md.Name,
			"provider":      md.Provider,
			"model":         md.Model,
			"compatibility": "responses",
		}
		if md.Compatibility != "" {
			e["compatibility"] = md.Compatibility
		}
		if md.ContextWindow > 0 {
			e["context_window"] = md.ContextWindow
		}
		if md.AutoCompact > 0 {
			e["auto_compact_token_limit"] = md.AutoCompact
		}
		if len(md.ReasoningEfforts) > 0 {
			e["reasoning_efforts"] = strList(md.ReasoningEfforts)
		}
		if md.DefaultEffort != "" {
			e["default_reasoning_effort"] = md.DefaultEffort
		}
		catalog = append(catalog, deepMerge(e, md.Overrides[spec.ToolXdcode]))
	}
	if len(catalog) > 0 {
		root["models"] = catalog
	}

	deepMerge(root, t.Extra)
	b, err := encodeTOML(root)
	if err != nil {
		return nil, err
	}
	return []File{{RelPath: "config.toml", Content: b}}, nil
}
