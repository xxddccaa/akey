package server

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
)

type installInfo struct {
	SH  string `json:"sh"`
	PS1 string `json:"ps1"`
}

type infoResponse struct {
	Version    string      `json:"version"`
	TLS        bool        `json:"tls"`
	Pin        string      `json:"pin"`
	CertSHA256 string      `json:"cert_sha256"`
	Install    installInfo `json:"install"`
}

// getInfo: GET /v1/info. Public; the one-liners use the Host the client
// reached us by (sanitised like /i).
func (s *server) getInfo(w http.ResponseWriter, r *http.Request) {
	base := s.baseURL(r)
	resp := infoResponse{Version: s.cfg.Version, TLS: !s.cfg.InsecureHTTP, Pin: s.pin, CertSHA256: s.certSHA}
	if s.cfg.InsecureHTTP {
		resp.Install = installInfo{SH: shPlainOneLiner(base), PS1: psPlainOneLiner(base)}
	} else {
		resp.Install = installInfo{SH: shPinnedOneLiner(base, s.pin), PS1: psPinnedOneLiner(base, s.certSHA)}
	}
	writeJSON(w, resp)
}

type notInitialisedError struct{ dir, missing string }

func (e *notInitialisedError) Error() string {
	return fmt.Sprintf("data dir %s is not initialised (%s does not exist); "+
		"start the server once first (`akey serve --data %s ...`, or run scripts/install-server.sh)", e.dir, e.missing, e.dir)
}

func notInitialised(dir, missing string) error { return &notInitialisedError{dir, missing} }

// IsNotInitialised reports whether err came from PrintInfo on an empty data dir.
func IsNotInitialised(err error) bool {
	var e *notInitialisedError
	return errors.As(err, &e)
}

// listenPort extracts the port from a --listen value (":8787", "0.0.0.0:8787", "8787").
func listenPort(listen string) (string, error) {
	if listen == "" {
		return "8787", nil
	}
	if !strings.Contains(listen, ":") {
		listen = ":" + listen
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return "", fmt.Errorf("bad --listen %q (want :PORT or HOST:PORT)", listen)
	}
	return port, nil
}

// PrintInfo writes the `akey serve` banner (TLS pin, cert fingerprint, panel
// URLs, client one-liners and the admin token) for an existing data dir,
// without listening and without creating or modifying anything in it.
func PrintInfo(cfg Config, w io.Writer) error {
	if cfg.DataDir == "" {
		return errors.New("--data is required")
	}
	if st, err := os.Stat(cfg.DataDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return notInitialised(cfg.DataDir, cfg.DataDir)
		}
		return err
	} else if !st.IsDir() {
		return fmt.Errorf("%s is not a directory", cfg.DataDir)
	}
	port, err := listenPort(cfg.Listen)
	if err != nil {
		return err
	}
	logf := func(format string, args ...any) { fmt.Fprintf(os.Stderr, "akey: "+format+"\n", args...) }
	s, err := newServerMode(cfg, logf, true)
	if err != nil {
		return err
	}
	header := fmt.Sprintf("akey server info (from %s; this command does not start a server)\n", cfg.DataDir)
	s.writeBanner(w, header, port, true)
	return nil
}
