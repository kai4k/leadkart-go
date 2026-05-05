// Package slug defines a URL-safe, DNS-compatible identifier value object.
//
// LeadKart uses slugs for tenant identifiers (`acme-pharma.leadkart.io`) and
// other public-facing handles. The shape mirrors RFC 1035 DNS label rules
// with two extra restrictions:
//
//   - 3–63 chars (DNS label cap is 63; we set a floor of 3 to avoid
//     collisions with reserved short tokens like `me`, `it`).
//   - No double hyphens — keeps URLs readable. RFC 1035 allows them but
//     the cosmetic cost outweighs the marginal flexibility.
//
// Per `tdd.md` "VO factories cross-validate, not just per-field" — slug
// validates the entire string atomically, not piecemeal.
package slug

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/leadkart/leadkart-go/internal/common/errs"
)

// ErrInvalid is the sentinel for slug-validation failure.
var ErrInvalid = errs.New(errs.KindInvalidInput, "slug", "invalid slug")

const (
	minLen = 3
	maxLen = 63
)

// slugRE: lowercase letters/digits + hyphens, must start + end with
// alphanumeric. Double-hyphen rejection happens via explicit substring check
// because RE2 has no negative lookahead.
var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*[a-z0-9]$`) //nolint:gochecknoglobals // canonical singleton

// Slug is a validated URL-safe identifier.
type Slug struct {
	value string
}

// New parses + validates raw. Returns ErrInvalid (wrapped) on failure.
func New(raw string) (Slug, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Slug{}, fmt.Errorf("%w: empty", ErrInvalid)
	}
	if len(trimmed) < minLen {
		return Slug{}, fmt.Errorf("%w: too short (min %d, got %d)", ErrInvalid, minLen, len(trimmed))
	}
	if len(trimmed) > maxLen {
		return Slug{}, fmt.Errorf("%w: too long (max %d, got %d)", ErrInvalid, maxLen, len(trimmed))
	}
	if !slugRE.MatchString(trimmed) {
		return Slug{}, fmt.Errorf("%w: must match [a-z0-9]([a-z0-9-]*[a-z0-9])?", ErrInvalid)
	}
	if strings.Contains(trimmed, "--") {
		return Slug{}, fmt.Errorf("%w: double hyphen disallowed", ErrInvalid)
	}
	return Slug{value: trimmed}, nil
}

// String returns the canonical form.
func (s Slug) String() string { return s.value }

// IsZero reports whether the slug was constructed.
func (s Slug) IsZero() bool { return s.value == "" }

// Equal reports whether two slugs are equal.
func (s Slug) Equal(other Slug) bool { return s.value == other.value }
