// Package pincode is the canonical India Post pincode lookup
// reference-data primitive per BRD §A-B.1.
//
// Layout per BRD §6 + ADR 0061 §"BuildingBlocks/Infrastructure/
// ReferenceData":
//
//	internal/common/refdata/pincode/
//	├── pincode.go            VO + Reader interface
//	├── pincodetest/          FakeReader for app-layer tests
//	└── (adapters live elsewhere — composed at the host)
//
// Data shape — India Post directory (data.gov.in seed) into
// `shared.pincodes`:
//
//	pincode      char(6)  PK
//	city         text     NOT NULL
//	district     text     NOT NULL
//	state        text     NOT NULL
//	state_code   char(2)  NOT NULL   (2-letter alpha — KA, MH, DL …)
//	state_gst_code text   NOT NULL   (2-digit GST state code — 27, 29, 07 …)
//
// Schema lives in the SHARED schema (no RLS) — reference data is the
// SAME for every tenant. Reads via the Reader interface; writes via
// SuperAdmin endpoint (future slice — initial migration seeds the
// canonical India Post dump).
package pincode

import (
	"context"
	"errors"
	"fmt"
	"regexp"
)

// ErrInvalid is the sentinel for VO invariant failures.
var ErrInvalid = errors.New("pincode: invalid")

// ErrNotFound is returned by [Reader.Lookup] when the supplied pincode
// has no row in `shared.pincodes`. Map to HTTP 404 at the port.
var ErrNotFound = errors.New("pincode: not found")

// pattern enforces India-Post canon: 6 digits, first digit 1-9.
var pattern = regexp.MustCompile(`^[1-9][0-9]{5}$`)

// Code is the validated 6-digit pincode string. Constructed via [New];
// stored as the raw 6-char string (Postgres `char(6)`).
type Code string

// New validates raw against the India Post pattern.
func New(raw string) (Code, error) {
	if !pattern.MatchString(raw) {
		return "", fmt.Errorf("%w: %q is not a 6-digit pincode (first digit 1-9)", ErrInvalid, raw)
	}
	return Code(raw), nil
}

// MustNew is the init-time / test variant. Panics on invalid input.
// NEVER use in request-path code per CLAUDE.md "MustNewX init-time
// only" canon.
func MustNew(raw string) Code {
	c, err := New(raw)
	if err != nil {
		panic(err)
	}
	return c
}

// String returns the underlying 6-char form.
func (c Code) String() string { return string(c) }

// IsZero reports whether c is unset.
func (c Code) IsZero() bool { return c == "" }

// Lookup is the resolved row — the values the front-end auto-populates
// after the user types a pincode. Immutable VO returned by reader.
type Lookup struct {
	Pincode      Code
	City         string
	District     string
	State        string
	StateCode    string // 2-letter alpha (KA, MH, DL)
	StateGSTCode string // 2-digit GST state code (27, 29, 07)
}

// Reader is the canonical "give me the lookup row for this pincode"
// primitive. Concrete impl lives in the adapter; FakeReader in
// pincodetest/ for unit tests.
type Reader interface {
	// Lookup returns the row or [ErrNotFound] when the pincode isn't
	// in the seed table.
	Lookup(ctx context.Context, code Code) (Lookup, error)
}
