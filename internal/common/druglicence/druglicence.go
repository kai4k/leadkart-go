// Package druglicence defines the [Number] value object — an Indian
// pharmaceutical Drug Licence number issued by State FDA (Food & Drug
// Administration) under the Drugs & Cosmetics Act 1940.
//
// Format: state-variant. Common patterns:
//   - Karnataka:  "KA-W-22B-12345" / "KA-R-22B-12345"
//   - Maharashtra: "20B/12345" / "MH-21B-12345"
//   - Delhi:      "DL-22B-12345-2025"
//
// There is no national checksum or single regex. Validation is loose:
//   - 8-30 chars
//   - Uppercase A-Z, digits 0-9, hyphens, slashes, spaces only
//   - At least one alphabetic + one numeric character
//
// Live verification against the State FDA portal is an Application-
// layer concern + manual ops process during onboarding (operator
// confirms the licence on paper before activating the tenant).
//
// Per LeadKart .NET parent's DrugLicenceNumber VO + audit-checklist.md
// "Domain-layer validation: structural, not authoritative".
package druglicence

import (
	"errors"
	"fmt"
	"regexp"
)

// ErrInvalid is returned by [New] for any validation failure.
var ErrInvalid = errors.New("druglicence: invalid")

const (
	minLen = 8
	maxLen = 30
)

// allowedChars is the closed character set: A-Z, 0-9, hyphen, slash, space.
var allowedChars = regexp.MustCompile(`^[A-Z0-9 /-]+$`)

// Number is a validated Indian Drug Licence number.
type Number struct {
	value string
}

// New validates raw + returns a [Number] on success.
func New(raw string) (Number, error) {
	if raw == "" {
		return Number{}, fmt.Errorf("%w: empty", ErrInvalid)
	}
	if len(raw) < minLen {
		return Number{}, fmt.Errorf("%w: length %d < min %d", ErrInvalid, len(raw), minLen)
	}
	if len(raw) > maxLen {
		return Number{}, fmt.Errorf("%w: length %d > max %d", ErrInvalid, len(raw), maxLen)
	}
	if !allowedChars.MatchString(raw) {
		return Number{}, fmt.Errorf("%w: %q contains characters outside [A-Z0-9 /-]", ErrInvalid, raw)
	}
	hasAlpha := false
	hasDigit := false
	for _, c := range raw {
		switch {
		case c >= 'A' && c <= 'Z':
			hasAlpha = true
		case c >= '0' && c <= '9':
			hasDigit = true
		}
	}
	if !hasAlpha {
		return Number{}, fmt.Errorf("%w: must contain at least one letter", ErrInvalid)
	}
	if !hasDigit {
		return Number{}, fmt.Errorf("%w: must contain at least one digit", ErrInvalid)
	}
	return Number{value: raw}, nil
}

// String returns the licence number.
func (n Number) String() string { return n.value }

// IsZero reports whether the number is the empty zero value.
func (n Number) IsZero() bool { return n.value == "" }

// Equal compares two licence values.
func (n Number) Equal(other Number) bool { return n.value == other.value }
