package command

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
)

// PersonChangeGuard protects every tenant's last active owner from a
// person-level change (global suspension, anonymisation): such a change takes
// the person's ownership away in every tenant at once, and no tenant-level
// command ever sees it. Run proves, per tenant the person is an ACTIVE member
// of, under that tenant's authority lock and RLS scope, that
// authority.GuardLastOwner holds, and only then applies the change. The change
// itself runs inside the last guarded tenant's transaction, so the decision and
// the write cannot be separated by a concurrent owner change there. A person
// has at most one active membership (partial unique index), so that is the
// only tenant a person-level change can leave ownerless.
type PersonChangeGuard struct{ w authorityWriter }

// NewPersonChangeGuard wires the guard over the same facts loader the
// membership commands use.
func NewPersonChangeGuard(uow pg.UnitOfWork, lock AuthorityLocker, m membership.Repository, r role.Repository, p person.Repository, now func() time.Time) PersonChangeGuard {
	return PersonChangeGuard{w: newAuthorityWriter("NewPersonChangeGuard", uow, lock, m, r, p, now)}
}

// Run applies change after guarding every tenant the person actively belongs
// to. authority.ErrLastOwnerProtected means nothing was changed.
func (g PersonChangeGuard) Run(ctx context.Context, personID person.ID, change func(ctx context.Context) error) error {
	if g.w.uow == nil {
		return errors.New("person change guard: not wired") // a zero guard must never skip the check
	}
	now := g.w.now()
	all, err := g.w.facts.memberships.ListAllForPerson(ctx, personID)
	if err != nil {
		return fmt.Errorf("person change guard: list memberships: %w", err)
	}
	var active []*membership.Membership
	for _, m := range all {
		if m.Status() == membership.StatusActive {
			active = append(active, m)
		}
	}
	if len(active) == 0 {
		return change(ctx)
	}
	slices.SortFunc(active, func(a, b *membership.Membership) int {
		return strings.Compare(a.TenantID().String(), b.TenantID().String())
	})
	for i, m := range active {
		last := i == len(active)-1
		err := g.w.locked(ctx, m.TenantID(), authority.Actor{}, now, func(ctx context.Context, _ authority.Actor) error {
			// Re-read under the lock: the list above was taken without it.
			cur, member, err := g.w.facts.memberByID(ctx, m.TenantID(), m.ID(), now)
			switch {
			case errors.Is(err, membership.ErrNotFound):
				// gone since the list: nothing to protect here
			case err != nil:
				return err
			case cur.Status() == membership.StatusActive && member.IsOwner():
				owners, err := g.w.facts.owners(ctx, m.TenantID(), now)
				if err != nil {
					return err
				}
				if err := authority.GuardLastOwner(member, owners); err != nil {
					return err
				}
			}
			if last {
				return change(ctx)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
