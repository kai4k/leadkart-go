package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/impersonation"
)

// ImpersonationSessionView is the wire-shape of an active impersonation session.
type ImpersonationSessionView struct {
	SessionID      string
	OperatorID     string
	TargetTenantID string
	Reason         string
	CreatedAt      time.Time
	ExpiresAt      time.Time
}

// ListImpersonationSessionsQuery returns active sessions for the supplied operator only.
type ListImpersonationSessionsQuery struct {
	OperatorID string
}

// ListImpersonationSessionsHandler runs the list query.
type ListImpersonationSessionsHandler struct {
	store impersonation.Store
}

// NewListImpersonationSessionsHandler wires the handler.
func NewListImpersonationSessionsHandler(store impersonation.Store) ListImpersonationSessionsHandler {
	if store == nil {
		panic("query: NewListImpersonationSessionsHandler store required")
	}
	return ListImpersonationSessionsHandler{store: store}
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
	out := make([]ImpersonationSessionView, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, ImpersonationSessionView{
			SessionID:      s.ID(),
			OperatorID:     s.OperatorID(),
			TargetTenantID: s.TargetTenantID(),
			Reason:         s.Reason(),
			CreatedAt:      s.CreatedAt(),
			ExpiresAt:      s.ExpiresAt(),
		})
	}
	return out, nil
}
