package vault

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
)

// fastParams: valid but cheap (8 MiB, t=1) to keep tests quick.
func fastParams(t *testing.T) Params {
	t.Helper()
	p, err := DefaultParams()
	if err != nil {
		t.Fatal(err)
	}
	p.Time, p.MemoryKiB, p.Threads = 1, 8*1024, 1
	return p
}

func TestDefaultParams(t *testing.T) {
	p, err := DefaultParams()
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != 1 || len(p.Salt) != 32 || p.Time != 3 || p.MemoryKiB != 65536 || p.Threads != 4 {
		t.Fatalf("unexpected defaults: %+v", p)
	}
	q, _ := DefaultParams()
	if bytes.Equal(p.Salt, q.Salt) {
		t.Fatal("salt not random")
	}
	// Real defaults once.
	k, err := Derive("correct horse battery staple", p)
	if err != nil {
		t.Fatal(err)
	}
	if len(k.ID) != 64 || len(k.WriteToken) != 64 || k.ID == k.WriteToken {
		t.Fatalf("bad keys: %+v", k)
	}
	// JSON roundtrip (salt as base64).
	b, _ := json.Marshal(p)
	var r Params
	if err := json.Unmarshal(b, &r); err != nil || !bytes.Equal(r.Salt, p.Salt) {
		t.Fatalf("json roundtrip: %v %s", err, b)
	}
}

func TestDeriveDeterministic(t *testing.T) {
	p := fastParams(t)
	a, err := Derive("pw", p)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Derive("pw", p)
	if *a != *b {
		t.Fatal("derive not deterministic")
	}
	c, _ := Derive("pw2", p)
	if c.ID == a.ID || c.Enc == a.Enc || c.WriteToken == a.WriteToken {
		t.Fatal("different passphrase gave same keys")
	}
	p2 := p
	p2.Salt = append([]byte(nil), p.Salt...)
	p2.Salt[0] ^= 1
	d, _ := Derive("pw", p2)
	if d.ID == a.ID {
		t.Fatal("different salt gave same id")
	}
}

func TestDeriveRejects(t *testing.T) {
	good := fastParams(t)
	if _, err := Derive("", good); err == nil {
		t.Fatal("empty passphrase accepted")
	}
	cases := map[string]func(*Params){
		"version":  func(p *Params) { p.Version = 2 },
		"salt":     func(p *Params) { p.Salt = p.Salt[:16] },
		"memlow":   func(p *Params) { p.MemoryKiB = 1024 },
		"memhigh":  func(p *Params) { p.MemoryKiB = 2 * 1024 * 1024 },
		"time0":    func(p *Params) { p.Time = 0 },
		"timehigh": func(p *Params) { p.Time = 11 },
		"threads":  func(p *Params) { p.Threads = 0 },
	}
	for name, mut := range cases {
		p := good
		p.Salt = append([]byte(nil), good.Salt...)
		mut(&p)
		if _, err := Derive("pw", p); err == nil {
			t.Errorf("%s: hostile params accepted", name)
		}
	}
}

func TestSealOpen(t *testing.T) {
	p := fastParams(t)
	k, _ := Derive("pw", p)
	pt := []byte("api_key: sk-test-123\n")
	env, err := Seal(k, pt)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(env, []byte("sk-test")) {
		t.Fatal("plaintext leaked")
	}
	var e envelope
	if err := json.Unmarshal(env, &e); err != nil || e.V != 1 || e.Alg != "xchacha20poly1305" {
		t.Fatalf("envelope: %v %s", err, env)
	}
	got, err := Open(k, env)
	if err != nil || !bytes.Equal(got, pt) {
		t.Fatalf("roundtrip: %v %q", err, got)
	}
	env2, _ := Seal(k, pt)
	if bytes.Equal(env, env2) {
		t.Fatal("nonce reuse")
	}

	wrong, _ := Derive("wrong", p)
	// Same Enc but different ID must fail too (AAD binds ID).
	if _, err := Open(wrong, env); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	k2 := *k
	k2.ID = wrong.ID
	if _, err := Open(&k2, env); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("wrong aad: %v", err)
	}

	tamper := func(field string) []byte {
		var e envelope
		_ = json.Unmarshal(env, &e)
		s := &e.CT
		if field == "nonce" {
			s = &e.Nonce
		}
		raw, _ := base64.StdEncoding.DecodeString(*s)
		raw[0] ^= 0x80
		*s = base64.StdEncoding.EncodeToString(raw)
		b, _ := json.Marshal(e)
		return b
	}
	for _, f := range []string{"ct", "nonce"} {
		if _, err := Open(k, tamper(f)); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("tampered %s: %v", f, err)
		}
	}
	if _, err := Open(k, []byte("not json")); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestHashWriteToken(t *testing.T) {
	// sha256("abc")
	if got := HashWriteToken("abc"); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatal(got)
	}
}

// TestDeriveKnownAnswer freezes the key-derivation protocol (salt prefix,
// HKDF info strings, output encoding). If this fails, every config already
// stored on a server would become unreachable — bump Params.Version instead.
func TestDeriveKnownAnswer(t *testing.T) {
	salt := make([]byte, 32)
	for i := range salt {
		salt[i] = byte(i)
	}
	k, err := Derive("correct horse battery staple", Params{Version: 1, Salt: salt, Time: 1, MemoryKiB: 8192, Threads: 1})
	if err != nil {
		t.Fatal(err)
	}
	const wantID = "5314cfa4e9509662c3b343be3471da1a44a21e73c98c29445bb6edad12c70eb7"
	const wantWT = "b7742d0bea2efaba4007f640173b6fd7aedfb2483746a2bc5f741276410a3aad"
	if k.ID != wantID || k.WriteToken != wantWT {
		t.Fatalf("derivation changed:\n id=%s\n wt=%s", k.ID, k.WriteToken)
	}
}
