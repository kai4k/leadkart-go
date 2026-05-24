package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
)

// ----- RevokeSessionCommand -------------------------------------------------

// RevokeSessionCommand revokes ONE refresh-token family by ID. Used by
// `DELETE /api/v1/auth/sessions/{familyId}` — the per-session "sign me
// out of THAT device" UI per Auth0 / Okta / GitHub session-management
// canon.
//
// PersonID is the verified caller from the JWT Subject claim. The
// handler enforces ownership: a Person can only revoke families that
// belong to them. Cross-Person revocation is a 404 (NOT 403 — defeats
// family-id enumeration).
type RevokeSessionCommand struct {
	PersonID person.ID
	FamilyID refreshtoken.FamilyID
	Reason   string // optional audit reason; defaults to "user_revoked"
}

// ErrSessionNotFound surfaces when the FamilyID doesn't exist OR
// belongs to a different Person OR is already revoked. All three cases
// collapse to one error per `security.md` enumeration-safety rule.
var ErrSessionNotFound = errors.New("revoke_session: session not found")

// RevokeSessionHandler revokes a single family.
type RevokeSessionHandler struct {
	families refreshtoken.Repository
	now      func() time.Time
}

// NewRevokeSessionHandler wires the handler. `now` is the explicit time
// source per the clock-injection refactor. Nil → time.Now.
func NewRevokeSessionHandler(families refreshtoken.Repository, now func() time.Time) RevokeSessionHandler {
	if families == nil {
		panic("command: NewRevokeSessionHandler families repository required")
	}
	if now == nil {
		now = time.Now
	}
	return RevokeSessionHandler{families: families, now: now}
}

// Handle runs the revoke flow.
func (h RevokeSessionHandler) Handle(ctx context.Context, cmd RevokeSessionCommand) error {
	if cmd.PersonID.IsZero() {
		return errors.New("revoke_session: person id required")
	}
	if cmd.FamilyID.IsZero() {
		return errors.New("revoke_session: family id required")
	}
	reason := cmd.Reason
	if reason == "" {
		reason = "user_revoked"
	}
	now := h.now()

	err := h.families.UpdateByID(ctx, cmd.FamilyID, func(f *refreshtoken.Family) (bool, error) {
		// Ownership gate: the caller's PersonID MUST match the family's.
		// Surface mismatch as ErrSessionNotFound to defeat enumeration —
		// an attacker probing FamilyIDs cannot distinguish "wrong owner"
		// from "doesn't exist".
		if f.PersonID() != cmd.PersonID {
			return false, ErrSessionNotFound
		}
		// Already revoked → idempotent no-op (no event, no commit).
		// Aligns with logout's idempotency contract.
		if f.IsRevoked() {
			return false, nil
		}
		if err := f.Revoke(reason, now); err != nil {
			return false, err
		}
		return true, nil
	})
	if err != nil {
		if errors.Is(err, refreshtoken.ErrNotFound) {
			return ErrSessionNotFound
		}
		if errors.Is(err, ErrSessionNotFound) {
			return ErrSessionNotFound
		}
		return fmt.Errorf("revoke_session: %w", err)
	}
	return nil
}

// ----- RevokeAllSessionsCommand --------------------------------------------

// RevokeAllSessionsCommand revokes EVERY active family for the caller's
// PersonID. Used by `DELETE /api/v1/auth/sessions` — the "sign me out
// of EVERYTHING" UI surface (Auth0/Okta/GitHub canon).
//
// Optional ExceptFamilyID lets the caller keep ONE family alive
// (typically the current device's family — "sign me out of OTHER
// devices"). When ExceptFamilyID is zero, ALL families are revoked.
type RevokeAllSessionsCommand struct {
	PersonID        person.ID
	ExceptFamilyID  refreshtoken.FamilyID // optional; zero = revoke all
	Reason          string                // optional; defaults to "user_revoked_all"
}

// RevokeAllSessionsResult reports how many families were revoked.
type RevokeAllSessionsResult struct {
	RevokedCount int
}

// RevokeAllSessionsHandler revokes the caller's active families.
type RevokeAllSessionsHandler struct {
	families refreshtoken.Repository
	now      func() time.Time
}

// NewRevokeAllSessionsHandler wires the handler. `now` is the explicit
// time source per the clock-injection refactor. Nil → time.Now.
func NewRevokeAllSessionsHandler(families refreshtoken.Repository, now func() time.Time) RevokeAllSessionsHandler {
	if families == nil {
		panic("command: NewRevokeAllSessionsHandler families repository required")
	}
	if now == nil {
		now = time.Now
	}
	return RevokeAllSessionsHandler{families: families, now: now}
}

// Handle revokes all (or all-except-one) active families.
//
// Implementation: list active families → revoke each via UpdateByID.
// Each UpdateByID opens its own transaction; partial failure leaves
// some revoked + some not. That's acceptable for revoke semantics
// (idempotent retry catches the rest), but contrast with the password-
// change cascade where atomicity matters.
func (h RevokeAllSessionsHandler) Handle(ctx context.Context, cmd RevokeAllSessionsCommand) (RevokeAllSessionsResult, error) {
	if cmd.PersonID.IsZero() {
		return RevokeAllSessionsResult{}, errors.New("revoke_all_sessions: person id required")
	}
	reason := cmd.Reason
	if reason == "" {
		reason = "user_revoked_all"
	}
	now := h.now()

	families, err := h.families.ListActiveForPerson(ctx, cmd.PersonID)
	if err != nil {
		return RevokeAllSessionsResult{}, fmt.Errorf("revoke_all_sessions: list: %w", err)
	}

	revoked := 0
	for _, f := range families {
		if !cmd.ExceptFamilyID.IsZero() && f.ID() == cmd.ExceptFamilyID {
			continue
		}
		fid := f.ID()
		uerr := h.families.UpdateByID(ctx, fid, func(loaded *refreshtoken.Family) (bool, error) {
			if loaded.IsRevoked() {
				return false, nil
			}
			if err := loaded.Revoke(reason, now); err != nil {
				return false, err
			}
			return true, nil
		})
		if uerr != nil {
			return RevokeAllSessionsResult{RevokedCount: revoked},
				fmt.Errorf("revoke_all_sessions: revoke %s: %w", fid, uerr)
		}
		revoked++
	}
	return RevokeAllSessionsResult{RevokedCount: revoked}, nil
}
