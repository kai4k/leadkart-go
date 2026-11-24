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

// ScheduleUsageReader is what Inventory knows about drug schedule use. This
// module cannot read Inventory's schema, so the composition root binds this
// interface to an Inventory query (the tasks hierarchy bridge precedent).
type ScheduleUsageReader interface {
	// ProductsWithSchedule counts the company's products that carry any
	// drug schedule class.
	ProductsWithSchedule(ctx context.Context, tenantID tenant.ID) (int64, error)
}

// ScheduleClassResult is the class list after a command and the policy version
// it moved to.
type ScheduleClassResult struct {
	Items         []commercepolicy.ScheduleClass
	PolicyVersion int64
	// Class is the class the command acted on (zero after a removal).
	Class commercepolicy.ScheduleClass
}

func scheduleResult(s *commercesettings.Settings, classID string) ScheduleClassResult {
	p := s.Policy()
	res := ScheduleClassResult{Items: p.DrugSchedule.Classes, PolicyVersion: s.Version()}
	if c, ok := p.DrugSchedule.ScheduleClassByID(classID); ok {
		res.Class = c
	}
	return res
}

// AddScheduleClassCommand adds a class to the company's list.
type AddScheduleClassCommand struct {
	TenantID tenant.ID
	Label    string
	Reason   string

	ActorMembershipID      membership.ID
	ImpersonatorOperatorID string
}

// AddScheduleClassHandler adds a class under the tenant's policy row lock.
type AddScheduleClassHandler struct {
	settings commercesettings.Repository
	now      func() time.Time
	newID    func() string
}

// NewAddScheduleClassHandler wires the handler.
func NewAddScheduleClassHandler(settings commercesettings.Repository, now func() time.Time, newID func() string) AddScheduleClassHandler {
	if settings == nil || newID == nil {
		panic("command: NewAddScheduleClassHandler settings and newID required")
	}
	if now == nil {
		now = time.Now
	}
	return AddScheduleClassHandler{settings: settings, now: now, newID: newID}
}

// Handle adds the class; the same label (any case) is refused as `duplicate`.
func (h AddScheduleClassHandler) Handle(ctx context.Context, cmd AddScheduleClassCommand) (ScheduleClassResult, error) {
	if cmd.TenantID == "" {
		return ScheduleClassResult{}, errors.New("add drug schedule class: tenant id required")
	}
	id := h.newID()
	var out ScheduleClassResult
	err := h.settings.UpdateByID(ctx, cmd.TenantID, func(s *commercesettings.Settings) (bool, error) {
		if err := s.AddScheduleClass(commercesettings.AddScheduleClassInput{
			ID: id, Label: cmd.Label, Reason: cmd.Reason,
			Actor: commercesettings.Actor{MembershipID: cmd.ActorMembershipID, ImpersonatorOperatorID: cmd.ImpersonatorOperatorID},
			Now:   h.now(),
		}); err != nil {
			return false, err
		}
		out = scheduleResult(s, id)
		return true, nil
	})
	if err != nil {
		return ScheduleClassResult{}, fmt.Errorf("add drug schedule class: %w", err)
	}
	return out, nil
}

// RenameScheduleClassCommand renames a class.
type RenameScheduleClassCommand struct {
	TenantID tenant.ID
	ClassID  string
	Label    string
	Reason   string

	ActorMembershipID      membership.ID
	ImpersonatorOperatorID string
}

// RenameScheduleClassHandler renames a class under the tenant's policy row lock.
type RenameScheduleClassHandler struct {
	settings commercesettings.Repository
	now      func() time.Time
}

// NewRenameScheduleClassHandler wires the handler.
func NewRenameScheduleClassHandler(settings commercesettings.Repository, now func() time.Time) RenameScheduleClassHandler {
	if settings == nil {
		panic("command: NewRenameScheduleClassHandler settings required")
	}
	if now == nil {
		now = time.Now
	}
	return RenameScheduleClassHandler{settings: settings, now: now}
}

// Handle renames the class; an unknown id is [commercesettings.ErrScheduleClassNotFound].
func (h RenameScheduleClassHandler) Handle(ctx context.Context, cmd RenameScheduleClassCommand) (ScheduleClassResult, error) {
	if cmd.TenantID == "" {
		return ScheduleClassResult{}, errors.New("rename drug schedule class: tenant id required")
	}
	var out ScheduleClassResult
	err := h.settings.UpdateByID(ctx, cmd.TenantID, func(s *commercesettings.Settings) (bool, error) {
		before := s.Version()
		if err := s.RenameScheduleClass(commercesettings.RenameScheduleClassInput{
			ID: cmd.ClassID, Label: cmd.Label, Reason: cmd.Reason,
			Actor: commercesettings.Actor{MembershipID: cmd.ActorMembershipID, ImpersonatorOperatorID: cmd.ImpersonatorOperatorID},
			Now:   h.now(),
		}); err != nil {
			return false, err
		}
		out = scheduleResult(s, cmd.ClassID)
		return s.Version() != before, nil
	})
	if err != nil {
		return ScheduleClassResult{}, fmt.Errorf("rename drug schedule class: %w", err)
	}
	return out, nil
}

// ChangeScheduleClassStatusCommand moves a class between active and inactive. A class is never
// deleted (ADR 0073).
type ChangeScheduleClassStatusCommand struct {
	TenantID tenant.ID
	ClassID  string
	Reason   string

	ActorMembershipID      membership.ID
	ImpersonatorOperatorID string
}

// DeactivateScheduleClassHandler makes a class inactive under the tenant's policy row lock.
type DeactivateScheduleClassHandler struct {
	settings commercesettings.Repository
	now      func() time.Time
}

// NewDeactivateScheduleClassHandler wires the handler.
func NewDeactivateScheduleClassHandler(settings commercesettings.Repository, now func() time.Time) DeactivateScheduleClassHandler {
	if settings == nil {
		panic("command: NewDeactivateScheduleClassHandler settings required")
	}
	if now == nil {
		now = time.Now
	}
	return DeactivateScheduleClassHandler{settings: settings, now: now}
}

// Handle deactivates the class; an unknown id is [commercesettings.ErrScheduleClassNotFound]. Repeating
// it changes nothing (no new policy version).
func (h DeactivateScheduleClassHandler) Handle(ctx context.Context, cmd ChangeScheduleClassStatusCommand) (ScheduleClassResult, error) {
	return changeScheduleClassStatus(ctx, h.settings, h.now, cmd, false)
}

// ReactivateScheduleClassHandler makes an inactive class selectable again.
type ReactivateScheduleClassHandler struct {
	settings commercesettings.Repository
	now      func() time.Time
}

// NewReactivateScheduleClassHandler wires the handler.
func NewReactivateScheduleClassHandler(settings commercesettings.Repository, now func() time.Time) ReactivateScheduleClassHandler {
	if settings == nil {
		panic("command: NewReactivateScheduleClassHandler settings required")
	}
	if now == nil {
		now = time.Now
	}
	return ReactivateScheduleClassHandler{settings: settings, now: now}
}

// Handle reactivates the class. Repeating it changes nothing.
func (h ReactivateScheduleClassHandler) Handle(ctx context.Context, cmd ChangeScheduleClassStatusCommand) (ScheduleClassResult, error) {
	return changeScheduleClassStatus(ctx, h.settings, h.now, cmd, true)
}

func changeScheduleClassStatus(ctx context.Context, settings commercesettings.Repository, now func() time.Time, cmd ChangeScheduleClassStatusCommand, active bool) (ScheduleClassResult, error) {
	what := "deactivate drug schedule class"
	if active {
		what = "reactivate drug schedule class"
	}
	if cmd.TenantID == "" {
		return ScheduleClassResult{}, fmt.Errorf("%s: tenant id required", what)
	}
	var out ScheduleClassResult
	err := settings.UpdateByID(ctx, cmd.TenantID, func(s *commercesettings.Settings) (bool, error) {
		before := s.Version()
		in := commercesettings.SetScheduleClassActiveInput{
			ID: cmd.ClassID, Reason: cmd.Reason,
			Actor: commercesettings.Actor{MembershipID: cmd.ActorMembershipID, ImpersonatorOperatorID: cmd.ImpersonatorOperatorID},
			Now:   now(),
		}
		var err error
		if active {
			err = s.ReactivateScheduleClass(in)
		} else {
			err = s.DeactivateScheduleClass(in)
		}
		if err != nil {
			return false, err
		}
		out = scheduleResult(s, cmd.ClassID)
		return s.Version() != before, nil
	})
	if err != nil {
		return ScheduleClassResult{}, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}
