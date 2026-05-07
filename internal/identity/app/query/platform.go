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

// PersonView is the wire-shape of a [person.Person] for Platform
// read endpoints. Email + names + flags + lifecycle timestamps; no
// password hash, no security stamp.
type PersonView struct {
	ID                     string
	Email                  string
	FirstName              string
	LastName               string
	IsActive               bool
	IsAnonymised           bool
	IsGloballySuspended    bool
	GlobalSuspensionReason string
	GloballySuspendedAt    time.Time
	CreatedAt              time.Time
	AnonymisedAt           time.Time
}

// ----- GetPersonQuery ------------------------------------------------------

// GetPersonQuery returns the Person view by global ID. Platform-only
// path — no tenant scope applies (Person is non-RLS).
type GetPersonQuery struct {
	PersonID person.ID
}

// GetPersonHandler runs the read.
type GetPersonHandler struct {
	persons person.Repository
}

// NewGetPersonHandler wires the handler.
func NewGetPersonHandler(p person.Repository) GetPersonHandler {
	if p == nil {
		panic("query: NewGetPersonHandler persons repository required")
	}
	return GetPersonHandler{persons: p}
}

// Handle returns the [PersonView] or [person.ErrNotFound].
func (h GetPersonHandler) Handle(ctx context.Context, q GetPersonQuery) (PersonView, error) {
	if q.PersonID.IsZero() {
		return PersonView{}, errors.New("get_person: person id required")
	}
	p, err := h.persons.GetByID(ctx, q.PersonID)
	if err != nil {
		return PersonView{}, fmt.Errorf("get_person: %w", err)
	}
	return projectPerson(p), nil
}

func projectPerson(p *person.Person) PersonView {
	return PersonView{
		ID:                     p.ID().String(),
		Email:                  p.Email().String(),
		FirstName:              p.FirstName(),
		LastName:               p.LastName(),
		IsActive:               p.IsActive(),
		IsAnonymised:           p.IsAnonymised(),
		IsGloballySuspended:    p.IsGloballySuspended(),
		GlobalSuspensionReason: p.GlobalSuspensionReason(),
		GloballySuspendedAt:    p.GloballySuspendedAt().UTC(),
		CreatedAt:              p.CreatedAt().UTC(),
		AnonymisedAt:           p.AnonymisedAt().UTC(),
	}
}

// ----- ListPersonMembershipsQuery ------------------------------------------

// ListPersonMembershipsQuery returns every Membership a Person holds
// across all tenants. Platform-only — cross-tenant. The Membership
// repo's ListAllForPerson is the underlying surface.
type ListPersonMembershipsQuery struct {
	PersonID person.ID
}

// ListPersonMembershipsHandler runs the cross-tenant lookup.
type ListPersonMembershipsHandler struct {
	memberships membership.Repository
	persons     person.Repository
}

// NewListPersonMembershipsHandler wires the handler.
func NewListPersonMembershipsHandler(m membership.Repository, p person.Repository) ListPersonMembershipsHandler {
	if m == nil {
		panic("query: NewListPersonMembershipsHandler memberships repository required")
	}
	if p == nil {
		panic("query: NewListPersonMembershipsHandler persons repository required")
	}
	return ListPersonMembershipsHandler{memberships: m, persons: p}
}

// Handle returns the slice of UserView for every Membership the
// Person holds. Reuses [composeUserView] from users.go for shape
// parity with GET /api/v1/users responses.
func (h ListPersonMembershipsHandler) Handle(ctx context.Context, q ListPersonMembershipsQuery) ([]UserView, error) {
	if q.PersonID.IsZero() {
		return nil, errors.New("list_person_memberships: person id required")
	}
	// Person fetch up-front so we don't repeat it per-membership in
	// composeUserView. ErrNotFound at this point is data integrity —
	// a Membership whose Person is gone — but possible during a
	// concurrent anonymise; surface as empty slice rather than 500.
	p, err := h.persons.GetByID(ctx, q.PersonID)
	if errors.Is(err, person.ErrNotFound) {
		return nil, person.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("list_person_memberships: load person: %w", err)
	}
	mems, err := h.memberships.ListAllForPerson(ctx, q.PersonID)
	if err != nil {
		return nil, fmt.Errorf("list_person_memberships: list: %w", err)
	}
	out := make([]UserView, 0, len(mems))
	for _, m := range mems {
		out = append(out, composeUserView(m, p))
	}
	return out, nil
}

// ----- ListAllTenantsQuery -------------------------------------------------

// ListAllTenantsQuery returns every Tenant for the Platform-operator
// dashboard. No filtering knobs at v0.2 — the result set is small
// enough (< 10K rows in any realistic deployment) for client-side
// pagination. Add server-side filtering when a real customer asks.
type ListAllTenantsQuery struct{}

// ListAllTenantsHandler runs the cross-tenant lookup.
type ListAllTenantsHandler struct {
	tenants tenant.Repository
}

// NewListAllTenantsHandler wires the handler.
func NewListAllTenantsHandler(t tenant.Repository) ListAllTenantsHandler {
	if t == nil {
		panic("query: NewListAllTenantsHandler tenants repository required")
	}
	return ListAllTenantsHandler{tenants: t}
}

// Handle returns the full TenantView slice ordered by created_at DESC.
func (h ListAllTenantsHandler) Handle(ctx context.Context, _ ListAllTenantsQuery) ([]TenantView, error) {
	tenants, err := h.tenants.ListAll(ctx)
	if err != nil {
		return nil, fmt.Errorf("list_all_tenants: %w", err)
	}
	out := make([]TenantView, 0, len(tenants))
	for _, t := range tenants {
		out = append(out, projectTenant(t))
	}
	return out, nil
}
