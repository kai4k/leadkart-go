// Package hsncode is the canonical HSN (Harmonized System of
// Nomenclature) code VO used by Inventory products + Invoice tax lines
// for GSTR-1 export. Per BRD §6.4 + §6.5 + GST canon.
//
// HSN code structure (India GST):
//
//   - 4 digits — heading (mandatory for tenants with annual turnover
//     > ₹5 crore)
//   - 6 digits — subheading (international WCO standard)
//   - 8 digits — tariff item (mandatory for export + most pharma SKUs;
//     enables full duty / GST classification)
//
// LeadKart's tenants are primarily PCD pharma operators whose turnover
// typically exceeds the 4-digit threshold, so the 2-digit chapter alone is not
// accepted as a product code. Operators pick the granularity their accountant
// needs among 4, 6 and 8.
//
// THE rule, the only one in the codebase (the Product domain uses it): the
// value is trimmed of surrounding whitespace, then holds ASCII digits only,
// exactly 4, 6 or 8 of them. A leading zero is part of the code: chapters
// 01-09 are real (`0402` is milk and cream), so the first digit is NOT
// restricted.
//
// Note: GSTR-1 + GSTR-3B reporting requires the code AS DECLARED to the
// GST portal — we don't cross-validate against the HSN master table
// (47k+ entries; would couple LeadKart to a reference-data update
// cadence). Operators are trusted to enter the correct code per their
// CA's advice; we enforce structural validity only.
//
// WHY THIS LIVES IN internal/common (ADR 0051, reviewed for the commerce
// integrity round): today its one production consumer is the Inventory product.
// ADR 0051 moves module-specific POLICY out of the shared kernel (the breach
// check, impersonation sessions) and keeps value-object primitives in it: its
// own list names the email and slug value objects, each with one consumer. An
// HSN code is such a primitive: the format is fixed by Indian GST law, not by an
// Inventory decision, exactly like the GSTIN, PAN and drug-licence value objects
// that sit beside it. Its second consumer is already designed: the invoice tax
// lines Orders must print per HSN (BRD §6.4, GSTR-1), and Orders may not import
// Inventory's domain (architecture rule 1), so a copy inside Inventory would have
// to move back here, or the rule would be written twice and drift (the defect
// the previous round fixed). It stays stdlib-only, with no Inventory type.
package hsncode

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrInvalid is the sentinel for VO invariant failures.
var ErrInvalid = errors.New("hsncode: invalid")

// pattern: 4, 6 or 8 ASCII digits, any first digit.
var pattern = regexp.MustCompile(`^(?:[0-9]{4}|[0-9]{6}|[0-9]{8})$`)

// Code is the validated HSN code string.
type Code string

// New trims raw and validates it against the HSN rule above.
func New(raw string) (Code, error) {
	t := strings.TrimSpace(raw)
	if !pattern.MatchString(t) {
		return "", fmt.Errorf("%w: %q is not a valid HSN code (4, 6 or 8 digits)", ErrInvalid, t)
	}
	return Code(t), nil
}

// MustNew is the init-time / test variant — panics on invalid input.
// NEVER use in request-path code per CLAUDE.md "MustNewX init-time
// only" canon.
func MustNew(raw string) Code {
	c, err := New(raw)
	if err != nil {
		panic(err)
	}
	return c
}

// String returns the underlying digit string.
func (c Code) String() string { return string(c) }

// IsZero reports whether c is unset.
func (c Code) IsZero() bool { return c == "" }

// Chapter returns the first 2 digits as the chapter classifier. Every
// valid Code has at least 2 digits — caller is guaranteed a non-empty
// result for a non-zero Code.
func (c Code) Chapter() string {
	if len(c) < 2 {
		return ""
	}
	return string(c[:2])
}

// Heading returns the first 4 digits, or empty when Code is shorter
// than 4. Most pharma SKUs are 4+ digits, so this is the common
// granularity for tenant reporting.
func (c Code) Heading() string {
	if len(c) < 4 {
		return ""
	}
	return string(c[:4])
}

// Subheading returns the first 6 digits, or empty when shorter.
func (c Code) Subheading() string {
	if len(c) < 6 {
		return ""
	}
	return string(c[:6])
}

// Length returns the digit count — useful for GSTR-1 export which
// needs to know the operator's chosen granularity.
func (c Code) Length() int { return len(c) }

// IsPharma reports whether the code falls under Chapter 30
// (Pharmaceutical Products) of the Indian HSN tariff. This is the
// dominant chapter for LeadKart's PCD-pharma tenants; the UI can use
// the flag for a "verify chapter looks right" sanity prompt at
// product-creation time.
func (c Code) IsPharma() bool {
	return c.Chapter() == "30"
}
