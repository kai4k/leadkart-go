// Package query holds Identity read-side query handlers.
//
// Mirrors the canonical TDL Wild Workouts `app/query` layout — query
// handlers are concrete structs with `Handle(ctx, query) (Result, error)`
// signatures, aggregated under [identity.Application.Queries] in the
// facade and dispatched directly by HTTP/event ports.
//
// Queries NEVER mutate. They project repository data into wire-friendly
// shapes; aggregates may be read but their domain methods (state
// transitions) MUST not be invoked here.
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

// ListSessionsQuery returns the active refresh-token families for the
// supplied PersonID. A "session" in the wire vocabulary is one Family
// (one device-bound refresh-token chain).
//
// PersonID arrives from the verified JWT Subject claim — never from the
// request body. The HTTP port populates it after RequireAuth.
type ListSessionsQuery struct {
	PersonID person.ID
}

// SessionView is the wire-shape of a single active session.
//
// FamilyID + DeviceLabel + CreatedAt + LastUsedAt are the four fields a
// security-conscious user reviews ("which devices have access to my
// account?") per Auth0 / Okta / GitHub session-management UI canon.
//
// Tokens / hashes are NEVER exposed — the caller receives metadata only.
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

// Handle returns the active sessions for PersonID, oldest first (matches
// the repository's ListActiveForPerson ORDER BY created_at).
//
// Empty result is NOT an error — a freshly-anonymised user has no active
// sessions but the query still succeeds.
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
