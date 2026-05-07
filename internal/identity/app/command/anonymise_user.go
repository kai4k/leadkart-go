package command

import (
	"context"
	"errors"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// AnonymiseUserCommand triggers a DPDP §12 / GDPR Art. 17 right-to-
// erasure on the underlying Person. The MembershipID is the wire-
// vocabulary entry point ("anonymise this user"), but the operation
// is Person-level — it cascades to ALL of the Person's Memberships
// across tenants per `data-retention.md` "Anonymisation field map".
//
// Caller authorization: this is a CROSS-TENANT operation (one Person
// can be in many tenants), so the HTTP layer gates it on the
// `identity.users.anonymise` permission, which only Platform admins
// hold by default. A tenant Admin cannot anonymise a Person that
// holds Memberships outside their tenant.
//
// Idempotent: already-anonymised Persons return nil with no event.
type AnonymiseUserCommand struct {
	MembershipID membership.ID
}

// AnonymiseUserHandler runs the flow.
type AnonymiseUserHandler struct {
	memberships membership.Repository
	persons     person.Repository
}

// NewAnonymiseUserHandler wires the handler.
func NewAnonymiseUserHandler(m membership.Repository, p person.Repository) AnonymiseUserHandler {
	if m == nil {
		panic("command: NewAnonymiseUserHandler memberships repository required")
	}
	if p == nil {
		panic("command: NewAnonymiseUserHandler persons repository required")
	}
	return AnonymiseUserHandler{memberships: m, persons: p}
}

// Handle resolves Membership → PersonID → Person.Anonymise(). The
// Person aggregate emits AnonymisedEvent which downstream subscribers
// consume to revoke refresh-token families + scrub free-text fields
// in CRM/Tasks/etc. (post-launch wiring per architecture.md).
func (h AnonymiseUserHandler) Handle(ctx context.Context, cmd AnonymiseUserCommand) error {
	if cmd.MembershipID.IsZero() {
		return errors.New("anonymise_user: membership id required")
	}
	m, err := h.memberships.GetByID(ctx, cmd.MembershipID)
	if err != nil {
		if errors.Is(err, membership.ErrNotFound) {
			return ErrUserNotFound
		}
		return fmt.Errorf("anonymise_user: load membership: %w", err)
	}
	personID := m.PersonID()
	err = h.persons.UpdateByID(ctx, personID, func(p *person.Person) (bool, error) {
		if err := p.Anonymise(); err != nil {
			return false, err
		}
		return true, nil
	})
	if errors.Is(err, person.ErrNotFound) {
		// Membership pointed at a missing Person — data-integrity
		// violation. Surface as 500 so operators see the alert
		// rather than silently presenting "anonymised".
		return fmt.Errorf("anonymise_user: person %s missing for membership %s",
			personID, cmd.MembershipID)
	}
	if err != nil {
		return fmt.Errorf("anonymise_user: %w", err)
	}
	return nil
}
