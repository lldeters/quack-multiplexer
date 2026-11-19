package kubernetes

import (
	"math/rand/v2"
	"strings"

	"quackmux/internal/engine"
)

const (
	// maxNameLength is the length limit of a DNS-1123 label (and of a label
	// value); pod names are kept within it so that the pod hostname equals
	// the pod name.
	maxNameLength = 63
	// suffixLength is the number of random characters appended to names.
	suffixLength = 5
	// maxPrefixLength leaves room for the share level, owner and suffix.
	maxPrefixLength = 30
	// defaultNamePrefix is used when provider.kubernetes.namePrefix is empty
	// or contains nothing usable.
	defaultNamePrefix = "quack"

	suffixAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
)

// randomSuffix returns suffixLength random characters from [a-z0-9]. The
// suffix only has to make names unique, it is not a secret.
func randomSuffix() string {
	b := make([]byte, suffixLength)
	for i := range b {
		b[i] = suffixAlphabet[rand.IntN(len(suffixAlphabet))]
	}
	return string(b)
}

// dnsPart lowercases s and turns it into a fragment of a DNS-1123 label:
// characters outside [a-z0-9] become '-', runs of '-' are collapsed and
// leading/trailing '-' are removed. The result may be empty.
func dnsPart(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// namePrefix sanitizes the configured pod name prefix.
func namePrefix(configured string) string {
	p := dnsPart(configured)
	if len(p) > maxPrefixLength {
		p = strings.TrimRight(p[:maxPrefixLength], "-")
	}
	if p == "" {
		return defaultNamePrefix
	}
	return p
}

// buildName joins the non-empty parts with '-', shortens the result so that
// "-<suffix>" still fits into a DNS-1123 label, and appends the suffix. The
// suffix must consist of [a-z0-9] only.
func buildName(suffix string, parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p = dnsPart(p); p != "" {
			kept = append(kept, p)
		}
	}
	head := strings.Join(kept, "-")
	if limit := maxNameLength - 1 - len(suffix); len(head) > limit {
		head = strings.TrimRight(head[:limit], "-")
	}
	if head == "" {
		return suffix
	}
	return head + "-" + suffix
}

// nameParts returns the readable parts of an engine pod name:
// <level>-<owner> for regular engines and warm-<profile> for warm-pool
// engines. CONNECTION engines are owned by a session id, which says nothing
// to a human, so they use "conn" instead of the owner.
func nameParts(key engine.Key, warm bool) []string {
	if warm {
		return []string{"warm", key.Profile}
	}
	level := string(key.ShareLevel)
	if dnsPart(level) == "" {
		level = "engine"
	}
	owner := key.Owner
	if key.ShareLevel == engine.ShareConnection {
		owner = "conn"
	}
	return []string{level, owner}
}

// isLabelValueChar reports whether c may appear inside a label value.
func isLabelValueChar(c byte) bool {
	return isAlnum(c) || c == '-' || c == '_' || c == '.'
}

func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// labelValue turns s into a valid label value: invalid characters become
// '-', the value starts and ends with an alphanumeric character and has at
// most 63 characters. The result may be empty (which is a valid value).
func labelValue(s string) string {
	b := []byte(s)
	for i, c := range b {
		if !isLabelValueChar(c) {
			b[i] = '-'
		}
	}
	trim := func(b []byte) []byte {
		for len(b) > 0 && !isAlnum(b[0]) {
			b = b[1:]
		}
		for len(b) > 0 && !isAlnum(b[len(b)-1]) {
			b = b[:len(b)-1]
		}
		return b
	}
	b = trim(b)
	if len(b) > maxNameLength {
		b = trim(b[:maxNameLength])
	}
	return string(b)
}
