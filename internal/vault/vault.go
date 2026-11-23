// Package vault is the end-to-end encryption layer.
// OWNER: server agent.
package vault

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// Params are public KDF parameters served by GET /v1/params.
type Params struct {
	Version   int    `json:"version"`    // 1
	Salt      []byte `json:"salt"`       // server_salt, 32 random bytes (base64 in JSON)
	Time      uint32 `json:"time"`       // argon2id t (3)
	MemoryKiB uint32 `json:"memory_kib"` // argon2id m (65536)
	Threads   uint8  `json:"threads"`    // argon2id p (4)
}

// Keys are derived from the passphrase. Only ID and WriteToken ever leave
// the client; Enc never does.
type Keys struct {
	ID         string   // 64 hex chars; blob address on the server
	Enc        [32]byte // XChaCha20-Poly1305 key
	WriteToken string   // 64 hex chars; server stores only sha256(WriteToken)
}

var ErrDecrypt = errors.New("decryption failed (wrong passphrase or corrupted data)")

const (
	version  = 1
	saltLen  = 32
	alg      = "xchacha20poly1305"
	prefix   = "akey-v1|"
	minMemKi = 8 * 1024    // 8 MiB
	maxMemKi = 1024 * 1024 // 1 GiB
	maxTime  = 10
)

// DefaultParams returns params with a fresh random salt.
func DefaultParams() (Params, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return Params{}, err
	}
	return Params{Version: version, Salt: salt, Time: 3, MemoryKiB: 64 * 1024, Threads: 4}, nil
}

// Validate rejects unsupported or hostile parameters (e.g. served by a
// malicious server to exhaust client memory or weaken the KDF).
func (p Params) Validate() error {
	switch {
	case p.Version != version:
		return fmt.Errorf("vault: unsupported params version %d", p.Version)
	case len(p.Salt) != saltLen:
		return fmt.Errorf("vault: salt must be %d bytes, got %d", saltLen, len(p.Salt))
	case p.MemoryKiB < minMemKi || p.MemoryKiB > maxMemKi:
		return fmt.Errorf("vault: argon2 memory %d KiB out of range [%d, %d]", p.MemoryKiB, minMemKi, maxMemKi)
	case p.Time == 0 || p.Time > maxTime:
		return fmt.Errorf("vault: argon2 time %d out of range [1, %d]", p.Time, maxTime)
	case p.Threads == 0:
		return errors.New("vault: argon2 threads must be > 0")
	}
	return nil
}

// Derive: master = Argon2id(pass, "akey-v1|"+salt); HKDF-SHA256(master) with
// info "github.com/xxddccaa/akey/id", "github.com/xxddccaa/akey/enc", "github.com/xxddccaa/akey/write" -> ID, Enc, WriteToken.
func Derive(passphrase string, p Params) (*Keys, error) {
	if passphrase == "" {
		return nil, errors.New("vault: empty passphrase")
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return derive(passphrase, p), nil
}

func derive(passphrase string, p Params) *Keys {
	salt := append([]byte(prefix), p.Salt...)
	master := argon2.IDKey([]byte(passphrase), salt, p.Time, p.MemoryKiB, p.Threads, 32)
	var k Keys
	k.ID = hex.EncodeToString(expand(master, "github.com/xxddccaa/akey/id"))
	copy(k.Enc[:], expand(master, "github.com/xxddccaa/akey/enc"))
	k.WriteToken = hex.EncodeToString(expand(master, "github.com/xxddccaa/akey/write"))
	return &k
}

func expand(master []byte, info string) []byte {
	out := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, master, nil, []byte(info)), out); err != nil {
		panic(err) // cannot happen for 32 bytes of SHA-256 HKDF
	}
	return out
}

type envelope struct {
	V     int    `json:"v"`
	Alg   string `json:"alg"`
	Nonce string `json:"nonce"`
	CT    string `json:"ct"`
}

func aad(k *Keys) []byte { return []byte(prefix + k.ID) }

// Seal encrypts plaintext into a self-describing envelope (JSON bytes).
func Seal(k *Keys, plaintext []byte) ([]byte, error) {
	if k == nil {
		return nil, errors.New("vault: nil keys")
	}
	aead, err := chacha20poly1305.NewX(k.Enc[:])
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ct := aead.Seal(nil, nonce, plaintext, aad(k))
	return json.Marshal(envelope{
		V:     version,
		Alg:   alg,
		Nonce: base64.StdEncoding.EncodeToString(nonce),
		CT:    base64.StdEncoding.EncodeToString(ct),
	})
}

// Open decrypts an envelope produced by Seal. Returns ErrDecrypt on failure.
func Open(k *Keys, env []byte) ([]byte, error) {
	if k == nil {
		return nil, errors.New("vault: nil keys")
	}
	var e envelope
	if err := json.Unmarshal(env, &e); err != nil {
		return nil, fmt.Errorf("vault: malformed envelope: %w", err)
	}
	if e.V != version || e.Alg != alg {
		return nil, fmt.Errorf("vault: unsupported envelope v=%d alg=%q", e.V, e.Alg)
	}
	nonce, err := base64.StdEncoding.DecodeString(e.Nonce)
	if err != nil || len(nonce) != chacha20poly1305.NonceSizeX {
		return nil, ErrDecrypt
	}
	ct, err := base64.StdEncoding.DecodeString(e.CT)
	if err != nil {
		return nil, ErrDecrypt
	}
	aead, err := chacha20poly1305.NewX(k.Enc[:])
	if err != nil {
		return nil, err
	}
	pt, err := aead.Open(nil, nonce, ct, aad(k))
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// HashWriteToken returns hex(sha256(token)) as stored by the server.
func HashWriteToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
