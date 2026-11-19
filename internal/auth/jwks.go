package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// jwksFetchTimeout bounds one JWKS download.
	jwksFetchTimeout = 10 * time.Second
	// jwksOnDemandInterval is the minimum spacing of the refreshes that
	// tokens with unknown key ids trigger.
	jwksOnDemandInterval = 30 * time.Second
	// jwksRetryMin and jwksRetryMax bound the back-off of the periodic
	// refresh after failures.
	jwksRetryMin = 5 * time.Second
	jwksRetryMax = 5 * time.Minute
	// maxJWKSBytes bounds a JWKS document.
	maxJWKSBytes = 1 << 20
)

var (
	errJWKSRateLimited = errors.New("the JWKS was fetched recently")
	errJWKSClosed      = errors.New("the authenticator is closed")
	// errSkipKey marks JWKS entries that are ignored on purpose (encryption
	// keys, symmetric keys, unsupported curves or algorithms).
	errSkipKey = errors.New("not a supported signature verification key")
)

// jwksSource downloads and caches a JSON Web Key Set. At most one download
// runs at a time; concurrent refresh calls wait for it.
type jwksSource struct {
	url    string
	logURL string // without password
	client *http.Client
	log    *slog.Logger
	now    func() time.Time
	// minOnDemand is the minimum time between the start of the last
	// download and an on-demand refresh.
	minOnDemand time.Duration
	retryMin    time.Duration

	ctx    context.Context // cancelled by close; aborts downloads
	cancel context.CancelFunc
	wg     sync.WaitGroup

	keys atomic.Pointer[keySet] // nil until the first successful download

	mu          sync.Mutex
	closed      bool
	inflight    *jwksFetch
	lastAttempt time.Time
	digest      [sha256.Size]byte // of the last accepted document
}

// jwksFetch is a running download. err is set before done is closed.
type jwksFetch struct {
	done chan struct{}
	err  error
}

func newJWKSSource(rawURL string, o jwtOptions, log *slog.Logger) (*jwksSource, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err // without the URL, which may hold a password
		}
		return nil, fmt.Errorf("auth.jwt.jwksURL is not a valid URL: %v", err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("auth.jwt.jwksURL must be an absolute http or https URL, got %q", u.Redacted())
	}
	if u.Scheme == "http" {
		log.Warn("the JWKS is fetched over plain HTTP; anyone on the network path could substitute keys", "url", u.Redacted())
	}
	client := o.httpClient
	if client == nil {
		client = &http.Client{Timeout: jwksFetchTimeout}
	}
	retryMin := o.retryMin
	if retryMin <= 0 {
		retryMin = jwksRetryMin
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &jwksSource{
		url:         u.String(),
		logURL:      u.Redacted(),
		client:      client,
		log:         log,
		now:         o.now,
		minOnDemand: o.minOnDemand,
		retryMin:    retryMin,
		ctx:         ctx,
		cancel:      cancel,
	}, nil
}

// start downloads the key set once (logging, not returning, a failure) and
// then refreshes it every interval in the background (0 disables).
func (s *jwksSource) start(interval time.Duration) {
	err := s.refresh(context.Background(), true)
	if interval > 0 {
		s.wg.Add(1)
		go s.run(interval, err != nil)
	}
}

// current returns the cached key set (nil before the first download). It is
// nil-safe.
func (s *jwksSource) current() *keySet {
	if s == nil {
		return nil
	}
	return s.keys.Load()
}

// refresh downloads the key set and waits for the result, or until ctx is
// done (the download then continues for other waiters). If a download is
// already running, it waits for that one instead. Unless force is set, it
// returns errJWKSRateLimited when the last download started less than
// minOnDemand ago.
func (s *jwksSource) refresh(ctx context.Context, force bool) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errJWKSClosed
	}
	f := s.inflight
	if f == nil {
		if !force && !s.lastAttempt.IsZero() && s.now().Sub(s.lastAttempt) < s.minOnDemand {
			s.mu.Unlock()
			return errJWKSRateLimited
		}
		f = s.startLocked()
	}
	s.mu.Unlock()
	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// startLocked starts a download. s.mu must be held and s.closed be false.
func (s *jwksSource) startLocked() *jwksFetch {
	f := &jwksFetch{done: make(chan struct{})}
	s.inflight = f
	s.lastAttempt = s.now()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		f.err = s.update()
		s.mu.Lock()
		s.inflight = nil
		s.mu.Unlock()
		close(f.done)
	}()
	return f
}

// run refreshes the key set periodically, retrying failures with back-off.
func (s *jwksSource) run(interval time.Duration, failed bool) {
	defer s.wg.Done()
	failures := 0
	if failed {
		failures = 1
	}
	timer := time.NewTimer(s.delay(interval, failures))
	defer timer.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-timer.C:
		}
		if err := s.refresh(s.ctx, true); err != nil {
			if s.ctx.Err() != nil {
				return
			}
			failures++
		} else {
			failures = 0
		}
		timer.Reset(s.delay(interval, failures))
	}
}

// delay is the time until the next periodic refresh: the interval, or after
// failures a back-off that starts at retryMin and doubles up to
// jwksRetryMax, but never exceeds the interval.
func (s *jwksSource) delay(interval time.Duration, failures int) time.Duration {
	if failures == 0 {
		return interval
	}
	d := s.retryMin
	for i := 1; i < failures && d < jwksRetryMax; i++ {
		d *= 2
	}
	return min(d, interval, jwksRetryMax)
}

// close stops the periodic refresh, aborts a running download and waits
// for all goroutines. It is idempotent.
func (s *jwksSource) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.cancel()
	s.wg.Wait()
}

// update downloads, parses and publishes the key set; on failure the cached
// keys stay. Only one update runs at a time.
func (s *jwksSource) update() error {
	body, err := s.download()
	if err == nil {
		err = s.store(body)
	}
	if err != nil && s.ctx.Err() == nil {
		if s.keys.Load() == nil {
			s.log.Error("JWKS download failed; tokens that need its keys are rejected until a download succeeds", "url", s.logURL, "err", err)
		} else {
			s.log.Warn("JWKS refresh failed; keeping the previous keys", "url", s.logURL, "err", err)
		}
	}
	return err
}

func (s *jwksSource) download() ([]byte, error) {
	ctx, cancel := context.WithTimeout(s.ctx, jwksFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/jwk-set+json, application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Drain a little so that the connection can be reused.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("unexpected HTTP status %q", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read JWKS: %w", err)
	}
	if len(body) > maxJWKSBytes {
		return nil, fmt.Errorf("the JWKS document is larger than %d bytes", maxJWKSBytes)
	}
	return body, nil
}

func (s *jwksSource) store(body []byte) error {
	digest := sha256.Sum256(body)
	s.mu.Lock()
	unchanged := digest == s.digest && s.keys.Load() != nil
	s.mu.Unlock()
	if unchanged {
		s.log.Debug("JWKS unchanged", "url", s.logURL)
		return nil
	}
	ks, skipped, err := parseJWKS(body)
	for _, sk := range skipped {
		level := slog.LevelWarn
		if errors.Is(sk.err, errSkipKey) {
			level = slog.LevelDebug
		}
		s.log.Log(context.Background(), level, "ignoring JWKS key", "url", s.logURL, "index", sk.index, "kid", sk.kid, "reason", sk.err)
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.digest = digest
	s.mu.Unlock()
	s.keys.Store(ks)
	s.log.Info("JWKS loaded", "url", s.logURL, "keys", len(ks.keys), "kids", ks.kids())
	return nil
}

// skippedKey describes a JWKS entry that was not used.
type skippedKey struct {
	index int
	kid   string
	err   error
}

// parseJWKS parses a JWKS document (RFC 7517). Entries that cannot be used
// are skipped; the document is only rejected if it is malformed or contains
// no usable key.
func parseJWKS(body []byte) (*keySet, []skippedKey, error) {
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, nil, fmt.Errorf("invalid JWKS document: %w", err)
	}
	if doc.Keys == nil {
		return nil, nil, errors.New(`invalid JWKS document: no "keys" array`)
	}
	ks := &keySet{}
	var skipped []skippedKey
	for i, raw := range doc.Keys {
		k, kid, err := parseJWK(raw)
		if err != nil {
			skipped = append(skipped, skippedKey{index: i, kid: kid, err: err})
			continue
		}
		ks.keys = append(ks.keys, k)
	}
	if len(ks.keys) == 0 {
		return nil, skipped, errors.New("the JWKS contains no usable signature verification key")
	}
	return ks, skipped, nil
}

// jwkJSON holds the JWK members (RFC 7517, RFC 7518, RFC 8037) we use.
type jwkJSON struct {
	Kty    string   `json:"kty"`
	Kid    string   `json:"kid"`
	Use    string   `json:"use"`
	Alg    string   `json:"alg"`
	KeyOps []string `json:"key_ops"`
	Crv    string   `json:"crv"`
	N      string   `json:"n"`
	E      string   `json:"e"`
	X      string   `json:"x"`
	Y      string   `json:"y"`
}

// parseJWK converts one JWK into a verification key. It also returns the
// key id (if it could be decoded) for error reports.
func parseJWK(raw json.RawMessage) (*verifyKey, string, error) {
	var j jwkJSON
	if err := json.Unmarshal(raw, &j); err != nil {
		return nil, "", fmt.Errorf("invalid JWK: %w", err)
	}
	switch {
	case j.Use != "" && j.Use != "sig":
		return nil, j.Kid, fmt.Errorf("%w: use is %q", errSkipKey, j.Use)
	case len(j.KeyOps) > 0 && !slices.Contains(j.KeyOps, "verify"):
		return nil, j.Kid, fmt.Errorf("%w: key_ops do not include verify", errSkipKey)
	case j.Alg != "" && !slices.Contains(asymmetricAlgorithms, j.Alg):
		return nil, j.Kid, fmt.Errorf("%w: algorithm %q", errSkipKey, j.Alg)
	}
	var key any
	switch j.Kty {
	case "RSA":
		k, err := parseRSAJWK(j.N, j.E)
		if err != nil {
			return nil, j.Kid, err
		}
		key = k
	case "EC":
		k, err := parseECJWK(j.Crv, j.X, j.Y)
		if err != nil {
			return nil, j.Kid, err
		}
		key = k
	case "OKP":
		k, err := parseOKPJWK(j.Crv, j.X)
		if err != nil {
			return nil, j.Kid, err
		}
		key = k
	case "":
		return nil, j.Kid, errors.New(`JWK without "kty"`)
	default:
		return nil, j.Kid, fmt.Errorf("%w: key type %q", errSkipKey, j.Kty)
	}
	if err := checkPublicKey(key); err != nil {
		return nil, j.Kid, err
	}
	k := &verifyKey{kid: j.Kid, alg: j.Alg, key: key, source: "jwks"}
	if len(k.algorithms()) == 0 {
		return nil, j.Kid, fmt.Errorf("algorithm %q does not fit key type %q", j.Alg, j.Kty)
	}
	return k, j.Kid, nil
}

func parseRSAJWK(n, e string) (*rsa.PublicKey, error) {
	nb, err := decodeJWKMember("n", n)
	if err != nil {
		return nil, err
	}
	eb, err := decodeJWKMember("e", e)
	if err != nil {
		return nil, err
	}
	if len(eb) > 4 {
		return nil, errors.New("RSA exponent is too large")
	}
	var exp uint64
	for _, b := range eb {
		exp = exp<<8 | uint64(b)
	}
	// The upper bound keeps int(exp) valid on 32-bit platforms.
	if exp < 3 || exp%2 == 0 || exp > math.MaxInt32 {
		return nil, fmt.Errorf("invalid RSA exponent %d", exp)
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(exp)}, nil
}

func parseECJWK(crv, x, y string) (*ecdsa.PublicKey, error) {
	var curve elliptic.Curve
	switch crv {
	case "P-256":
		curve = elliptic.P256()
	case "P-384":
		curve = elliptic.P384()
	case "P-521":
		curve = elliptic.P521()
	default:
		return nil, fmt.Errorf("%w: elliptic curve %q", errSkipKey, crv)
	}
	xb, err := decodeJWKMember("x", x)
	if err != nil {
		return nil, err
	}
	yb, err := decodeJWKMember("y", y)
	if err != nil {
		return nil, err
	}
	size := (curve.Params().BitSize + 7) / 8
	if len(xb) > size || len(yb) > size {
		return nil, fmt.Errorf("EC coordinates are longer than %d bytes", size)
	}
	// RFC 7518 wants full-size coordinates; shorter ones (leading zeros
	// stripped by some implementations) are padded.
	point := make([]byte, 1+2*size)
	point[0] = 4 // uncompressed
	copy(point[1+size-len(xb):1+size], xb)
	copy(point[1+2*size-len(yb):], yb)
	k, err := ecdsa.ParseUncompressedPublicKey(curve, point)
	if err != nil {
		return nil, fmt.Errorf("invalid EC key: %w", err)
	}
	return k, nil
}

func parseOKPJWK(crv, x string) (ed25519.PublicKey, error) {
	if crv != "Ed25519" {
		return nil, fmt.Errorf("%w: OKP curve %q", errSkipKey, crv)
	}
	xb, err := decodeJWKMember("x", x)
	if err != nil {
		return nil, err
	}
	if len(xb) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("Ed25519 key has %d bytes, want %d", len(xb), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(xb), nil
}

// decodeJWKMember decodes a base64url member; padding is tolerated.
func decodeJWKMember(name, s string) ([]byte, error) {
	if s == "" {
		return nil, fmt.Errorf("JWK member %q is missing", name)
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	if err != nil {
		return nil, fmt.Errorf("JWK member %q is not base64url encoded: %w", name, err)
	}
	return b, nil
}
