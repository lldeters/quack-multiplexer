package auth

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testLogger returns a logger that records every level.
func testLogger() (*slog.Logger, *syncBuffer) {
	b := &syncBuffer{}
	return slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})), b
}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// eventually polls cond until it holds or the timeout expires.
func eventually(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// writeFile writes content to dir/name and returns the path.
func writeFile(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeClock is a manually advanced clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// Test keys. RSA key generation is slow on 32-bit platforms, so one RSA key
// is shared by all tests.
var sharedRSAKey = sync.OnceValues(func() (*rsa.PrivateKey, error) {
	return rsa.GenerateKey(rand.Reader, 2048)
})

func rsaKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := sharedRSAKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func ecKey(t *testing.T, curve elliptic.Curve) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func edKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// pemPKIX encodes a public key as a "PUBLIC KEY" PEM block.
func pemPKIX(t *testing.T, pub any) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// pemPKCS1 encodes an RSA public key as an "RSA PUBLIC KEY" PEM block.
func pemPKCS1(pub *rsa.PublicKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(pub)})
}

// pemCert encodes a self-signed certificate for priv.
func pemCert(t *testing.T, priv crypto.Signer, notAfter time.Time) []byte {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "quackmux test issuer"},
		NotBefore:    notAfter.Add(-48 * time.Hour),
		NotAfter:     notAfter,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, priv.Public(), priv)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// jwkOf renders a public key as a JWK; extra members are added verbatim.
func jwkOf(t *testing.T, kid string, pub any, extra map[string]any) map[string]any {
	t.Helper()
	m := map[string]any{}
	if kid != "" {
		m["kid"] = kid
	}
	switch k := pub.(type) {
	case *rsa.PublicKey:
		m["kty"], m["n"], m["e"] = "RSA", b64(k.N.Bytes()), b64(big.NewInt(int64(k.E)).Bytes())
	case *ecdsa.PublicKey:
		raw, err := k.Bytes()
		if err != nil {
			t.Fatal(err)
		}
		size := (len(raw) - 1) / 2
		m["kty"], m["crv"], m["x"], m["y"] = "EC", k.Curve.Params().Name, b64(raw[1:1+size]), b64(raw[1+size:])
	case ed25519.PublicKey:
		m["kty"], m["crv"], m["x"] = "OKP", "Ed25519", b64(k)
	default:
		t.Fatalf("jwkOf: unsupported key %T", pub)
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// jwksDoc renders a JWKS document.
func jwksDoc(t *testing.T, keys ...map[string]any) []byte {
	t.Helper()
	if keys == nil {
		keys = []map[string]any{}
	}
	b, err := json.Marshal(map[string]any{"keys": keys})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// jwksServer serves a replaceable JWKS document and counts requests.
type jwksServer struct {
	*httptest.Server
	hits atomic.Int32

	mu     sync.Mutex
	status int
	body   []byte
}

func newJWKSServer(t *testing.T, body []byte) *jwksServer {
	t.Helper()
	s := &jwksServer{status: http.StatusOK, body: body}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		s.mu.Lock()
		status, body := s.status, s.body
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/jwk-set+json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *jwksServer) set(status int, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.body = status, body
}

// signJWT signs claims; header entries are added to the JOSE header.
func signJWT(t *testing.T, method jwt.SigningMethod, key any, header map[string]any, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(method, claims)
	for k, v := range header {
		tok.Header[k] = v
	}
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// stdClaims are valid claims for user alice at now.
func stdClaims(now time.Time) jwt.MapClaims {
	return jwt.MapClaims{
		"sub": "alice",
		"iat": now.Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
}

// with returns a copy of c with changes applied; a nil value deletes.
func with(c jwt.MapClaims, changes jwt.MapClaims) jwt.MapClaims {
	out := jwt.MapClaims{}
	for k, v := range c {
		out[k] = v
	}
	for k, v := range changes {
		if v == nil {
			delete(out, k)
		} else {
			out[k] = v
		}
	}
	return out
}

// kid returns a JOSE header with a key id.
func kid(id string) map[string]any { return map[string]any{"kid": id} }
