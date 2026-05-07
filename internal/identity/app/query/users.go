package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
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
func (h ListUsersHandler) Handle(ctx context.Context, q ListUsersQuery) ([]UserView, error) {
	if q.TenantID.IsZero() {
		return nil, errors.New("list_users: tenant id required")
	}
	mems, err := h.memberships.ListForTenant(ctx, q.TenantID)
	if err != nil {
		return nil, fmt.Errorf("list_users: list memberships: %w", err)
	}
	out := make([]UserView, 0, len(mems))
	for _, m := range mems {
		p, perr := h.persons.GetByID(ctx, m.PersonID())
		if perr != nil {
			return nil, fmt.Errorf("list_users: load person %s: %w", m.PersonID(), perr)
		}
		out = append(out, composeUserView(m, p))
	}
	return out, nil
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
