package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// POST /v1/probe lists a provider's models on behalf of the browser panel
// (browsers can't call most provider APIs directly because of CORS). The key
// is used for this one outbound request only: never logged, stored or echoed.

const (
	probeMaxBody      = 64 << 10
	probeMaxResp      = 2 << 20
	probeTimeout      = 10 * time.Second
	probeMaxRedirects = 3
	probeMaxKey       = 8 << 10
)

type probeRequest struct {
	BaseURL string `json:"base_url"`
	API     string `json:"api"`  // openai | anthropic
	Key     string `json:"key"`  // provider API key (may be empty for keyless local servers)
	Auth    string `json:"auth"` // anthropic only: api_key (default) | bearer
}

type probeModel struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	ContextWindow int64  `json:"context_window,omitempty"`
}

type upstreamError struct{ msg string }

func (e *upstreamError) Error() string { return e.msg }

// Cloud metadata endpoints are never legitimate model APIs. Loopback and
// private ranges stay allowed (local model servers on the VPS or LAN).
var probeBlockedIPs = []net.IP{
	net.ParseIP("100.100.100.200"), // Alibaba Cloud metadata
	net.ParseIP("fd00:ec2::254"),   // AWS IMDS over IPv6
}

// checkProbeHost rejects link-local / metadata IP literals. It runs on the
// URL host (also catches targets reached through an HTTP(S)_PROXY) and on
// every dialed address (catches DNS names resolving there).
func checkProbeHost(host string) error {
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return nil
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("refusing to connect to %s (link-local / metadata address)", ip)
	}
	for _, b := range probeBlockedIPs {
		if ip.Equal(b) {
			return fmt.Errorf("refusing to connect to %s (cloud metadata address)", ip)
		}
	}
	return nil
}

func probeDialControl(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	return checkProbeHost(host)
}

func newProbeClient() *http.Client {
	d := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second, Control: probeDialControl}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           d.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: probeTimeout,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{Transport: tr, Timeout: probeTimeout, CheckRedirect: probeRedirect}
}

func probeRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > probeMaxRedirects {
		return fmt.Errorf("more than %d redirects", probeMaxRedirects)
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("redirect to unsupported scheme %q", req.URL.Scheme)
	}
	if err := checkProbeHost(req.URL.Hostname()); err != nil {
		return err
	}
	// net/http drops Authorization on cross-host redirects but not custom
	// headers; never forward the key to another host.
	if !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
		req.Header.Del("Authorization")
		req.Header.Del("X-Api-Key")
	}
	return nil
}

// probeAuth accepts the X-Akey-Id + X-Akey-Write-Token of an existing config,
// or the admin token (needed before the first config exists).
func (s *server) probeAuth(r *http.Request) (status int, msg string) {
	id, tok := r.Header.Get("X-Akey-Id"), r.Header.Get("X-Akey-Write-Token")
	bearer, hasBearer := bearerToken(r)
	if id == "" && tok == "" && !hasBearer {
		return http.StatusUnauthorized, "credentials required: X-Akey-Id + X-Akey-Write-Token, or Authorization: Bearer <admin token>"
	}
	if idRe.MatchString(id) && tokenRe.MatchString(tok) {
		s.mu.RLock()
		st, m := s.checkWriteToken(filepath.Join(s.blobDir, id), tok)
		s.mu.RUnlock()
		if st == 0 {
			return 0, ""
		}
		if st == http.StatusInternalServerError {
			return st, m
		}
	}
	if hasBearer && s.adminOK(bearer) {
		return 0, ""
	}
	return http.StatusForbidden, "invalid credentials"
}

func (s *server) probe(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.tooMany(w, ip) {
		return
	}
	if status, msg := s.probeAuth(r); status != 0 {
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			s.miss(ip)
		}
		http.Error(w, msg, status)
		return
	}
	if r.ContentLength > probeMaxBody {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, probeMaxBody))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "read body", http.StatusBadRequest)
		}
		return
	}
	var req probeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, `body must be JSON {"base_url","api","key","auth"}`, http.StatusBadRequest)
		return
	}
	target, hdr, err := req.build()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	models, err := s.fetchModels(r.Context(), target, hdr)
	if err != nil {
		var ue *upstreamError
		if !errors.As(err, &ue) {
			err = &upstreamError{"upstream request failed: " + redactURLError(err)}
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"models": models})
}

// build validates the request and returns the models URL and auth headers.
// Error messages never include the key.
func (p *probeRequest) build() (string, http.Header, error) {
	u, err := url.Parse(strings.TrimSpace(p.BaseURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", nil, errors.New("base_url must be an absolute http:// or https:// URL")
	}
	if u.User != nil {
		return "", nil, errors.New("base_url must not contain credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", nil, errors.New("base_url must not contain a query or fragment")
	}
	if len(p.Key) > probeMaxKey || strings.ContainsAny(p.Key, "\r\n\x00") {
		return "", nil, errors.New("invalid key")
	}
	key := strings.TrimSpace(p.Key)
	path := strings.TrimRight(u.Path, "/")
	hdr := http.Header{}
	hdr.Set("Accept", "application/json")
	hdr.Set("User-Agent", "akey-probe")
	switch p.API {
	case "openai", "":
		u.Path = path + "/models"
		if key != "" {
			hdr.Set("Authorization", "Bearer "+key)
		}
	case "anthropic":
		u.Path = strings.TrimSuffix(path, "/v1") + "/v1/models"
		u.RawQuery = "limit=1000"
		hdr.Set("anthropic-version", "2023-06-01")
		switch p.Auth {
		case "api_key", "":
			if key != "" {
				hdr.Set("X-Api-Key", key)
			}
		case "bearer":
			if key != "" {
				hdr.Set("Authorization", "Bearer "+key)
			}
		default:
			return "", nil, errors.New(`auth must be "api_key" or "bearer"`)
		}
	default:
		return "", nil, errors.New(`api must be "openai" or "anthropic"`)
	}
	u.RawPath = ""
	return u.String(), hdr, nil
}

func (s *server) fetchModels(ctx context.Context, target string, hdr http.Header) ([]probeModel, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header = hdr
	if err := checkProbeHost(req.URL.Hostname()); err != nil {
		return nil, &upstreamError{err.Error()}
	}
	resp, err := s.probeHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return nil, &upstreamError{fmt.Sprintf("upstream returned HTTP %d %s for GET %s",
			resp.StatusCode, http.StatusText(resp.StatusCode), redactURL(resp.Request.URL))}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, probeMaxResp+1))
	if err != nil {
		return nil, err
	}
	if len(body) > probeMaxResp {
		return nil, &upstreamError{fmt.Sprintf("upstream response larger than %d bytes", probeMaxResp)}
	}
	models, err := parseModels(body)
	if err != nil {
		return nil, &upstreamError{"upstream response is not a model list: " + err.Error()}
	}
	return models, nil
}

type upstreamModel struct {
	ID            string `json:"id"`
	DisplayName   string `json:"display_name"` // anthropic
	Name          string `json:"name"`         // openrouter
	ContextLength any    `json:"context_length"`
}

// parseModels accepts {"data":[{id,...}]} (OpenAI, Anthropic, OpenRouter,
// most compatible gateways) or a bare array of the same objects.
func parseModels(body []byte) ([]probeModel, error) {
	var list []upstreamModel
	var doc struct {
		Data *[]upstreamModel `json:"data"`
	}
	trimmed := bytes.TrimSpace(body)
	switch {
	case len(trimmed) > 0 && trimmed[0] == '[':
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return nil, errors.New("invalid JSON array")
		}
	default:
		if err := json.Unmarshal(trimmed, &doc); err != nil {
			return nil, errors.New("invalid JSON")
		}
		if doc.Data == nil {
			return nil, errors.New(`no "data" array`)
		}
		list = *doc.Data
	}
	seen := map[string]bool{}
	out := []probeModel{}
	for _, m := range list {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		pm := probeModel{ID: id, Name: strings.TrimSpace(m.DisplayName)}
		if pm.Name == "" {
			pm.Name = strings.TrimSpace(m.Name)
		}
		switch v := m.ContextLength.(type) {
		case float64:
			if v > 0 && v < 1e12 {
				pm.ContextWindow = int64(v)
			}
		case string:
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				pm.ContextWindow = n
			}
		}
		out = append(out, pm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// redactURL drops userinfo and the query (a few gateways take keys there).
func redactURL(u *url.URL) string {
	c := *u
	c.User = nil
	c.RawQuery = ""
	c.Fragment = ""
	return c.String()
}

func redactURLError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		msg := ue.Err.Error()
		if p, perr := url.Parse(ue.URL); perr == nil {
			return fmt.Sprintf("%s %s: %s", ue.Op, redactURL(p), msg)
		}
		return msg
	}
	return err.Error()
}
