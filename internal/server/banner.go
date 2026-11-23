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

func (s *server) banner(w io.Writer, addr net.Addr) {
	port := "8787"
	if a, ok := addr.(*net.TCPAddr); ok {
		port = strconv.Itoa(a.Port)
	}
	scheme := "https"
	if s.cfg.InsecureHTTP {
		scheme = "http"
	}
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	p("akey server listening on %s://%s\n", scheme, addr)
	p("  data dir:    %s\n", s.cfg.DataDir)
	if s.cfg.DistDir != "" {
		p("  dist dir:    %s\n", s.cfg.DistDir)
	} else {
		p("  dist dir:    (none; /dl/ downloads disabled)\n")
	}
	if !s.cfg.InsecureHTTP {
		p("  tls pin:     %s\n", s.pin)
		p("  cert sha256: %s\n", s.certSHA)
	}
	if s.adminNew {
		p("  admin token: %s\n", s.adminToken)
		p("               (generated now, saved to %s; shown only once - needed for the first `akey push`)\n", s.adminPath)
	} else {
		p("  admin token: stored in %s\n", s.adminPath)
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
