package authn

import "github.com/google/uuid"

// CanonicalID parses raw exactly as the handlers do (uuid.Parse accepts
// upper case, the "urn:uuid:" prefix, "{…}" braces and the 32-hex form) and
// returns the canonical lowercase hyphenated form. It is the ONE place an id
// from a path or a claim is turned into a comparable value: every authz
// comparison and every handler-side path parser goes through it, so two
// spellings of one id can never be treated as two different ids.
func CanonicalID(raw string) (string, bool) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return "", false
	}
	return id.String(), true
}

// sameID reports whether a and b both parse as UUIDs and are equal as values.
func sameID(a, b string) bool {
	ca, okA := CanonicalID(a)
	cb, okB := CanonicalID(b)
	return okA && okB && ca == cb
}
