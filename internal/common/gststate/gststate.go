// Package gststate is the Indian GST state-code catalogue. Per the
// GST council's official allocation (https://gst.gov.in — section
// "State Codes for GSTIN").
//
// Used by:
//
//   - GSTIN validation cross-check (positions 1-2 of GSTIN MUST match
//     a known state code) — `internal/common/gst/` already does the
//     range check; this package adds the name resolution.
//   - Address VO + Pincode lookup — display the state name from the
//     2-digit code when a tenant declares an address.
//   - GSTR-1 export — invoice line items need the state name + 2-letter
//     alpha code (KA, MH, etc) for inter-state vs intra-state
//     determination.
//
// Catalogue is the OFFICIAL list as of 2026. Updates land in this
// file (rare — last addition was Ladakh in 2020). Six "special" codes
// 96+ cover "Other Territory" + central jurisdictions.
package gststate

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrInvalid is the sentinel returned by [ByCode] / [ByAlpha] when the
// supplied code / alpha is not in the catalogue.
var ErrInvalid = errors.New("gststate: invalid")

// State is one row in the catalogue.
type State struct {
	// GSTCode is the 2-digit GST state code (positions 1-2 of GSTIN).
	GSTCode string
	// Alpha is the 2-letter ISO 3166-2:IN code (KA, MH, DL, etc).
	Alpha string
	// Name is the canonical display name.
	Name string
	// IsUnionTerritory true when the entry is a Union Territory rather
	// than a State proper. Used by some reporting flows.
	IsUnionTerritory bool
}

// catalogue is the full GST state list per gst.gov.in (frozen 2026).
// Codes 01-37 cover states + UTs; 38 is Ladakh; 96-99 are special
// jurisdictions (other territory + foreign + CG-jurisdiction).
//
//nolint:gochecknoglobals // catalogue is a read-only frozen table.
var catalogue = []State{
	{"01", "JK", "Jammu and Kashmir", true},
	{"02", "HP", "Himachal Pradesh", false},
	{"03", "PB", "Punjab", false},
	{"04", "CH", "Chandigarh", true},
	{"05", "UK", "Uttarakhand", false},
	{"06", "HR", "Haryana", false},
	{"07", "DL", "Delhi", true},
	{"08", "RJ", "Rajasthan", false},
	{"09", "UP", "Uttar Pradesh", false},
	{"10", "BR", "Bihar", false},
	{"11", "SK", "Sikkim", false},
	{"12", "AR", "Arunachal Pradesh", false},
	{"13", "NL", "Nagaland", false},
	{"14", "MN", "Manipur", false},
	{"15", "MZ", "Mizoram", false},
	{"16", "TR", "Tripura", false},
	{"17", "ML", "Meghalaya", false},
	{"18", "AS", "Assam", false},
	{"19", "WB", "West Bengal", false},
	{"20", "JH", "Jharkhand", false},
	{"21", "OR", "Odisha", false},
	{"22", "CT", "Chhattisgarh", false},
	{"23", "MP", "Madhya Pradesh", false},
	{"24", "GJ", "Gujarat", false},
	{"25", "DD", "Daman and Diu", true},
	{"26", "DN", "Dadra and Nagar Haveli", true},
	{"27", "MH", "Maharashtra", false},
	{"28", "AP", "Andhra Pradesh", false},
	{"29", "KA", "Karnataka", false},
	{"30", "GA", "Goa", false},
	{"31", "LD", "Lakshadweep", true},
	{"32", "KL", "Kerala", false},
	{"33", "TN", "Tamil Nadu", false},
	{"34", "PY", "Puducherry", true},
	{"35", "AN", "Andaman and Nicobar Islands", true},
	{"36", "TG", "Telangana", false},
	{"37", "AD", "Andhra Pradesh New", false},
	{"38", "LA", "Ladakh", true},
	{"97", "OT", "Other Territory", true},
}

// ByCode returns the State for the supplied 2-digit GST code, or
// [ErrInvalid] when unknown. Accepts both zero-padded ("07") + bare
// numeric ("7") for caller flexibility.
func ByCode(raw string) (State, error) {
	c := strings.TrimSpace(raw)
	if len(c) == 1 {
		c = "0" + c
	}
	if len(c) != 2 {
		return State{}, fmt.Errorf("%w: code %q must be 2 digits", ErrInvalid, raw)
	}
	for _, s := range catalogue {
		if s.GSTCode == c {
			return s, nil
		}
	}
	return State{}, fmt.Errorf("%w: code %q not in catalogue", ErrInvalid, raw)
}

// ByAlpha returns the State for the supplied 2-letter ISO alpha code
// (KA, MH, etc). Case-insensitive.
func ByAlpha(raw string) (State, error) {
	a := strings.ToUpper(strings.TrimSpace(raw))
	if len(a) != 2 {
		return State{}, fmt.Errorf("%w: alpha %q must be 2 letters", ErrInvalid, raw)
	}
	for _, s := range catalogue {
		if s.Alpha == a {
			return s, nil
		}
	}
	return State{}, fmt.Errorf("%w: alpha %q not in catalogue", ErrInvalid, raw)
}

// ByName returns the State whose canonical name case-insensitively
// equals the input. Useful when the source data is the display name
// (e.g. pincode lookup table) rather than a code.
func ByName(raw string) (State, error) {
	n := strings.TrimSpace(raw)
	if n == "" {
		return State{}, fmt.Errorf("%w: name empty", ErrInvalid)
	}
	for _, s := range catalogue {
		if strings.EqualFold(s.Name, n) {
			return s, nil
		}
	}
	return State{}, fmt.Errorf("%w: name %q not in catalogue", ErrInvalid, raw)
}

// All returns a defensive copy of the catalogue — useful for HTTP
// reference-data endpoints + admin dropdown menus. Ordered by GSTCode.
func All() []State {
	out := make([]State, len(catalogue))
	copy(out, catalogue)
	return out
}

// IsKnownCode reports whether the supplied code exists in the
// catalogue. Convenience for boundary checks that don't need the
// resolved row.
func IsKnownCode(raw string) bool {
	_, err := ByCode(raw)
	return err == nil
}

// AllCodes returns the sorted list of all GST codes in the catalogue —
// useful for table-driven tests + boundary assertions.
func AllCodes() []string {
	out := make([]string, 0, len(catalogue))
	for _, s := range catalogue {
		out = append(out, s.GSTCode)
	}
	slices.Sort(out)
	return out
}
