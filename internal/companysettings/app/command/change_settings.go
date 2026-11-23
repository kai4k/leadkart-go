// Package command holds companysettings CQRS command handlers.
//
// Each handler is a concrete struct with a single Handle method; handlers
// aggregate as fields on the [app.Application] facade. Boundary discipline
// (ADR 0047): handlers depend on the domain repository INTERFACE only.
package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/companysettings/domain/commercesettings"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ChangeSettingsCommand is one PATCH of the commerce policy.
type ChangeSettingsCommand struct {
	TenantID tenant.ID
	// ExpectedVersion is the version the caller form was loaded at.
	ExpectedVersion int64
	Changes         commercepolicy.Changes
	Reason          string

	ActorMembershipID      membership.ID
	ImpersonatorOperatorID string
	// MayManage and MayManageTax are the caller permissions, decided by the
	// route; the aggregate enforces them per changed key.
	MayManage    bool
	MayManageTax bool
}

// SettingsResult is the policy after a command: the response IS the refetch
// (ADR 0038), so a caller never reads again to learn the new version.
type SettingsResult struct {
	Version   int64
	Policy    commercepolicy.Policy
	UpdatedAt time.Time
	UpdatedBy membership.ID
	// Changed is false when the request matched the current policy and the
	// version did not move.
	Changed bool
}

// ChangeSettingsHandler applies a patch under the tenant's policy row lock.
type ChangeSettingsHandler struct {
	settings commercesettings.Repository
	now      func() time.Time
}

// NewChangeSettingsHandler wires the handler.
func NewChangeSettingsHandler(settings commercesettings.Repository, now func() time.Time) ChangeSettingsHandler {
	if settings == nil {
		panic("command: NewChangeSettingsHandler settings required")
	}
	if now == nil {
		now = time.Now
	}
	return ChangeSettingsHandler{settings: settings, now: now}
}

// Handle runs the change. A stale ExpectedVersion is
// [commercesettings.ErrVersionConflict]; a refused value is a
// commercepolicy.FieldErrors error (see commercepolicy.Fields).
func (h ChangeSettingsHandler) Handle(ctx context.Context, cmd ChangeSettingsCommand) (SettingsResult, error) {
	if cmd.TenantID == "" {
		return SettingsResult{}, errors.New("change company settings: tenant id required")
	}
	var out SettingsResult
	err := h.settings.UpdateByID(ctx, cmd.TenantID, func(s *commercesettings.Settings) (bool, error) {
		res, err := s.Change(commercesettings.ChangeInput{
			ExpectedVersion: cmd.ExpectedVersion, Changes: cmd.Changes, Reason: cmd.Reason,
			Actor:     commercesettings.Actor{MembershipID: cmd.ActorMembershipID, ImpersonatorOperatorID: cmd.ImpersonatorOperatorID},
			MayManage: cmd.MayManage, MayManageTax: cmd.MayManageTax, Now: h.now(),
		})
		if err != nil {
			return false, err
		}
		out = resultOf(s, res.Changed)
		return res.Changed, nil
	})
	if err != nil {
		return SettingsResult{}, fmt.Errorf("change company settings: %w", err)
	}
	return out, nil
}

func resultOf(s *commercesettings.Settings, changed bool) SettingsResult {
	return SettingsResult{Version: s.Version(), Policy: s.Policy(), UpdatedAt: s.UpdatedAt(), UpdatedBy: s.UpdatedBy(), Changed: changed}
}
