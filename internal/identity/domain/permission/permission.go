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
