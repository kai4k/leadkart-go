// Package query holds companysettings read-side handlers per TDL canon. Read
// only: no state mutation. Strict CQRS (ADR 0067): handlers return flat *View
// read models, never the aggregate.
package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/companysettings/domain/commercesettings"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// SettingsView is the read model of a company's policy: the policy, the version
// the next save must name, and the option metadata the web renders controls from.
type SettingsView struct {
	TenantID  string
	Version   int64
	Policy    commercepolicy.Policy
	Options   []commercepolicy.SettingInfo
	UpdatedAt *time.Time
	UpdatedBy string
}

// ----- GetSettings ------------------------------------------------------------

// GetSettingsQuery selects the tenant's policy.
type GetSettingsQuery struct {
	TenantID tenant.ID
}

// GetSettingsHandler reads the policy. A tenant with no stored policy reads the
// defaults at version 0 (commercepolicy.Initial), never a not-found.
type GetSettingsHandler struct {
	settings commercesettings.Repository
}

// NewGetSettingsHandler wires the handler.
func NewGetSettingsHandler(settings commercesettings.Repository) GetSettingsHandler {
	if settings == nil {
		panic("query: NewGetSettingsHandler settings required")
	}
	return GetSettingsHandler{settings: settings}
}

// Handle returns the read model.
func (h GetSettingsHandler) Handle(ctx context.Context, q GetSettingsQuery) (*SettingsView, error) {
	if q.TenantID == "" {
		return nil, errors.New("get company settings: tenant id required")
	}
	s, err := h.settings.GetByID(ctx, q.TenantID)
	if err != nil {
		return nil, fmt.Errorf("get company settings: %w", err)
	}
	v := &SettingsView{
		TenantID: s.TenantID().String(), Version: s.Version(), Policy: s.Policy(),
		Options: commercepolicy.Describe(), UpdatedBy: s.UpdatedBy().String(),
	}
	if !s.UpdatedAt().IsZero() {
		at := s.UpdatedAt()
		v.UpdatedAt = &at
	}
	return v, nil
}

// ----- ListGSTRates -----------------------------------------------------------

// GSTRatesView is the company's GST rate list and the policy version it was read at.
type GSTRatesView struct {
	PolicyVersion int64
	Items         []commercepolicy.GSTRate
}

// ListGSTRatesQuery selects the tenant's GST rate list.
type ListGSTRatesQuery struct {
	TenantID tenant.ID
	// ActiveOnly drops deactivated rates (the product picker).
	ActiveOnly bool
}

// ListGSTRatesHandler reads the list (empty for a company that has not added one).
type ListGSTRatesHandler struct {
	settings commercesettings.Repository
}

// NewListGSTRatesHandler wires the handler.
func NewListGSTRatesHandler(settings commercesettings.Repository) ListGSTRatesHandler {
	if settings == nil {
		panic("query: NewListGSTRatesHandler settings required")
	}
	return ListGSTRatesHandler{settings: settings}
}

// Handle returns the list in rate order.
func (h ListGSTRatesHandler) Handle(ctx context.Context, q ListGSTRatesQuery) (GSTRatesView, error) {
	if q.TenantID == "" {
		return GSTRatesView{}, errors.New("list gst rates: tenant id required")
	}
	s, err := h.settings.GetByID(ctx, q.TenantID)
	if err != nil {
		return GSTRatesView{}, fmt.Errorf("list gst rates: %w", err)
	}
	items := make([]commercepolicy.GSTRate, 0)
	for _, r := range s.Policy().Tax.GSTRates {
		if q.ActiveOnly && !r.Active {
			continue
		}
		items = append(items, r)
	}
	return GSTRatesView{PolicyVersion: s.Version(), Items: items}, nil
}

// ----- ListScheduleClasses ---------------------------------------------------

// ScheduleClassesView is the company's drug schedule class list and the policy
// version it was read at.
type ScheduleClassesView struct {
	PolicyVersion int64
	Mode          string
	Items         []commercepolicy.ScheduleClass
}

// ListScheduleClassesQuery selects the tenant's class list.
type ListScheduleClassesQuery struct {
	TenantID tenant.ID
}

// ListScheduleClassesHandler reads the list.
type ListScheduleClassesHandler struct {
	settings commercesettings.Repository
}

// NewListScheduleClassesHandler wires the handler.
func NewListScheduleClassesHandler(settings commercesettings.Repository) ListScheduleClassesHandler {
	if settings == nil {
		panic("query: NewListScheduleClassesHandler settings required")
	}
	return ListScheduleClassesHandler{settings: settings}
}

// Handle returns the list in label order.
func (h ListScheduleClassesHandler) Handle(ctx context.Context, q ListScheduleClassesQuery) (ScheduleClassesView, error) {
	if q.TenantID == "" {
		return ScheduleClassesView{}, errors.New("list drug schedule classes: tenant id required")
	}
	s, err := h.settings.GetByID(ctx, q.TenantID)
	if err != nil {
		return ScheduleClassesView{}, fmt.Errorf("list drug schedule classes: %w", err)
	}
	p := s.Policy()
	return ScheduleClassesView{PolicyVersion: s.Version(), Mode: p.DrugSchedule.Mode, Items: p.DrugSchedule.Classes}, nil
}

// ----- ListHistory ------------------------------------------------------------

// HistoryEntry is one stored version: the whole policy then, what changed, who
// and why.
type HistoryEntry struct {
	Version                int64
	Policy                 commercepolicy.Policy
	ChangedKeys            []string
	Reason                 string
	ActorMembershipID      string
	ImpersonatorOperatorID string
	ChangedAt              time.Time
}

// HistoryReader is the read port of the version history; the pg adapter
// implements it.
type HistoryReader interface {
	ListHistory(ctx context.Context, tenantID tenant.ID, cursor pagination.Cursor, pageSize int) (pagination.Page[HistoryEntry], error)
}

// ListHistoryQuery pages the history newest-first.
type ListHistoryQuery struct {
	TenantID tenant.ID
	Cursor   pagination.Cursor
	PageSize int
}

// ListHistoryHandler reads one page of the history.
type ListHistoryHandler struct {
	history HistoryReader
}

// NewListHistoryHandler wires the handler.
func NewListHistoryHandler(history HistoryReader) ListHistoryHandler {
	if history == nil {
		panic("query: NewListHistoryHandler history required")
	}
	return ListHistoryHandler{history: history}
}

// Handle returns the page; the size is clamped to the platform policy.
func (h ListHistoryHandler) Handle(ctx context.Context, q ListHistoryQuery) (pagination.Page[HistoryEntry], error) {
	if q.TenantID == "" {
		return pagination.Page[HistoryEntry]{}, errors.New("list company settings history: tenant id required")
	}
	page, err := h.history.ListHistory(ctx, q.TenantID, q.Cursor, pagination.ClampPageSize(q.PageSize))
	if err != nil {
		return pagination.Page[HistoryEntry]{}, fmt.Errorf("list company settings history: %w", err)
	}
	return page, nil
}
