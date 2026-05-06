package tenant

import (
	"github.com/leadkart/leadkart-go/internal/common/phone"
	"github.com/leadkart/leadkart-go/internal/common/postaladdress"
)

// AdminContact bundles the tenant's primary administrative contact:
//   - AdminPhone (E.164)
//   - AdminAddress (Indian postal)
//
// AdminEmail lives on Tenant directly (it's part of the registration
// invariant — every tenant has an email). This VO captures the
// optional follow-up details collected post-onboarding.
//
// Both fields are optional individually — a tenant may onboard
// without a phone number or address, supply them later, or clear them.
//
// Per LeadKart .NET parent's Contact composite VO + the .NET-shipped
// MembershipContactUpdatedIntegrationEvent / TenantContactUpdated
// vocabulary in messaging.md.
type AdminContact struct {
	phone   phone.Number
	address postaladdress.Address
}

// NewAdminContact composes the optional contact fields. Pass zero
// values for fields the tenant hasn't declared.
//
// No cross-validation required — phone and address are independent
// concerns. (PostalAddress's own factory cross-validates city against
// the pincode lookup table when the caller uses [postaladdress.Create]
// instead of [postaladdress.New].)
func NewAdminContact(p phone.Number, a postaladdress.Address) AdminContact {
	return AdminContact{phone: p, address: a}
}

// Phone returns the admin phone; zero if not declared.
func (c AdminContact) Phone() phone.Number { return c.phone }

// Address returns the admin postal address; zero if not declared.
func (c AdminContact) Address() postaladdress.Address { return c.address }

// IsZero reports whether NO contact details are declared.
func (c AdminContact) IsZero() bool {
	return c.phone.IsZero() && c.address.IsZero()
}

// Equal compares two AdminContact by both fields.
func (c AdminContact) Equal(other AdminContact) bool {
	return c.phone.Equal(other.phone) && c.address.Equal(other.address)
}
