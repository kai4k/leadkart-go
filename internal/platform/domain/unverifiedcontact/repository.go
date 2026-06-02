package unverifiedcontact

import (
	"context"
	"errors"
)

// ErrNotFound is returned when the row does not exist.
var ErrNotFound = errors.New("unverifiedcontact: not found")

// Repository persists UnverifiedContact aggregates. Lives in the domain
// package (Cheney: accept interfaces); pgx impl in adapters/.
// Implementations must be safe for concurrent use.
type Repository interface {
	// Add persists a new contact, draining PullEvents to the outbox in the same tx.
	Add(ctx context.Context, c *UnverifiedContact) error

	// UpdateByID loads, applies updateFn, then persists + drains events in one tx.
	// updateFn: (true, nil) commits; (false, nil) aborts cleanly; (_, err) rolls back.
	UpdateByID(ctx context.Context, id ID, updateFn func(*UnverifiedContact) (bool, error)) error

	// GetByID returns the contact or [ErrNotFound].
	GetByID(ctx context.Context, id ID) (*UnverifiedContact, error)
}
