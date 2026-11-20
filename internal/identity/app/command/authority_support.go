package command

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// AuthorityLocker serialises every authority-changing write within one tenant
// (ADR 0069): taken FIRST inside the UnitOfWork so the facts a policy decision
// reads (target roles, role permission sets, owner set, manager chain) cannot
// change before the write commits. Consumer-side interface;
// adapters.AuthorityLockPG implements it.
type AuthorityLocker interface {
	LockTenantAuthority(ctx context.Context, tenantID tenant.ID) error
}

// ErrMemberRolesInconsistent surfaces when a membership's assigned roles
// cannot all be loaded as that tenant's roles, so its rank cannot be decided.
// Wire shape: 409 member_roles_inconsistent. identity.role_assignments
// references identity.roles(id) with no tenant in the key, so such a row can
// exist when it was written outside the commands (an operator repair, a
// restored backup, a role id of another tenant); the commands refuse to guess.
var ErrMemberRolesInconsistent = errors.New("authority: the member's assigned roles cannot be loaded consistently")

// memberRolesInconsistentError is [ErrMemberRolesInconsistent] with the
// membership and the policy's own wording for the log. It deliberately does
// NOT unwrap to the cause: the cause is an authority sentinel (ErrNoAuthority)
// and the wire answer for this state is its own 409, not a 403.
type memberRolesInconsistentError struct {
	membershipID membership.ID
	cause        error
}

func (e *memberRolesInconsistentError) Error() string {
	return fmt.Sprintf("%s: membership %s: %v", ErrMemberRolesInconsistent, e.membershipID, e.cause)
}

func (e *memberRolesInconsistentError) Is(target error) bool {
	return target == ErrMemberRolesInconsistent
}

// maxManagerChainDepth bounds the reports-to walk (cycle prevention).
const maxManagerChainDepth = 64

// authorityFacts loads the authority-relevant projection of memberships. One
// loader serves every authority command (membership, role, permission
// request); everything it reads joins the caller's UnitOfWork transaction, so
// the facts are exactly the ones the held tenant lock protects.
type authorityFacts struct {
	memberships membership.Repository
	roles       role.Repository
	persons     person.Repository
}

// member projects m through every role it holds (soft-deleted included) and
// its person's state, decided at now. A role set that does not load
// completely is [ErrMemberRolesInconsistent].
func (f authorityFacts) member(ctx context.Context, tenantID tenant.ID, m *membership.Membership, now time.Time) (authority.Member, error) {
	out, _, err := f.project(ctx, tenantID, m, now)
	return out, err
}

// project is [authorityFacts.member] that also returns the roles it loaded
// (soft-deleted included, exactly the roles m is assigned), so a caller that
// needs the effective permissions does not load them a second time.
func (f authorityFacts) project(ctx context.Context, tenantID tenant.ID, m *membership.Membership, now time.Time) (authority.Member, []*role.Role, error) {
	if now.IsZero() {
		return authority.Member{}, nil, fmt.Errorf("%w: the decision instant is required", authority.ErrNoAuthority)
	}
	var roles []*role.Role
	if ids := m.RoleAssignments(); len(ids) > 0 {
		var err error
		if roles, err = f.roles.GetByIDsIncludingDeleted(ctx, tenantID, ids); err != nil {
			return authority.Member{}, nil, fmt.Errorf("authority facts: load roles: %w", err)
		}
	}
	p, err := f.persons.GetByID(ctx, m.PersonID())
	if err != nil {
		return authority.Member{}, nil, fmt.Errorf("authority facts: load person %s: %w", m.PersonID(), err)
	}
	state := authority.NewPersonState(p.IsActive(), p.IsGloballySuspended(), p.IsAnonymised(), p.LockedUntil(), now)
	out, err := authority.MemberOf(m, roles, now, state)
	if err != nil {
		return authority.Member{}, nil, &memberRolesInconsistentError{membershipID: m.ID(), cause: err}
	}
	return out, roles, nil
}

// memberByID loads a membership and projects it. membership.ErrNotFound
// passes through unchanged.
func (f authorityFacts) memberByID(ctx context.Context, tenantID tenant.ID, id membership.ID, now time.Time) (*membership.Membership, authority.Member, error) {
	m, err := f.memberships.GetByID(ctx, tenantID, id)
	if err != nil {
		return nil, authority.Member{}, err
	}
	out, err := f.member(ctx, tenantID, m, now)
	if err != nil {
		return nil, authority.Member{}, err
	}
	return m, out, nil
}

// role loads a role of the tenant by id, soft-deleted ones included (the
// policy decides what a deleted role may still be used for). role.ErrNotFound
// when the tenant has no such row.
func (f authorityFacts) role(ctx context.Context, tenantID tenant.ID, id role.ID) (*role.Role, error) {
	got, err := f.roles.GetByIDsIncludingDeleted(ctx, tenantID, []role.ID{id})
	if err != nil {
		return nil, fmt.Errorf("authority facts: load role: %w", err)
	}
	for _, r := range got {
		if r.ID() == id {
			return r, nil
		}
	}
	return nil, role.ErrNotFound
}

// owners lists every membership of the tenant holding the system Company Owner
// role (active or not), each projected like any other member.
func (f authorityFacts) owners(ctx context.Context, tenantID tenant.ID, now time.Time) ([]authority.Member, error) {
	ownerRole, err := f.roles.GetByTenantAndName(ctx, tenantID, role.SystemRoles.Tenant.CompanyOwner)
	if errors.Is(err, role.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("authority facts: owner role: %w", err)
	}
	if !ownerRole.IsCompanyOwner() {
		return nil, nil
	}
	all, err := f.memberships.ListForTenant(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("authority facts: list members: %w", err)
	}
	var out []authority.Member
	for _, m := range all {
		if !slices.Contains(m.RoleAssignments(), ownerRole.ID()) {
			continue
		}
		o, err := f.member(ctx, tenantID, m, now)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// currentManager is the Member that sub reports to. found is false when sub
// has no manager, and also when its manager cannot be loaded: the policy then
// denies (a member with a manager is never decided without it), see [asCurrent].
func (f authorityFacts) currentManager(ctx context.Context, tenantID tenant.ID, sub *membership.Membership, now time.Time) (cur authority.Member, found bool, err error) {
	if sub.ReportsTo().IsZero() {
		return authority.Member{}, false, nil
	}
	_, cur, err = f.memberByID(ctx, tenantID, sub.ReportsTo(), now)
	if errors.Is(err, membership.ErrNotFound) {
		return authority.Member{}, false, nil
	}
	if err != nil {
		return authority.Member{}, false, err
	}
	return cur, true, nil
}

// asCurrent is the policy's "current manager" argument: nil only when there is
// none to give.
func asCurrent(cur authority.Member, found bool) *authority.Member {
	if !found {
		return nil
	}
	return &cur
}

// rejectManagerCycle walks the proposed manager's chain upward; reaching the
// subordinate means the new link would close a cycle. Rank ordering alone
// cannot prevent cycles because ranks change after links exist.
func (f authorityFacts) rejectManagerCycle(ctx context.Context, tenantID tenant.ID, manager *membership.Membership, subordinate membership.ID) error {
	cur := manager.ReportsTo()
	for range maxManagerChainDepth {
		if cur.IsZero() {
			return nil
		}
		if cur == subordinate {
			return fmt.Errorf("%w: the reporting line would form a cycle", authority.ErrManagerNotEligible)
		}
		next, err := f.memberships.GetByID(ctx, tenantID, cur)
		if errors.Is(err, membership.ErrNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("authority facts: manager chain: %w", err)
		}
		cur = next.ReportsTo()
	}
	return fmt.Errorf("%w: reporting line deeper than %d levels", authority.ErrManagerNotEligible, maxManagerChainDepth)
}

// authorityWriter bundles one UnitOfWork + the tenant authority lock + facts.
type authorityWriter struct {
	uow   pg.UnitOfWork
	lock  AuthorityLocker
	facts authorityFacts
	now   func() time.Time
}

func newAuthorityWriter(ctor string, uow pg.UnitOfWork, lock AuthorityLocker, m membership.Repository, r role.Repository, p person.Repository, now func() time.Time) authorityWriter {
	if uow == nil || lock == nil || m == nil || r == nil || p == nil {
		panic("command: " + ctor + " uow, lock, memberships, roles and persons required")
	}
	if now == nil {
		now = time.Now
	}
	return authorityWriter{uow: uow, lock: lock, facts: authorityFacts{memberships: m, roles: r, persons: p}, now: now}
}

// freshActor is the actor the policy decides with: the token's actor narrowed
// by the actor's own membership read INSIDE the locked transaction. The more
// junior of the token's rank and the stored rank, and the intersection of the
// token's permissions with the ones resolved now from the stored roles and
// overrides: a token can only ever lose authority relative to the stored
// facts, never gain it. A membership that is missing, inactive, or whose
// person cannot act yields the zero actor, which denies every decision.
//
// Actors that need no fresh read ([authority.Actor.NeedsFreshFacts]: the zero
// actor, super users, impersonation sessions, operators under the cross-tenant
// exemption) are returned as they are.
func (f authorityFacts) freshActor(ctx context.Context, tenantID tenant.ID, token authority.Actor, now time.Time) (authority.Actor, error) {
	if !token.NeedsFreshFacts(tenantID) {
		return token, nil
	}
	m, err := f.memberships.GetByID(ctx, tenantID, token.MembershipID())
	if errors.Is(err, membership.ErrNotFound) {
		return authority.Actor{}, fmt.Errorf("%w: the acting member no longer exists in this tenant", authority.ErrNoAuthority)
	}
	if err != nil {
		return authority.Actor{}, fmt.Errorf("authority facts: load acting member: %w", err)
	}
	fresh, roles, err := f.project(ctx, tenantID, m, now)
	if errors.Is(err, ErrMemberRolesInconsistent) {
		return authority.Actor{}, fmt.Errorf("%w: the acting member's roles cannot be decided", authority.ErrNoAuthority)
	}
	if err != nil {
		return authority.Actor{}, err
	}
	live := make([]*role.Role, 0, len(roles))
	for _, r := range roles {
		if !r.IsDeleted() {
			live = append(live, r)
		}
	}
	narrowed := token.NarrowedBy(fresh, permissionNames(m.EffectivePermissions(live, now)))
	if !narrowed.Valid() {
		return authority.Actor{}, fmt.Errorf("%w: the acting member cannot act", authority.ErrNoAuthority)
	}
	return narrowed, nil
}

// locked runs fn in ONE transaction under the TARGET tenant's row-level
// security scope (the tenant is bound from tenantID, whatever the request
// context carried), after taking that tenant's authority lock. It is the one
// place every authority command passes through, so it is also the one place
// the acting member is re-read: fn receives the actor the policy must decide
// with ([authorityFacts.freshActor]) and MUST NOT use the token's actor. A
// zero actor is passed through untouched (it denies every decision).
func (w authorityWriter) locked(ctx context.Context, tenantID tenant.ID, actor authority.Actor, now time.Time, fn func(ctx context.Context, actor authority.Actor) error) error {
	ctx = tenancy.WithID(ctx, tenancy.ID(tenantID.String()))
	return w.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		if err := w.lock.LockTenantAuthority(ctx, tenantID); err != nil {
			return fmt.Errorf("authority lock: %w", err)
		}
		fresh, err := w.facts.freshActor(ctx, tenantID, actor, now)
		if err != nil {
			return err
		}
		return fn(ctx, fresh)
	})
}

// updateMember loads the target under the lock, projects its facts, and lets
// decide authorise + mutate it with the fresh actor.
func (w authorityWriter) updateMember(ctx context.Context, tenantID tenant.ID, id membership.ID, actor authority.Actor, now time.Time,
	decide func(ctx context.Context, actor authority.Actor, m *membership.Membership, target authority.Member) error) error {
	return w.locked(ctx, tenantID, actor, now, func(ctx context.Context, actor authority.Actor) error {
		return w.facts.memberships.UpdateByID(ctx, tenantID, id, func(m *membership.Membership) (bool, error) {
			target, err := w.facts.member(ctx, tenantID, m, now)
			if err != nil {
				return false, err
			}
			if err := decide(ctx, actor, m, target); err != nil {
				return false, err
			}
			return true, nil
		})
	})
}

// rotateStampIfChanged ends the access tokens of m's person when the mutation
// just applied to m recorded an authority change (a role, the overrides or the
// status; an idempotent no-op records nothing and rotates nothing). It runs in
// the command's own transaction, so the change and the rotation commit or roll
// back together; the event it records makes the subscriber evict the cached
// stamp, and the member's next request answers 401 stale_token. It is the one
// rotation call every authority command uses; manager changes and profile edits
// do NOT call it.
func (w authorityWriter) rotateStampIfChanged(ctx context.Context, m *membership.Membership, reason string, now time.Time) error {
	if !m.HasPendingEvents() {
		return nil
	}
	err := w.facts.persons.UpdateByID(ctx, m.PersonID(), func(p *person.Person) (bool, error) {
		return true, p.RotateSecurityStamp(reason, now)
	})
	if err != nil {
		return fmt.Errorf("rotate security stamp: %w", err)
	}
	return nil
}

// translateAuthorityWrite maps repository sentinels to command sentinels;
// authority errors pass through wrapped (errors.Is still matches).
func translateAuthorityWrite(op string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, membership.ErrNotFound):
		return ErrUserNotFound
	case errors.Is(err, role.ErrNotFound):
		return ErrRoleNotFound
	case errors.Is(err, role.ErrNameTaken):
		return ErrRoleNameTaken
	}
	return fmt.Errorf("%s: %w", op, err)
}
