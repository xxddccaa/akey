// Package client implements the akey client: pull, push, edit, render.
package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/xxddccaa/akey/internal/paths"
	"github.com/xxddccaa/akey/internal/render"
	"github.com/xxddccaa/akey/internal/spec"
	"github.com/xxddccaa/akey/internal/vault"
)

// Options are the CLI flags shared by pull/push/edit.
type Options struct {
	Server         string
	Pin            string
	PinSet         bool     // --pin given explicitly (an empty pin is meaningful)
	AdminToken     string   // push/edit; falls back to AKEY_ADMIN_TOKEN
	Only           []string // pull/render; nil = all enabled tools
	DryRun         bool
	Yes            bool // accepted for scripts; pull never prompts anyway
	InstallMissing bool
}

// ParseOnly splits and validates a comma-separated tool list.
func ParseOnly(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []string
	seen := map[string]bool{}
	for _, t := range strings.Split(s, ",") {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" || seen[t] {
			continue
		}
		if !spec.IsTool(t) {
			return nil, fmt.Errorf("unknown tool %q in --only (known: %s)", t, strings.Join(spec.ToolNames, ", "))
		}
		seen[t] = true
		out = append(out, t)
	}
	return out, nil
}

// selectTools intersects --only with the master's enabled tools, in
// canonical order, warning about tools that are not enabled.
func selectTools(m *spec.Master, only []string, w io.Writer) ([]string, error) {
	if only == nil {
		tools := m.EnabledTools()
		if len(tools) == 0 {
			return nil, errors.New("the master config enables no tools")
		}
		return tools, nil
	}
	want := map[string]bool{}
	for _, t := range only {
		want[t] = true
	}
	var out []string
	for _, t := range spec.ToolNames {
		if !want[t] {
			continue
		}
		if !m.ToolEnabled(t) {
			fmt.Fprintf(w, "warning: %s is not enabled in the master config; skipping\n", t)
			continue
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil, errors.New("none of the --only tools are enabled in the master config")
	}
	return out, nil
}

// session is a connected server plus derived keys.
type session struct {
	cfg  Config
	api  *api
	keys *vault.Keys
}

func connect(ctx context.Context, opts Options, pass string) (*session, error) {
	cfg, err := resolveConfig(opts.Server, opts.Pin, opts.PinSet)
	if err != nil {
		return nil, err
	}
	a, err := newAPI(cfg)
	if err != nil {
		return nil, err
	}
	p, err := a.params(ctx)
	if err != nil {
		return nil, err
	}
	fmt.Fprintln(Stderr, "deriving keys (argon2id)...")
	k, err := vault.Derive(pass, p)
	if err != nil {
		return nil, err
	}
	return &session{cfg: cfg, api: a, keys: k}, nil
}

// fetch downloads and decrypts the master YAML. ErrNotFound if absent.
func (s *session) fetch(ctx context.Context) ([]byte, string, error) {
	env, ver, err := s.api.getBlob(ctx, s.keys.ID)
	if err != nil {
		return nil, "", err
	}
	pt, err := vault.Open(s.keys, env)
	if err != nil {
		return nil, "", err
	}
	return pt, ver, nil
}

func (s *session) saveConfig() {
	if err := SaveConfig(s.cfg); err != nil {
		fmt.Fprintf(Stderr, "warning: could not save client config: %v\n", err)
	}
}

// Pull fetches the master config and writes every enabled tool's files.
func Pull(ctx context.Context, opts Options) error {
	if _, err := resolveConfig(opts.Server, opts.Pin, opts.PinSet); err != nil {
		return err
	}
	pass, err := ReadPassphrase(false)
	if err != nil {
		return err
	}
	s, err := connect(ctx, opts, pass)
	if err != nil {
		return err
	}
	raw, ver, err := s.fetch(ctx)
	if err != nil {
		return err
	}
	m, err := spec.Parse(raw)
	if err != nil {
		return fmt.Errorf("the stored master config is invalid (fix it with `akey edit`): %w", err)
	}
	s.saveConfig()
	fmt.Fprintf(Stdout, "fetched master config (saved %s) from %s\n", describeVersion(ver), s.cfg.Server)
	return applyMaster(m, opts, paths.ToolDir, Stdout)
}

// applyMaster renders and writes (or dry-runs) the selected tools.
func applyMaster(m *spec.Master, opts Options, toolDir func(string) (string, error), w io.Writer) error {
	tools, err := selectTools(m, opts.Only, w)
	if err != nil {
		return err
	}
	if opts.InstallMissing {
		installMissing(m, tools, opts.DryRun, w)
	}
	files, err := render.RenderAll(m, tools)
	if err != nil {
		return err
	}
	plan, err := planFiles(files, toolDir)
	if err != nil {
		return err
	}
	if opts.DryRun {
		printDryRun(plan, m.Secrets(), w)
		n := 0
		for _, p := range plan {
			if p.Status != StatusUnchanged {
				n++
			}
		}
		fmt.Fprintf(w, "dry run: %d of %d file(s) would be written; nothing changed\n", n, len(plan))
		return nil
	}
	res, err := applyPlan(plan, w)
	fmt.Fprintf(w, "done: %d written, %d unchanged, %d failed (tools: %s)\n", res.Written, res.Unchanged, res.Failed, strings.Join(tools, ", "))
	if !opts.InstallMissing {
		var missing []string
		for _, t := range tools {
			if _, lerr := exec.LookPath(paths.ToolBinary(t)); lerr != nil {
				missing = append(missing, t)
			}
		}
		if len(missing) > 0 {
			fmt.Fprintf(w, "note: not on PATH: %s (configs were written anyway; `akey pull --install-missing` runs tools.<name>.install)\n", strings.Join(missing, ", "))
		}
	}
	return err
}

// installMissing runs Tool.Install for enabled tools whose binary is not
// on PATH. Failures are reported but not fatal.
func installMissing(m *spec.Master, tools []string, dryRun bool, w io.Writer) {
	for _, t := range tools {
		bin := paths.ToolBinary(t)
		if _, err := exec.LookPath(bin); err == nil {
			continue
		}
		tl := m.Tools[t]
		if tl == nil || strings.TrimSpace(tl.Install) == "" {
			fmt.Fprintf(w, "note: %s is not installed and tools.%s.install is not set\n", bin, t)
			continue
		}
		fmt.Fprintf(w, "==> installing %s: %s\n", t, tl.Install)
		if dryRun {
			fmt.Fprintln(w, "    (dry run: not executed)")
			continue
		}
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.Command("cmd", "/C", tl.Install)
		} else {
			cmd = exec.Command("sh", "-c", tl.Install)
		}
		cmd.Stdin = os.Stdin
		cmd.Stdout = w
		cmd.Stderr = Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(w, "warning: installing %s failed: %v\n", t, err)
			continue
		}
		if _, err := exec.LookPath(bin); err != nil {
			fmt.Fprintf(w, "note: %s installed but not on PATH yet (open a new shell)\n", bin)
		}
	}
}

// RenderLocal renders a local master file into outDir/<tool>/<RelPath>,
// without any server (for testing).
func RenderLocal(file string, only []string, outDir string) error {
	if outDir == "" {
		return errors.New("--out DIR is required")
	}
	m, err := spec.Load(file)
	if err != nil {
		return err
	}
	tools, err := selectTools(m, only, Stderr)
	if err != nil {
		return err
	}
	files, err := render.RenderAll(m, tools)
	if err != nil {
		return err
	}
	return writeRendered(files, outDir, Stdout)
}

func writeRendered(files []render.File, outDir string, w io.Writer) error {
	for _, f := range files {
		rel := filepath.FromSlash(f.RelPath)
		if filepath.IsAbs(rel) || filepath.Clean(rel) != rel || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("%s: refusing unsafe path %q", f.Tool, f.RelPath)
		}
		target := filepath.Join(outDir, f.Tool, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		mode := os.FileMode(f.Mode).Perm()
		if mode == 0 {
			mode = 0o600
		}
		if err := atomicWrite(target, f.Content, mode); err != nil {
			return err
		}
		fmt.Fprintf(w, "wrote %s\n", target)
	}
	return nil
}
