package auth

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"quackmux/internal/config"
)

// Defaults for unset auth.jwt fields (the same as config.ApplyDefaults).
const (
	defaultUserClaim   = "sub"
	defaultGroupsClaim = "groups"
	defaultJWTLeeway   = 30 * time.Second
	defaultJWKSRefresh = 15 * time.Minute
)

// jwtOptions are internal settings that tests replace.
type jwtOptions struct {
	now        func() time.Time
	httpClient *http.Client
	// minOnDemand is the minimum spacing of JWKS refreshes triggered by
	// unknown key ids.
	minOnDemand time.Duration
	// retryMin is the first back-off after a failed periodic JWKS refresh.
	retryMin time.Duration
}

func defaultJWTOptions() jwtOptions {
	return jwtOptions{
		now:         time.Now,
		httpClient:  &http.Client{Timeout: jwksFetchTimeout},
		minOnDemand: jwksOnDemandInterval,
		retryMin:    jwksRetryMin,
	}
}

// jwtAuthenticator verifies JSON Web Tokens.
type jwtAuthenticator struct {
	parser *jwt.Parser
	// fileKeys are the HMAC secret and the PEM keys (immutable, no key ids).
	fileKeys *keySet
	// jwks is nil without auth.jwt.jwksURL.
	jwks        *jwksSource
	userClaim   string
	groupsClaim string
	// closeAtExpiry sets Identity.Expires (auth.jwt.closeSessionsAtExpiry).
	closeAtExpiry bool
}

func newJWT(cfg config.JWTAuthConfig, log *slog.Logger, o jwtOptions) (*jwtAuthenticator, error) {
	if o.now == nil {
		o.now = time.Now
	}
	leeway := durationOr(cfg.Leeway, defaultJWTLeeway)
	if leeway < 0 {
		return nil, fmt.Errorf("auth.jwt.leeway must not be negative, got %s", leeway)
	}
	refresh := durationOr(cfg.JWKSRefresh, defaultJWKSRefresh)
	if refresh < 0 {
		return nil, fmt.Errorf("auth.jwt.jwksRefresh must not be negative, got %s", refresh)
	}

	files := &keySet{}
	if cfg.HMACSecretFile != "" {
		secret, err := loadHMACSecret(cfg.HMACSecretFile)
		if err != nil {
			return nil, fmt.Errorf("auth.jwt.hmacSecretFile: %w", err)
		}
		if len(secret) < minHMACSecretBytes {
			log.Warn("the JWT HMAC secret is short; use at least 32 random bytes", "file", cfg.HMACSecretFile, "bytes", len(secret))
		}
		files.keys = append(files.keys, &verifyKey{key: secret, source: cfg.HMACSecretFile})
	}
	for i, path := range cfg.PublicKeyFiles {
		if strings.TrimSpace(path) == "" {
			return nil, fmt.Errorf("auth.jwt.publicKeyFiles[%d] is empty", i)
		}
		keys, err := loadPublicKeyFile(path, log, o.now())
		if err != nil {
			return nil, fmt.Errorf("auth.jwt.publicKeyFiles[%d]: %w", i, err)
		}
		files.keys = append(files.keys, keys...)
	}
	var jwks *jwksSource
	if strings.TrimSpace(cfg.JWKSURL) != "" {
		var err error
		if jwks, err = newJWKSSource(cfg.JWKSURL, o, log); err != nil {
			return nil, err
		}
	}
	if len(files.keys) == 0 && jwks == nil {
		return nil, errors.New("auth.jwt needs hmacSecretFile, publicKeyFiles or jwksURL")
	}
	algs, err := allowedAlgorithms(cfg.Algorithms, files, jwks != nil, log)
	if err != nil {
		return nil, err
	}

	opts := []jwt.ParserOption{
		jwt.WithValidMethods(algs),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(leeway),
		jwt.WithTimeFunc(o.now),
	}
	asymmetric := jwks != nil || slices.ContainsFunc(files.keys, func(k *verifyKey) bool {
		_, hmac := k.key.([]byte)
		return !hmac
	})
	issuer := strings.TrimSpace(cfg.Issuer)
	if issuer != "" {
		opts = append(opts, jwt.WithIssuer(issuer))
	} else if asymmetric {
		log.Warn("auth.jwt.issuer is not set: tokens from every issuer that uses the configured keys are accepted")
	}
	var audience []string
	for _, aud := range cfg.Audience {
		if aud = strings.TrimSpace(aud); aud != "" && !slices.Contains(audience, aud) {
			audience = append(audience, aud)
		}
	}
	if len(audience) > 0 {
		opts = append(opts, jwt.WithAudience(audience...))
	} else if asymmetric {
		log.Warn("auth.jwt.audience is not set: tokens issued for any audience (any application) are accepted")
	}

	a := &jwtAuthenticator{
		parser:      jwt.NewParser(opts...),
		fileKeys:    files,
		jwks:        jwks,
		userClaim:   cmp.Or(strings.TrimSpace(cfg.UserClaim), defaultUserClaim),
		groupsClaim: cmp.Or(strings.TrimSpace(cfg.GroupsClaim), defaultGroupsClaim),

		closeAtExpiry: cfg.CloseSessionsAtExpiry,
	}
	attrs := []any{"algorithms", algs, "fileKeys", len(files.keys), "issuer", issuer, "audience", audience,
		"userClaim", a.userClaim, "groupsClaim", a.groupsClaim, "leeway", leeway}
	if jwks != nil {
		attrs = append(attrs, "jwksURL", jwks.logURL, "jwksRefresh", refresh)
	}
	log.Info("JWT authentication configured", attrs...)
	if jwks != nil {
		jwks.start(refresh)
	}
	return a, nil
}

// allowedAlgorithms resolves auth.jwt.algorithms. Without configuration, all
// algorithms that the configured keys can verify are allowed.
func allowedAlgorithms(configured []string, files *keySet, haveJWKS bool, log *slog.Logger) ([]string, error) {
	possible := map[string]bool{}
	for _, k := range files.keys {
		for _, alg := range k.algorithms() {
			possible[alg] = true
		}
	}
	if haveJWKS {
		for _, alg := range asymmetricAlgorithms {
			possible[alg] = true
		}
	}
	if len(configured) == 0 {
		var algs []string
		for _, alg := range supportedAlgorithms {
			if possible[alg] {
				algs = append(algs, alg)
			}
		}
		return algs, nil
	}
	var algs []string
	usable := false
	for i, name := range configured {
		alg, ok := canonicalAlgorithm(name)
		switch {
		case !ok && strings.EqualFold(strings.TrimSpace(name), "none"):
			return nil, fmt.Errorf("auth.jwt.algorithms[%d]: %q (unsigned tokens) is never allowed", i, name)
		case !ok:
			return nil, fmt.Errorf("auth.jwt.algorithms[%d]: unsupported algorithm %q (supported: %s)", i, name, strings.Join(supportedAlgorithms, ", "))
		case slices.Contains(algs, alg):
			continue
		}
		if possible[alg] {
			usable = true
		} else {
			log.Warn("no configured JWT key can verify this algorithm", "algorithm", alg)
		}
		algs = append(algs, alg)
	}
	if !usable {
		return nil, fmt.Errorf("auth.jwt.algorithms %v: no configured key can verify any of them", algs)
	}
	return algs, nil
}

// canonicalAlgorithm maps a configured algorithm name (case-insensitive) to
// its JOSE spelling.
func canonicalAlgorithm(name string) (string, bool) {
	name = strings.TrimSpace(name)
	for _, alg := range supportedAlgorithms {
		if strings.EqualFold(alg, name) {
			return alg, true
		}
	}
	return "", false
}

// Authenticate implements Authenticator.
func (a *jwtAuthenticator) Authenticate(ctx context.Context, req Request) (Identity, error) {
	if req.Credential == "" {
		return Identity{}, unauthenticated("empty credential")
	}
	claims := jwt.MapClaims{}
	_, err := a.parser.ParseWithClaims(req.Credential, claims, func(t *jwt.Token) (any, error) {
		return a.keyFor(ctx, t)
	})
	if err != nil {
		return Identity{}, fmt.Errorf("%w: invalid JWT: %w", ErrUnauthenticated, err)
	}
	user, err := a.user(claims)
	if err != nil {
		return Identity{}, unauthenticated("%v", err)
	}
	groups, err := a.groups(claims)
	if err != nil {
		return Identity{}, unauthenticated("%v", err)
	}
	id := Identity{User: user, Groups: groups, Method: MethodJWT}
	if a.closeAtExpiry {
		// The parser required exp.
		if exp, err := claims.GetExpirationTime(); err == nil && exp != nil {
			id.Expires = exp.Time
		}
	}
	return id, nil
}

// Close implements Authenticator. It stops the JWKS refresh.
func (a *jwtAuthenticator) Close() error {
	if a.jwks != nil {
		a.jwks.close()
	}
	return nil
}

// keyFor selects the keys that may have signed t (a jwt.Keyfunc). The
// parser has already checked that t's algorithm is allowed.
func (a *jwtAuthenticator) keyFor(ctx context.Context, t *jwt.Token) (any, error) {
	if _, ok := t.Header["crit"]; ok {
		// RFC 7515 4.1.11: extensions we do not understand must be rejected.
		return nil, errors.New(`the "crit" header is not supported`)
	}
	alg := t.Method.Alg()
	var kid string
	if v, ok := t.Header["kid"]; ok {
		s, isString := v.(string)
		if !isString {
			return nil, errors.New(`the "kid" header must be a string`)
		}
		kid = s
	}

	if kid != "" {
		// Only JWKS keys have ids.
		if keys := a.jwks.current().withKID(kid, alg); len(keys) > 0 {
			return keyList(keys), nil
		}
		// An unknown id may mean that the keys were rotated.
		if a.jwks != nil && a.jwks.refresh(ctx, false) == nil {
			if keys := a.jwks.current().withKID(kid, alg); len(keys) > 0 {
				return keyList(keys), nil
			}
		}
		// Keys without id are a fallback for every token.
		keys := append(a.fileKeys.withoutKID(alg), a.jwks.current().withoutKID(alg)...)
		if len(keys) == 0 {
			return nil, fmt.Errorf("no %s key with id %q", alg, kid)
		}
		return keyList(keys), nil
	}

	keys := append(a.fileKeys.forAlg(alg), a.jwks.current().forAlg(alg)...)
	if len(keys) == 0 && a.jwks != nil && a.jwks.refresh(ctx, false) == nil {
		// No key for the algorithm yet: the key set may never have been
		// downloaded, or rotated to a new key type.
		keys = a.jwks.current().forAlg(alg)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no key for algorithm %s", alg)
	}
	return keyList(keys), nil
}

// keyList converts keys into a jwt.Keyfunc result.
func keyList(keys []*verifyKey) any {
	if len(keys) == 1 {
		return keys[0].key
	}
	set := jwt.VerificationKeySet{Keys: make([]jwt.VerificationKey, 0, len(keys))}
	for _, k := range keys {
		set.Keys = append(set.Keys, k.key)
	}
	return set
}

// user extracts the user name from the claims.
func (a *jwtAuthenticator) user(claims jwt.MapClaims) (string, error) {
	v, ok := lookupClaim(claims, a.userClaim)
	if !ok || v == nil {
		return "", fmt.Errorf("user claim %q is missing", a.userClaim)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("user claim %q is not a string", a.userClaim)
	}
	if err := checkName(s); err != nil {
		return "", fmt.Errorf("user claim %q %v", a.userClaim, err)
	}
	return s, nil
}

// groups extracts the groups: a string is one group, an array of strings a
// list of groups; a missing or null claim means no groups.
func (a *jwtAuthenticator) groups(claims jwt.MapClaims) ([]string, error) {
	v, ok := lookupClaim(claims, a.groupsClaim)
	if !ok || v == nil {
		return nil, nil
	}
	var groups []string
	switch g := v.(type) {
	case string:
		if g != "" {
			groups = []string{g}
		}
	case []any:
		for i, e := range g {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("groups claim %q: element %d is not a string", a.groupsClaim, i)
			}
			if s != "" {
				groups = append(groups, s)
			}
		}
	default:
		return nil, fmt.Errorf("groups claim %q must be a string or an array of strings", a.groupsClaim)
	}
	for _, g := range groups {
		if err := checkName(g); err != nil {
			return nil, fmt.Errorf("groups claim %q: group %q %v", a.groupsClaim, g, err)
		}
	}
	return dedupe(groups), nil
}

// lookupClaim finds a claim by name. A name with dots that is not a claim
// itself is resolved as a path through nested objects ("realm_access.roles").
func lookupClaim(claims jwt.MapClaims, name string) (any, bool) {
	if v, ok := claims[name]; ok {
		return v, true
	}
	if !strings.Contains(name, ".") {
		return nil, false
	}
	var cur any = map[string]any(claims)
	for _, part := range strings.Split(name, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = obj[part]; !ok {
			return nil, false
		}
	}
	return cur, true
}
