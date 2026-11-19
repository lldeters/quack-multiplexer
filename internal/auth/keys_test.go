package auth

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadPublicKeyFile(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	rk := rsaKey(t)
	ec256, ec384, ec521 := ecKey(t, elliptic.P256()), ecKey(t, elliptic.P384()), ecKey(t, elliptic.P521())
	ed := edKey(t)
	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	ec224 := ecKey(t, elliptic.P224())
	x25519, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecPriv, err := x509.MarshalECPrivateKey(ec256)
	if err != nil {
		t.Fatal(err)
	}
	pkixRSA := pemPKIX(t, &rk.PublicKey)
	der, _ := pem.Decode(pkixRSA)

	tests := []struct {
		name     string
		content  []byte
		wantAlgs [][]string // per key, in file order
		wantErr  string
	}{
		{name: "PKIX RSA", content: pkixRSA, wantAlgs: [][]string{rsaAlgorithms}},
		{name: "PKCS1 RSA", content: pemPKCS1(&rk.PublicKey), wantAlgs: [][]string{rsaAlgorithms}},
		{name: "P-256", content: pemPKIX(t, &ec256.PublicKey), wantAlgs: [][]string{{"ES256"}}},
		{name: "P-384", content: pemPKIX(t, &ec384.PublicKey), wantAlgs: [][]string{{"ES384"}}},
		{name: "P-521", content: pemPKIX(t, &ec521.PublicKey), wantAlgs: [][]string{{"ES512"}}},
		{name: "Ed25519", content: pemPKIX(t, ed.Public()), wantAlgs: [][]string{{"EdDSA"}}},
		{name: "certificate", content: pemCert(t, ec256, now.Add(24*time.Hour)), wantAlgs: [][]string{{"ES256"}}},
		{name: "expired certificate is still used", content: pemCert(t, ed, now.Add(-time.Hour)), wantAlgs: [][]string{{"EdDSA"}}},
		{
			name:     "several blocks with text around them",
			content:  bytes.Join([][]byte{[]byte("# keys\n"), pkixRSA, []byte("comment\n"), pemPKIX(t, ed.Public()), pemCert(t, ec384, now.Add(time.Hour))}, nil),
			wantAlgs: [][]string{rsaAlgorithms, {"EdDSA"}, {"ES384"}},
		},
		{name: "private key", content: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: ecPriv}), wantErr: "private keys are not accepted"},
		{name: "unknown block type", content: pem.EncodeToMemory(&pem.Block{Type: "OPENSSH KEY", Bytes: []byte{1}}), wantErr: "unsupported PEM block type"},
		{name: "invalid DER", content: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte{1, 2, 3}}), wantErr: "PEM block 1 (PUBLIC KEY)"},
		{name: "RSA key too small", content: pemPKIX(t, &small.PublicKey), wantErr: "at least 2048"},
		{name: "P-224", content: pemPKIX(t, &ec224.PublicKey), wantErr: "unsupported elliptic curve P-224"},
		{name: "X25519", content: pemPKIX(t, x25519.PublicKey()), wantErr: "cannot verify signatures"},
		{name: "second block invalid", content: append(pemPKIX(t, ed.Public()), pemPKIX(t, &small.PublicKey)...), wantErr: "PEM block 2"},
		{name: "DER file", content: der.Bytes, wantErr: "no PEM encoded public key"},
		{name: "empty file", content: nil, wantErr: "no PEM encoded public key"},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeFile(t, dir, "key"+string(rune('a'+i))+".pem", tt.content)
			log, buf := testLogger()
			keys, err := loadPublicKeyFile(path, log, now)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				if !strings.Contains(err.Error(), path) {
					t.Errorf("error %q does not name the file", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got [][]string
			for _, k := range keys {
				got = append(got, k.algorithms())
				if k.kid != "" || k.source != path {
					t.Errorf("key kid=%q source=%q", k.kid, k.source)
				}
			}
			if !reflect.DeepEqual(got, tt.wantAlgs) {
				t.Fatalf("algorithms = %v, want %v", got, tt.wantAlgs)
			}
			if strings.HasPrefix(tt.name, "expired") && !strings.Contains(buf.String(), "certificate has expired") {
				t.Errorf("no warning for the expired certificate:\n%s", buf)
			}
		})
	}
	if _, err := loadPublicKeyFile(filepath.Join(dir, "missing.pem"), discardLogger(), now); err == nil {
		t.Error("missing file accepted")
	}
}

func TestLoadHMACSecret(t *testing.T) {
	dir := t.TempDir()
	got, err := loadHMACSecret(writeFile(t, dir, "s1", []byte("  \tsecret value\r\n\n")))
	if err != nil || string(got) != "secret value" {
		t.Fatalf("secret = %q, %v", got, err)
	}
	if _, err := loadHMACSecret(writeFile(t, dir, "s2", []byte(" \n\t\n"))); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("blank secret: %v", err)
	}
	if _, err := loadHMACSecret(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing secret file accepted")
	}
}

func TestCheckPublicKey(t *testing.T) {
	huge := &rsa.PublicKey{N: new(big.Int).Lsh(big.NewInt(1), maxRSABits), E: 65537} // maxRSABits+1 bits
	tests := []struct {
		name    string
		key     any
		wantErr string
	}{
		{name: "RSA", key: &rsaKey(t).PublicKey},
		{name: "nil RSA", key: (*rsa.PublicKey)(nil), wantErr: "without modulus"},
		{name: "RSA without modulus", key: &rsa.PublicKey{E: 65537}, wantErr: "without modulus"},
		{name: "huge RSA", key: huge, wantErr: "at most"},
		{name: "nil ECDSA", key: (*ecdsa.PublicKey)(nil), wantErr: "without curve"},
		{name: "short Ed25519", key: ed25519.PublicKey(make([]byte, 31)), wantErr: "31 bytes"},
		{name: "HMAC bytes", key: []byte("secret"), wantErr: "unsupported key type"},
		{name: "string", key: "secret", wantErr: "unsupported key type"},
		{name: "nil", key: nil, wantErr: "unsupported key type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkPublicKey(tt.key)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestVerifyKeyAlgorithms(t *testing.T) {
	ec := &ecKey(t, elliptic.P256()).PublicKey
	tests := []struct {
		name string
		key  *verifyKey
		want []string
	}{
		{name: "HMAC", key: &verifyKey{key: []byte("s")}, want: hmacAlgorithms},
		{name: "RSA", key: &verifyKey{key: &rsaKey(t).PublicKey}, want: rsaAlgorithms},
		{name: "RSA restricted by alg", key: &verifyKey{key: &rsaKey(t).PublicKey, alg: "PS384"}, want: []string{"PS384"}},
		{name: "alg that does not fit", key: &verifyKey{key: ec, alg: "ES384"}, want: nil},
		{name: "ECDSA", key: &verifyKey{key: ec}, want: []string{"ES256"}},
		{name: "nil ECDSA", key: &verifyKey{key: (*ecdsa.PublicKey)(nil)}, want: nil},
		{name: "Ed25519", key: &verifyKey{key: edKey(t).Public()}, want: []string{"EdDSA"}},
		{name: "unknown", key: &verifyKey{key: 42}, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.key.algorithms(); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("algorithms = %v, want %v", got, tt.want)
			}
			for _, alg := range supportedAlgorithms {
				want := false
				for _, w := range tt.want {
					want = want || w == alg
				}
				if got := tt.key.allows(alg); got != want {
					t.Errorf("allows(%s) = %v", alg, got)
				}
			}
		})
	}
}

func TestKeySetLookups(t *testing.T) {
	hm := &verifyKey{key: []byte("s")}
	e1 := &verifyKey{kid: "e1", key: &ecKey(t, elliptic.P256()).PublicKey}
	e2 := &verifyKey{kid: "e2", key: &ecKey(t, elliptic.P256()).PublicKey}
	e3 := &verifyKey{key: &ecKey(t, elliptic.P256()).PublicKey}
	ks := &keySet{keys: []*verifyKey{hm, e1, e2, e3}}
	if got := ks.withKID("e2", "ES256"); !reflect.DeepEqual(got, []*verifyKey{e2}) {
		t.Errorf("withKID = %v", got)
	}
	if got := ks.withKID("e2", "ES384"); got != nil {
		t.Errorf("withKID with another algorithm = %v", got)
	}
	if got := ks.withoutKID("ES256"); !reflect.DeepEqual(got, []*verifyKey{e3}) {
		t.Errorf("withoutKID = %v", got)
	}
	if got := ks.forAlg("ES256"); !reflect.DeepEqual(got, []*verifyKey{e1, e2, e3}) {
		t.Errorf("forAlg = %v", got)
	}
	if got := ks.forAlg("HS512"); !reflect.DeepEqual(got, []*verifyKey{hm}) {
		t.Errorf("forAlg(HS512) = %v", got)
	}
	if got := ks.kids(); !reflect.DeepEqual(got, []string{"e1", "e2"}) {
		t.Errorf("kids = %v", got)
	}
	var none *keySet
	if none.forAlg("ES256") != nil || none.withKID("x", "ES256") != nil || none.withoutKID("ES256") != nil {
		t.Error("nil key set returned keys")
	}
}
