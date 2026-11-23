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

// AddGSTRateCommand adds a confirmed rate to the company's list.
type AddGSTRateCommand struct {
	TenantID    tenant.ID
	RateBps     int
	Label       string
	ConfirmedBy string
	ConfirmedOn string
	Reason      string

	ActorMembershipID      membership.ID
	ImpersonatorOperatorID string
}

// GSTRateResult is the rate after a command, with the policy version it moved to.
type GSTRateResult struct {
	Rate          commercepolicy.GSTRate
	PolicyVersion int64
}

// AddGSTRateHandler adds a rate under the tenant's policy row lock.
type AddGSTRateHandler struct {
	settings commercesettings.Repository
	now      func() time.Time
	newID    func() string
}

// NewAddGSTRateHandler wires the handler.
func NewAddGSTRateHandler(settings commercesettings.Repository, now func() time.Time, newID func() string) AddGSTRateHandler {
	if settings == nil || newID == nil {
		panic("command: NewAddGSTRateHandler settings and newID required")
	}
	if now == nil {
		now = time.Now
	}
	return AddGSTRateHandler{settings: settings, now: now, newID: newID}
}

// Handle adds the rate. The same rate under the same label is refused with a
// `duplicate` field code.
func (h AddGSTRateHandler) Handle(ctx context.Context, cmd AddGSTRateCommand) (GSTRateResult, error) {
	if cmd.TenantID == "" {
		return GSTRateResult{}, errors.New("add gst rate: tenant id required")
	}
	id := h.newID()
	var out GSTRateResult
	err := h.settings.UpdateByID(ctx, cmd.TenantID, func(s *commercesettings.Settings) (bool, error) {
		if err := s.AddGSTRate(commercesettings.AddGSTRateInput{
			ID: id, RateBps: cmd.RateBps, Label: cmd.Label, ConfirmedBy: cmd.ConfirmedBy, ConfirmedOn: cmd.ConfirmedOn,
			Reason: cmd.Reason, Actor: commercesettings.Actor{MembershipID: cmd.ActorMembershipID, ImpersonatorOperatorID: cmd.ImpersonatorOperatorID},
			Now: h.now(),
		}); err != nil {
			return false, err
		}
		out = GSTRateResult{Rate: rateByID(s.Policy(), id), PolicyVersion: s.Version()}
		return true, nil
	})
	if err != nil {
		return GSTRateResult{}, fmt.Errorf("add gst rate: %w", err)
	}
	return out, nil
}

// UpdateGSTRateCommand edits or deactivates a rate. A nil field is left alone.
type UpdateGSTRateCommand struct {
	TenantID    tenant.ID
	RateID      string
	Label       *string
	ConfirmedBy *string
	ConfirmedOn *string
	Active      *bool
	Reason      string

	ActorMembershipID      membership.ID
	ImpersonatorOperatorID string
}

// UpdateGSTRateHandler edits a rate under the tenant's policy row lock.
type UpdateGSTRateHandler struct {
	settings commercesettings.Repository
	now      func() time.Time
}

// NewUpdateGSTRateHandler wires the handler.
func NewUpdateGSTRateHandler(settings commercesettings.Repository, now func() time.Time) UpdateGSTRateHandler {
	if settings == nil {
		panic("command: NewUpdateGSTRateHandler settings required")
	}
	if now == nil {
		now = time.Now
	}
	return UpdateGSTRateHandler{settings: settings, now: now}
}

// Handle edits the rate; an unknown id is [commercesettings.ErrGSTRateNotFound].
// An edit that changes nothing leaves the version where it was.
func (h UpdateGSTRateHandler) Handle(ctx context.Context, cmd UpdateGSTRateCommand) (GSTRateResult, error) {
	if cmd.TenantID == "" {
		return GSTRateResult{}, errors.New("update gst rate: tenant id required")
	}
	var out GSTRateResult
	err := h.settings.UpdateByID(ctx, cmd.TenantID, func(s *commercesettings.Settings) (bool, error) {
		before := s.Version()
		if err := s.UpdateGSTRate(commercesettings.UpdateGSTRateInput{
			ID: cmd.RateID, Label: cmd.Label, ConfirmedBy: cmd.ConfirmedBy, ConfirmedOn: cmd.ConfirmedOn, Active: cmd.Active,
			Reason: cmd.Reason, Actor: commercesettings.Actor{MembershipID: cmd.ActorMembershipID, ImpersonatorOperatorID: cmd.ImpersonatorOperatorID},
			Now: h.now(),
		}); err != nil {
			return false, err
		}
		out = GSTRateResult{Rate: rateByID(s.Policy(), cmd.RateID), PolicyVersion: s.Version()}
		return s.Version() != before, nil
	})
	if err != nil {
		return GSTRateResult{}, fmt.Errorf("update gst rate: %w", err)
	}
	return out, nil
}

func rateByID(p commercepolicy.Policy, id string) commercepolicy.GSTRate {
	for _, r := range p.Tax.GSTRates {
		if r.ID == id {
			return r
		}
	}
	return commercepolicy.GSTRate{}
}
