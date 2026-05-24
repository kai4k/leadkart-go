package query

import (
	"context"
	"errors"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit"
)

// GetLeadCreditBalanceQuery — tenant fetches its own credit balance.
// Platform-tier operator can fetch any tenant's via the same handler
// (RLS allows the read either way; the HTTP gate decides access).
type GetLeadCreditBalanceQuery struct {
	TenantID leadcredit.TenantID
}

// LeadCreditBalanceView is the wire-shaped projection.
type LeadCreditBalanceView struct {
	TenantID  string
	Balance   int64
}

// ErrCreditRowNotFound is the typed sentinel surfaced to HTTP for the
// "no row yet" case — distinct from a balance of 0 (the latter
// requires an existing row). The HTTP handler may translate both to a
// canonical zero-balance response — that's an HTTP layer policy.
var ErrCreditRowNotFound = errors.New("lead credit: row not found")

// GetLeadCreditBalanceHandler returns the per-tenant balance projection.
type GetLeadCreditBalanceHandler struct {
	credits leadcredit.Repository
}

// NewGetLeadCreditBalanceHandler wires the handler.
func NewGetLeadCreditBalanceHandler(credits leadcredit.Repository) GetLeadCreditBalanceHandler {
	return GetLeadCreditBalanceHandler{credits: credits}
}

// Handle returns the projection. Translates [leadcredit.ErrNotFound] to
// the package-local [ErrCreditRowNotFound] so the HTTP layer has a
// stable typed surface (the domain sentinel is implementation detail).
func (h GetLeadCreditBalanceHandler) Handle(
	ctx context.Context,
	q GetLeadCreditBalanceQuery,
) (LeadCreditBalanceView, error) {
	c, err := h.credits.GetByTenant(ctx, q.TenantID)
	if err != nil {
		if errors.Is(err, leadcredit.ErrNotFound) {
			return LeadCreditBalanceView{}, ErrCreditRowNotFound
		}
		return LeadCreditBalanceView{}, fmt.Errorf("get lead credit balance: %w", err)
	}
	return LeadCreditBalanceView{
		TenantID: c.TenantID().String(),
		Balance:  c.Balance(),
	}, nil
}
