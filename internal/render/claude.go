package render

import (
	"github.com/xxddccaa/akey/internal/spec"
)

// renderClaude writes settings.json for Claude Code:
//
//	env.ANTHROPIC_BASE_URL                    provider's anthropic.base_url (no trailing /v1)
//	env.ANTHROPIC_API_KEY | ANTHROPIC_AUTH_TOKEN   by anthropic.auth (api_key | bearer)
//	model                                     default model's upstream name
//	modelSettings.<model>.effortLevel         effective effort, if any
//
// Claude Code has no provider block, so the provider's overrides.claude is
// merged into the settings root (handy for env such as NO_PROXY), and the
// default model's overrides.claude into modelSettings.<model>. Then
// tools.claude.extra. Plain JSON: no comment header.
func renderClaude(m *spec.Master, t *spec.Tool, def *spec.Model) ([]File, error) {
	p := m.Providers[def.Provider]
	env := map[string]any{
		"ANTHROPIC_BASE_URL": anthropicRoot(p.Anthropic.BaseURL),
	}
	if p.Key != "" {
		if p.Anthropic.Auth == spec.AuthBearer {
			env["ANTHROPIC_AUTH_TOKEN"] = p.Key
		} else {
			env["ANTHROPIC_API_KEY"] = p.Key
		}
	}
	root := map[string]any{
		"env":   env,
		"model": def.Model,
	}
	if e := m.Effort(spec.ToolClaude); e != "" {
		child(child(root, "modelSettings"), def.Model)["effortLevel"] = e
	}
	deepMerge(root, p.Overrides[spec.ToolClaude])
	if ov := def.Overrides[spec.ToolClaude]; ov != nil {
		deepMerge(child(child(root, "modelSettings"), def.Model), ov)
	}
	deepMerge(root, t.Extra)

	b, err := encodeJSON(root, false)
	if err != nil {
		return nil, err
	}
	return []File{{RelPath: "settings.json", Content: b}}, nil
}
