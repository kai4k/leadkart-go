// Package query holds CRM query handlers per TDL canon. Read-side
// only — no state mutation. Mirrors the [command] facade.
//
// Per ADR 0047 boundary discipline: this package may NOT import
// internal/crm/adapters/db, pgx, pgxpool, pgtype, or
// internal/crm/adapters (concrete). Handlers depend on domain
// repository interfaces only.
package query

import (
	"context"
	"errors"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ErrLeadNotFound surfaces when the lead ID does not exist in the
// caller's tenant scope (RLS-filtered). Mirrors the command-side
// sentinel — HTTP handler maps both to 404.
var ErrLeadNotFound = errors.New("crm query: lead not found")

// ----- GetLead --------------------------------------------------------------

// GetLeadQuery selects a single lead by ID under the supplied tenant
// scope. TenantID is the explicit tenant scope per ADR 0062 (TDL canon).
type GetLeadQuery struct {
	TenantID tenant.ID
	LeadID   crmlead.ID
}

// GetLeadHandler runs the single-lead read.
type GetLeadHandler struct {
	leads crmlead.Repository
}

// NewGetLeadHandler wires the handler.
func NewGetLeadHandler(leads crmlead.Repository) GetLeadHandler {
	if leads == nil {
		panic("query: NewGetLeadHandler leads repository required")
	}
	return GetLeadHandler{leads: leads}
}

// Handle returns the lead or [ErrLeadNotFound].
func (h GetLeadHandler) Handle(ctx context.Context, q GetLeadQuery) (*crmlead.CrmLead, error) {
	if q.TenantID.IsZero() {
		return nil, errors.New("crm get_lead: tenant id required")
	}
	if q.LeadID.IsZero() {
		return nil, errors.New("crm get_lead: lead id required")
	}
	l, err := h.leads.GetByID(ctx, q.TenantID, q.LeadID)
	if err != nil {
		if errors.Is(err, crmlead.ErrNotFound) {
			return nil, ErrLeadNotFound
		}
		return nil, fmt.Errorf("crm get_lead: %w", err)
	}
	return l, nil
}

// ----- ListLeads ------------------------------------------------------------

// ListLeadsQuery carries the cursor-pagination + filter inputs per
// ADR 0038 + BRD §6.3.
//
// TenantID is the explicit tenant scope per ADR 0062 (TDL canon).
//
// SelfFilter is the per-handler "only my assigned leads" enforcement —
// HTTP layer populates it from the JWT membership claim when the caller
// LACKS `crm.leads.read_all`. The repository-side filter applies it as
// an AssigneeMembershipID exact-match.
type ListLeadsQuery struct {
	TenantID   tenant.ID
	Cursor     pagination.Cursor
	PageSize   int
	Filter     crmlead.ListFilter
	SelfFilter string
}

// ListLeadsHandler runs the cursor-paginated list query.
type ListLeadsHandler struct {
	leads crmlead.Repository
}

// NewListLeadsHandler wires the handler.
func NewListLeadsHandler(leads crmlead.Repository) ListLeadsHandler {
	if leads == nil {
		panic("query: NewListLeadsHandler leads repository required")
	}
	return ListLeadsHandler{leads: leads}
}

// Handle returns the paginated lead list. PageSize is clamped per
// ADR 0038; SelfFilter (if non-empty) is merged into Filter.SelfFilter
// before the repository call.
func (h ListLeadsHandler) Handle(ctx context.Context, q ListLeadsQuery) (pagination.Page[*crmlead.CrmLead], error) {
	if q.TenantID.IsZero() {
		return pagination.Page[*crmlead.CrmLead]{}, errors.New("crm list_leads: tenant id required")
	}
	filter := q.Filter
	if q.SelfFilter != "" {
		filter.SelfFilter = q.SelfFilter
	}
	page, err := h.leads.ListPage(ctx, q.TenantID, filter, q.Cursor, pagination.ClampPageSize(q.PageSize))
	if err != nil {
		return pagination.Page[*crmlead.CrmLead]{}, fmt.Errorf("crm list_leads: %w", err)
	}
	return page, nil
}
