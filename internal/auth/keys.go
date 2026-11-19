package auth

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
)

// JWS algorithms (RFC 7518, RFC 8037) that the jwt authenticator supports.
var (
	hmacAlgorithms = []string{"HS256", "HS384", "HS512"}
	rsaAlgorithms  = []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512"}
	// asymmetricAlgorithms are the algorithms a JWKS may provide keys for.
	asymmetricAlgorithms = append(slices.Clone(rsaAlgorithms), "ES256", "ES384", "ES512", "EdDSA")
	supportedAlgorithms  = append(slices.Clone(hmacAlgorithms), asymmetricAlgorithms...)
)

// RSA key size limits (RFC 7518 requires at least 2048 bits; the upper bound
// keeps verification cheap).
const (
	minRSABits = 2048
	maxRSABits = 16384
)

// maxKeyFileBytes bounds key and secret files.
const maxKeyFileBytes = 1 << 20

// minHMACSecretBytes is the size below which an HMAC secret is reported as
// weak (RFC 7518 wants at least the hash size, 32 bytes for HS256).
const minHMACSecretBytes = 32

// verifyKey is a key that can verify token signatures.
type verifyKey struct {
	// kid is the key id; "" means the key has none and may verify tokens
	// with any (or no) "kid" header.
	kid string
	// alg restricts the key to one algorithm (the JWK "alg" member); ""
	// allows every algorithm of the key type.
	alg string
	// key is []byte (HMAC), *rsa.PublicKey, *ecdsa.PublicKey or
	// ed25519.PublicKey.
	key any
	// source describes the origin for logs (a file name or "jwks").
	source string
}

// algorithms returns the algorithms the key may verify.
func (k *verifyKey) algorithms() []string {
	algs := keyAlgorithms(k.key)
	if k.alg == "" {
		return algs
	}
	if slices.Contains(algs, k.alg) {
		return []string{k.alg}
	}
	return nil
}

// allows reports whether the key may verify tokens signed with alg.
func (k *verifyKey) allows(alg string) bool { return slices.Contains(k.algorithms(), alg) }

// keyAlgorithms returns the algorithms a key type supports. The result must
// not be modified.
func keyAlgorithms(key any) []string {
	switch k := key.(type) {
	case []byte:
		return hmacAlgorithms
	case *rsa.PublicKey:
		return rsaAlgorithms
	case *ecdsa.PublicKey:
		if k == nil {
			return nil
		}
		switch k.Curve {
		case elliptic.P256():
			return []string{"ES256"}
		case elliptic.P384():
			return []string{"ES384"}
		case elliptic.P521():
			return []string{"ES512"}
		}
	case ed25519.PublicKey:
		return []string{"EdDSA"}
	}
	return nil
}

// checkPublicKey rejects key types and sizes that must not verify tokens.
func checkPublicKey(key any) error {
	switch k := key.(type) {
	case *rsa.PublicKey:
		if k == nil || k.N == nil {
			return errors.New("RSA key without modulus")
		}
		switch bits := k.N.BitLen(); {
		case bits < minRSABits:
			return fmt.Errorf("RSA key has %d bits, at least %d are required", bits, minRSABits)
		case bits > maxRSABits:
			return fmt.Errorf("RSA key has %d bits, at most %d are supported", bits, maxRSABits)
		}
		return nil
	case *ecdsa.PublicKey:
		if k == nil || k.Curve == nil {
			return errors.New("ECDSA key without curve")
		}
		if keyAlgorithms(k) == nil {
			return fmt.Errorf("unsupported elliptic curve %s (want P-256, P-384 or P-521)", k.Curve.Params().Name)
		}
		return nil
	case ed25519.PublicKey:
		if len(k) != ed25519.PublicKeySize {
			return fmt.Errorf("Ed25519 key has %d bytes, want %d", len(k), ed25519.PublicKeySize)
		}
		return nil
	case *ecdh.PublicKey:
		return errors.New("ECDH (X25519) keys cannot verify signatures")
	default:
		return fmt.Errorf("unsupported key type %T", key)
	}
}

// keySet is an immutable list of verification keys.
type keySet struct {
	keys []*verifyKey
}

// find returns the keys that allow alg and satisfy match. It is nil-safe.
func (s *keySet) find(alg string, match func(*verifyKey) bool) []*verifyKey {
	if s == nil {
		return nil
	}
	var out []*verifyKey
	for _, k := range s.keys {
		if k.allows(alg) && match(k) {
			out = append(out, k)
		}
	}
	return out
}

// withKID returns the keys with key id kid that allow alg.
func (s *keySet) withKID(kid, alg string) []*verifyKey {
	return s.find(alg, func(k *verifyKey) bool { return k.kid == kid })
}

// withoutKID returns the keys without key id that allow alg.
func (s *keySet) withoutKID(alg string) []*verifyKey {
	return s.find(alg, func(k *verifyKey) bool { return k.kid == "" })
}

// forAlg returns all keys that allow alg.
func (s *keySet) forAlg(alg string) []*verifyKey {
	return s.find(alg, func(*verifyKey) bool { return true })
}

// kids lists the distinct key ids (for logs).
func (s *keySet) kids() []string {
	var out []string
	for _, k := range s.keys {
		if k.kid != "" && !slices.Contains(out, k.kid) {
			out = append(out, k.kid)
		}
	}
	return out
}

// loadHMACSecret reads a shared secret; surrounding white space (such as a
// trailing newline) is removed.
func loadHMACSecret(path string) ([]byte, error) {
	data, err := readFileLimited(path, maxKeyFileBytes)
	if err != nil {
		return nil, err
	}
	secret := bytes.TrimSpace(data)
	if len(secret) == 0 {
		return nil, fmt.Errorf("%s is empty", path)
	}
	return bytes.Clone(secret), nil
}

// loadPublicKeyFile reads all public keys from a PEM file. It accepts
// "PUBLIC KEY" (PKIX), "RSA PUBLIC KEY" (PKCS #1) and "CERTIFICATE" blocks.
func loadPublicKeyFile(path string, log *slog.Logger, now time.Time) ([]*verifyKey, error) {
	data, err := readFileLimited(path, maxKeyFileBytes)
	if err != nil {
		return nil, err
	}
	var keys []*verifyKey
	rest := data
	for n := 1; ; n++ {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		key, err := parsePEMPublicKey(block, path, log, now)
		if err != nil {
			return nil, fmt.Errorf("%s: PEM block %d (%s): %w", path, n, block.Type, err)
		}
		keys = append(keys, &verifyKey{key: key, source: path})
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%s: no PEM encoded public key found (DER files must be converted to PEM)", path)
	}
	return keys, nil
}

func parsePEMPublicKey(block *pem.Block, path string, log *slog.Logger, now time.Time) (any, error) {
	var (
		key any
		err error
	)
	switch block.Type {
	case "PUBLIC KEY":
		key, err = x509.ParsePKIXPublicKey(block.Bytes)
	case "RSA PUBLIC KEY":
		key, err = x509.ParsePKCS1PublicKey(block.Bytes)
	case "CERTIFICATE":
		var cert *x509.Certificate
		if cert, err = x509.ParseCertificate(block.Bytes); err == nil {
			key = cert.PublicKey
			if now.After(cert.NotAfter) {
				// Only the key is used; the certificate's validity does not
				// limit it, but an expired certificate hints at a stale file.
				log.Warn("JWT verification certificate has expired; its key is used anyway",
					"file", path, "subject", cert.Subject.String(), "notAfter", cert.NotAfter)
			}
		}
	default:
		if strings.Contains(block.Type, "PRIVATE KEY") {
			return nil, errors.New("private keys are not accepted; configure the public key")
		}
		return nil, errors.New("unsupported PEM block type (want PUBLIC KEY, RSA PUBLIC KEY or CERTIFICATE)")
	}
	if err != nil {
		return nil, err
	}
	if err := checkPublicKey(key); err != nil {
		return nil, err
	}
	return key, nil
}
