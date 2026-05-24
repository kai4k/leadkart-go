// Person-level operator commands. Cross-tenant blast radius — every
// flow here cascades to ALL of the Person's Memberships (refresh-
// token revocation via integration event, audit-log writes, etc.).
// HTTP layer gates the surface on RequirePlatform per
// multi-tenancy.md "SuperUser god-mode".

package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// ErrPersonNotFound surfaces when the Person ID has no row. Platform
// path only — cross-tenant Person lookups bypass RLS by design (Person
// is non-RLS); a real not-found means no row exists at all.
var ErrPersonNotFound = errors.New("person: not found")

// ----- GlobalSuspendPerson -------------------------------------------------

// GlobalSuspendPersonCommand applies a rare global ban (compliance,
// fraud, cross-tenant abuse). Reason MUST be non-empty per
// data-retention.md audit canon. The aggregate emits
// GloballySuspendedEvent; downstream subscribers revoke every refresh-
// token family across the Person's tenants.
type GlobalSuspendPersonCommand struct {
	PersonID person.ID
	Reason   string
}

// GlobalSuspendPersonHandler runs the suspension.
type GlobalSuspendPersonHandler struct {
	persons person.Repository
	now     func() time.Time
}

// NewGlobalSuspendPersonHandler wires the handler. `now` is the explicit
// time source per the clock-injection refactor. Nil → time.Now.
func NewGlobalSuspendPersonHandler(p person.Repository, now func() time.Time) GlobalSuspendPersonHandler {
	if p == nil {
		panic("command: NewGlobalSuspendPersonHandler persons repository required")
	}
	if now == nil {
		now = time.Now
	}
	return GlobalSuspendPersonHandler{persons: p, now: now}
}

// Handle dispatches to [Person.GloballySuspend].
func (h GlobalSuspendPersonHandler) Handle(ctx context.Context, cmd GlobalSuspendPersonCommand) error {
	if cmd.PersonID.IsZero() {
		return errors.New("global_suspend_person: person id required")
	}
	now := h.now()
	err := h.persons.UpdateByID(ctx, cmd.PersonID, func(p *person.Person) (bool, error) {
		if err := p.GloballySuspend(cmd.Reason, now); err != nil {
			return false, err
		}
		return true, nil
	})
	if errors.Is(err, person.ErrNotFound) {
		return ErrPersonNotFound
	}
	if err != nil {
		return fmt.Errorf("global_suspend_person: %w", err)
	}
	return nil
}

// ----- LiftPersonGlobalSuspension ------------------------------------------

// LiftPersonGlobalSuspensionCommand restores a globally-suspended
// Person. Idempotent — already-active Persons return nil.
type LiftPersonGlobalSuspensionCommand struct {
	PersonID person.ID
}

// LiftPersonGlobalSuspensionHandler runs the lift flow.
type LiftPersonGlobalSuspensionHandler struct {
	persons person.Repository
	now     func() time.Time
}

// NewLiftPersonGlobalSuspensionHandler wires the handler. `now` is the
// explicit time source per the clock-injection refactor. Nil → time.Now.
func NewLiftPersonGlobalSuspensionHandler(p person.Repository, now func() time.Time) LiftPersonGlobalSuspensionHandler {
	if p == nil {
		panic("command: NewLiftPersonGlobalSuspensionHandler persons repository required")
	}
	if now == nil {
		now = time.Now
	}
	return LiftPersonGlobalSuspensionHandler{persons: p, now: now}
}

// Handle dispatches to [Person.LiftGlobalSuspension].
func (h LiftPersonGlobalSuspensionHandler) Handle(ctx context.Context, cmd LiftPersonGlobalSuspensionCommand) error {
	if cmd.PersonID.IsZero() {
		return errors.New("lift_person_global_suspension: person id required")
	}
	now := h.now()
	err := h.persons.UpdateByID(ctx, cmd.PersonID, func(p *person.Person) (bool, error) {
		if err := p.LiftGlobalSuspension(now); err != nil {
			return false, err
		}
		return true, nil
	})
	if errors.Is(err, person.ErrNotFound) {
		return ErrPersonNotFound
	}
	if err != nil {
		return fmt.Errorf("lift_person_global_suspension: %w", err)
	}
	return nil
}

// ----- AnonymisePerson (direct) --------------------------------------------

// AnonymisePersonCommand triggers DPDP §12 / GDPR Art. 17 right-to-
// erasure directly by PersonID. Distinct from
// [AnonymiseUserCommand] which takes a MembershipID and resolves the
// Person — this entry point is the operator-facing path under
// /api/v1/platform/persons/{personId}/anonymise.
type AnonymisePersonCommand struct {
	PersonID person.ID
}

// AnonymisePersonHandler runs the cascade.
type AnonymisePersonHandler struct {
	persons person.Repository
	now     func() time.Time
}

// NewAnonymisePersonHandler wires the handler. `now` is the explicit
// time source per the clock-injection refactor. Nil → time.Now.
func NewAnonymisePersonHandler(p person.Repository, now func() time.Time) AnonymisePersonHandler {
	if p == nil {
		panic("command: NewAnonymisePersonHandler persons repository required")
	}
	if now == nil {
		now = time.Now
	}
	return AnonymisePersonHandler{persons: p, now: now}
}

// Handle dispatches to [Person.Anonymise]. Idempotent — already-
// anonymised Persons return nil with no event.
func (h AnonymisePersonHandler) Handle(ctx context.Context, cmd AnonymisePersonCommand) error {
	if cmd.PersonID.IsZero() {
		return errors.New("anonymise_person: person id required")
	}
	now := h.now()
	err := h.persons.UpdateByID(ctx, cmd.PersonID, func(p *person.Person) (bool, error) {
		if err := p.Anonymise(now); err != nil {
			return false, err
		}
		return true, nil
	})
	if errors.Is(err, person.ErrNotFound) {
		return ErrPersonNotFound
	}
	if err != nil {
		return fmt.Errorf("anonymise_person: %w", err)
	}
	return nil
}

// ----- UpdatePersonProfile -------------------------------------------------

// UpdatePersonProfileCommand updates the Person's global profile
// (FirstName, LastName). Per-Tenant fields (designation/department/
// status_message) live on Membership and update via UpdateUserProfile.
type UpdatePersonProfileCommand struct {
	PersonID  person.ID
	FirstName string
	LastName  string
}

// UpdatePersonProfileHandler runs the profile update.
type UpdatePersonProfileHandler struct {
	persons person.Repository
	now     func() time.Time
}

// NewUpdatePersonProfileHandler wires the handler. `now` is the explicit
// time source per the clock-injection refactor. Nil → time.Now.
func NewUpdatePersonProfileHandler(p person.Repository, now func() time.Time) UpdatePersonProfileHandler {
	if p == nil {
		panic("command: NewUpdatePersonProfileHandler persons repository required")
	}
	if now == nil {
		now = time.Now
	}
	return UpdatePersonProfileHandler{persons: p, now: now}
}

// Handle dispatches to [Person.UpdateProfile].
func (h UpdatePersonProfileHandler) Handle(ctx context.Context, cmd UpdatePersonProfileCommand) error {
	if cmd.PersonID.IsZero() {
		return errors.New("update_person_profile: person id required")
	}
	now := h.now()
	err := h.persons.UpdateByID(ctx, cmd.PersonID, func(p *person.Person) (bool, error) {
		if err := p.UpdateProfile(cmd.FirstName, cmd.LastName, now); err != nil {
			return false, err
		}
		return true, nil
	})
	if errors.Is(err, person.ErrNotFound) {
		return ErrPersonNotFound
	}
	if err != nil {
		return fmt.Errorf("update_person_profile: %w", err)
	}
	return nil
}
