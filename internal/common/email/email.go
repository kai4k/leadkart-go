// Package email defines a validated Email value object.
//
// Per the always-valid-domain-model rule (Khorikov DDD canon, Vernon ch.6),
// every Email instance has passed validation at construction time. Callers
// receiving an `Address` can assume it's well-formed; no re-validation
// downstream.
//
// Validation is RFC 5321 + RFC 5322 -lite — strict enough to catch typos,
// loose enough to accept the long tail of legitimate addresses (the full
// RFC 5322 grammar is famously almost-but-not-quite regex-able).
// stdlib `net/mail.ParseAddress` is the trusted gatekeeper; we add length
// caps and lowercase normalisation on top.
package email

import (
	"fmt"
	"net/mail"
	"strings"

	"github.com/leadkart/leadkart-go/internal/common/errs"
)

// ErrInvalid is the sentinel returned (wrapped via %w) by New on validation
// failure. Callers can match via errors.Is(err, email.ErrInvalid).
var ErrInvalid = errs.New(errs.KindInvalidInput, "email", "invalid email address")

// Address is a validated email — the zero value reports IsZero() and is
// only meaningful to indicate "not set yet" in optional contexts.
//
// Construction is via New(). External code cannot construct via literal
// because the field is unexported (sealed-type pattern, ADR 0002).
type Address struct {
	value string // canonical lowercase form
}

// New parses and normalises a raw input. Returns ErrInvalid (wrapped with
// the specific failure reason) on rejection.
//
// Normalisation applied:
//   - Trim surrounding whitespace.
//   - Lowercase the entire address (RFC 5321 says local part is case-
//     sensitive; in practice every mailer treats it as case-insensitive,
//     and storing one canonical form prevents duplicate accounts).
//   - Length cap at 254 chars (RFC 5321 §4.5.3.1.3).
//
// Validation:
//   - stdlib `net/mail.ParseAddress` for syntactic validity.
//   - Domain must contain a dot (rejects "alice@example" with no TLD).
//   - No spaces inside the local part or domain.
func New(raw string) (Address, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Address{}, fmt.Errorf("%w: empty", ErrInvalid)
	}
	if len(trimmed) > 254 {
		return Address{}, fmt.Errorf("%w: exceeds 254 chars", ErrInvalid)
	}
	if strings.ContainsAny(trimmed, " \t\n\r") {
		return Address{}, fmt.Errorf("%w: contains whitespace", ErrInvalid)
	}
	addr, err := mail.ParseAddress(trimmed)
	if err != nil {
		return Address{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	// mail.ParseAddress accepts "user@example" without TLD — reject.
	at := strings.LastIndex(addr.Address, "@")
	if at < 0 || at == len(addr.Address)-1 {
		return Address{}, fmt.Errorf("%w: missing domain", ErrInvalid)
	}
	domain := addr.Address[at+1:]
	if !strings.Contains(domain, ".") {
		return Address{}, fmt.Errorf("%w: domain missing TLD", ErrInvalid)
	}
	return Address{value: strings.ToLower(addr.Address)}, nil
}

// String returns the canonical lowercase form.
func (a Address) String() string { return a.value }

// IsZero reports whether the address was constructed (true = empty/never set).
func (a Address) IsZero() bool { return a.value == "" }

// Equal reports whether two addresses are equal (canonical-form comparison).
func (a Address) Equal(other Address) bool { return a.value == other.value }

// Compile-time guarantee that Address implements fmt.Stringer.
var _ fmt.Stringer = Address{}
