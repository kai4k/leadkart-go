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
