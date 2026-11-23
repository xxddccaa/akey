package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// loadCert loads tls/cert.pem + tls/key.pem, generating them on first start.
// An existing cert is never regenerated (that would break every pin); hosts
// missing from its SANs only produce a warning.
func (s *server) loadCert() error {
	dir := filepath.Join(s.cfg.DataDir, "tls")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	_, errC := os.Stat(certPath)
	_, errK := os.Stat(keyPath)
	switch {
	case errors.Is(errC, fs.ErrNotExist) && errors.Is(errK, fs.ErrNotExist):
		certPEM, keyPEM, err := genCert(s.cfg.Hosts)
		if err != nil {
			return err
		}
		if err := writeFileAtomic(keyPath, keyPEM); err != nil {
			return err
		}
		if err := writeFileAtomic(certPath, certPEM); err != nil {
			return err
		}
		s.logf("generated self-signed TLS certificate in %s", dir)
	case errC != nil || errK != nil:
		return fmt.Errorf("server: need both %s and %s (cert=%v, key=%v)", certPath, keyPath, errC, errK)
	}
	c, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return fmt.Errorf("server: load TLS keypair: %w", err)
	}
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return err
	}
	c.Leaf = leaf
	s.tlsCert = &c
	s.pin = pinOf(leaf)
	sum := sha256.Sum256(leaf.Raw)
	s.certSHA = strings.ToUpper(hex.EncodeToString(sum[:]))
	for _, h := range s.cfg.Hosts {
		if leaf.VerifyHostname(h) != nil {
			s.logf("warning: host %q is not in the TLS certificate SANs; keeping the existing cert so pins stay valid "+
				"(pinned clients still work). Delete %s to regenerate - this changes the pin.", h, dir)
		}
	}
	return nil
}

func pinOf(leaf *x509.Certificate) string {
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return "sha256//" + base64.StdEncoding.EncodeToString(sum[:])
}

func genCert(hosts []string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "akey"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	seen := map[string]bool{}
	for _, h := range append(append([]string{}, hosts...), "127.0.0.1", "localhost") {
		if seen[h] {
			continue
		}
		seen[h] = true
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), nil
}
