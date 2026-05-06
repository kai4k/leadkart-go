package integrationevents

import (
	"fmt"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// FromDomainEvent translates ANY recognised domain event into its
// canonical integration event. Used by [drainXEvents] in repository
// adapters: domain events emitted by aggregates flow through this
// function before they hit the outbox table.
//
// Returns ErrUnknown for events the mapper hasn't been taught about
// — surfaces in CI as a clear "you minted a domain event but never
// wired the integration counterpart" failure.
//
//nolint:cyclop // Switch dispatcher — one case per recognised domain
// event. Cyclomatic complexity scales with catalogue size by
// definition; refactoring into a registry map costs more than it pays.
func FromDomainEvent(d any) (Event, error) {
	switch e := d.(type) {

	// ----- Tenant -----------------------------------------------------

	case tenant.RegisteredEvent:
		// AdminPersonID + AdminMembershipID are NOT carried on the
		// domain event because the Tenant aggregate doesn't own them
		// — they're created in a sibling aggregate during the
		// orchestrated RegisterTenant flow. The Application service
		// emits the integration event directly with the full tuple
		// after persisting all three aggregates. This mapper handles
		// the domain-event-only path (Tenant created in isolation,
		// e.g. test seeds) with zeroed admin fields.
		return TenantRegisteredV1{
			TenantID:      mustParseUUID(e.TenantID.String()),
			Slug:          e.Slug.String(),
			LegalName:     e.LegalName,
			DisplayName:   e.DisplayName,
			AdminEmail:    e.AdminEmail.String(),
			OccurredAtUTC: e.At.UTC(),
		}, nil

	case tenant.ActivatedEvent:
		return TenantActivatedV1{
			TenantID:      mustParseUUID(e.TenantID.String()),
			OccurredAtUTC: e.At.UTC(),
		}, nil

	case tenant.ProfileUpdatedEvent:
		return TenantProfileUpdatedV1{
			TenantID:       mustParseUUID(e.TenantID.String()),
			OldLegalName:   e.OldLegalName,
			OldDisplayName: e.OldDisplayName,
			NewLegalName:   e.NewLegalName,
			NewDisplayName: e.NewDisplayName,
			OccurredAtUTC:  e.At.UTC(),
		}, nil

	case tenant.StatutoryUpdatedEvent:
		return TenantStatutoryUpdatedV1{
			TenantID:       mustParseUUID(e.TenantID.String()),
			OldGST:         e.OldStatutory.GST().String(),
			OldPAN:         e.OldStatutory.PAN().String(),
			OldDrugLicence: e.OldStatutory.DrugLicence().String(),
			NewGST:         e.NewStatutory.GST().String(),
			NewPAN:         e.NewStatutory.PAN().String(),
			NewDrugLicence: e.NewStatutory.DrugLicence().String(),
			OccurredAtUTC:  e.At.UTC(),
		}, nil

	case tenant.SuspendedEvent:
		return TenantSuspendedV1{
			TenantID:      mustParseUUID(e.TenantID.String()),
			Reason:        e.Reason,
			OccurredAtUTC: e.At.UTC(),
		}, nil

	case tenant.MarkedForDeletionEvent:
		return TenantMarkedForDeletionV1{
			TenantID:       mustParseUUID(e.TenantID.String()),
			Reason:         e.Reason,
			ScheduledAtUTC: e.ScheduledAt.UTC(),
			OccurredAtUTC:  e.At.UTC(),
		}, nil

	case tenant.RestoredEvent:
		return TenantRestoredV1{
			TenantID:      mustParseUUID(e.TenantID.String()),
			OccurredAtUTC: e.At.UTC(),
		}, nil

	case tenant.DeletedEvent:
		return TenantDeletedV1{
			TenantID:      mustParseUUID(e.TenantID.String()),
			Reason:        e.Reason,
			OccurredAtUTC: e.At.UTC(),
		}, nil

	// ----- Person -----------------------------------------------------

	case person.CreatedEvent:
		return PersonCreatedV1{
			PersonID:      mustParseUUID(e.PersonID.String()),
			Email:         e.Email.String(),
			FirstName:     e.FirstName,
			LastName:      e.LastName,
			OccurredAtUTC: e.At.UTC(),
		}, nil

	case person.PasswordChangedEvent:
		return PersonPasswordChangedV1{
			PersonID:      mustParseUUID(e.PersonID.String()),
			OccurredAtUTC: e.At.UTC(),
		}, nil

	case person.ProfileUpdatedEvent:
		return PersonProfileUpdatedV1{
			PersonID:      mustParseUUID(e.PersonID.String()),
			OldFirstName:  e.OldFirstName,
			OldLastName:   e.OldLastName,
			NewFirstName:  e.NewFirstName,
			NewLastName:   e.NewLastName,
			OccurredAtUTC: e.At.UTC(),
		}, nil

	case person.GloballySuspendedEvent:
		return PersonGloballySuspendedV1{
			PersonID:      mustParseUUID(e.PersonID.String()),
			Reason:        e.Reason,
			OccurredAtUTC: e.At.UTC(),
		}, nil

	case person.GlobalSuspensionLiftedEvent:
		return PersonGlobalSuspensionLiftedV1{
			PersonID:      mustParseUUID(e.PersonID.String()),
			OccurredAtUTC: e.At.UTC(),
		}, nil

	case person.AnonymisedEvent:
		return PersonAnonymisedV1{
			PersonID:      mustParseUUID(e.PersonID.String()),
			OccurredAtUTC: e.At.UTC(),
		}, nil

	// ----- Membership -------------------------------------------------

	case membership.CreatedEvent:
		return MembershipCreatedV1{
			MembershipID:  mustParseUUID(e.MembershipID.String()),
			PersonID:      mustParseUUID(e.PersonID.String()),
			TenantIDClaim: mustParseUUID(e.TenantID.String()),
			OccurredAtUTC: e.At.UTC(),
		}, nil

	case membership.DeactivatedEvent:
		return MembershipDeactivatedV1{
			MembershipID:  mustParseUUID(e.MembershipID.String()),
			PersonID:      mustParseUUID(e.PersonID.String()),
			TenantIDClaim: mustParseUUID(e.TenantID.String()),
			Reason:        e.Reason,
			OccurredAtUTC: e.At.UTC(),
		}, nil

	case membership.ReactivatedEvent:
		return MembershipReactivatedV1{
			MembershipID:  mustParseUUID(e.MembershipID.String()),
			PersonID:      mustParseUUID(e.PersonID.String()),
			TenantIDClaim: mustParseUUID(e.TenantID.String()),
			OccurredAtUTC: e.At.UTC(),
		}, nil

	case membership.RoleAssignedEvent:
		return MembershipRoleAssignedV1{
			MembershipID:  mustParseUUID(e.MembershipID.String()),
			PersonID:      mustParseUUID(e.PersonID.String()),
			TenantIDClaim: mustParseUUID(e.TenantID.String()),
			RoleID:        mustParseUUID(e.RoleID.String()),
			OccurredAtUTC: e.At.UTC(),
		}, nil

	case membership.RoleRevokedEvent:
		return MembershipRoleRevokedV1{
			MembershipID:  mustParseUUID(e.MembershipID.String()),
			PersonID:      mustParseUUID(e.PersonID.String()),
			TenantIDClaim: mustParseUUID(e.TenantID.String()),
			RoleID:        mustParseUUID(e.RoleID.String()),
			OccurredAtUTC: e.At.UTC(),
		}, nil

	case membership.PermissionsUpdatedEvent:
		return MembershipPermissionsUpdatedV1{
			MembershipID:  mustParseUUID(e.MembershipID.String()),
			PersonID:      mustParseUUID(e.PersonID.String()),
			TenantIDClaim: mustParseUUID(e.TenantID.String()),
			OccurredAtUTC: e.At.UTC(),
		}, nil

	case membership.ProfileUpdatedEvent:
		return MembershipProfileUpdatedV1{
			MembershipID:  mustParseUUID(e.MembershipID.String()),
			PersonID:      mustParseUUID(e.PersonID.String()),
			TenantIDClaim: mustParseUUID(e.TenantID.String()),
			Designation:   e.Designation,
			Department:    e.Department,
			StatusMessage: e.StatusMessage,
			OccurredAtUTC: e.At.UTC(),
		}, nil

	case membership.ManagerAssignedEvent:
		var prev uuid.UUID
		if !e.PreviousManager.IsZero() {
			prev = mustParseUUID(e.PreviousManager.String())
		}
		return MembershipManagerAssignedV1{
			MembershipID:    mustParseUUID(e.MembershipID.String()),
			PersonID:        mustParseUUID(e.PersonID.String()),
			TenantIDClaim:   mustParseUUID(e.TenantID.String()),
			ManagerID:       mustParseUUID(e.ManagerID.String()),
			PreviousManager: prev,
			OccurredAtUTC:   e.At.UTC(),
		}, nil

	case membership.ManagerRemovedEvent:
		return MembershipManagerRemovedV1{
			MembershipID:    mustParseUUID(e.MembershipID.String()),
			PersonID:        mustParseUUID(e.PersonID.String()),
			TenantIDClaim:   mustParseUUID(e.TenantID.String()),
			PreviousManager: mustParseUUID(e.PreviousManager.String()),
			OccurredAtUTC:   e.At.UTC(),
		}, nil

	// ----- Refresh-token family --------------------------------------

	case refreshtoken.FamilyCreatedEvent:
		return RefreshTokenFamilyCreatedV1{
			FamilyID:      mustParseUUID(e.FamilyID.String()),
			PersonID:      mustParseUUID(e.PersonID.String()),
			TenantIDClaim: mustParseUUID(e.TenantID.String()),
			DeviceLabel:   e.DeviceLabel,
			OccurredAtUTC: e.At.UTC(),
		}, nil

	case refreshtoken.RotatedEvent:
		return RefreshTokenRotatedV1{
			FamilyID:           mustParseUUID(e.FamilyID.String()),
			PersonID:           mustParseUUID(e.PersonID.String()),
			TenantIDClaim:      mustParseUUID(e.TenantID.String()),
			ConsumedTokenID:    mustParseUUID(e.ConsumedTokenID.String()),
			NewTokenID:         mustParseUUID(e.NewTokenID.String()),
			NewTokenGeneration: e.NewTokenGeneration,
			OccurredAtUTC:      e.At.UTC(),
		}, nil

	case refreshtoken.RevokedEvent:
		return RefreshTokenFamilyRevokedV1{
			FamilyID:      mustParseUUID(e.FamilyID.String()),
			PersonID:      mustParseUUID(e.PersonID.String()),
			TenantIDClaim: mustParseUUID(e.TenantID.String()),
			Reason:        e.Reason,
			OccurredAtUTC: e.At.UTC(),
		}, nil

	// ----- Role -------------------------------------------------------

	case role.CreatedEvent:
		return RoleCreatedV1{
			RoleID:          mustParseUUID(e.RoleID.String()),
			TenantIDClaim:   mustParseUUID(e.TenantID.String()),
			Name:            e.Name,
			IsSystemDefault: e.IsSystemDefault,
			IsSuperAdmin:    e.IsSuperAdmin,
			HierarchyLevel:  e.HierarchyLevel,
			OccurredAtUTC:   e.At.UTC(),
		}, nil

	case role.RenamedEvent:
		return RoleRenamedV1{
			RoleID:        mustParseUUID(e.RoleID.String()),
			TenantIDClaim: mustParseUUID(e.TenantID.String()),
			OldName:       e.OldName,
			NewName:       e.NewName,
			OccurredAtUTC: e.At.UTC(),
		}, nil

	case role.PermissionGrantedEvent:
		return RolePermissionGrantedV1{
			RoleID:        mustParseUUID(e.RoleID.String()),
			TenantIDClaim: mustParseUUID(e.TenantID.String()),
			Permission:    e.Permission,
			OccurredAtUTC: e.At.UTC(),
		}, nil

	case role.PermissionRevokedEvent:
		return RolePermissionRevokedV1{
			RoleID:        mustParseUUID(e.RoleID.String()),
			TenantIDClaim: mustParseUUID(e.TenantID.String()),
			Permission:    e.Permission,
			OccurredAtUTC: e.At.UTC(),
		}, nil

	case role.DeletedEvent:
		return RoleDeletedV1{
			RoleID:        mustParseUUID(e.RoleID.String()),
			TenantIDClaim: mustParseUUID(e.TenantID.String()),
			DeletedBy:     e.DeletedBy,
			OccurredAtUTC: e.At.UTC(),
		}, nil
	}

	return nil, fmt.Errorf("integrationevents: %w: %T", ErrUnknownDomainEvent, d)
}

// ErrUnknownDomainEvent surfaces when [FromDomainEvent] is handed a
// type the mapper hasn't been taught. CI surfaces as "you minted a
// domain event but the integration counterpart isn't wired".
var ErrUnknownDomainEvent = unknownErr("unknown domain event type")

type unknownErr string

func (u unknownErr) Error() string { return string(u) }

// mustParseUUID panics on a malformed UUID string. Domain IDs are
// minted via [ids.NewV7] which produces canonical RFC 9562 UUIDs;
// a parse failure here means the aggregate constructed an ID via a
// non-canonical path (programmer error) — fail-fast is the right
// response per `coding-standards.md` "Result vs exceptions".
func mustParseUUID(s string) uuid.UUID {
	u, err := uuid.Parse(s)
	if err != nil {
		panic(fmt.Sprintf("integrationevents: malformed UUID %q: %v", s, err))
	}
	return u
}
