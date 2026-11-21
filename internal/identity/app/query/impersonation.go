package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/impersonation"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ImpersonationSessionView is the wire-shape of an active impersonation session.
type ImpersonationSessionView struct {
	SessionID      string
	OperatorID     string
	TargetTenantID string
	// TenantDisplayName is the target tenant's CURRENT display name, read when the
	// view is built (a rename shows on the next read); empty only when the tenant
	// row is gone.
	TenantDisplayName string
	Reason            string
	CreatedAt         time.Time
	ExpiresAt         time.Time
}

// ListImpersonationSessionsQuery returns active sessions for the supplied operator only.
type ListImpersonationSessionsQuery struct {
	OperatorID string
}

// ListImpersonationSessionsHandler runs the list query.
type ListImpersonationSessionsHandler struct {
	store   impersonation.Store
	tenants tenant.Repository
}

// NewListImpersonationSessionsHandler wires the handler.
func NewListImpersonationSessionsHandler(store impersonation.Store, tenants tenant.Repository) ListImpersonationSessionsHandler {
	if store == nil {
		panic("query: NewListImpersonationSessionsHandler store required")
	}
	if tenants == nil {
		panic("query: NewListImpersonationSessionsHandler tenants required")
	}
	return ListImpersonationSessionsHandler{store: store, tenants: tenants}
}

// Handle returns the active sessions for the operator.
func (h ListImpersonationSessionsHandler) Handle(ctx context.Context, q ListImpersonationSessionsQuery) ([]ImpersonationSessionView, error) {
	if q.OperatorID == "" {
		return nil, errors.New("list_impersonation_sessions: operator id required")
	}
	sessions, err := h.store.ListByOperator(ctx, q.OperatorID)
	if err != nil {
		return nil, fmt.Errorf("list_impersonation_sessions: %w", err)
	}
	// One tenant read per DISTINCT target (an operator holds a handful of live
	// sessions, often several against one company), so the list is never N+1.
	names := make(map[string]string, len(sessions))
	out := make([]ImpersonationSessionView, 0, len(sessions))
	for _, s := range sessions {
		name, seen := names[s.TargetTenantID()]
		if !seen {
			name, err = h.tenantDisplayName(ctx, s.TargetTenantID())
			if err != nil {
				return nil, fmt.Errorf("list_impersonation_sessions: %w", err)
			}
			names[s.TargetTenantID()] = name
		}
		out = append(out, ImpersonationSessionView{
			SessionID:         s.ID(),
			OperatorID:        s.OperatorID(),
			TargetTenantID:    s.TargetTenantID(),
			TenantDisplayName: name,
			Reason:            s.Reason(),
			CreatedAt:         s.CreatedAt(),
			ExpiresAt:         s.ExpiresAt(),
		})
	}
	return out, nil
}

// tenantDisplayName reads the tenant's current display name. A tenant row that is
// gone yields "" (the session outlived it); any other read failure is an error.
func (h ListImpersonationSessionsHandler) tenantDisplayName(ctx context.Context, id string) (string, error) {
	tn, err := h.tenants.GetByID(ctx, tenant.ID(id))
	switch {
	case err == nil:
		return tn.DisplayName(), nil
	case errors.Is(err, tenant.ErrNotFound):
		return "", nil
	default:
		return "", fmt.Errorf("read tenant: %w", err)
	}
}
