package client

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/xxddccaa/akey/internal/render"
)

// KeepBackups is how many .akey-bak-* files are kept per target.
const KeepBackups = 5

const backupInfix = ".akey-bak-"

// now is replaceable in tests.
var now = time.Now

// atomicWrite writes data to path via a temp file in the same directory
// and a rename, so readers never see a half-written file.
func atomicWrite(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".akey-tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(name)
		}
	}()
	if err := tmp.Chmod(mode); err != nil && runtime.GOOS != "windows" {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// backupFile copies path to path.akey-bak-YYYYMMDD-HHMMSS (0600) and prunes
// old backups so only the newest KeepBackups remain. Returns the backup path.
func backupFile(path string, content []byte) (string, error) {
	stamp := now().Format("20060102-150405")
	bak := path + backupInfix + stamp
	for i := 1; ; i++ {
		if _, err := os.Lstat(bak); errors.Is(err, fs.ErrNotExist) {
			break
		}
		bak = fmt.Sprintf("%s%s%s-%d", path, backupInfix, stamp, i)
	}
	f, err := os.OpenFile(bak, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	pruneBackups(path)
	return bak, nil
}

func listBackups(path string) []string {
	matches, _ := filepath.Glob(globEscape(path) + backupInfix + "*")
	var out []string
	for _, m := range matches {
		if !strings.Contains(filepath.Base(m), ".akey-tmp-") {
			out = append(out, m)
		}
	}
	sort.Strings(out) // fixed-width timestamps sort chronologically
	return out
}

func pruneBackups(path string) {
	bs := listBackups(path)
	for len(bs) > KeepBackups {
		os.Remove(bs[0])
		bs = bs[1:]
	}
}

func globEscape(p string) string {
	if runtime.GOOS == "windows" {
		r := strings.NewReplacer("[", "[[]", "*", "[*]", "?", "[?]")
		return r.Replace(p)
	}
	r := strings.NewReplacer(`\`, `\\`, "[", `\[`, "*", `\*`, "?", `\?`)
	return r.Replace(p)
}

// Status of a planned file write.
const (
	StatusNew       = "new"
	StatusChanged   = "changed"
	StatusUnchanged = "unchanged"
)

type plannedFile struct {
	render.File
	Target  string // absolute path (symlinks resolved)
	Old     []byte // existing content (nil if new)
	Status  string
	OldMode fs.FileMode
}

// planFiles resolves targets and compares against existing files.
func planFiles(files []render.File, toolDir func(string) (string, error)) ([]plannedFile, error) {
	var out []plannedFile
	for _, f := range files {
		dir, err := toolDir(f.Tool)
		if err != nil {
			return nil, err
		}
		rel := filepath.FromSlash(f.RelPath)
		if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.Clean(rel) != rel {
			return nil, fmt.Errorf("%s: refusing unsafe path %q", f.Tool, f.RelPath)
		}
		target := filepath.Join(dir, rel)
		if real, err := filepath.EvalSymlinks(target); err == nil {
			target = real // keep dotfile symlinks intact; write through them
		}
		p := plannedFile{File: f, Target: target, Status: StatusNew}
		if p.Mode == 0 {
			p.Mode = 0o600
		}
		if st, err := os.Stat(target); err == nil {
			if st.IsDir() {
				return nil, fmt.Errorf("%s is a directory", target)
			}
			old, err := os.ReadFile(target)
			if err != nil {
				return nil, err
			}
			p.Old = old
			p.OldMode = st.Mode().Perm()
			if bytes.Equal(old, f.Content) {
				p.Status = StatusUnchanged
			} else {
				p.Status = StatusChanged
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// applyResult summarizes applyPlan.
type applyResult struct {
	Written, Unchanged, Failed int
}

// applyPlan writes changed/new files: back up, atomic write, mode, dirs 0700.
func applyPlan(plan []plannedFile, w io.Writer) (applyResult, error) {
	var res applyResult
	var errs []string
	for _, p := range plan {
		mode := fs.FileMode(p.Mode).Perm()
		switch p.Status {
		case StatusUnchanged:
			if runtime.GOOS != "windows" && p.OldMode != mode {
				if err := os.Chmod(p.Target, mode); err == nil {
					fmt.Fprintf(w, "  unchanged  %s (mode set to %04o)\n", p.Target, mode)
					res.Unchanged++
					continue
				}
			}
			fmt.Fprintf(w, "  unchanged  %s\n", p.Target)
			res.Unchanged++
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p.Target), 0o700); err != nil {
			res.Failed++
			errs = append(errs, err.Error())
			fmt.Fprintf(w, "  FAILED     %s: %v\n", p.Target, err)
			continue
		}
		bak := ""
		if p.Old != nil {
			b, err := backupFile(p.Target, p.Old)
			if err != nil {
				res.Failed++
				errs = append(errs, fmt.Sprintf("backup %s: %v", p.Target, err))
				fmt.Fprintf(w, "  FAILED     %s: backup: %v\n", p.Target, err)
				continue
			}
			bak = b
		}
		if err := atomicWrite(p.Target, p.Content, mode); err != nil {
			res.Failed++
			errs = append(errs, fmt.Sprintf("write %s: %v", p.Target, err))
			fmt.Fprintf(w, "  FAILED     %s: %v\n", p.Target, err)
			continue
		}
		res.Written++
		if bak != "" {
			fmt.Fprintf(w, "  updated    %s (backup: %s)\n", p.Target, filepath.Base(bak))
		} else {
			fmt.Fprintf(w, "  created    %s\n", p.Target)
		}
	}
	if len(errs) > 0 {
		return res, fmt.Errorf("%d file(s) failed:\n  %s", len(errs), strings.Join(errs, "\n  "))
	}
	return res, nil
}

// printDryRun shows each target with its status and a redacted diff.
func printDryRun(plan []plannedFile, secrets []string, w io.Writer) {
	for _, p := range plan {
		fmt.Fprintf(w, "%-9s  %s  (mode %04o)\n", p.Status, p.Target, fs.FileMode(p.Mode).Perm())
		if p.Status == StatusUnchanged {
			continue
		}
		old := redact(string(p.Old), secrets)
		nw := redact(string(p.Content), secrets)
		fmt.Fprint(w, lineDiff(old, nw, 2))
		fmt.Fprintln(w)
	}
}
