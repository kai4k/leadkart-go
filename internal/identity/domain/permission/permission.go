// Package permission defines the Permission value object and the
// closed-set IdentityPermissions catalogue.
package permission

import "errors"

// ErrUnknown is the sentinel returned when an input string does not
// match any catalogue entry. HTTP layer maps to 400 with code
// `permission_unknown`.
var ErrUnknown = errors.New("permission: unknown name")

// ErrEmpty is returned for empty / whitespace-only input.
var ErrEmpty = errors.New("permission: name required")

// ErrFormat is returned for charset / length-bound failures.
var ErrFormat = errors.New("permission: invalid format")

// Permission is a value object — comparable by Name. Identity-equality
// holds for any two pointers obtained from the intern table.
type Permission struct {
	name string
}

// Name returns the canonical wire-string form. Nil-safe.
func (p *Permission) Name() string {
	if p == nil {
		return ""
	}
	return p.name
}

// String implements fmt.Stringer for log + error formatting.
func (p *Permission) String() string { return p.Name() }

// Equal reports whether two permissions are the same. Pointer equality
// for interned instances; name compare otherwise. nil == nil is true.
func (p *Permission) Equal(other *Permission) bool {
	if p == other {
		return true
	}
	if p == nil || other == nil {
		return false
	}
	return p.name == other.name
}
