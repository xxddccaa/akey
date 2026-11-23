package paths

import (
	"path/filepath"
	"testing"
)

func TestToolDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, e := range []string{"CODEX_HOME", "XDCODE_HOME", "CLAUDE_CONFIG_DIR", "XDG_CONFIG_HOME", "PI_CODING_AGENT_DIR"} {
		t.Setenv(e, "")
	}
	want := map[string]string{
		"codex":    filepath.Join(home, ".codex"),
		"xdcode":   filepath.Join(home, ".xdcode"),
		"claude":   filepath.Join(home, ".claude"),
		"opencode": filepath.Join(home, ".config", "opencode"),
		"pi":       filepath.Join(home, ".pi", "agent"),
	}
	for tool, w := range want {
		got, err := ToolDir(tool)
		if err != nil || got != w {
			t.Errorf("ToolDir(%s) = %q, %v; want %q", tool, got, err, w)
		}
	}
	other := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(other, "c"))
	t.Setenv("XDCODE_HOME", filepath.Join(other, "x"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(other, "cl"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(other, "xdg"))
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(other, "pi"))
	want = map[string]string{
		"codex":    filepath.Join(other, "c"),
		"xdcode":   filepath.Join(other, "x"),
		"claude":   filepath.Join(other, "cl"),
		"opencode": filepath.Join(other, "xdg", "opencode"),
		"pi":       filepath.Join(other, "pi"),
	}
	for tool, w := range want {
		got, err := ToolDir(tool)
		if err != nil || got != w {
			t.Errorf("env ToolDir(%s) = %q, %v; want %q", tool, got, err, w)
		}
	}
	t.Setenv("CODEX_HOME", "~/cx")
	if got, _ := ToolDir("codex"); got != filepath.Join(home, "cx") {
		t.Errorf("tilde expansion: %q", got)
	}
	if _, err := ToolDir("nope"); err == nil {
		t.Error("unknown tool should fail")
	}
}
