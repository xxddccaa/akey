package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/xxddccaa/akey/internal/spec"
	"github.com/xxddccaa/akey/internal/vault"
)

func adminToken(opts Options) string {
	if opts.AdminToken != "" {
		return opts.AdminToken
	}
	return os.Getenv("AKEY_ADMIN_TOKEN")
}

// askAdminFn is replaceable in tests.
var askAdminFn = askAdmin

func askAdmin() (string, error) {
	t, err := openTTY()
	if err != nil {
		return "", err
	}
	defer t.Close()
	fmt.Fprintln(t.out, "this passphrase has no config yet; creating one needs the server admin token (printed by `akey serve` on first start).")
	return t.secret("admin token: ")
}

// upload seals the YAML and PUTs it.
func (s *session) upload(ctx context.Context, yamlBytes []byte, opts Options) error {
	env, err := vault.Seal(s.keys, yamlBytes)
	if err != nil {
		return err
	}
	if err := putWithAdminRetry(ctx, s.api, s.keys.ID, env, s.keys.WriteToken, adminToken(opts), askAdminFn); err != nil {
		return err
	}
	s.saveConfig()
	return nil
}

// Push validates a local master file, encrypts it and uploads it.
func Push(ctx context.Context, file string, opts Options) error {
	if file == "" {
		return errors.New("usage: akey push FILE")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	m, err := spec.Parse(raw)
	if err != nil {
		return err
	}
	if _, err := resolveConfig(opts.Server, opts.Pin, opts.PinSet); err != nil {
		return err
	}
	pass, err := ReadPassphrase(true)
	if err != nil {
		return err
	}
	s, err := connect(ctx, opts, pass)
	if err != nil {
		return err
	}
	if err := s.upload(ctx, raw, opts); err != nil {
		return err
	}
	fmt.Fprintf(Stdout, "pushed %s to %s (%d providers, %d models, tools: %s)\n", file, s.cfg.Server,
		len(m.Providers), len(m.Models), strings.Join(m.EnabledTools(), ", "))
	fmt.Fprintln(Stdout, "on other machines: akey pull")
	return nil
}

// Edit downloads the master config, opens it in $EDITOR, validates it and
// uploads the result. A missing config starts from a template.
func Edit(ctx context.Context, opts Options) error {
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
	orig, ver, err := s.fetch(ctx)
	exists := err == nil
	switch {
	case errors.Is(err, ErrNotFound):
		fmt.Fprintln(Stderr, "no config exists for this passphrase yet; starting from a template.")
		if err := confirmPassphrase(pass); err != nil {
			return err
		}
		orig = spec.Template()
	case err != nil:
		return err
	default:
		fmt.Fprintf(Stderr, "editing master config (saved %s)\n", describeVersion(ver))
	}

	tmp, err := os.CreateTemp("", "akey-master-*.yaml")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	_ = tmp.Chmod(0o600)
	if _, err := tmp.Write(orig); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	var edited []byte
	var m *spec.Master
	for {
		if err := runEditor(tmpName); err != nil {
			return err
		}
		edited, err = os.ReadFile(tmpName)
		if err != nil {
			return err
		}
		if bytes.Equal(edited, orig) {
			if exists {
				fmt.Fprintln(Stdout, "no changes; nothing pushed")
			} else {
				fmt.Fprintln(Stdout, "template left unchanged; nothing pushed")
			}
			return nil
		}
		m, err = spec.Parse(edited)
		if err == nil {
			break
		}
		fmt.Fprintf(Stderr, "\n%v\n\n", err)
		again, aerr := askYesNo("re-open the editor to fix it? [Y/n] ", true)
		if aerr != nil || !again {
			return errors.New("aborted; nothing pushed (the temporary file was deleted)")
		}
	}
	if err := s.upload(ctx, edited, opts); err != nil {
		return err
	}
	fmt.Fprintf(Stdout, "pushed (%d providers, %d models, tools: %s); run `akey pull` to apply here\n",
		len(m.Providers), len(m.Models), strings.Join(m.EnabledTools(), ", "))
	return nil
}

func runEditor(file string) error {
	ed := os.Getenv("VISUAL")
	if ed == "" {
		ed = os.Getenv("EDITOR")
	}
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		if ed == "" {
			ed = "notepad"
		}
		f := strings.Fields(ed)
		cmd = exec.Command(f[0], append(f[1:], file)...)
	} else {
		if ed == "" {
			ed = "vi"
		}
		cmd = exec.Command("sh", "-c", ed+` "$1"`, "sh", file)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if t, err := openTTY(); err == nil {
		defer t.Close()
		cmd.Stdin, cmd.Stdout = t.in, t.out
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("editor %q failed: %w", ed, err)
	}
	return nil
}
