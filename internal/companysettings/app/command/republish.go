package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/companysettings/domain/commercesettings"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// RepublishCommand re-announces one tenant's current stored policy.
type RepublishCommand struct {
	TenantID tenant.ID
}

// RepublishHandler is the anti-entropy step of the daily job: it re-sends the
// current version so a replica that missed an event (a dead letter) converges.
type RepublishHandler struct {
	settings commercesettings.Repository
	now      func() time.Time
}

// NewRepublishHandler wires the handler.
func NewRepublishHandler(settings commercesettings.Repository, now func() time.Time) RepublishHandler {
	if settings == nil {
		panic("command: NewRepublishHandler settings required")
	}
	if now == nil {
		now = time.Now
	}
	return RepublishHandler{settings: settings, now: now}
}

// Handle emits the current version for the tenant. A tenant with nothing
// stored has nothing to republish and is skipped without error.
func (h RepublishHandler) Handle(ctx context.Context, cmd RepublishCommand) error {
	if cmd.TenantID == "" {
		return errors.New("republish company settings: tenant id required")
	}
	err := h.settings.UpdateByID(ctx, cmd.TenantID, func(s *commercesettings.Settings) (bool, error) {
		if err := s.Republish(h.now()); err != nil {
			if errors.Is(err, commercesettings.ErrNothingToRepublish) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("republish company settings: %w", err)
	}
	return nil
}
