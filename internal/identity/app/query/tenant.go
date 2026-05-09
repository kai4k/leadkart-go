package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ----- GetTenantQuery -------------------------------------------------------

// GetTenantQuery returns the full Tenant view for the supplied ID.
// HTTP layer enforces the same-tenant-or-platform gate before
// dispatching; the query handler trusts that the caller is authorised.
type GetTenantQuery struct {
	TenantID tenant.ID
}

// TenantView is the wire-shape of a Tenant for read endpoints. Mirrors
// the .NET LeadKart `TenantDto` — profile + statutory + contact +
// settings + display preferences + status + lifecycle timestamps.
//
// Empty/zero fields surface as JSON nulls or defaults; the wire shape
// stays stable regardless of which fields the tenant has populated.
type TenantView struct {
	ID          string
	Slug        string
	LegalName   string
	DisplayName string
	// AdminEmail removed in migration 20260507000008 — current admin
	// is derived via the CompanyOwner-role membership. Use the
	// ListUsers / GetMembership query path to retrieve the current
	// admin Person + their email.
	Status              string
	CreatedAt           time.Time
	ActivatedAt         time.Time
	SuspendedAt         time.Time
	DeletionScheduledAt time.Time
	DeletionReason      string

	// Statutory — Indian compliance IDs (each may be empty until
	// declared by the tenant).
	GSTNumber       string
	PANNumber       string
	DrugLicenceNumber string

	// AdminContact — phone + postal address (each may be empty).
	AdminPhone     string
	AdminAddress   AdminAddressView

	// Settings — operational policy.
	PasswordPolicy PasswordPolicyView

	// DisplayPreferences — UI presentation.
	Locale     string
	TimeZone   string
	DateFormat string
	Currency   string
}

// AdminAddressView is the postal-address slice of TenantView. Mirrors
// the [postaladdress.Address] VO accessors — Indian-style street +
// city + district + state + state-code + pincode.
type AdminAddressView struct {
	Street    string
	City      string
	District  string
	State     string
	StateCode string
	Pincode   string
}

// PasswordPolicyView is the password-policy slice of TenantView.
type PasswordPolicyView struct {
	MinLength         int
	RequireUppercase  bool
	RequireLowercase  bool
	RequireDigit      bool
	RequireSymbol     bool
	MaxFailedAttempts int
	LockoutMinutes    int
}

// GetTenantHandler runs the read.
type GetTenantHandler struct {
	tenants tenant.Repository
}

// NewGetTenantHandler wires the handler.
func NewGetTenantHandler(tenants tenant.Repository) GetTenantHandler {
	if tenants == nil {
		panic("query: NewGetTenantHandler tenants repository required")
	}
	return GetTenantHandler{tenants: tenants}
}

// Handle returns the TenantView or [tenant.ErrNotFound] when the ID
// has no corresponding row.
func (h GetTenantHandler) Handle(ctx context.Context, q GetTenantQuery) (TenantView, error) {
	if q.TenantID.IsZero() {
		return TenantView{}, errors.New("get_tenant: tenant id required")
	}
	t, err := h.tenants.GetByID(ctx, q.TenantID)
	if err != nil {
		return TenantView{}, fmt.Errorf("get_tenant: %w", err)
	}
	return projectTenant(t), nil
}

func projectTenant(t *tenant.Tenant) TenantView {
	statutory := t.Statutory()
	contact := t.AdminContact()
	settings := t.Settings()
	policy := settings.PasswordPolicy()
	prefs := t.DisplayPreferences()
	addr := contact.Address()

	return TenantView{
		ID:                  t.ID().String(),
		Slug:                t.Slug().String(),
		LegalName:           t.LegalName(),
		DisplayName:         t.DisplayName(),
		Status:              t.Status().String(),
		CreatedAt:           t.CreatedAt().UTC(),
		ActivatedAt:         t.ActivatedAt().UTC(),
		SuspendedAt:         t.SuspendedAt().UTC(),
		DeletionScheduledAt: t.DeletionScheduledAt().UTC(),
		DeletionReason:      t.DeletionReason(),

		GSTNumber:         statutory.GST().String(),
		PANNumber:         statutory.PAN().String(),
		DrugLicenceNumber: statutory.DrugLicence().String(),

		AdminPhone: contact.Phone().String(),
		AdminAddress: AdminAddressView{
			Street:    addr.Street(),
			City:      addr.City(),
			District:  addr.District(),
			State:     addr.State(),
			StateCode: addr.StateCode(),
			Pincode:   addr.Pincode(),
		},

		PasswordPolicy: PasswordPolicyView{
			MinLength:         policy.MinLength(),
			RequireUppercase:  policy.RequireUppercase(),
			RequireLowercase:  policy.RequireLowercase(),
			RequireDigit:      policy.RequireDigit(),
			RequireSymbol:     policy.RequireSymbol(),
			MaxFailedAttempts: policy.MaxFailedAttempts(),
			LockoutMinutes:    policy.LockoutMinutes(),
		},

		Locale:     prefs.Locale(),
		TimeZone:   prefs.TimeZone(),
		DateFormat: prefs.DateFormat(),
		Currency:   prefs.Currency(),
	}
}
