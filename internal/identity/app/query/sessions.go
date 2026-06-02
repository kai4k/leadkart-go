// Package query holds Identity read-side query handlers.
//
// TDL Wild Workouts layout: each handler is a concrete struct with
// Handle(ctx, query) (Result, error), aggregated under
// identity.Application.Queries and dispatched by HTTP/event ports.
// Handlers project; they never call domain state-transition methods.
package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
)

// ----- ListSessionsQuery ----------------------------------------------------

// ListSessionsQuery returns active refresh-token families for PersonID.
// A "session" in the wire vocab is one Family (one device-bound chain).
// PersonID comes from the verified JWT Subject; HTTP port sets it after RequireAuth.
type ListSessionsQuery struct {
	PersonID person.ID
}

// SessionView is the wire-shape of a single active session.
// Exposes metadata only (FamilyID, DeviceLabel, timestamps); tokens and
// hashes are never included.
type SessionView struct {
	FamilyID    string
	DeviceLabel string
	TenantID    string
	CreatedAt   time.Time
	LastUsedAt  time.Time
	IsCurrent   bool // populated when caller marks the current family
}

// ListSessionsHandler is the read-side handler.
type ListSessionsHandler struct {
	families refreshtoken.Repository
}

// NewListSessionsHandler wires the handler.
func NewListSessionsHandler(families refreshtoken.Repository) ListSessionsHandler {
	if families == nil {
		panic("query: NewListSessionsHandler families repository required")
	}
	return ListSessionsHandler{families: families}
}

// Handle returns active sessions for PersonID, oldest first.
// Empty result is not an error (e.g. freshly-anonymised user).
func (h ListSessionsHandler) Handle(ctx context.Context, q ListSessionsQuery) ([]SessionView, error) {
	if q.PersonID.IsZero() {
		return nil, errors.New("list_sessions: person id required")
	}
	families, err := h.families.ListActiveForPerson(ctx, q.PersonID)
	if err != nil {
		return nil, fmt.Errorf("list_sessions: %w", err)
	}
	out := make([]SessionView, 0, len(families))
	for _, f := range families {
		out = append(out, SessionView{
			FamilyID:    f.ID().String(),
			DeviceLabel: f.DeviceLabel(),
			TenantID:    f.TenantID().String(),
			CreatedAt:   f.CreatedAt().UTC(),
			LastUsedAt:  f.LastUsedAt().UTC(),
		})
	}
	return out, nil
}
