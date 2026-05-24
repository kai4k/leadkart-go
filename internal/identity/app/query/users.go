package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Sentinel for "first page" cursor — the tuple (maxTime, maxUUID) is
// guaranteed to be strictly greater than every real (joined_at, id)
// pair, so the WHERE (joined_at, id) < (sentinel) predicate admits
// every row. Used by ListUsersPaged when the caller supplied no
// cursor (q.Cursor.ID == "" + SortValue.IsZero()).
//
// Per ADR 0038 — keyset predicate needs concrete tuple values; this
// is the canonical "no cursor = first page" encoding.
var (
	pageStartSortValue = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	pageStartID        = "ffffffff-ffff-ffff-ffff-ffffffffffff"
)

// UserView is the wire-shape composing per-Membership context with
// the underlying Person's global identity fields. Mirrors the .NET
// LeadKart `MembershipDetailDto` — each "user" in the HTTP vocabulary
// is one (Person, Tenant) Membership row plus enough Person data to
// make the row legible without a second roundtrip.
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

// GetUserQuery returns the UserView for a Membership in the caller's
// tenant. Cross-tenant access surfaces as [membership.ErrNotFound]
// (the repository's RLS scope filters silently).
type GetUserQuery struct {
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
	if q.MembershipID.IsZero() {
		return UserView{}, errors.New("get_user: membership id required")
	}
	m, err := h.memberships.GetByID(ctx, q.MembershipID)
	if err != nil {
		return UserView{}, fmt.Errorf("get_user: load membership: %w", err)
	}
	p, err := h.persons.GetByID(ctx, m.PersonID())
	if err != nil {
		// Person fetched globally by ID — non-RLS table — should not
		// fail with NotFound when Membership exists. Surface as
		// generic error so the operator sees the data-integrity
		// violation rather than silently presenting an incomplete
		// view.
		return UserView{}, fmt.Errorf("get_user: load person %s: %w", m.PersonID(), err)
	}
	return composeUserView(m, p), nil
}

// ----- ListUsersQuery ------------------------------------------------------

// ListUsersQuery returns all Memberships in the supplied tenant.
//
// Tenant scope is supplied explicitly so the handler stays repository-
// pattern-pure; the RLS GUC + JWT-bridge middleware ensure the caller
// can only pass their own tenant ID anyway.
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

// Handle returns the slice of UserView for all Memberships under the
// tenant. Implementation note: N+1 Person fetches per Membership are
// acceptable for v0.2 (small-tenant UX); the future denormalised view
// or batched lookup is a per-tenant-scale optimization.
//
// Kept for callers that genuinely want every row (full-tenant export,
// internal admin tools). The HTTP boundary uses ListUsersPagedHandler
// instead for the standard list endpoint per ADR 0038.
func (h ListUsersHandler) Handle(ctx context.Context, q ListUsersQuery) ([]UserView, error) {
	if q.TenantID.IsZero() {
		return nil, errors.New("list_users: tenant id required")
	}
	mems, err := h.memberships.ListForTenant(ctx, q.TenantID)
	if err != nil {
		return nil, fmt.Errorf("list_users: list memberships: %w", err)
	}
	// Batched hydration — one query for N persons, not N queries
	// (Brandur "Postgres at Scale"; per the runtime QueryCounter gate
	// in [pg.QueryCounter] this brings the per-request query budget
	// from O(N) to O(1)).
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
			// Race with soft-delete is the only legal absence; treat
			// as the same opaque load error the prior shape returned.
			return nil, fmt.Errorf("list_users: load person %s: %w", m.PersonID(), person.ErrNotFound)
		}
		out = append(out, composeUserView(m, p))
	}
	return out, nil
}

// ----- ListUsersPagedQuery -------------------------------------------------

// ListUsersPagedQuery returns one keyset-paginated page of ACTIVE
// Memberships under the supplied tenant. Per ADR 0038:
//
//   - Cursor with zero values = first page (sentinel sort tuple admits
//     every row).
//   - PageSize is clamped via [pagination.ClampPageSize] before the
//     query; callers can pass 0 to get the default.
//   - Returns [pagination.Page[UserView]] with HasMore + NextCursor
//     populated.
//
// Status filter is hard-coded to 'active' to match the partial index
// shipped in migration 20260518000001. Inactive listing lands as a
// separate ?status=inactive endpoint when frontend asks.
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

	// Sentinel for first page when caller supplied no cursor.
	beforeAt := q.Cursor.SortValue
	beforeID := q.Cursor.ID
	if beforeID == "" && beforeAt.IsZero() {
		beforeAt = pageStartSortValue
		beforeID = pageStartID
	}

	// LIMIT page_size+1 — the "peek one extra" pattern. BuildPage drops
	// the extra row + sets HasMore + NextCursor accordingly.
	mems, err := h.memberships.ListForTenantPage(ctx, beforeAt, beforeID, pageSize+1)
	if err != nil {
		return pagination.Page[UserView]{}, fmt.Errorf("list_users_paged: list memberships: %w", err)
	}

	// Batched hydration — one query for N persons, not N queries.
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
