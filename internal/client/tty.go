package client

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"golang.org/x/term"
)

// Version is reported in the User-Agent; cmd/akey sets it.
var Version = "dev"

// Stdout/Stderr are where user-facing output goes (tests replace them).
var (
	Stdout io.Writer = os.Stdout
	Stderr io.Writer = os.Stderr
)

// errNoTTY means there is no terminal to prompt on.
var errNoTTY = errors.New("no terminal available for prompting")

// tty is the controlling terminal: /dev/tty, or CONIN$/CONOUT$ on Windows.
// We never prompt on stdin directly because `curl ... | sh` makes stdin a
// pipe.
type tty struct {
	in, out *os.File
	rd      *bufio.Reader
}

func openTTY() (*tty, error) {
	if runtime.GOOS == "windows" {
		in, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errNoTTY, err)
		}
		out, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
		if err != nil {
			in.Close()
			return nil, fmt.Errorf("%w: %v", errNoTTY, err)
		}
		return &tty{in: in, out: out}, nil
	}
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		if term.IsTerminal(int(os.Stdin.Fd())) {
			return &tty{in: os.Stdin, out: os.Stderr}, nil
		}
		return nil, fmt.Errorf("%w: %v", errNoTTY, err)
	}
	return &tty{in: f, out: f}, nil
}

func (t *tty) Close() {
	if t.in != os.Stdin {
		t.in.Close()
	}
	if t.out != t.in && t.out != os.Stderr {
		t.out.Close()
	}
}

func (t *tty) secret(prompt string) (string, error) {
	fmt.Fprint(t.out, prompt)
	b, err := term.ReadPassword(int(t.in.Fd()))
	fmt.Fprintln(t.out)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

func (t *tty) line(prompt string) (string, error) {
	fmt.Fprint(t.out, prompt)
	if t.rd == nil {
		t.rd = bufio.NewReader(t.in)
	}
	s, err := t.rd.ReadString('\n')
	if err != nil && s == "" {
		return "", err
	}
	return strings.TrimSpace(s), nil
}

// MinPassphraseLen is the length below which we warn on create/edit.
const MinPassphraseLen = 12

// ReadPassphrase returns AKEY_PASSPHRASE or prompts on the terminal.
// With confirm, it asks twice (retrying up to 3 times on mismatch) and
// warns about short passphrases.
func ReadPassphrase(confirm bool) (string, error) {
	if v := os.Getenv("AKEY_PASSPHRASE"); v != "" {
		if confirm {
			warnShort(Stderr, v)
		}
		return v, nil
	}
	t, err := openTTY()
	if err != nil {
		return "", fmt.Errorf("%w; set AKEY_PASSPHRASE to run non-interactively", err)
	}
	defer t.Close()
	for attempt := 0; attempt < 3; attempt++ {
		p, err := t.secret("akey passphrase: ")
		if err != nil {
			return "", err
		}
		if p == "" {
			fmt.Fprintln(t.out, "empty passphrase, try again")
			continue
		}
		if !confirm {
			return p, nil
		}
		q, err := t.secret("repeat passphrase: ")
		if err != nil {
			return "", err
		}
		if p != q {
			fmt.Fprintln(t.out, "passphrases do not match, try again")
			continue
		}
		warnShort(t.out, p)
		return p, nil
	}
	return "", errors.New("no passphrase entered")
}

// confirmPassphrase asks the user to re-type an already-entered passphrase
// (used when `akey edit` is about to create a new config).
func confirmPassphrase(pass string) error {
	if os.Getenv("AKEY_PASSPHRASE") != "" {
		warnShort(Stderr, pass)
		return nil
	}
	t, err := openTTY()
	if err != nil {
		return err
	}
	defer t.Close()
	q, err := t.secret("repeat passphrase to create a new config: ")
	if err != nil {
		return err
	}
	if q != pass {
		return errors.New("passphrases do not match")
	}
	warnShort(t.out, pass)
	return nil
}

func warnShort(w io.Writer, p string) {
	if n := len([]rune(p)); n < MinPassphraseLen {
		fmt.Fprintf(w, "warning: passphrase is only %d characters; %d+ is strongly recommended (it is the only thing protecting your keys and locating your config)\n", n, MinPassphraseLen)
	}
}

// askYesNo prompts a yes/no question; def is returned on empty input.
func askYesNo(prompt string, def bool) (bool, error) {
	t, err := openTTY()
	if err != nil {
		return false, err
	}
	defer t.Close()
	s, err := t.line(prompt)
	if err != nil {
		return false, err
	}
	switch strings.ToLower(s) {
	case "":
		return def, nil
	case "y", "yes":
		return true, nil
	}
	return false, nil
}
