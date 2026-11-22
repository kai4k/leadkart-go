// Package postaladdress defines the [Address] value object — an
// Indian-style postal address tied to a 6-digit Pincode.
//
// Reference: India Post canon (postoffice.indiapost.gov.in) +
// LeadKart.NET BuildingBlocks.Domain.ValueObjects.Address.
//
// Validation:
//   - Pincode: 6 digits, first digit 1-9 (India Post canon — 0 is reserved).
//   - State: non-empty, ≤80 chars.
//   - City: non-empty, ≤80 chars.
//   - District: optional but ≤80 chars when present.
//   - StateCode: optional 2-char code (e.g. "KA", "MH", "DL"); ≤4 chars.
//   - Street: non-empty, ≤200 chars (address line 1 + 2 concatenated).
//
// A refusal is a [*PartError] (it matches [ErrInvalid]) naming the part and a
// stable wire code, so an HTTP layer answers per part without parsing a message.
//
// Cross-validation against the pincodes seed table (city belongs to
// pincode) is an Application-layer concern — domain-layer Address
// trusts the inputs are mutually consistent. Per `coding-standards.md`
// "VO factories cross-validate, not just per-field": when reference
// data is needed for cross-validation, the caller resolves it and
// passes a [LookupData] value to [Create]. Pure-format [New] is for
// hot paths (re-hydration, internal copy).
package postaladdress

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrInvalid is returned by [New] / [Create] for validation failures.
var ErrInvalid = errors.New("postaladdress: invalid")

// Parts of an address a refusal can name. They are the wire field names of the
// address object (snake_case), so a client marks the input that is wrong.
const (
	PartStreet    = "street"
	PartCity      = "city"
	PartDistrict  = "district"
	PartState     = "state"
	PartStateCode = "state_code"
	PartPincode   = "pincode"
)

// Reasons a part is refused, stable wire codes (the same words humantext uses for
// "blank" and "over its limit").
const (
	ReasonRequired       = "required"
	ReasonTooLong        = "too_long"
	ReasonInvalidPincode = "invalid_pincode"
	// ReasonNotServed is a city that the pincode's lookup does not list.
	ReasonNotServed = "invalid_city"
)

// PartError is the typed refusal of one part of an address. It matches
// [ErrInvalid] with [errors.Is]; Part and Reason never carry the value.
type PartError struct {
	Part   string // one of the Part* constants
	Reason string // one of the Reason* constants
	detail string
}

// Error is the log-side message.
func (e *PartError) Error() string { return ErrInvalid.Error() + ": " + e.detail }

// Unwrap makes errors.Is(err, ErrInvalid) true.
func (e *PartError) Unwrap() error { return ErrInvalid }

func refuse(part, reason, detail string) error {
	return &PartError{Part: part, Reason: reason, detail: detail}
}

const (
	maxStreet    = 200
	maxLine      = 80
	maxStateCode = 4
)

// pincodePattern: 6 digits, first 1-9 (India Post canon).
var pincodePattern = regexp.MustCompile(`^[1-9][0-9]{5}$`)

// Address is a validated Indian postal address.
type Address struct {
	street    string
	city      string
	district  string
	state     string
	stateCode string
	pincode   string
}

// New validates each field structurally + returns an [Address]. Does
// NOT cross-validate city/district/state against pincode — use
// [Create] when you have lookup data from a pincodes table.
func New(street, city, district, state, stateCode, pincode string) (Address, error) {
	street = strings.TrimSpace(street)
	city = strings.TrimSpace(city)
	district = strings.TrimSpace(district)
	state = strings.TrimSpace(state)
	stateCode = strings.TrimSpace(stateCode)
	pincode = strings.TrimSpace(pincode)

	if street == "" {
		return Address{}, refuse(PartStreet, ReasonRequired, "street required")
	}
	if len(street) > maxStreet {
		return Address{}, refuse(PartStreet, ReasonTooLong, fmt.Sprintf("street too long (max %d)", maxStreet))
	}
	if city == "" {
		return Address{}, refuse(PartCity, ReasonRequired, "city required")
	}
	if len(city) > maxLine {
		return Address{}, refuse(PartCity, ReasonTooLong, fmt.Sprintf("city too long (max %d)", maxLine))
	}
	if len(district) > maxLine {
		return Address{}, refuse(PartDistrict, ReasonTooLong, fmt.Sprintf("district too long (max %d)", maxLine))
	}
	if state == "" {
		return Address{}, refuse(PartState, ReasonRequired, "state required")
	}
	if len(state) > maxLine {
		return Address{}, refuse(PartState, ReasonTooLong, fmt.Sprintf("state too long (max %d)", maxLine))
	}
	if len(stateCode) > maxStateCode {
		return Address{}, refuse(PartStateCode, ReasonTooLong, fmt.Sprintf("stateCode too long (max %d)", maxStateCode))
	}
	if pincode == "" {
		return Address{}, refuse(PartPincode, ReasonRequired, "pincode required")
	}
	if !pincodePattern.MatchString(pincode) {
		return Address{}, refuse(PartPincode, ReasonInvalidPincode, fmt.Sprintf("pincode %q must be 6 digits with leading 1-9", pincode))
	}
	return Address{
		street:    street,
		city:      city,
		district:  district,
		state:     state,
		stateCode: stateCode,
		pincode:   pincode,
	}, nil
}

// LookupData carries the reference-data lookup result a caller passes
// to [Create] for cross-validation. The Application layer resolves
// this from the pincodes seed table (per `architecture.md` Path 2
// collapse) before invoking the domain factory.
type LookupData struct {
	Cities    []string // canonical city names served by the pincode
	District  string   // canonical district
	State     string   // canonical state
	StateCode string   // canonical 2-char code
}

// Create validates structurally + cross-validates city/district/state
// against [LookupData]. Use this when the caller has the pincode
// lookup result; falls back to [New]'s pure-format check when the
// lookup is unavailable (re-hydration, dev seeding).
//
// Per `coding-standards.md` "VO factories cross-validate, not just
// per-field": single-field validation alone passes
// (pincode 400001, city "Bangalore", state "Karnataka") because each
// individually formats correctly. Cross-validation is the bug-catcher.
func Create(street, city, pincode string, lookup LookupData) (Address, error) {
	if !pincodePattern.MatchString(strings.TrimSpace(pincode)) {
		return Address{}, refuse(PartPincode, ReasonInvalidPincode, fmt.Sprintf("pincode %q must be 6 digits with leading 1-9", pincode))
	}
	cityTrim := strings.TrimSpace(city)
	if cityTrim == "" {
		return Address{}, refuse(PartCity, ReasonRequired, "city required")
	}
	canonicalCity := ""
	for _, c := range lookup.Cities {
		if strings.EqualFold(c, cityTrim) {
			canonicalCity = c
			break
		}
	}
	if canonicalCity == "" {
		return Address{}, refuse(PartCity, ReasonNotServed,
			fmt.Sprintf("city %q not served by pincode %s (lookup: %v)", cityTrim, pincode, lookup.Cities))
	}
	return New(street, canonicalCity, lookup.District, lookup.State, lookup.StateCode, pincode)
}

// String returns a flat one-line representation suitable for logs.
// NOT for printing on labels — use the struct fields for layout.
func (a Address) String() string {
	if a.IsZero() {
		return ""
	}
	parts := []string{a.street, a.city}
	if a.district != "" {
		parts = append(parts, a.district)
	}
	parts = append(parts, fmt.Sprintf("%s %s", a.state, a.pincode))
	return strings.Join(parts, ", ")
}

// IsZero reports whether the address is the empty zero value.
func (a Address) IsZero() bool {
	return a.pincode == "" && a.city == "" && a.state == "" && a.street == ""
}

// Street returns the optional street/line-1 component.
func (a Address) Street() string { return a.street }

// City returns the city/town component.
func (a Address) City() string { return a.city }

// District returns the district / administrative subdivision.
func (a Address) District() string { return a.district }

// State returns the full state name (e.g. "Maharashtra").
func (a Address) State() string { return a.state }

// StateCode returns the 2-3 letter state code (e.g. "MH").
func (a Address) StateCode() string { return a.stateCode }

// Pincode returns the 6-digit Indian pincode.
func (a Address) Pincode() string { return a.pincode }

// Equal compares two addresses by all fields.
func (a Address) Equal(other Address) bool {
	return a.street == other.street &&
		a.city == other.city &&
		a.district == other.district &&
		a.state == other.state &&
		a.stateCode == other.stateCode &&
		a.pincode == other.pincode
}
