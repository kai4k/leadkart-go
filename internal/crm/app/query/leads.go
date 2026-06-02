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
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ErrLeadNotFound surfaces when the lead ID does not exist in the
// caller's tenant scope (RLS-filtered). Mirrors the command-side
// sentinel — HTTP handler maps both to 404.
var ErrLeadNotFound = errors.New("crm query: lead not found")

// LeadView is the flat read model for a single CRM lead. Per STRICT
// CQRS (ADR 0041 + TDL canon) query handlers project the write
// aggregate into this read DTO; the [crmlead.CrmLead] aggregate NEVER
// leaks past the app layer into ports/. The port serializes this View
// into the wire LeadDto (1:1).
type LeadView struct {
	ID                      string
	TenantID                string
	Stage                   string
	Temperature             string
	ContactName             string
	PhoneE164               string
	City                    string
	District                string
	State                   string
	Pincode                 string
	BusinessType            string
	MedicineSystem          string
	OrderValue              string
	BuyTimeline             string
	HasDrugLicence          bool
	HasGst                  bool
	GstVerified             bool
	ProductRanges           []string
	DosageForms             []string
	ExtraProfile            map[string]any
	AssigneeMembershipID    string
	AssignedAt              time.Time
	SourcePurchaseID        string
	SourcePlatformLeadID    string
	ConvertedAt             time.Time
	ConvertedByMembershipID string
	LostAt                  time.Time
	LostByMembershipID      string
	LostReason              string
	CreatedAt               time.Time
	CreatedByMembershipID   string
}

// projectLead maps the write aggregate to the flat read View. This is
// the single source of truth for lead read projection — the port maps
// LeadView → LeadDto trivially (1:1).
func projectLead(l *crmlead.CrmLead) LeadView {
	p := l.Profile()
	// extra_profile as a generic map keeps the wire shape forward-
	// compatible: the JSONB column can grow new keys without forcing a
	// wire-DTO field bump.
	extra := map[string]any{}
	if p.Extra.Street != "" {
		extra["street"] = p.Extra.Street
	}
	if p.Extra.GstNumber != "" {
		extra["gst_number"] = p.Extra.GstNumber
	}
	if p.Extra.PanNumber != "" {
		extra["pan_number"] = p.Extra.PanNumber
	}
	if p.Extra.HasPan {
		extra["has_pan"] = true
	}
	if p.Extra.Email != "" {
		extra["email"] = p.Extra.Email
	}
	if p.Extra.Notes != "" {
		extra["notes"] = p.Extra.Notes
	}
	return LeadView{
		ID:                      l.ID().String(),
		TenantID:                l.TenantID().String(),
		Stage:                   l.Stage().String(),
		Temperature:             l.Temperature().String(),
		ContactName:             p.ContactName,
		PhoneE164:               p.PhoneE164,
		City:                    p.City,
		District:                p.District,
		State:                   p.State,
		Pincode:                 p.Pincode,
		BusinessType:            p.BusinessType.String(),
		MedicineSystem:          p.MedicineSystem.String(),
		OrderValue:              p.OrderValue.String(),
		BuyTimeline:             p.BuyTimeline.String(),
		HasDrugLicence:          p.HasDrugLicence,
		HasGst:                  p.HasGst,
		GstVerified:             p.GstVerified,
		ProductRanges:           p.ProductRanges,
		DosageForms:             p.DosageForms,
		ExtraProfile:            extra,
		AssigneeMembershipID:    l.AssigneeMembershipID(),
		AssignedAt:              l.AssignedAt(),
		SourcePurchaseID:        l.SourcePurchaseID(),
		SourcePlatformLeadID:    l.SourcePlatformLeadID(),
		ConvertedAt:             l.ConvertedAt(),
		ConvertedByMembershipID: l.ConvertedByMembershipID(),
		LostAt:                  l.LostAt(),
		LostByMembershipID:      l.LostByMembershipID(),
		LostReason:              l.LostReason(),
		CreatedAt:               l.CreatedAt(),
		CreatedByMembershipID:   l.CreatedByMembershipID(),
	}
}

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

// Handle returns the lead View or [ErrLeadNotFound].
func (h GetLeadHandler) Handle(ctx context.Context, q GetLeadQuery) (LeadView, error) {
	if q.TenantID.IsZero() {
		return LeadView{}, errors.New("crm get_lead: tenant id required")
	}
	if q.LeadID.IsZero() {
		return LeadView{}, errors.New("crm get_lead: lead id required")
	}
	l, err := h.leads.GetByID(ctx, q.TenantID, q.LeadID)
	if err != nil {
		if errors.Is(err, crmlead.ErrNotFound) {
			return LeadView{}, ErrLeadNotFound
		}
		return LeadView{}, fmt.Errorf("crm get_lead: %w", err)
	}
	return projectLead(l), nil
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
func (h ListLeadsHandler) Handle(ctx context.Context, q ListLeadsQuery) (pagination.Page[LeadView], error) {
	if q.TenantID.IsZero() {
		return pagination.Page[LeadView]{}, errors.New("crm list_leads: tenant id required")
	}
	filter := q.Filter
	if q.SelfFilter != "" {
		filter.SelfFilter = q.SelfFilter
	}
	page, err := h.leads.ListPage(ctx, q.TenantID, filter, q.Cursor, pagination.ClampPageSize(q.PageSize))
	if err != nil {
		return pagination.Page[LeadView]{}, fmt.Errorf("crm list_leads: %w", err)
	}
	views := make([]LeadView, 0, len(page.Items))
	for _, l := range page.Items {
		views = append(views, projectLead(l))
	}
	return pagination.Page[LeadView]{
		Items:      views,
		HasMore:    page.HasMore,
		NextCursor: page.NextCursor,
	}, nil
}
