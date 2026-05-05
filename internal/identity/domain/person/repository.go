package person

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/errs"
)

// ErrNotFound is returned by Repository read methods when no matching row exists.
var ErrNotFound = errs.New(errs.KindNotFound, "person", "person not found")

// ErrEmailTaken is returned by Repository.Add when a Person with the same
// email already exists. Email is globally unique per `database.md` canon.
var ErrEmailTaken = errs.New(errs.KindAlreadyExists, "person", "email already taken")

// Repository persists Person aggregates.
//
// Person is NOT tenant-scoped — the underlying table has no RLS. Cross-
// tenant lookups (login flow: email → tenant resolution) hit the
// auth_routing index per ADR 0006.
type Repository interface {
	// Add persists a brand-new Person from [New]. Returns [ErrEmailTaken]
	// on duplicate email (DB unique constraint surfaces as this typed error).
	Add(ctx context.Context, p *Person) error

	// UpdateByID loads, mutates via updateFn, persists, drains events.
	// Per ADR 0004 + TDL Sep 2024 UpdateFn pattern. updateFn returns
	// (true, nil) to commit; (false, nil) for no-op; (_, err) to rollback.
	UpdateByID(ctx context.Context, id ID, updateFn func(*Person) (bool, error)) error

	// GetByID returns the Person or [ErrNotFound]. Read-only path.
	GetByID(ctx context.Context, id ID) (*Person, error)

	// GetByEmail returns the Person by globally-unique email or [ErrNotFound].
	// Used by login + password-reset + email-change flows.
	GetByEmail(ctx context.Context, e email.Address) (*Person, error)
}
