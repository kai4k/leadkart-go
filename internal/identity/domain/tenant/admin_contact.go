package tenant

import (
	"github.com/leadkart/leadkart-go/internal/common/phone"
	"github.com/leadkart/leadkart-go/internal/common/postaladdress"
)

// AdminContact holds the tenant's optional admin phone (E.164) and postal
// address. Both fields are independently optional and may be supplied or
// cleared post-onboarding. Mirrors the .NET Contact composite VO.
type AdminContact struct {
	phone   phone.Number
	address postaladdress.Address
}

// NewAdminContact composes the optional contact fields. Pass zero values for
// undeclared fields. Phone and address are validated independently by their
// own VOs; no cross-validation needed here.
func NewAdminContact(p phone.Number, a postaladdress.Address) AdminContact {
	return AdminContact{phone: p, address: a}
}

// Phone returns the admin phone; zero if not declared.
func (c AdminContact) Phone() phone.Number { return c.phone }

// Address returns the admin postal address; zero if not declared.
func (c AdminContact) Address() postaladdress.Address { return c.address }

// IsZero reports whether no contact details are declared.
func (c AdminContact) IsZero() bool {
	return c.phone.IsZero() && c.address.IsZero()
}

// Equal compares two AdminContact values by all fields.
func (c AdminContact) Equal(other AdminContact) bool {
	return c.phone.Equal(other.phone) && c.address.Equal(other.address)
}
