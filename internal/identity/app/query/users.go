package query

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// First-page keyset sentinel: (maxTime, maxUUID) is strictly greater than
// every real (joined_at, id) pair, admitting all rows (ADR 0038).
var (
	pageStartSortValue = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	pageStartID        = "ffffffff-ffff-ffff-ffff-ffffffffffff"
)

// UserView composes per-Membership context with the Person's global identity fields.
// A "user" in HTTP vocabulary is one (Person, Tenant) Membership row.
type UserView struct {
	MembershipID  string
	PersonID      string
	TenantID      string
	Email         string
	FirstName     string
	LastName      string
	Status        string
	Designation   string
	Department    string
	StatusMessage string
	JoinedAt      time.Time
	LeftAt        time.Time
	ReportsTo     string
	RoleIDs       []string
}

// ----- GetUserQuery --------------------------------------------------------

// GetUserQuery returns the UserView for a Membership in the supplied tenant.
// TenantID is explicit per ADR 0062; cross-tenant access surfaces as ErrNotFound.
type GetUserQuery struct {
	TenantID     tenant.ID
	MembershipID membership.ID
}

// GetUserHandler composes membership + person reads.
type GetUserHandler struct {
	memberships membership.Repository
	persons     person.Repository
}

// NewGetUserHandler wires the handler.
func NewGetUserHandler(m membership.Repository, p person.Repository) GetUserHandler {
	if m == nil {
		panic("query: NewGetUserHandler memberships repository required")
	}
	if p == nil {
		panic("query: NewGetUserHandler persons repository required")
	}
	return GetUserHandler{memberships: m, persons: p}
}

// Handle returns the composed view or [membership.ErrNotFound] when
// the Membership doesn't exist (or is in a different tenant).
func (h GetUserHandler) Handle(ctx context.Context, q GetUserQuery) (UserView, error) {
	if q.TenantID.IsZero() {
		return UserView{}, errors.New("get_user: tenant id required")
	}
	if q.MembershipID.IsZero() {
		return UserView{}, errors.New("get_user: membership id required")
	}
	m, err := h.memberships.GetByID(ctx, q.TenantID, q.MembershipID)
	if err != nil {
		return UserView{}, fmt.Errorf("get_user: load membership: %w", err)
	}
	p, err := h.persons.GetByID(ctx, m.PersonID())
	if err != nil {
		// Non-RLS table: ErrNotFound here is a data-integrity violation;
		// surface as error so operators see it rather than an empty view.
		return UserView{}, fmt.Errorf("get_user: load person %s: %w", m.PersonID(), err)
	}
	return composeUserView(m, p), nil
}

// ----- ListUsersQuery ------------------------------------------------------

// ListUsersQuery returns all Memberships in the supplied tenant.
// TenantID is explicit per ADR 0062; RLS + JWT-bridge ensure it matches the caller's scope.
type ListUsersQuery struct {
	TenantID tenant.ID
}

// ListUsersHandler runs the list query.
type ListUsersHandler struct {
	memberships membership.Repository
	persons     person.Repository
}

// NewListUsersHandler wires the handler.
func NewListUsersHandler(m membership.Repository, p person.Repository) ListUsersHandler {
	if m == nil {
		panic("query: NewListUsersHandler memberships repository required")
	}
	if p == nil {
		panic("query: NewListUsersHandler persons repository required")
	}
	return ListUsersHandler{memberships: m, persons: p}
}

// Handle returns all UserViews for the tenant.
// HTTP boundary uses ListUsersPagedHandler for the standard list endpoint (ADR 0038).
// This handler is for full-tenant exports / internal admin tools.
func (h ListUsersHandler) Handle(ctx context.Context, q ListUsersQuery) ([]UserView, error) {
	if q.TenantID.IsZero() {
		return nil, errors.New("list_users: tenant id required")
	}
	mems, err := h.memberships.ListForTenant(ctx, q.TenantID)
	if err != nil {
		return nil, fmt.Errorf("list_users: list memberships: %w", err)
	}
	// Batched hydration: one GetByIDs query for all persons, not N queries.
	pIDs := make([]person.ID, 0, len(mems))
	for _, m := range mems {
		pIDs = append(pIDs, m.PersonID())
	}
	personByID, err := h.persons.GetByIDs(ctx, pIDs)
	if err != nil {
		return nil, fmt.Errorf("list_users: hydrate persons: %w", err)
	}
	out := make([]UserView, 0, len(mems))
	for _, m := range mems {
		p, ok := personByID[m.PersonID()]
		if !ok {
			// Only legal absence: race with soft-delete.
			return nil, fmt.Errorf("list_users: load person %s: %w", m.PersonID(), person.ErrNotFound)
		}
		out = append(out, composeUserView(m, p))
	}
	return out, nil
}

// ----- ListUsersPagedQuery -------------------------------------------------

// ListUsersPagedQuery returns one keyset page of ACTIVE Memberships (ADR 0038).
// Zero Cursor = first page. PageSize is clamped by pagination.ClampPageSize.
// Status filter is hard-coded to 'active' to match the partial index
// from migration 20260518000001.
type ListUsersPagedQuery struct {
	TenantID tenant.ID
	Cursor   pagination.Cursor
	PageSize int
}

// ListUsersPagedHandler runs the keyset list query + Person hydration.
type ListUsersPagedHandler struct {
	memberships membership.Repository
	persons     person.Repository
}

// NewListUsersPagedHandler wires the handler.
func NewListUsersPagedHandler(m membership.Repository, p person.Repository) ListUsersPagedHandler {
	if m == nil {
		panic("query: NewListUsersPagedHandler memberships repository required")
	}
	if p == nil {
		panic("query: NewListUsersPagedHandler persons repository required")
	}
	return ListUsersPagedHandler{memberships: m, persons: p}
}

// Handle returns one page of UserView. See ADR 0038 for the keyset
// pagination contract.
func (h ListUsersPagedHandler) Handle(ctx context.Context, q ListUsersPagedQuery) (pagination.Page[UserView], error) {
	if q.TenantID.IsZero() {
		return pagination.Page[UserView]{}, errors.New("list_users_paged: tenant id required")
	}
	pageSize := pagination.ClampPageSize(q.PageSize)

	// Use sentinel when caller supplied no cursor.
	beforeAt := q.Cursor.SortValue
	beforeID := q.Cursor.ID
	if beforeID == "" && beforeAt.IsZero() {
		beforeAt = pageStartSortValue
		beforeID = pageStartID
	}

	// Fetch page_size+1; BuildPage sets HasMore + NextCursor accordingly.
	mems, err := h.memberships.ListForTenantPage(ctx, q.TenantID, beforeAt, beforeID, pageSize+1)
	if err != nil {
		return pagination.Page[UserView]{}, fmt.Errorf("list_users_paged: list memberships: %w", err)
	}

	// Batched hydration: one query for all persons.
	pIDs := make([]person.ID, 0, len(mems))
	for _, m := range mems {
		pIDs = append(pIDs, m.PersonID())
	}
	personByID, err := h.persons.GetByIDs(ctx, pIDs)
	if err != nil {
		return pagination.Page[UserView]{}, fmt.Errorf("list_users_paged: hydrate persons: %w", err)
	}
	views := make([]UserView, 0, len(mems))
	for _, m := range mems {
		p, ok := personByID[m.PersonID()]
		if !ok {
			return pagination.Page[UserView]{}, fmt.Errorf("list_users_paged: load person %s: %w", m.PersonID(), person.ErrNotFound)
		}
		views = append(views, composeUserView(m, p))
	}

	return pagination.BuildPage(views, pageSize, func(v UserView) pagination.Cursor {
		return pagination.Cursor{SortValue: v.JoinedAt, ID: v.MembershipID}
	}), nil
}

// ----- helpers -------------------------------------------------------------

func composeUserView(m *membership.Membership, p *person.Person) UserView {
	roleIDs := m.RoleAssignments()
	roles := make([]string, len(roleIDs))
	for i, rid := range roleIDs {
		roles[i] = rid.String()
	}
	return UserView{
		MembershipID:  m.ID().String(),
		PersonID:      p.ID().String(),
		TenantID:      m.TenantID().String(),
		Email:         p.Email().String(),
		FirstName:     p.FirstName(),
		LastName:      p.LastName(),
		Status:        m.Status().String(),
		Designation:   m.Designation(),
		Department:    m.Department(),
		StatusMessage: m.StatusMessage(),
		JoinedAt:      m.JoinedAt().UTC(),
		LeftAt:        m.LeftAt().UTC(),
		ReportsTo:     m.ReportsTo().String(),
		RoleIDs:       roles,
	}
}

// ----- GetUserOverridesQuery -----------------------------------------------

// OverrideView is one live granted override. A zero ExpiresAt is a permanent
// grant; a non-zero one is time-bound (typically from an approved permission
// request).
type OverrideView struct {
	Name      string
	ExpiresAt time.Time
}

// UserOverridesView is a member's live permission overrides: the grants not yet
// expired and the revoked names. It is what a replace-overrides save starts
// from, so a client can re-send what is there instead of erasing it.
type UserOverridesView struct {
	// Visible is false when the actor may not change this member's overrides: the
	// other fields are then empty and the port omits the field.
	Visible bool
	Granted []OverrideView
	Revoked []string
}

// GetUserOverridesQuery asks for one member's overrides on behalf of Actor.
type GetUserOverridesQuery struct {
	TenantID     tenant.ID
	MembershipID membership.ID
	// Actor is built once by the port via authn.AuthorityFrom; the zero value sees nothing.
	Actor authority.Actor
}

// GetUserOverridesHandler loads the overrides for callers allowed to change them.
type GetUserOverridesHandler struct {
	memberships membership.Repository
	roles       role.Repository
	persons     person.Repository
	now         func() time.Time
}

// NewGetUserOverridesHandler wires the handler. Nil `now` means time.Now.
func NewGetUserOverridesHandler(m membership.Repository, r role.Repository, p person.Repository, now func() time.Time) GetUserOverridesHandler {
	if m == nil {
		panic("query: NewGetUserOverridesHandler memberships repository required")
	}
	if r == nil {
		panic("query: NewGetUserOverridesHandler roles repository required")
	}
	if p == nil {
		panic("query: NewGetUserOverridesHandler persons repository required")
	}
	if now == nil {
		now = time.Now
	}
	return GetUserOverridesHandler{memberships: m, roles: r, persons: p, now: now}
}

// Handle returns the member's live overrides, Visible when the actor may change
// them (the same authority decision the replace route makes first:
// [authority.Actor.MayActOn], so a member never sees their own overrides unless
// they are a super user, and a caller of lower or equal rank sees nothing). A
// refusal is (zero view, nil): the overrides are simply absent, never an
// error that tells a caller anything. The route's permission gate is the
// port's job; this decides rank. [membership.ErrNotFound] when the member is
// not in the tenant.
func (h GetUserOverridesHandler) Handle(ctx context.Context, q GetUserOverridesQuery) (UserOverridesView, error) {
	if q.TenantID.IsZero() {
		return UserOverridesView{}, errors.New("get_user_overrides: tenant id required")
	}
	if q.MembershipID.IsZero() {
		return UserOverridesView{}, errors.New("get_user_overrides: membership id required")
	}
	if !q.Actor.Valid() {
		return UserOverridesView{}, nil
	}
	m, err := h.memberships.GetByID(ctx, q.TenantID, q.MembershipID)
	if err != nil {
		return UserOverridesView{}, fmt.Errorf("get_user_overrides: load membership: %w", err)
	}
	var roles []*role.Role
	if ids := m.RoleAssignments(); len(ids) > 0 {
		if roles, err = h.roles.GetByIDsIncludingDeleted(ctx, q.TenantID, ids); err != nil {
			return UserOverridesView{}, fmt.Errorf("get_user_overrides: load roles: %w", err)
		}
	}
	p, err := h.persons.GetByID(ctx, m.PersonID())
	if err != nil {
		return UserOverridesView{}, fmt.Errorf("get_user_overrides: load person %s: %w", m.PersonID(), err)
	}
	now := h.now()
	target, err := authority.MemberOf(m, roles, now, authority.NewPersonState(p.IsActive(), p.IsGloballySuspended(), p.IsAnonymised()))
	if err != nil {
		// A member whose roles cannot be loaded consistently has no decidable rank:
		// show nothing rather than guess.
		return UserOverridesView{}, nil
	}
	if q.Actor.MayActOn(target) != nil {
		return UserOverridesView{}, nil
	}
	live := target.Overrides()
	out := UserOverridesView{Granted: make([]OverrideView, 0, len(live.Granted)), Revoked: slices.Clone(live.Revoked)}
	for _, g := range live.Granted {
		out.Granted = append(out.Granted, OverrideView{Name: g.Name, ExpiresAt: g.ExpiresAt.UTC()})
	}
	slices.SortFunc(out.Granted, func(a, b OverrideView) int { return strings.Compare(a.Name, b.Name) })
	slices.Sort(out.Revoked)
	if out.Revoked == nil {
		out.Revoked = []string{}
	}
	out.Visible = true
	return out, nil
}
