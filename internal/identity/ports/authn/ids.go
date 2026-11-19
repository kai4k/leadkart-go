package authn

import "github.com/leadkart/leadkart-go/internal/common/ids"

// CanonicalID returns the canonical lowercase hyphenated form of raw, or
// false when raw is not exactly one non-nil UUID (whitespace is rejected, not
// trimmed). It delegates to [ids.Canonical], the ONE canonical-id function:
// every authz comparison and every handler-side path parser goes through it, so
// two spellings of one id can never be treated as two different ids.
func CanonicalID(raw string) (string, bool) {
	id, err := ids.Canonical(raw)
	return id, err == nil
}

// sameID reports whether a and b both parse as UUIDs and are equal as values.
func sameID(a, b string) bool {
	ca, okA := CanonicalID(a)
	cb, okB := CanonicalID(b)
	return okA && okB && ca == cb
}
