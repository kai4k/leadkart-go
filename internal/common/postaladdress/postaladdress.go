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
		return Address{}, fmt.Errorf("%w: street required", ErrInvalid)
	}
	if len(street) > maxStreet {
		return Address{}, fmt.Errorf("%w: street too long (max %d)", ErrInvalid, maxStreet)
	}
	if city == "" {
		return Address{}, fmt.Errorf("%w: city required", ErrInvalid)
	}
	if len(city) > maxLine {
		return Address{}, fmt.Errorf("%w: city too long (max %d)", ErrInvalid, maxLine)
	}
	if len(district) > maxLine {
		return Address{}, fmt.Errorf("%w: district too long (max %d)", ErrInvalid, maxLine)
	}
	if state == "" {
		return Address{}, fmt.Errorf("%w: state required", ErrInvalid)
	}
	if len(state) > maxLine {
		return Address{}, fmt.Errorf("%w: state too long (max %d)", ErrInvalid, maxLine)
	}
	if len(stateCode) > maxStateCode {
		return Address{}, fmt.Errorf("%w: stateCode too long (max %d)", ErrInvalid, maxStateCode)
	}
	if !pincodePattern.MatchString(pincode) {
		return Address{}, fmt.Errorf("%w: pincode %q must be 6 digits with leading 1-9", ErrInvalid, pincode)
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
		return Address{}, fmt.Errorf("%w: pincode %q must be 6 digits with leading 1-9", ErrInvalid, pincode)
	}
	cityTrim := strings.TrimSpace(city)
	if cityTrim == "" {
		return Address{}, fmt.Errorf("%w: city required", ErrInvalid)
	}
	canonicalCity := ""
	for _, c := range lookup.Cities {
		if strings.EqualFold(c, cityTrim) {
			canonicalCity = c
			break
		}
	}
	if canonicalCity == "" {
		return Address{}, fmt.Errorf(
			"%w: city %q not served by pincode %s (lookup: %v)",
			ErrInvalid, cityTrim, pincode, lookup.Cities,
		)
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
