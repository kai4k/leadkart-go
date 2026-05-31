package verificationcall

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
)

// Repository persists VerificationCall aggregates. Append-only: no
// UpdateByID since calls are immutable post-insert.
type Repository interface {
	// Add persists a call from [New], draining PullEvents to the outbox
	// in the same tx.
	Add(ctx context.Context, c *VerificationCall) error

	// ListByContact returns a contact's calls ordered by logged_at DESC.
	// Unpaginated: a contact rarely accumulates > 10 calls.
	ListByContact(ctx context.Context, contactID unverifiedcontact.ID) ([]*VerificationCall, error)
}
