// Package pan defines the [Number] value object — an Indian Permanent
// Account Number (PAN) per Income Tax Act 1961 §139A + Income Tax Dept
// canon.
//
// Format: 10 chars, [A-Z]{5}[0-9]{4}[A-Z].
//
//	Position | Length | Content
//	---------+--------+---------------------------------------------
//	1-3      | 3      | Sequence (alphabetic, IT Dept assigned)
//	4        | 1      | Entity type — P=Individual, F=Firm,
//	                    H=HUF, C=Company, A=AOP, T=Trust,
//	                    B=Body of Individuals, L=Local Authority,
//	                    J=Artificial Juridical Person, G=Govt
//	5        | 1      | First letter of surname/entity name
//	6-9      | 4      | Sequential digits
//	10       | 1      | Checksum letter
//
// Domain validation is structural only (length + character set + entity
// type set). Live verification against the IT Dept portal is an
// Application-layer concern. Per LeadKart .NET parent's PanNumber VO.
package pan

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrInvalid is returned by [New] for any validation failure.
var ErrInvalid = errors.New("pan: invalid")

// panPattern enforces the structural shape: 5 alpha + 4 digits + 1 alpha.
var panPattern = regexp.MustCompile(`^[A-Z]{5}[0-9]{4}[A-Z]$`)

// validEntityTypes is the closed set of position-4 entity-type codes.
// {Income Tax Dept "Procedure for allotment of PAN"; CBDT canon.}
const validEntityTypes = "PFHCATBLJG"

// Number is a validated PAN.
type Number struct {
	value string
}

// New validates raw + returns a [Number] on success.
func New(raw string) (Number, error) {
	if raw == "" {
		return Number{}, fmt.Errorf("%w: empty", ErrInvalid)
	}
	if len(raw) != 10 {
		return Number{}, fmt.Errorf("%w: length %d, want 10", ErrInvalid, len(raw))
	}
	if !panPattern.MatchString(raw) {
		return Number{}, fmt.Errorf("%w: %q does not match PAN format", ErrInvalid, raw)
	}
	if !strings.ContainsRune(validEntityTypes, rune(raw[3])) {
		return Number{}, fmt.Errorf(
			"%w: position-4 entity type %q not in %q",
			ErrInvalid, raw[3:4], validEntityTypes,
		)
	}
	return Number{value: raw}, nil
}

// String returns the canonical PAN.
func (n Number) String() string { return n.value }

// IsZero reports whether the number is the empty zero value.
func (n Number) IsZero() bool { return n.value == "" }

// Equal compares two PAN values.
func (n Number) Equal(other Number) bool { return n.value == other.value }

// EntityType returns the position-4 entity-type code:
//   - 'P' Individual
//   - 'F' Firm
//   - 'H' HUF (Hindu Undivided Family)
//   - 'C' Company
//   - 'A' Association of Persons (AOP)
//   - 'T' Trust
//   - 'B' Body of Individuals
//   - 'L' Local Authority
//   - 'J' Artificial Juridical Person
//   - 'G' Government
//
// Returns 0 on zero value.
func (n Number) EntityType() byte {
	if n.IsZero() {
		return 0
	}
	return n.value[3]
}
