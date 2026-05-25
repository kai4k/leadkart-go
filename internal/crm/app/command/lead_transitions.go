package command

import (
	"context"
	"errors"
	"time"

	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
)

// ----- ChangeLeadStage ------------------------------------------------------

// ChangeLeadStageCommand carries a stage-advance request. The state
// machine in crmlead/stage.go enforces the allowed transitions; the
// handler maps domain errors to the typed [ErrLeadNotFound] /
// [ErrLeadTerminal] / generic invalid sentinels.
type ChangeLeadStageCommand struct {
	LeadID                crmlead.ID
	NewStage              crmlead.Stage
	ChangedByMembershipID string
	Reason                string
}

// ChangeLeadStageHandler runs the stage-advance flow.
type ChangeLeadStageHandler struct {
	leads crmlead.Repository
	now   func() time.Time
}

// NewChangeLeadStageHandler wires the handler. `now` is the injected
// wall-clock (Pure Domain canon — ADR 0047); nil → time.Now.
func NewChangeLeadStageHandler(leads crmlead.Repository, now func() time.Time) ChangeLeadStageHandler {
	if leads == nil {
		panic("command: NewChangeLeadStageHandler leads repository required")
	}
	if now == nil {
		now = time.Now
	}
	return ChangeLeadStageHandler{leads: leads, now: now}
}

// Handle advances the stage. Returns [ErrLeadNotFound] / [ErrLeadTerminal]
// / [crmlead.ErrInvalid] on the respective failures.
func (h ChangeLeadStageHandler) Handle(ctx context.Context, cmd ChangeLeadStageCommand) error {
	if cmd.LeadID.IsZero() {
		return errors.New("crm change_stage: lead id required")
	}
	if cmd.ChangedByMembershipID == "" {
		return errors.New("crm change_stage: changed-by membership id required")
	}
	now := h.now()
	err := h.leads.UpdateByID(ctx, cmd.LeadID, func(l *crmlead.CrmLead) (bool, error) {
		oldStage := l.Stage()
		if err := l.ChangeStage(cmd.NewStage, cmd.ChangedByMembershipID, cmd.Reason, now); err != nil {
			return false, err
		}
		// no-op transitions (same stage) → no persist needed.
		if l.Stage() == oldStage {
			return false, nil
		}
		return true, nil
	})
	return mapLeadError(err)
}

// ----- ChangeLeadTemperature -----------------------------------------------

// ChangeLeadTemperatureCommand carries a temperature-update request.
type ChangeLeadTemperatureCommand struct {
	LeadID                crmlead.ID
	NewTemperature        crmlead.Temperature
	ChangedByMembershipID string
}

// ChangeLeadTemperatureHandler runs the temperature-change flow.
type ChangeLeadTemperatureHandler struct {
	leads crmlead.Repository
	now   func() time.Time
}

// NewChangeLeadTemperatureHandler wires the handler. `now` is the
// injected wall-clock (Pure Domain canon — ADR 0047); nil → time.Now.
func NewChangeLeadTemperatureHandler(leads crmlead.Repository, now func() time.Time) ChangeLeadTemperatureHandler {
	if leads == nil {
		panic("command: NewChangeLeadTemperatureHandler leads repository required")
	}
	if now == nil {
		now = time.Now
	}
	return ChangeLeadTemperatureHandler{leads: leads, now: now}
}

// Handle changes the temperature axis.
func (h ChangeLeadTemperatureHandler) Handle(ctx context.Context, cmd ChangeLeadTemperatureCommand) error {
	if cmd.LeadID.IsZero() {
		return errors.New("crm change_temperature: lead id required")
	}
	if cmd.ChangedByMembershipID == "" {
		return errors.New("crm change_temperature: changed-by membership id required")
	}
	now := h.now()
	err := h.leads.UpdateByID(ctx, cmd.LeadID, func(l *crmlead.CrmLead) (bool, error) {
		oldTemp := l.Temperature()
		if err := l.ChangeTemperature(cmd.NewTemperature, cmd.ChangedByMembershipID, now); err != nil {
			return false, err
		}
		if l.Temperature() == oldTemp {
			return false, nil
		}
		return true, nil
	})
	return mapLeadError(err)
}

// ----- ConvertLead ---------------------------------------------------------

// ConvertLeadCommand is the terminal-success transition request.
type ConvertLeadCommand struct {
	LeadID                  crmlead.ID
	ConvertedByMembershipID string
}

// ConvertLeadHandler runs the convert flow.
type ConvertLeadHandler struct {
	leads crmlead.Repository
	now   func() time.Time
}

// NewConvertLeadHandler wires the handler. `now` is the injected
// wall-clock (Pure Domain canon — ADR 0047); nil → time.Now.
func NewConvertLeadHandler(leads crmlead.Repository, now func() time.Time) ConvertLeadHandler {
	if leads == nil {
		panic("command: NewConvertLeadHandler leads repository required")
	}
	if now == nil {
		now = time.Now
	}
	return ConvertLeadHandler{leads: leads, now: now}
}

// Handle terminally converts the lead.
func (h ConvertLeadHandler) Handle(ctx context.Context, cmd ConvertLeadCommand) error {
	if cmd.LeadID.IsZero() {
		return errors.New("crm convert: lead id required")
	}
	if cmd.ConvertedByMembershipID == "" {
		return errors.New("crm convert: converted-by membership id required")
	}
	now := h.now()
	err := h.leads.UpdateByID(ctx, cmd.LeadID, func(l *crmlead.CrmLead) (bool, error) {
		if err := l.Convert(cmd.ConvertedByMembershipID, now); err != nil {
			return false, err
		}
		return true, nil
	})
	return mapLeadError(err)
}

// ----- LoseLead ------------------------------------------------------------

// LoseLeadCommand is the terminal-failure transition request.
type LoseLeadCommand struct {
	LeadID             crmlead.ID
	LostByMembershipID string
	Reason             string
}

// LoseLeadHandler runs the lose flow.
type LoseLeadHandler struct {
	leads crmlead.Repository
	now   func() time.Time
}

// NewLoseLeadHandler wires the handler. `now` is the injected
// wall-clock (Pure Domain canon — ADR 0047); nil → time.Now.
func NewLoseLeadHandler(leads crmlead.Repository, now func() time.Time) LoseLeadHandler {
	if leads == nil {
		panic("command: NewLoseLeadHandler leads repository required")
	}
	if now == nil {
		now = time.Now
	}
	return LoseLeadHandler{leads: leads, now: now}
}

// Handle terminally loses the lead. Reason is required per audit
// doctrine.
func (h LoseLeadHandler) Handle(ctx context.Context, cmd LoseLeadCommand) error {
	if cmd.LeadID.IsZero() {
		return errors.New("crm lose: lead id required")
	}
	if cmd.LostByMembershipID == "" {
		return errors.New("crm lose: lost-by membership id required")
	}
	if cmd.Reason == "" {
		return errors.New("crm lose: reason required")
	}
	now := h.now()
	err := h.leads.UpdateByID(ctx, cmd.LeadID, func(l *crmlead.CrmLead) (bool, error) {
		if err := l.Lose(cmd.LostByMembershipID, cmd.Reason, now); err != nil {
			return false, err
		}
		return true, nil
	})
	return mapLeadError(err)
}

// mapLeadError collapses the common domain errors to the app-layer
// sentinels HTTP handlers branch on.
func mapLeadError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, crmlead.ErrNotFound):
		return ErrLeadNotFound
	case errors.Is(err, crmlead.ErrTerminal):
		return ErrLeadTerminal
	default:
		return err
	}
}
