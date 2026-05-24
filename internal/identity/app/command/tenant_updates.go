package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/druglicence"
	"github.com/leadkart/leadkart-go/internal/common/gst"
	"github.com/leadkart/leadkart-go/internal/common/pan"
	"github.com/leadkart/leadkart-go/internal/common/phone"
	"github.com/leadkart/leadkart-go/internal/common/postaladdress"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// All five handlers share a thin shape: load aggregate via UpdateByID,
// invoke the corresponding domain method, persist + drain events.
// VOs are constructed at the boundary (HTTP DTO → command) by the
// handler caller; this layer trusts that valid VOs arrive and
// surfaces only the aggregate's lifecycle invariants.

// ----- UpdateTenantProfile --------------------------------------------------

// UpdateTenantProfileCommand carries new legal + display names.
type UpdateTenantProfileCommand struct {
	TenantID    tenant.ID
	LegalName   string
	DisplayName string
}

// UpdateTenantProfileHandler runs the profile update.
type UpdateTenantProfileHandler struct {
	tenants tenant.Repository
	now     func() time.Time
}

// NewUpdateTenantProfileHandler wires the handler. `now` is the
// explicit time source per the clock-injection refactor. Nil → time.Now.
func NewUpdateTenantProfileHandler(tenants tenant.Repository, now func() time.Time) UpdateTenantProfileHandler {
	if tenants == nil {
		panic("command: NewUpdateTenantProfileHandler tenants repository required")
	}
	if now == nil {
		now = time.Now
	}
	return UpdateTenantProfileHandler{tenants: tenants, now: now}
}

// Handle dispatches to [Tenant.UpdateProfile]. ErrNotFound surfaces
// when the ID has no row; aggregate-level invariant violations
// (empty / over-length names) wrap [tenant.ErrInvalid].
func (h UpdateTenantProfileHandler) Handle(ctx context.Context, cmd UpdateTenantProfileCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("update_tenant_profile: tenant id required")
	}
	now := h.now()
	return h.tenants.UpdateByID(ctx, cmd.TenantID, func(t *tenant.Tenant) (bool, error) {
		if err := t.UpdateProfile(cmd.LegalName, cmd.DisplayName, now); err != nil {
			return false, err
		}
		return true, nil
	})
}

// ----- UpdateTenantStatutory ------------------------------------------------

// UpdateTenantStatutoryCommand replaces the Indian statutory IDs.
//
// Empty strings are honoured — the aggregate accepts a zero
// [tenant.Statutory] to clear all declarations. The application
// builds the Statutory VO from the strings before calling the
// aggregate so the handler stays thin.
type UpdateTenantStatutoryCommand struct {
	TenantID          tenant.ID
	GSTNumber         string
	PANNumber         string
	DrugLicenceNumber string
}

// UpdateTenantStatutoryHandler runs the statutory update.
type UpdateTenantStatutoryHandler struct {
	tenants tenant.Repository
	now     func() time.Time
}

// NewUpdateTenantStatutoryHandler wires the handler. `now` is the
// explicit time source per the clock-injection refactor. Nil → time.Now.
func NewUpdateTenantStatutoryHandler(tenants tenant.Repository, now func() time.Time) UpdateTenantStatutoryHandler {
	if tenants == nil {
		panic("command: NewUpdateTenantStatutoryHandler tenants repository required")
	}
	if now == nil {
		now = time.Now
	}
	return UpdateTenantStatutoryHandler{tenants: tenants, now: now}
}

// Handle constructs the Statutory VO + dispatches to the aggregate.
func (h UpdateTenantStatutoryHandler) Handle(ctx context.Context, cmd UpdateTenantStatutoryCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("update_tenant_statutory: tenant id required")
	}

	var (
		gstVO gst.Number
		panVO pan.Number
		dlVO  druglicence.Number
		err   error
	)
	if cmd.GSTNumber != "" {
		gstVO, err = gst.New(cmd.GSTNumber)
		if err != nil {
			return fmt.Errorf("update_tenant_statutory: gst: %w", err)
		}
	}
	if cmd.PANNumber != "" {
		panVO, err = pan.New(cmd.PANNumber)
		if err != nil {
			return fmt.Errorf("update_tenant_statutory: pan: %w", err)
		}
	}
	if cmd.DrugLicenceNumber != "" {
		dlVO, err = druglicence.New(cmd.DrugLicenceNumber)
		if err != nil {
			return fmt.Errorf("update_tenant_statutory: drug_licence: %w", err)
		}
	}

	statutory, err := tenant.NewStatutory(gstVO, panVO, dlVO)
	if err != nil {
		return fmt.Errorf("update_tenant_statutory: compose: %w", err)
	}

	now := h.now()
	return h.tenants.UpdateByID(ctx, cmd.TenantID, func(t *tenant.Tenant) (bool, error) {
		if err := t.UpdateStatutory(statutory, now); err != nil {
			return false, err
		}
		return true, nil
	})
}

// ----- UpdateTenantAdminContact ---------------------------------------------

// UpdateTenantAdminContactCommand replaces admin phone + postal address.
type UpdateTenantAdminContactCommand struct {
	TenantID      tenant.ID
	Phone         string
	AddressStreet string
	AddressCity   string
	AddressDistrict string
	AddressState     string
	AddressStateCode string
	AddressPincode   string
}

// UpdateTenantAdminContactHandler runs the contact update.
type UpdateTenantAdminContactHandler struct {
	tenants tenant.Repository
	now     func() time.Time
}

// NewUpdateTenantAdminContactHandler wires the handler. `now` is the
// explicit time source per the clock-injection refactor. Nil → time.Now.
func NewUpdateTenantAdminContactHandler(tenants tenant.Repository, now func() time.Time) UpdateTenantAdminContactHandler {
	if tenants == nil {
		panic("command: NewUpdateTenantAdminContactHandler tenants repository required")
	}
	if now == nil {
		now = time.Now
	}
	return UpdateTenantAdminContactHandler{tenants: tenants, now: now}
}

// Handle builds phone + postaladdress VOs + dispatches.
func (h UpdateTenantAdminContactHandler) Handle(ctx context.Context, cmd UpdateTenantAdminContactCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("update_tenant_admin_contact: tenant id required")
	}

	var (
		phoneVO phone.Number
		addrVO  postaladdress.Address
		err     error
	)
	if cmd.Phone != "" {
		phoneVO, err = phone.New(cmd.Phone)
		if err != nil {
			return fmt.Errorf("update_tenant_admin_contact: phone: %w", err)
		}
	}
	if cmd.AddressPincode != "" || cmd.AddressCity != "" || cmd.AddressStreet != "" {
		addrVO, err = postaladdress.New(
			cmd.AddressStreet, cmd.AddressCity, cmd.AddressDistrict,
			cmd.AddressState, cmd.AddressStateCode, cmd.AddressPincode,
		)
		if err != nil {
			return fmt.Errorf("update_tenant_admin_contact: address: %w", err)
		}
	}

	contact := tenant.NewAdminContact(phoneVO, addrVO)

	now := h.now()
	return h.tenants.UpdateByID(ctx, cmd.TenantID, func(t *tenant.Tenant) (bool, error) {
		if err := t.UpdateAdminContact(contact, now); err != nil {
			return false, err
		}
		return true, nil
	})
}

// ----- UpdateTenantSettings -------------------------------------------------

// UpdateTenantSettingsCommand replaces the password policy block.
type UpdateTenantSettingsCommand struct {
	TenantID          tenant.ID
	MinLength         int
	RequireUppercase  bool
	RequireLowercase  bool
	RequireDigit      bool
	RequireSymbol     bool
	MaxFailedAttempts int
	LockoutMinutes    int
}

// UpdateTenantSettingsHandler runs the settings update.
type UpdateTenantSettingsHandler struct {
	tenants tenant.Repository
	now     func() time.Time
}

// NewUpdateTenantSettingsHandler wires the handler. `now` is the
// explicit time source per the clock-injection refactor. Nil → time.Now.
func NewUpdateTenantSettingsHandler(tenants tenant.Repository, now func() time.Time) UpdateTenantSettingsHandler {
	if tenants == nil {
		panic("command: NewUpdateTenantSettingsHandler tenants repository required")
	}
	if now == nil {
		now = time.Now
	}
	return UpdateTenantSettingsHandler{tenants: tenants, now: now}
}

// Handle constructs Settings + dispatches.
func (h UpdateTenantSettingsHandler) Handle(ctx context.Context, cmd UpdateTenantSettingsCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("update_tenant_settings: tenant id required")
	}
	policy, err := tenant.NewPasswordPolicy(
		cmd.MinLength,
		cmd.RequireUppercase, cmd.RequireLowercase, cmd.RequireDigit, cmd.RequireSymbol,
		cmd.MaxFailedAttempts, cmd.LockoutMinutes,
	)
	if err != nil {
		return fmt.Errorf("update_tenant_settings: policy: %w", err)
	}
	settings := tenant.NewSettings(policy)

	now := h.now()
	return h.tenants.UpdateByID(ctx, cmd.TenantID, func(t *tenant.Tenant) (bool, error) {
		if err := t.UpdateSettings(settings, now); err != nil {
			return false, err
		}
		return true, nil
	})
}

// ----- UpdateTenantDisplayPreferences ---------------------------------------

// UpdateTenantDisplayPreferencesCommand replaces locale/tz/format/currency.
type UpdateTenantDisplayPreferencesCommand struct {
	TenantID   tenant.ID
	Locale     string
	TimeZone   string
	DateFormat string
	Currency   string
}

// UpdateTenantDisplayPreferencesHandler runs the prefs update.
type UpdateTenantDisplayPreferencesHandler struct {
	tenants tenant.Repository
	now     func() time.Time
}

// NewUpdateTenantDisplayPreferencesHandler wires the handler. `now` is
// the explicit time source per the clock-injection refactor. Nil → time.Now.
func NewUpdateTenantDisplayPreferencesHandler(tenants tenant.Repository, now func() time.Time) UpdateTenantDisplayPreferencesHandler {
	if tenants == nil {
		panic("command: NewUpdateTenantDisplayPreferencesHandler tenants repository required")
	}
	if now == nil {
		now = time.Now
	}
	return UpdateTenantDisplayPreferencesHandler{tenants: tenants, now: now}
}

// Handle constructs DisplayPreferences + dispatches.
func (h UpdateTenantDisplayPreferencesHandler) Handle(ctx context.Context, cmd UpdateTenantDisplayPreferencesCommand) error {
	if cmd.TenantID.IsZero() {
		return errors.New("update_tenant_display_preferences: tenant id required")
	}
	prefs, err := tenant.NewDisplayPreferences(cmd.Locale, cmd.TimeZone, cmd.DateFormat, cmd.Currency)
	if err != nil {
		return fmt.Errorf("update_tenant_display_preferences: compose: %w", err)
	}
	now := h.now()
	return h.tenants.UpdateByID(ctx, cmd.TenantID, func(t *tenant.Tenant) (bool, error) {
		if err := t.UpdateDisplayPreferences(prefs, now); err != nil {
			return false, err
		}
		return true, nil
	})
}
