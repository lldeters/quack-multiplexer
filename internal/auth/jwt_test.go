package auth

import (
	"context"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"quackmux/internal/config"
)

var jwtNow = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

// newTestJWT builds a JWT authenticator at the fixed time jwtNow.
func newTestJWT(t *testing.T, cfg config.JWTAuthConfig) *jwtAuthenticator {
	t.Helper()
	o := defaultJWTOptions()
	o.now = func() time.Time { return jwtNow }
	o.minOnDemand = time.Hour
	a, err := newJWT(cfg, discardLogger(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func authJWT(a *jwtAuthenticator, token string) (Identity, error) {
	return a.Authenticate(context.Background(), Request{Credential: token})
}

func mustReject(t *testing.T, a *jwtAuthenticator, what, token string) {
	t.Helper()
	id, err := authJWT(a, token)
	if err == nil {
		t.Errorf("%s: accepted as %+v", what, id)
	} else if !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("%s: error %v is not ErrUnauthenticated", what, err)
	}
}

// A token signed with the configured key is accepted with its user and
// groups; tampering, expiry, a future nbf or iat, a missing exp and a bad
// user claim are refused.
func TestJWTHMACAcceptsAndRefuses(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	a := newTestJWT(t, config.JWTAuthConfig{HMACSecretFile: writeFile(t, t.TempDir(), "hmac", secret)})
	good := stdClaims(jwtNow)
	good["groups"] = []any{"analytics", "ops", "analytics"}

	id, err := authJWT(a, signJWT(t, jwt.SigningMethodHS256, secret, nil, good))
	if err != nil {
		t.Fatal(err)
	}
	if id.User != "alice" || strings.Join(id.Groups, ",") != "analytics,ops" || id.Method != MethodJWT {
		t.Fatalf("identity %+v, want alice with analytics,ops", id)
	}

	tok := signJWT(t, jwt.SigningMethodHS256, secret, nil, good)
	parts := strings.Split(tok, ".")
	forged, _ := base64.RawURLEncoding.DecodeString(parts[1])
	forged = []byte(strings.Replace(string(forged), `"alice"`, `"admin"`, 1))
	mustReject(t, a, "a payload changed after signing", parts[0]+"."+base64.RawURLEncoding.EncodeToString(forged)+"."+parts[2])
	mustReject(t, a, "another secret", signJWT(t, jwt.SigningMethodHS256, []byte("another-secret-another-secret-00"), nil, good))
	mustReject(t, a, "an empty credential", "")
	mustReject(t, a, "no JWT at all", "not-a-jwt")

	for what, change := range map[string]jwt.MapClaims{
		"expired":                  {"exp": jwtNow.Add(-time.Minute).Unix()},
		"without exp":              {"exp": nil},
		"not valid yet (nbf)":      {"nbf": jwtNow.Add(time.Minute).Unix()},
		"issued in the future":     {"iat": jwtNow.Add(time.Minute).Unix()},
		"without the user claim":   {"sub": nil},
		"a numeric user claim":     {"sub": 42},
		"an empty user":            {"sub": ""},
		"a user with a line break": {"sub": "alice\nadmin"},
		"groups that are numbers":  {"groups": []any{1, 2}},
		"a group with white space": {"groups": []any{" ops"}},
	} {
		mustReject(t, a, what, signJWT(t, jwt.SigningMethodHS256, secret, nil, with(good, change)))
	}
	// The leeway (30 s by default) tolerates clock skew, not more.
	if _, err := authJWT(a, signJWT(t, jwt.SigningMethodHS256, secret, nil, with(good, jwt.MapClaims{"exp": jwtNow.Add(-20 * time.Second).Unix()}))); err != nil {
		t.Errorf("a token that expired 20s ago is within the leeway: %v", err)
	}
}

// Unsigned tokens and algorithms the keys do not fit are refused: "none",
// HMAC with the public key as its secret (algorithm confusion), and an
// algorithm outside auth.jwt.algorithms.
func TestJWTRefusesNoneAndAlgorithmConfusion(t *testing.T) {
	priv := rsaKey(t)
	dir := t.TempDir()
	pubPEM := pemPKIX(t, &priv.PublicKey)
	a := newTestJWT(t, config.JWTAuthConfig{PublicKeyFiles: []string{writeFile(t, dir, "rsa.pem", pubPEM)}})
	claims := stdClaims(jwtNow)

	if _, err := authJWT(a, signJWT(t, jwt.SigningMethodRS256, priv, nil, claims)); err != nil {
		t.Fatalf("RS256 with the configured key: %v", err)
	}
	mustReject(t, a, "alg none", signJWT(t, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, nil, claims))
	// The classic confusion: HS256 keyed with the bytes of the public key.
	der, _ := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	for what, key := range map[string][]byte{"the PEM file": pubPEM, "the DER bytes": der} {
		mustReject(t, a, "HS256 keyed with "+what+" of the public key", signJWT(t, jwt.SigningMethodHS256, key, nil, claims))
	}
	mustReject(t, a, "a crit header", signJWT(t, jwt.SigningMethodRS256, priv, map[string]any{"crit": []string{"exp"}}, claims))
	mustReject(t, a, "a numeric kid", signJWT(t, jwt.SigningMethodRS256, priv, map[string]any{"kid": 7}, claims))
	mustReject(t, a, "another RSA key", signJWT(t, jwt.SigningMethodRS256, otherRSAKey(t), nil, claims))

	// An HMAC secret next to the public key does not open the confusion
	// either: HS256 verifies with the secret only.
	secret := []byte("0123456789abcdef0123456789abcdef")
	both := newTestJWT(t, config.JWTAuthConfig{
		PublicKeyFiles: []string{writeFile(t, dir, "rsa2.pem", pubPEM)},
		HMACSecretFile: writeFile(t, dir, "hmac", secret),
	})
	mustReject(t, both, "HS256 keyed with the public key next to an HMAC secret", signJWT(t, jwt.SigningMethodHS256, pubPEM, nil, claims))
	if _, err := authJWT(both, signJWT(t, jwt.SigningMethodHS256, secret, nil, claims)); err != nil {
		t.Errorf("HS256 with the secret: %v", err)
	}

	// auth.jwt.algorithms narrows what the keys could verify.
	only512 := newTestJWT(t, config.JWTAuthConfig{
		PublicKeyFiles: []string{writeFile(t, dir, "rsa3.pem", pubPEM)}, Algorithms: []string{"rs512"},
	})
	mustReject(t, only512, "RS256 with algorithms [RS512]", signJWT(t, jwt.SigningMethodRS256, priv, nil, claims))
	if _, err := authJWT(only512, signJWT(t, jwt.SigningMethodRS512, priv, nil, claims)); err != nil {
		t.Errorf("RS512 with algorithms [RS512]: %v", err)
	}
	for _, algs := range [][]string{{"none"}, {"HS256"}, {"XS256"}} {
		o := defaultJWTOptions()
		if _, err := newJWT(config.JWTAuthConfig{PublicKeyFiles: []string{writeFile(t, dir, "rsa4.pem", pubPEM)}, Algorithms: algs},
			discardLogger(), o); err == nil {
			t.Errorf("auth.jwt.algorithms %v with an RSA key only was accepted", algs)
		}
	}
}

// otherRSAKey is an RSA key that is not the shared test key.
func otherRSAKey(t *testing.T) any {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// Issuer and audience are checked when configured.
func TestJWTIssuerAndAudience(t *testing.T) {
	priv := ecKey(t, elliptic.P256())
	a := newTestJWT(t, config.JWTAuthConfig{
		PublicKeyFiles: []string{writeFile(t, t.TempDir(), "ec.pem", pemPKIX(t, &priv.PublicKey))},
		Issuer:         "https://issuer.example", Audience: []string{"quackmux"},
	})
	good := with(stdClaims(jwtNow), jwt.MapClaims{"iss": "https://issuer.example", "aud": []any{"other", "quackmux"}})
	if _, err := authJWT(a, signJWT(t, jwt.SigningMethodES256, priv, nil, good)); err != nil {
		t.Fatalf("matching issuer and audience: %v", err)
	}
	for what, change := range map[string]jwt.MapClaims{
		"another issuer":   {"iss": "https://evil.example"},
		"no issuer":        {"iss": nil},
		"another audience": {"aud": "other"},
		"no audience":      {"aud": nil},
	} {
		mustReject(t, a, what, signJWT(t, jwt.SigningMethodES256, priv, nil, with(good, change)))
	}
}

// Keys from a JWKS are selected by key id; an unknown id refreshes the key
// set once (rotation), rate limited; keys for encryption are not used.
func TestJWTWithJWKSRotation(t *testing.T) {
	old, next := ecKey(t, elliptic.P256()), edKey(t)
	srv := newJWKSServer(t, jwksDoc(t, jwkOf(t, "k1", &old.PublicKey, nil),
		jwkOf(t, "enc", &rsaKey(t).PublicKey, map[string]any{"use": "enc"})))
	clock := newFakeClock(jwtNow)
	o := defaultJWTOptions()
	o.now, o.minOnDemand = clock.Now, 30*time.Second
	a, err := newJWT(config.JWTAuthConfig{JWKSURL: srv.URL}, discardLogger(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	claims := stdClaims(jwtNow)

	if _, err := authJWT(a, signJWT(t, jwt.SigningMethodES256, old, kid("k1"), claims)); err != nil {
		t.Fatalf("a token of key k1: %v", err)
	}
	mustReject(t, a, "a key published for encryption only", signJWT(t, jwt.SigningMethodRS256, rsaKey(t), kid("enc"), claims))
	mustReject(t, a, "k1 with another algorithm's key", signJWT(t, jwt.SigningMethodEdDSA, next, kid("k1"), claims))

	// The issuer rotates to k2. The first token with the unknown id came
	// too soon after the last download and is refused without a fetch.
	srv.set(http.StatusOK, jwksDoc(t, jwkOf(t, "k2", next.Public(), nil)))
	hits := srv.hits.Load()
	rotated := signJWT(t, jwt.SigningMethodEdDSA, next, kid("k2"), claims)
	mustReject(t, a, "an unknown key id right after a download", rotated)
	if got := srv.hits.Load(); got != hits {
		t.Fatalf("%d JWKS downloads within the on-demand interval, want none", got-hits)
	}
	clock.Add(31 * time.Second)
	if _, err := authJWT(a, rotated); err != nil {
		t.Fatalf("a token of the rotated key k2: %v", err)
	}
	if got := srv.hits.Load(); got != hits+1 {
		t.Fatalf("%d JWKS downloads for the rotation, want 1", got-hits)
	}
	mustReject(t, a, "the key that was rotated out", signJWT(t, jwt.SigningMethodES256, old, kid("k1"), claims))

	// A flood of unknown ids does not turn into a flood of downloads, and a
	// failing JWKS endpoint keeps the keys.
	srv.set(http.StatusInternalServerError, nil)
	clock.Add(31 * time.Second)
	for i := range 20 {
		mustReject(t, a, "an unknown key id", signJWT(t, jwt.SigningMethodEdDSA, next, kid("nope"+string(rune('a'+i))), claims))
	}
	if got := srv.hits.Load(); got != hits+2 {
		t.Fatalf("%d downloads for 20 unknown ids, want 1", got-hits-1)
	}
	if _, err := authJWT(a, rotated); err != nil {
		t.Fatalf("k2 after a failed refresh: %v", err)
	}
}

// A nested groups claim is found by its path (Keycloak's realm_access.roles).
func TestJWTNestedGroupsClaim(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	a := newTestJWT(t, config.JWTAuthConfig{
		HMACSecretFile: writeFile(t, t.TempDir(), "hmac", secret),
		UserClaim:      "preferred_username", GroupsClaim: "realm_access.roles",
	})
	claims := with(stdClaims(jwtNow), jwt.MapClaims{
		"preferred_username": "bob", "realm_access": map[string]any{"roles": []any{"analytics"}},
	})
	id, err := authJWT(a, signJWT(t, jwt.SigningMethodHS256, secret, nil, claims))
	if err != nil {
		t.Fatal(err)
	}
	if id.User != "bob" || strings.Join(id.Groups, ",") != "analytics" {
		t.Fatalf("identity %+v, want bob with analytics", id)
	}
}
