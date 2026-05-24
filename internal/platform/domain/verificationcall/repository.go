package verificationcall

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
)

// Repository persists VerificationCall aggregates. Append-only — no
// UpdateByID since calls are immutable post-insert.
type Repository interface {
	// Add persists a brand-new call created via [New]. Drains
	// PullEvents to the outbox in the same tx.
	Add(ctx context.Context, c *VerificationCall) error

	// ListByContact returns every call for a contact, ordered by
	// logged_at DESC. Slice 1 query-side surface; no pagination at
	// this scale (a contact rarely accumulates > 10 calls).
	ListByContact(ctx context.Context, contactID unverifiedcontact.ID) ([]*VerificationCall, error)
}
