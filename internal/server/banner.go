package server

import (
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
)

// psPinnedOneLiner is for Windows PowerShell 5.1: enable TLS 1.2, accept only
// the certificate whose DER SHA-256 matches, fetch /i.ps1 and run it.
func psPinnedOneLiner(base, certSHA string) string {
	return "[Net.ServicePointManager]::SecurityProtocol=[Net.ServicePointManager]::SecurityProtocol -bor 3072;" +
		"[Net.ServicePointManager]::ServerCertificateValidationCallback={param($s,$c,$ch,$e)" +
		"[BitConverter]::ToString([Security.Cryptography.SHA256]::Create().ComputeHash($c.GetRawCertData())).Replace('-','') -eq '" + certSHA + "'};" +
		"iex (New-Object Net.WebClient).DownloadString('" + base + "/i.ps1')"
}

func psPlainOneLiner(base string) string {
	return "iex (New-Object Net.WebClient).DownloadString('" + base + "/i.ps1')"
}

func shPinnedOneLiner(base, pin string) string {
	return "curl -fsSk --pinnedpubkey '" + pin + "' " + base + "/i | sh"
}

func shPlainOneLiner(base string) string {
	return "curl -fsSL " + base + "/i | sh"
}

func (s *server) scheme() string {
	if s.cfg.InsecureHTTP {
		return "http"
	}
	return "https"
}

// banner is printed by `akey serve` once it listens on addr.
func (s *server) banner(w io.Writer, addr net.Addr) {
	port := "8787"
	if a, ok := addr.(*net.TCPAddr); ok {
		port = strconv.Itoa(a.Port)
	}
	s.writeBanner(w, fmt.Sprintf("akey %s server listening on %s://%s\n", s.cfg.Version, s.scheme(), addr), port, s.adminNew)
}

// writeBanner prints header plus connection details for port. showAdmin
// prints the admin token itself (first start, or `akey info`).
func (s *server) writeBanner(w io.Writer, header, port string, showAdmin bool) {
	scheme := s.scheme()
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	p("%s", header)
	p("  data dir:    %s\n", s.cfg.DataDir)
	switch {
	case s.cfg.DistDir != "":
		p("  dist dir:    %s\n", s.cfg.DistDir)
	case !s.readOnly: // `akey info` doesn't know the server's --dist
		p("  dist dir:    (none; /dl/ downloads disabled)\n")
	}
	if !s.cfg.InsecureHTTP {
		p("  tls pin:     %s\n", s.pin)
		p("  cert sha256: %s\n", s.certSHA)
	}
	switch {
	case showAdmin && s.adminNew:
		p("  admin token: %s\n", s.adminToken)
		p("               (generated now, saved to %s; needed once to create your config:\n", s.adminPath)
		p("                first panel save or `akey push`; print it again with `akey info --data %s`)\n", s.cfg.DataDir)
	case showAdmin:
		p("  admin token: %s\n", s.adminToken)
		p("               (from %s; needed once to create your config: first panel save or `akey push`)\n", s.adminPath)
	default:
		p("  admin token: stored in %s (print it with `akey info --data %s`)\n", s.adminPath, s.cfg.DataDir)
	}
	if s.cfg.InsecureHTTP {
		p("\n")
		p("  !!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!\n")
		p("  !! WARNING: --insecure-http: NO TLS. The install script, the binaries    !!\n")
		p("  !! and all traffic are unauthenticated; anyone on the network path can  !!\n")
		p("  !! swap the installer or binary. Use only on a trusted LAN.             !!\n")
		p("  !!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!\n")
	}
	hosts := s.cfg.Hosts
	if len(hosts) == 0 {
		hosts = []string{"127.0.0.1"}
		p("\n  (no --host given; pass --host <public-ip> to print commands for remote machines)\n")
	}
	p("\nWeb panel (open in a browser; edit the config, history, install commands):\n")
	for _, h := range hosts {
		p("  panel: %s://%s/\n", scheme, net.JoinHostPort(h, port))
	}
	if !s.cfg.InsecureHTTP {
		p("  The certificate is self-signed: before accepting the browser warning, check that the\n")
		p("  certificate's SHA-256 fingerprint equals the cert sha256 above.\n")
	}
	for _, h := range hosts {
		base := scheme + "://" + net.JoinHostPort(h, port)
		p("\nInstall + pull on a new machine via %s:\n", h)
		if s.cfg.InsecureHTTP {
			p("  Linux/macOS:\n    %s\n", shPlainOneLiner(base))
			p("  Windows PowerShell:\n    %s\n", psPlainOneLiner(base))
		} else {
			p("  Linux/macOS:\n    %s\n", shPinnedOneLiner(base, s.pin))
			p("  Windows PowerShell 5.1:\n    %s\n", psPinnedOneLiner(base, s.certSHA))
		}
	}
	p("\n  (extra pull flags: ... | sh -s -- --only codex,claude)\n")
	io.WriteString(w, b.String())
}
