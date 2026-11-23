package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Config is the per-machine client config, stored in
// os.UserConfigDir()/akey/client.json.
type Config struct {
	Server string `json:"server"`
	Pin    string `json:"pin"`
}

// ConfigPath returns the location of client.json. AKEY_CLIENT_CONFIG
// overrides it (used by tests).
func ConfigPath() (string, error) {
	if p := os.Getenv("AKEY_CLIENT_CONFIG"); p != "" {
		return p, nil
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "akey", "client.json"), nil
}

// LoadConfig reads client.json; a missing file yields an empty Config.
func LoadConfig() (Config, error) {
	var c Config
	p, err := ConfigPath()
	if err != nil {
		return c, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", p, err)
	}
	return c, nil
}

// SaveConfig writes client.json (0600, dir 0700).
func SaveConfig(c Config) error {
	p, err := ConfigPath()
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return atomicWrite(p, b, 0o600)
}

// resolveConfig merges flags over the saved client.json. A saved pin is
// only reused when the server matches (a pin belongs to one server).
// pinSet means --pin was given explicitly (possibly empty: install.sh in
// --insecure-http mode passes --pin ”).
func resolveConfig(flagServer, flagPin string, pinSet bool) (Config, error) {
	saved, err := LoadConfig()
	if err != nil {
		return Config{}, err
	}
	c := saved
	if flagServer != "" {
		s, err := normalizeServer(flagServer)
		if err != nil {
			return Config{}, err
		}
		if s != saved.Server {
			c.Pin = ""
		}
		c.Server = s
	}
	if pinSet || flagPin != "" {
		c.Pin = strings.TrimSpace(flagPin)
	}
	if c.Server == "" {
		return Config{}, errors.New("no server configured: pass --server https://IP:PORT --pin sha256//... (printed by `akey serve`)")
	}
	s, err := normalizeServer(c.Server)
	if err != nil {
		return Config{}, err
	}
	c.Server = s
	return c, nil
}

func normalizeServer(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, "/")
	if s == "" {
		return "", errors.New("empty server URL")
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	if !strings.HasPrefix(s, "https://") && !strings.HasPrefix(s, "http://") {
		return "", fmt.Errorf("server URL %q must start with https:// or http://", s)
	}
	return s, nil
}
