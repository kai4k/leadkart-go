// Package command holds Identity CQRS command handlers.
//
// Per TDL Wild Workouts canonical layout (verified Nov 2025): each
// handler is a concrete struct with a single Handle method. Handlers
// are aggregated as fields on the Application facade in
// `internal/identity/app/app.go`; HTTP ports call
// `app.Commands.X.Handle(...)` directly.
//
// Handlers stay THIN per `architecture.md` "Command handler scope" —
// validate → dispatch to service → translate result. Multi-aggregate
// orchestration lives in [internal/identity/app/service/].
package command

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/app/service"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// RegisterTenantCommand carries the validated input for the
// register-tenant use case — a brand-new pharma company onboarding to
// LeadKart. The handler delegates to [service.TenantOnboardingService]
// which writes Tenant + admin Person + admin Membership in ONE
// transaction (no saga; per TDL canon).
type RegisterTenantCommand struct {
	Slug           slug.Slug
	LegalName      string
	DisplayName    string
	AdminEmail     email.Address
	AdminPassword  string
	AdminFirstName string
	AdminLastName  string
}

// RegisterTenantResult mirrors [service.OnboardTenantResult] —
// surfaced through the handler boundary so HTTP ports don't import
// the service package directly.
type RegisterTenantResult struct {
	TenantID     tenant.ID
	PersonID     person.ID
	MembershipID membership.ID
}

// ErrEmailHasActiveMembership re-exports
// [service.ErrEmailHasActiveMembership] under the command package's
// error vocabulary. HTTP ports match on this sentinel for the 409
// Conflict mapping.
var ErrEmailHasActiveMembership = service.ErrEmailHasActiveMembership

// RegisterTenantHandler is a thin dispatcher over
// [service.TenantOnboardingService]. Per `architecture.md` "Command
// handler scope" — handler body ≤40 lines, ctor ≤6 deps; orchestration
// is the service's responsibility.
type RegisterTenantHandler struct {
	onboarding *service.TenantOnboardingService
}

// NewRegisterTenantHandler wires the handler.
func NewRegisterTenantHandler(onboarding *service.TenantOnboardingService) RegisterTenantHandler {
	return RegisterTenantHandler{onboarding: onboarding}
}

// Handle translates the command into the service-layer command + maps
// the result. Errors propagate unchanged.
func (h RegisterTenantHandler) Handle(
	ctx context.Context,
	cmd RegisterTenantCommand,
) (RegisterTenantResult, error) {
	out, err := h.onboarding.Onboard(ctx, service.OnboardTenantCommand{
		Slug:           cmd.Slug,
		LegalName:      cmd.LegalName,
		DisplayName:    cmd.DisplayName,
		AdminEmail:     cmd.AdminEmail,
		AdminPassword:  cmd.AdminPassword,
		AdminFirstName: cmd.AdminFirstName,
		AdminLastName:  cmd.AdminLastName,
	})
	if err != nil {
		return RegisterTenantResult{}, err
	}
	return RegisterTenantResult{
		TenantID:     out.TenantID,
		PersonID:     out.PersonID,
		MembershipID: out.MembershipID,
	}, nil
}
