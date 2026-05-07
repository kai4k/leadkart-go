// Package gst defines the [Number] value object — an Indian Goods &
// Services Tax (GST) Identification Number (GSTIN).
//
// Format reference: GST Act 2017 + GSTN portal canon.
//
//	Position | Length | Content
//	---------+--------+---------------------------------------------
//	1-2      | 2      | State code (01-37, 97 for Other Territory)
//	3-12     | 10     | PAN of the registered person/entity
//	13       | 1      | Entity number (1-9, A-Z) — registrations
//	                    under the same PAN within a state
//	14       | 1      | Always 'Z' (reserved by GSTN)
//	15       | 1      | Checksum (0-9 or A-Z)
//
// Total: 15 chars, [0-9A-Z] uppercase.
//
// Domain validation is structural only (length + character set + state
// code range + position-14 'Z'). Checksum validation is intentionally
// deferred — runtime calls to the GSTN portal verify the live status
// of a registered GSTIN; checksum is necessary but insufficient (a
// checksum-valid GSTIN may be cancelled). Per LeadKart .NET parent's
// GstNumber VO + audit-checklist.md "Validation at the boundary".
package gst

import (
	"errors"
	"fmt"
	"regexp"
)

// ErrInvalid is returned by [New] for any validation failure.
var ErrInvalid = errors.New("gst: invalid")

// gstPattern enforces the structural shape: 2 state digits, 10 PAN
// chars, 1 entity, 'Z', 1 checksum. {GSTN portal docs.}
var gstPattern = regexp.MustCompile(`^[0-9]{2}[A-Z]{5}[0-9]{4}[A-Z][0-9A-Z]Z[0-9A-Z]$`)

// minStateCode + maxStateCode bracket the valid GST state codes.
// {Indian state codes per Census of India + GSTN canon: 01-37 active,
// 97 = Other Territory. Compact enforcement: 1-37 OR 97.}
const (
	minStateCode = 1
	maxStateCode = 37
	otherTerritoryStateCode = 97
)

// Number is a validated GSTIN.
type Number struct {
	value string
}

// New validates raw + returns a [Number] on success.
func New(raw string) (Number, error) {
	if raw == "" {
		return Number{}, fmt.Errorf("%w: empty", ErrInvalid)
	}
	if len(raw) != 15 {
		return Number{}, fmt.Errorf("%w: length %d, want 15", ErrInvalid, len(raw))
	}
	if !gstPattern.MatchString(raw) {
		return Number{}, fmt.Errorf("%w: %q does not match GSTIN format", ErrInvalid, raw)
	}
	state := int(raw[0]-'0')*10 + int(raw[1]-'0')
	if (state < minStateCode || state > maxStateCode) && state != otherTerritoryStateCode {
		return Number{}, fmt.Errorf("%w: state code %02d out of range", ErrInvalid, state)
	}
	return Number{value: raw}, nil
}

// String returns the canonical GSTIN.
func (n Number) String() string { return n.value }

// IsZero reports whether the number is the empty zero value.
func (n Number) IsZero() bool { return n.value == "" }

// Equal compares two GSTIN values.
func (n Number) Equal(other Number) bool { return n.value == other.value }

// StateCode returns the 2-digit state code embedded in the GSTIN.
// Returns 0 on the zero value (caller checks IsZero first).
func (n Number) StateCode() int {
	if n.IsZero() {
		return 0
	}
	return int(n.value[0]-'0')*10 + int(n.value[1]-'0')
}

// PAN returns the embedded PAN (positions 3-12). Useful for
// cross-validating that GST + PAN supplied separately by the user
// agree on the same legal entity. Returns "" on zero value.
func (n Number) PAN() string {
	if n.IsZero() {
		return ""
	}
	return n.value[2:12]
}
