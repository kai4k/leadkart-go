// Package phone defines the [Number] value object — an E.164-formatted
// phone number used across Tenant.AdminContact, future Person profile
// fields, and CRM lead contacts.
//
// E.164 reference: ITU-T E.164 + RFC 3966. `+` followed by 8-15 digits;
// no spaces, no hyphens, no parentheses. Indian numbers are `+91` +
// 10 digits = 13 chars total; we allow the full E.164 range so the
// VO covers cross-border integrations later (B2B partners outside India).
//
// Domain-layer validation only — does NOT verify the number is in
// service or call SMS gateways. That's an Application-layer concern
// per `architecture.md` "Validation: DDD ctor (domain) +
// go-playground/validator (HTTP DTO)".
package phone

import (
	"errors"
	"fmt"
	"regexp"
)

// ErrInvalid is returned by [New] for any validation failure.
//
// Wrapped with [fmt.Errorf] so callers reach the specific reason via
// `err.Error()` while still pattern-matching on errors.Is.
var ErrInvalid = errors.New("phone: invalid")

// e164Pattern enforces ITU-T E.164: leading `+`, 8-15 digits, no
// other characters. {ITU-T E.164 (10/2010); RFC 3966 §5.1.}
var e164Pattern = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)

// Number is a validated E.164 phone number. Zero value is rejected by
// the boundary checks in any aggregate that consumes it.
type Number struct {
	value string
}

// New validates raw against the E.164 pattern + returns a wrapped
// [Number] on success. The raw must already be in canonical form —
// callers (HTTP DTO mappers) strip spaces / hyphens / parentheses
// before invoking.
func New(raw string) (Number, error) {
	if raw == "" {
		return Number{}, fmt.Errorf("%w: empty", ErrInvalid)
	}
	if !e164Pattern.MatchString(raw) {
		return Number{}, fmt.Errorf("%w: %q is not E.164", ErrInvalid, raw)
	}
	return Number{value: raw}, nil
}

// String returns the E.164 representation.
func (n Number) String() string { return n.value }

// IsZero reports whether the number is the empty zero value.
func (n Number) IsZero() bool { return n.value == "" }

// Equal compares two phone numbers by value.
func (n Number) Equal(other Number) bool { return n.value == other.value }
