// Package auth authenticates Quack clients.
//
// The credential is the token a client sends in CONNECTION_REQUEST (the
// "auth_string"), with any "?option=value" suffix already removed (see
// ParseToken). New builds the Authenticator that auth.mode selects:
//
//   - none: development only. Every credential is accepted. The user is the
//     "user" option (1-64 characters of [A-Za-z0-9._@-]) or
//     auth.none.defaultUser ("anonymous"); every identity gets
//     auth.none.defaultGroups.
//   - static: users, groups and token hashes come from a YAML file that is
//     re-read when it changes (see "Users file").
//   - jwt: the credential is a signed JSON Web Token (see "JSON Web Tokens").
//
// Failed authentications return an error that wraps ErrUnauthenticated
// together with a reason for the logs. Reasons never contain the credential.
//
// # Users file
//
// auth.mode=static reads auth.static.file:
//
//	users:
//	  - name: alice
//	    groups: [analytics, admins]
//	    tokens:
//	      - sha256: "<hex sha256 of the token>"   # quackmux hash-token TOKEN
//	        description: "laptop"
//	        expires: "2027-01-01T00:00:00Z"        # optional, RFC 3339
//	      - plain: "dev-only-token"                # allowed, but logged as a warning
//	    disabled: false
//
// Rules:
//
//   - The file must contain a "users" list (an empty list rejects everybody).
//     Unknown fields and duplicate keys are errors, so that a typo such as
//     "disable: true" cannot silently leave a user enabled.
//   - Names are unique, non-empty, at most 256 bytes and contain no control
//     characters or surrounding white space. The same rules apply to groups.
//     The order of groups matters: the first one is the default group for
//     share_level=group.
//   - Every token sets exactly one of sha256 (64 hexadecimal digits, case
//     insensitive) or plain. A token may appear only once in the whole file.
//     Tokens must not contain '?', which starts the options suffix.
//   - A token is valid until its optional expires time. Expired tokens and
//     all tokens of disabled users are rejected.
//
// HashToken computes the sha256 form and GenerateToken creates new random
// tokens. ValidateUsersFile checks a file without starting an authenticator.
//
// With auth.static.reloadInterval > 0 (default 10s), the file's modification
// time and size are polled. A changed file is loaded and validated; if it is
// invalid, the previous users stay active and the error is logged. Replace
// the file atomically (write a temporary file, then rename it, as Kubernetes
// does for mounted Secrets and ConfigMaps) so that no partial file is read.
// Reload forces a re-read, for example on SIGHUP.
//
// # JSON Web Tokens
//
// auth.mode=jwt verifies signed tokens with the keys from
//
//   - auth.jwt.hmacSecretFile: a shared secret (the file's bytes without
//     surrounding white space) for HS256, HS384 and HS512;
//   - auth.jwt.publicKeyFiles: PEM files with RSA, ECDSA (P-256, P-384,
//     P-521) or Ed25519 public keys as "PUBLIC KEY" (PKIX), "RSA PUBLIC KEY"
//     (PKCS #1) or "CERTIFICATE" (X.509) blocks; a file may hold several;
//   - auth.jwt.jwksURL: a JSON Web Key Set with RSA, EC and OKP (Ed25519)
//     keys, fetched when the authenticator starts, every auth.jwt.jwksRefresh
//     (default 15m, 0 disables) and on demand when a token names an unknown
//     key id (at most once per 30s). Until the first successful fetch, tokens
//     that need the key set are rejected; a failed refresh keeps the previous
//     keys.
//
// Key selection: a token with a "kid" header is verified with the JWKS keys
// of that id; keys without an id (all file keys) serve as fallback. A token
// without "kid" is tried against every key that matches its algorithm. Keys
// are only ever used with the algorithms of their type (an RSA key never
// verifies an HMAC token), and RSA keys need at least 2048 bits.
//
// Permitted algorithms are auth.jwt.algorithms or, by default, all
// algorithms the configured key types support: HS* for the secret, RS*/PS*
// for RSA keys, ES256/ES384/ES512 for the matching curves, EdDSA for Ed25519
// keys, and all asymmetric algorithms for a JWKS. "none" is never permitted.
//
// Claims: exp is required; exp, nbf and iat are checked with
// auth.jwt.leeway (default 30s). If configured, iss must equal
// auth.jwt.issuer and aud must contain at least one of auth.jwt.audience.
// Tokens with a "crit" header are rejected. The user is the string claim
// auth.jwt.userClaim (default "sub"); the groups are the claim
// auth.jwt.groupsClaim (default "groups"): a string is one group, an array of
// strings is a list of groups, a missing claim means no groups. A claim name
// that contains dots and is not present verbatim is looked up as a path in
// nested objects (for example "realm_access.roles").
package auth
