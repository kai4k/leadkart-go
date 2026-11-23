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

// ErrPersonMembershipsChanging means the person's active memberships kept
// changing underneath the guard for every attempt (a reactivation or
// deactivation raced each of them); nothing was changed. Wire shape: 409
// person_memberships_changing, retryable.
var ErrPersonMembershipsChanging = errors.New("person change guard: the person's memberships kept changing; retry")

// errMembershipsChanged is the internal "the set I checked is not the set that
// is committed now": the guard starts over.
var errMembershipsChanged = errors.New("person change guard: memberships changed since they were checked")

// maxGuardAttempts bounds the restart on errMembershipsChanged.
const maxGuardAttempts = 3

// PersonChangeGuard protects every tenant's last active owner from a
// person-level change (global suspension): such a change takes
// the person's ownership away in every tenant at once, and no tenant-level
// command ever sees it. Run proves, per tenant the person is an ACTIVE member
// of, under that tenant's authority lock and RLS scope, that
// authority.GuardLastOwner holds, and only then applies the change. The change
// itself runs inside the last guarded tenant's transaction (or, when the person
// has no active membership, in a transaction of its own), so the decision and
// the write cannot be separated by a concurrent owner change there.
//
// The membership list the decision starts from is read outside any lock, so the
// change transaction first takes the PERSON ROW lock (a reactivation reads the
// person first and takes the same row lock when it rotates the stamp, before it
// can commit, so it cannot commit between this guard's check and its write) and
// reads the memberships again: when the
// active set is not the one that was guarded, nothing is applied and the guard
// starts over (at most [maxGuardAttempts] times, then [ErrPersonMembershipsChanging]).
//
// One connection: from the moment a transaction is open the guard reads only
// THROUGH it. While it holds a tenant authority lock and the person row, every
// authority command of that tenant and every login of that person waits on it,
// each holding a pooled connection; a read that asked the pool for one more
// would wait for a connection none of them can release, and the API would stall
// for every tenant. The first list is read before any transaction, where a
// connection of its own is harmless.
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
	for range maxGuardAttempts {
		err := g.runOnce(ctx, personID, change)
		if !errors.Is(err, errMembershipsChanged) {
			return err
		}
	}
	return ErrPersonMembershipsChanging
}

// activeMemberships lists the person's ACTIVE memberships, ordered by tenant.
func (g PersonChangeGuard) activeMemberships(ctx context.Context, personID person.ID) ([]*membership.Membership, error) {
	all, err := g.w.facts.memberships.ListAllForPerson(ctx, personID)
	if err != nil {
		return nil, fmt.Errorf("person change guard: list memberships: %w", err)
	}
	var active []*membership.Membership
	for _, m := range all {
		if m.Status() == membership.StatusActive {
			active = append(active, m)
		}
	}
	slices.SortFunc(active, func(a, b *membership.Membership) int {
		return strings.Compare(a.TenantID().String(), b.TenantID().String())
	})
	return active, nil
}

func (g PersonChangeGuard) runOnce(ctx context.Context, personID person.ID, change func(ctx context.Context) error) error {
	now := g.w.now()
	active, err := g.activeMemberships(ctx, personID)
	if err != nil {
		return err
	}
	if len(active) == 0 {
		return g.w.uow.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context) error {
			return g.lockVerifyAndChange(ctx, personID, active, change)
		})
	}
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
				return g.lockVerifyAndChange(ctx, personID, active, change)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// lockVerifyAndChange runs inside the change transaction (a tenant's, or the
// platform's when the person has no active membership): it takes the person
// row, re-reads the person's active memberships ON THAT TRANSACTION and applies
// the change only when they are the ones the guard decided on.
func (g PersonChangeGuard) lockVerifyAndChange(ctx context.Context, personID person.ID, guarded []*membership.Membership, change func(ctx context.Context) error) error {
	if err := g.w.lockPerson(ctx, personID); err != nil {
		return err
	}
	// The tenant authority lock and the person row are held: this read must not
	// ask the pool for a second connection (see the type's comment), so it is the
	// repository's one read that runs on the caller's transaction.
	now, err := g.w.facts.memberships.ListActiveRefsForPersonInTx(ctx, personID)
	if err != nil {
		return fmt.Errorf("person change guard: re-read memberships: %w", err)
	}
	if !sameActiveSet(guarded, now) {
		return errMembershipsChanged
	}
	return change(ctx)
}

// sameActiveSet reports whether the guarded memberships are exactly the active
// ones read now (both ordered by tenant; a person has at most one active
// membership per tenant).
func sameActiveSet(guarded []*membership.Membership, now []membership.ActiveRef) bool {
	now = slices.Clone(now)
	slices.SortFunc(now, func(a, b membership.ActiveRef) int {
		return strings.Compare(a.TenantID.String(), b.TenantID.String())
	})
	return slices.EqualFunc(guarded, now, func(x *membership.Membership, y membership.ActiveRef) bool { return x.ID() == y.ID })
}
