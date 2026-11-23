// Package paths knows where each agent tool keeps its config on
// Linux, macOS and Windows.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xxddccaa/akey/internal/spec"
)

// ToolDir returns the config directory of a tool. Each tool's own override
// environment variable wins over the default location:
//
//	codex    $CODEX_HOME          or ~/.codex
//	xdcode   $XDCODE_HOME         or ~/.xdcode
//	claude   $CLAUDE_CONFIG_DIR   or ~/.claude
//	opencode $XDG_CONFIG_HOME/opencode or ~/.config/opencode (also on Windows)
//	pi       $PI_CODING_AGENT_DIR or ~/.pi/agent
func ToolDir(tool string) (string, error) {
	switch tool {
	case spec.ToolCodex:
		return envOrHome("CODEX_HOME", ".codex")
	case spec.ToolXdcode:
		return envOrHome("XDCODE_HOME", ".xdcode")
	case spec.ToolClaude:
		return envOrHome("CLAUDE_CONFIG_DIR", ".claude")
	case spec.ToolOpencode:
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
			x, err := expand(x)
			if err != nil {
				return "", err
			}
			return filepath.Join(x, "opencode"), nil
		}
		return envOrHome("", ".config", "opencode")
	case spec.ToolPi:
		return envOrHome("PI_CODING_AGENT_DIR", ".pi", "agent")
	}
	return "", fmt.Errorf("unknown tool %q (known: %s)", tool, strings.Join(spec.ToolNames, ", "))
}

// ToolBinary returns the executable name of a tool, suitable for
// exec.LookPath (which adds .exe/.cmd via PATHEXT on Windows).
func ToolBinary(tool string) string {
	switch tool {
	case spec.ToolCodex, spec.ToolXdcode, spec.ToolClaude, spec.ToolOpencode, spec.ToolPi:
		return tool
	}
	return tool
}

func envOrHome(env string, rel ...string) (string, error) {
	if env != "" {
		if v := os.Getenv(env); v != "" {
			return expand(v)
		}
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(append([]string{h}, rel...)...), nil
}

// expand resolves a leading "~" and makes the path absolute.
func expand(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(h, p[1:])
	}
	return filepath.Abs(p)
}
