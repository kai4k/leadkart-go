package person

import (
	"context"
	"time"

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
// Person is NOT tenant-scoped — the underlying table has no RLS.
// Cross-tenant lookups go through dedicated paths: this repo's
// GetByEmail handles single-Person resolution; the login flow uses
// [command.AuthRouter] (postgres impl
// `internal/identity/adapters/auth_router_pg.go`) which JOINs
// persons + tenant_memberships in one indexed roundtrip — current
// canon per Brandur Leach / DHH "Postgres scales further than you
// think." Earlier comments in this codebase referenced an
// "auth_routing index" as a future denormalised table; that
// 2014-era escalation is no longer the canon, the JOIN is.
type Repository interface {
	// Add persists a brand-new Person from [New]. Returns [ErrEmailTaken]
	// on duplicate email (DB unique constraint surfaces as this typed error).
	Add(ctx context.Context, p *Person) error

	// UpdateByID loads, mutates via updateFn, persists, drains events.
	// Per ADR 0004 + TDL Sep 2024 UpdateFn pattern. updateFn returns
	// (true, nil) to commit; (false, nil) for no-op; (_, err) to rollback.
	UpdateByID(ctx context.Context, id ID, updateFn func(*Person) (bool, error)) error

	// UpdateAfterFailedLogin counts one failed password attempt for the person and
	// applies the lock policy in ONE atomic statement (row locked, new count and
	// lock computed from the locked version, no read-modify-write in Go), so
	// concurrent attempts neither lose updates nor guess past the lock. now is the
	// attempt's time, taken by the caller after its password check. It implements
	// exactly [Person.RegisterFailedLogin]'s policy (MaxFailedLogins within
	// LockoutWindow, locked for LockoutDuration) and records the account-locked
	// event, in the same transaction, for the attempt that crosses the threshold.
	// Returns [ErrNotFound] when the person does not exist.
	UpdateAfterFailedLogin(ctx context.Context, id ID, now time.Time) (LockoutOutcome, error)

	// UpdateAfterSuccessfulLogin undoes the attempt's own count after the password
	// verified, in ONE atomic statement that never clears a lock set by anyone else:
	// the counters are reset only when no lock is in force at now, or the lock in
	// force is ownLock (the one THIS attempt's registration set when it crossed the
	// threshold; zero for every other attempt). A concurrent failure may have locked
	// the account while this attempt was being verified. LockoutClear reports
	// whether it cleared and, when not, the lock that prevented it. Records
	// the account-unlocked event, in the same transaction, when failures or an
	// expired lock preceded this attempt. Returns [ErrNotFound] when the person
	// does not exist.
	UpdateAfterSuccessfulLogin(ctx context.Context, id ID, now, ownLock time.Time) (LockoutClear, error)

	// GetByIDForShare returns the Person read under a row SHARE lock (it waits for
	// a password writer that is committing and blocks a writer that starts later
	// until the caller's transaction ends). The login's session insert uses it to
	// confirm the credential it verified is still the stored one. Must run inside a
	// unit of work to mean anything; returns [ErrNotFound] when missing.
	GetByIDForShare(ctx context.Context, id ID) (*Person, error)

	// GetByID returns the Person or [ErrNotFound]. Read-only path.
	GetByID(ctx context.Context, id ID) (*Person, error)

	// GetByIDs is the batched form for hydration sweeps (e.g. list-users
	// rendering N memberships → N persons in ONE query instead of N).
	// Replaces the per-loop GetByID N+1 pattern per Brandur "What I
	// learned running Postgres at scale" + the project's runtime
	// QueryCounter gate in [pg.QueryCounter].
	//
	// Returns a map[ID]*Person keyed by the input ID (NOT in input order
	// — caller iterates the originating Membership slice + does its own
	// composition). Missing IDs are simply absent from the map; this is
	// NOT an error condition (race-with-soft-delete is possible). Pass
	// an empty slice → returns an empty map.
	GetByIDs(ctx context.Context, ids []ID) (map[ID]*Person, error)

	// GetByEmail returns the Person by globally-unique email or [ErrNotFound].
	// Used by login + password-reset + email-change flows.
	GetByEmail(ctx context.Context, e email.Address) (*Person, error)

	// GetByPasswordResetTokenHash returns the Person whose pending
	// password-reset matches the supplied hash, or [ErrNotFound]. The
	// caller hashes the user-presented plaintext + queries via this
	// method; uniqueness enforced by the partial unique index per
	// migration 20260507000004.
	GetByPasswordResetTokenHash(ctx context.Context, hash PasswordResetTokenHash) (*Person, error)

	// GetByEmailChangeTokenHash returns the Person whose pending
	// email-change matches the supplied hash, or [ErrNotFound].
	// Same hash-only lookup pattern as GetByPasswordResetTokenHash.
	GetByEmailChangeTokenHash(ctx context.Context, hash EmailChangeTokenHash) (*Person, error)
}

// StampEvictor drops a person's cached security stamp. The person repository
// calls it after a committed write that rotated the stamp; the stamp cache
// implements it.
type StampEvictor interface {
	Invalidate(ctx context.Context, id ID) error
}
