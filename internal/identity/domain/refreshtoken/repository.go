package refreshtoken

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// ErrNotFound is returned when no family matches the lookup key.
var ErrNotFound = errs.New(errs.KindNotFound, "refreshtoken", "family not found")

// Repository persists RefreshTokenFamily aggregates.
//
// The underlying tables (refresh_token_families, refresh_tokens) are
// non-RLS — refresh tokens are session-management infrastructure that
// carries the tenant context as a data column. Cross-tenant lookups by
// token hash are intentional (Auth0/Okta canon).
//
// Token-hash uniqueness is the load-bearing isolation guarantee: each
// SHA-256 hash is unique system-wide, so resolution by hash is a single
// indexed lookup with zero ambiguity.
type Repository interface {
	// Add persists a brand-new family + its first token from [NewFamily].
	Add(ctx context.Context, f *Family) error

	// UpdateByID loads, mutates, persists, drains events.
	// Per ADR 0004 + TDL Sep 2024 UpdateFn pattern.
	UpdateByID(ctx context.Context, id FamilyID, updateFn func(*Family) (bool, error)) error

	// GetByID returns the family + all its tokens, or [ErrNotFound].
	GetByID(ctx context.Context, id FamilyID) (*Family, error)

	// GetByTokenHash resolves a presented refresh token to its family.
	// Used during the rotation flow: client presents token; server hashes
	// it; this lookup finds the family containing it. Returns
	// [ErrNotFound] if no family has a token with the given hash.
	GetByTokenHash(ctx context.Context, hash TokenHash) (*Family, error)

	// ListActiveForPerson returns all non-revoked families for a Person.
	// Used by the user "manage sessions" UI + the family-cap-per-person
	// enforcement at family creation time.
	ListActiveForPerson(ctx context.Context, personID person.ID) ([]*Family, error)
}
