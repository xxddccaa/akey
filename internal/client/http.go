package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/xxddccaa/akey/internal/vault"
)

var (
	// ErrNotFound: no blob at the derived id.
	ErrNotFound = errors.New("no config for this passphrase (or wrong passphrase)")
	// ErrRateLimited: the server throttles this IP after failed lookups.
	ErrRateLimited = errors.New("rate limited by the server (too many failed lookups from this IP); wait a few minutes and retry")
	// ErrUnauthorized: PUT rejected with 401 (admin token missing or wrong).
	ErrUnauthorized = errors.New("unauthorized: creating a new config needs the server admin token (--admin-token or AKEY_ADMIN_TOKEN; printed by `akey serve` on first start)")
	// ErrForbidden: PUT rejected with 403 (wrong admin token on create, or
	// wrong write token on an existing id).
	ErrForbidden = errors.New("forbidden: the server rejected the admin token (or the write token for this config)")
)

// PinFromCert returns the curl-style pin of a certificate:
// "sha256//" + base64(sha256(SubjectPublicKeyInfo)).
func PinFromCert(c *x509.Certificate) string {
	sum := sha256.Sum256(c.RawSubjectPublicKeyInfo)
	return "sha256//" + base64.StdEncoding.EncodeToString(sum[:])
}

// newHTTPClient builds an HTTP client for the server:
//   - pin set: TLS without CA verification, but the leaf public key must
//     match the pin (fail closed).
//   - https without pin: normal system CA verification.
//   - http: plain.
func newHTTPClient(server, pin string) (*http.Client, error) {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConns:          4,
	}
	isHTTPS := strings.HasPrefix(server, "https://")
	if pin != "" {
		if !isHTTPS {
			return nil, fmt.Errorf("a pin was given but the server URL %q is not https", server)
		}
		if !strings.HasPrefix(pin, "sha256//") {
			return nil, fmt.Errorf("invalid pin %q: expected sha256//<base64>", pin)
		}
		want := []byte(pin)
		tr.TLSClientConfig = &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true, // replaced by the pin check below
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 {
					return errors.New("server sent no certificate")
				}
				got := PinFromCert(cs.PeerCertificates[0])
				if subtle.ConstantTimeCompare([]byte(got), want) != 1 {
					return fmt.Errorf("server public key pin mismatch (got %s, want %s): possible man-in-the-middle, or the server's TLS key was regenerated", got, pin)
				}
				return nil
			},
		}
	} else if isHTTPS {
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Transport: tr, Timeout: 2 * time.Minute}, nil
}

// api is a thin client for the akey HTTP protocol (see CONTRACT.md).
type api struct {
	base string
	hc   *http.Client
}

func newAPI(c Config) (*api, error) {
	hc, err := newHTTPClient(c.Server, c.Pin)
	if err != nil {
		return nil, err
	}
	return &api{base: c.Server, hc: hc}, nil
}

func (a *api) do(ctx context.Context, method, path string, body []byte, hdr map[string]string) (*http.Response, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, rd)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", "akey/"+Version)
	for k, v := range hdr {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", method, a.base+path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: reading response: %w", method, a.base+path, err)
	}
	return resp, b, nil
}

func statusErr(method, path string, resp *http.Response, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	if msg != "" {
		return fmt.Errorf("%s %s: server returned %s: %s", method, path, resp.Status, msg)
	}
	return fmt.Errorf("%s %s: server returned %s", method, path, resp.Status)
}

func (a *api) params(ctx context.Context) (vault.Params, error) {
	var p vault.Params
	resp, b, err := a.do(ctx, http.MethodGet, "/v1/params", nil, nil)
	if err != nil {
		return p, err
	}
	if resp.StatusCode != http.StatusOK {
		return p, statusErr("GET", "/v1/params", resp, b)
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("GET /v1/params: bad JSON: %w", err)
	}
	return p, nil
}

func rateLimited(resp *http.Response) error {
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil {
			return fmt.Errorf("%w (retry after %s)", ErrRateLimited, time.Duration(secs)*time.Second)
		}
		return fmt.Errorf("%w (retry after %s)", ErrRateLimited, ra)
	}
	return ErrRateLimited
}

// describeVersion renders the opaque X-Akey-Version (unix nanoseconds on
// the reference server) for humans.
func describeVersion(v string) string {
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 1e15 {
		return time.Unix(0, n).Local().Format("2006-01-02 15:04:05")
	}
	if v == "" {
		return "?"
	}
	return v
}

// getBlob returns the envelope and the opaque server version for id.
func (a *api) getBlob(ctx context.Context, id string) ([]byte, string, error) {
	resp, b, err := a.do(ctx, http.MethodGet, "/v1/blob/"+id, nil, nil)
	if err != nil {
		return nil, "", err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return b, resp.Header.Get("X-Akey-Version"), nil
	case http.StatusNotFound:
		return nil, "", ErrNotFound
	case http.StatusTooManyRequests:
		return nil, "", rateLimited(resp)
	}
	return nil, "", statusErr("GET", "/v1/blob/…", resp, b)
}

func (a *api) putBlob(ctx context.Context, id string, env []byte, writeToken, adminToken string) error {
	hdr := map[string]string{
		"Content-Type":       "application/json",
		"X-Akey-Write-Token": writeToken,
	}
	if adminToken != "" {
		hdr["Authorization"] = "Bearer " + adminToken
	}
	resp, b, err := a.do(ctx, http.MethodPut, "/v1/blob/"+id, env, hdr)
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return nil
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusForbidden:
		return ErrForbidden
	case http.StatusTooManyRequests:
		return rateLimited(resp)
	}
	return statusErr("PUT", "/v1/blob/…", resp, b)
}

// putWithAdminRetry PUTs the envelope; when creating a new id without an
// admin token (401) it asks for one once (via ask) and retries.
func putWithAdminRetry(ctx context.Context, a *api, id string, env []byte, writeToken, adminToken string, ask func() (string, error)) error {
	err := a.putBlob(ctx, id, env, writeToken, adminToken)
	if !errors.Is(err, ErrUnauthorized) || adminToken != "" || ask == nil {
		return err
	}
	tok, aerr := ask()
	if aerr != nil || tok == "" {
		return err
	}
	return a.putBlob(ctx, id, env, writeToken, tok)
}
