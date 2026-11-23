// Package render turns a spec.Master into native config files per tool.
// OWNER: render agent. This file holds the public contract; implementation
// lives in codex.go, xdcode.go, claude.go, opencode.go, pi.go, merge.go.
package render

import (
	"github.com/xxddccaa/akey/internal/spec"
)

// File is one generated file.
type File struct {
	Tool string // spec.Tool* name
	// RelPath is relative to the tool's config dir (see paths.ToolDir),
	// slash-separated, e.g. "config.toml", "settings.json", "models.json".
	RelPath string
	Content []byte
	Mode    uint32 // 0600 for files that contain secrets, else 0644
}

// Render generates all files for one tool. The master must be validated.
func Render(m *spec.Master, tool string) ([]File, error) {
	return renderTool(m, tool)
}

// RenderAll renders the given tools (nil = m.EnabledTools()) in canonical order.
func RenderAll(m *spec.Master, tools []string) ([]File, error) {
	if tools == nil {
		tools = m.EnabledTools()
	}
	var out []File
	for _, t := range tools {
		fs, err := Render(m, t)
		if err != nil {
			return nil, err
		}
		out = append(out, fs...)
	}
	return out, nil
}
