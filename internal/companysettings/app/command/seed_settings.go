package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/companysettings/domain/commercesettings"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// SeedSettingsCommand stores version 1 (the defaults) for a tenant that has no
// stored policy. Driven by the tenant-registered subscriber.
type SeedSettingsCommand struct {
	TenantID tenant.ID
}

// SeedSettingsResult says whether the policy already existed (a replay).
type SeedSettingsResult struct {
	AlreadyExisted bool
}

// SeedSettingsHandler seeds the first version. Idempotent: a stored policy is
// left alone and the replay acknowledges.
type SeedSettingsHandler struct {
	settings commercesettings.Repository
	now      func() time.Time
}

// NewSeedSettingsHandler wires the handler.
func NewSeedSettingsHandler(settings commercesettings.Repository, now func() time.Time) SeedSettingsHandler {
	if settings == nil {
		panic("command: NewSeedSettingsHandler settings required")
	}
	if now == nil {
		now = time.Now
	}
	return SeedSettingsHandler{settings: settings, now: now}
}

// Handle stores version 1 once; the policy_changed event rides the same
// transaction, so Orders and Inventory learn it like any other version.
func (h SeedSettingsHandler) Handle(ctx context.Context, cmd SeedSettingsCommand) (SeedSettingsResult, error) {
	if cmd.TenantID == "" {
		return SeedSettingsResult{}, errors.New("seed company settings: tenant id required")
	}
	var out SeedSettingsResult
	err := h.settings.UpdateByID(ctx, cmd.TenantID, func(s *commercesettings.Settings) (bool, error) {
		if err := s.Seed(h.now()); err != nil {
			if errors.Is(err, commercesettings.ErrAlreadySeeded) {
				out.AlreadyExisted = true
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
	if err != nil {
		return SeedSettingsResult{}, fmt.Errorf("seed company settings: %w", err)
	}
	return out, nil
}
