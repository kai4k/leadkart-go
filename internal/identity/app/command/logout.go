package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/app/refreshmint"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
)

// LogoutCommand carries the plaintext refresh-token to revoke. Same
// hash-lookup pattern as Refresh — token self-identifies.
type LogoutCommand struct {
	RefreshTokenPlain string
	Reason            string // e.g. "user-logout", "admin-revoke"
}

// LogoutHandler revokes a single refresh-token family. Idempotent:
// looking up an unknown hash OR an already-revoked family both succeed
// silently (the security-relevant goal is "this token can't be used
// again"; if it never could, success). The HTTP port returns 204.
type LogoutHandler struct {
	families refreshtoken.Repository
	now      func() time.Time
}

// NewLogoutHandler wires the handler. `now` is the explicit time
// source per the clock-injection refactor — composition root wires
// `time.Now`. Nil → time.Now.
func NewLogoutHandler(families refreshtoken.Repository, now func() time.Time) LogoutHandler {
	if now == nil {
		now = time.Now
	}
	return LogoutHandler{families: families, now: now}
}

// Handle revokes the family containing the presented token.
func (h LogoutHandler) Handle(ctx context.Context, cmd LogoutCommand) error {
	presentedHash, err := refreshtoken.NewTokenHash(refreshmint.HashOf(cmd.RefreshTokenPlain))
	if err != nil {
		// Malformed plaintext — treat as already-revoked-success.
		return nil
	}

	family, err := h.families.GetByTokenHash(ctx, presentedHash)
	if err != nil {
		if errors.Is(err, refreshtoken.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("logout: lookup family: %w", err)
	}

	reason := cmd.Reason
	if reason == "" {
		reason = "user-logout"
	}

	now := h.now()
	err = h.families.UpdateByID(ctx, family.ID(), func(f *refreshtoken.Family) (bool, error) {
		return true, f.Revoke(reason, now)
	})
	if err != nil {
		return fmt.Errorf("logout: persist revoke: %w", err)
	}
	return nil
}
