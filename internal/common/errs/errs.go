// Package errs provides typed error construction + classification.
//
// Two layers per Go-canon error handling (Russ Cox 2019, Cheney "Don't just
// check errors, handle them gracefully"):
//
//   - **Kind**: a coarse classification used for HTTP status mapping +
//     retry decisions + audit. Maps 1:1 to gRPC codes / HTTP statuses.
//   - **Sentinel/typed errors**: per-domain exact errors (e.g.
//     `identity.ErrTenantNotFound`) declared as package vars in their
//     own bounded contexts. Sentinels are PREFERRED over typed structs
//     unless callers need extra fields.
//
// Wrapping uses `fmt.Errorf("...: %w", err)` per Go 1.13 convention.
// Classification via `errs.KindOf(err)` walks the wrap chain.
package errs

import (
	"errors"
	"fmt"
)

// Kind classifies an error for HTTP status mapping + retry decisions.
//
// Maps to standard HTTP/gRPC code semantics. Use `KindUnknown` only for
// errors that escape from third-party libraries; never construct one
// explicitly via `New`.
type Kind int

const (
	// KindUnknown is the zero value; reserved for foreign errors with
	// no LeadKart-known classification.
	KindUnknown Kind = iota
	// KindNotFound — entity missing (HTTP 404).
	KindNotFound
	// KindAlreadyExists — uniqueness violation (HTTP 409).
	KindAlreadyExists
	// KindInvalidInput — caller-supplied data fails validation (HTTP 400).
	KindInvalidInput
	// KindUnauthenticated — caller has no credentials (HTTP 401).
	KindUnauthenticated
	// KindPermissionDenied — caller authenticated but not authorised (HTTP 403).
	KindPermissionDenied
	// KindConflict — operation contradicts current state (HTTP 409 distinct from AlreadyExists for state machine transitions).
	KindConflict
	// KindUnavailable — transient downstream failure; retry safe (HTTP 503).
	KindUnavailable
	// KindInternal — bug / invariant violation; retry-unsafe (HTTP 500).
	KindInternal
)

// String returns the snake_case form for logging + audit.
func (k Kind) String() string {
	switch k {
	case KindNotFound:
		return "not_found"
	case KindAlreadyExists:
		return "already_exists"
	case KindInvalidInput:
		return "invalid_input"
	case KindUnauthenticated:
		return "unauthenticated"
	case KindPermissionDenied:
		return "permission_denied"
	case KindConflict:
		return "conflict"
	case KindUnavailable:
		return "unavailable"
	case KindInternal:
		return "internal"
	default:
		return "unknown"
	}
}

// Error is the typed application error carrying a Kind.
//
// Domain code typically declares package-level sentinels:
//
//	var ErrTenantNotFound = errs.New(errs.KindNotFound, "identity", "tenant not found")
//
// Then wraps with fmt.Errorf("...: %w", ErrTenantNotFound) at boundaries.
type Error struct {
	kind   Kind
	domain string // bounded-context name for log/audit (e.g. "identity", "crm")
	msg    string
}

// Error renders as "{domain}: {msg}" — concise for logs without leaking
// stack traces. The Kind is recovered via KindOf() / Is().
func (e *Error) Error() string {
	return e.domain + ": " + e.msg
}

// Kind returns the classification for HTTP/gRPC mapping.
func (e *Error) Kind() Kind { return e.kind }

// Domain returns the bounded-context tag (for structured logging).
func (e *Error) Domain() string { return e.domain }

// New constructs a typed error.
//
// Panics on KindUnknown — that's reserved for foreign errors entering the
// system from third-party libraries; never constructed deliberately.
func New(kind Kind, domain, msg string) *Error {
	if kind == KindUnknown {
		panic(fmt.Sprintf("errs: KindUnknown not allowed for explicit construction (domain=%q msg=%q)", domain, msg))
	}
	return &Error{kind: kind, domain: domain, msg: msg}
}

// KindOf walks the wrap chain via errors.As and returns the first
// `*errs.Error`'s Kind. Returns KindUnknown for foreign errors or nil.
//
// HTTP middleware uses this to map errors to status codes:
//
//	switch errs.KindOf(err) {
//	case errs.KindNotFound:        return 404
//	case errs.KindPermissionDenied: return 403
//	default:                       return 500
//	}
func KindOf(err error) Kind {
	if err == nil {
		return KindUnknown
	}
	var e *Error
	if errors.As(err, &e) {
		return e.kind
	}
	return KindUnknown
}

// Is checks whether err's Kind (walking the wrap chain) matches the supplied
// Kind. Differs from errors.Is — that checks identity; this checks Kind.
//
// Rule of thumb: use errs.Is when retrying or classifying; use errors.Is
// when comparing against a specific sentinel.
func Is(err error, kind Kind) bool {
	if err == nil {
		return false
	}
	return KindOf(err) == kind
}
