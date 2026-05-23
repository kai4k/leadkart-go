package unverifiedcontact

import (
	"context"
	"errors"
)

// ErrNotFound is returned when GetByID or UpdateByID can't locate the row.
var ErrNotFound = errors.New("unverifiedcontact: not found")

// Repository persists UnverifiedContact aggregates. Declared in the
// domain package per Cheney "accept interfaces, return structs"; the
// concrete pgx-backed impl lives in internal/platform/adapters/.
//
// All methods MUST be safe for concurrent use.
type Repository interface {
	// Add persists a brand-new contact created via [New]. Drains
	// PullEvents to the outbox in the same tx.
	Add(ctx context.Context, c *UnverifiedContact) error

	// UpdateByID loads, runs updateFn (which mutates state via aggregate
	// methods), persists + drains events — all in one tx.
	//
	// updateFn returns (true, nil) to commit; (false, nil) to abort
	// without changes; (_, err) to roll back.
	UpdateByID(ctx context.Context, id ID, updateFn func(*UnverifiedContact) (bool, error)) error

	// GetByID returns the contact or [ErrNotFound].
	GetByID(ctx context.Context, id ID) (*UnverifiedContact, error)
}
