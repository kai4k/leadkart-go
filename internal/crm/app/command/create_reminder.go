package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ErrReminderAlreadyExists surfaces when the partial unique index on
// callback / mature-lead pending reminders fires. The HTTP surface maps
// this to 409 Conflict; the subscriber + cron paths swallow it (the
// pending row they wanted already exists, so the post-condition holds).
var ErrReminderAlreadyExists = errors.New("crm: reminder already exists")

// ErrReminderNotFound surfaces when the reminder ID does not exist in
// the caller's tenant scope (RLS-filtered).
var ErrReminderNotFound = errors.New("crm: reminder not found")

// ErrReminderTerminal surfaces when a mutating command targets a
// terminal-state reminder (sent / cancelled).
var ErrReminderTerminal = errors.New("crm: reminder is in a terminal state")

// CreateReminderCommand carries a reminder creation request from the
// HTTP surface, the CallLogged subscriber, or the mature-lead scheduler.
//
// TenantID is the caller's tenant scope (TDL canon per ADR 0062 — flows
// through as an explicit value, not via ctx-tenancy).
//
// Type selects which domain factory runs:
//   - [reminder.TypeCallback]   — SourceCallLogID REQUIRED (UUID).
//   - [reminder.TypeMatureLead] — SourceCallLogID + CreatedBy left empty.
//   - [reminder.TypeManual]     — CreatedByMembershipID REQUIRED.
type CreateReminderCommand struct {
	TenantID               tenant.ID
	LeadID                 crmlead.ID
	AssignedToMembershipID string
	CreatedByMembershipID  string
	SourceCallLogID        string
	Type                   reminder.Type
	DueAt                  time.Time
	Notes                  string
}

// CreateReminderResult returns the new reminder ID + an
// `AlreadyExisted` flag the subscriber / cron callers use to short-
// circuit the duplicate path without retry.
type CreateReminderResult struct {
	ReminderID     reminder.ID
	AlreadyExisted bool
}

// CreateReminderHandler runs the reminder-creation flow. Verifies the
// parent lead exists in the caller's tenant before constructing the
// aggregate (so a typo'd lead_id surfaces as 404 not a hard FK error).
type CreateReminderHandler struct {
	leads         crmlead.Repository
	reminders     reminder.Repository
	now           func() time.Time
	newReminderID func() reminder.ID
}

// NewCreateReminderHandler wires the handler.
//
// newReminderID is the reminder ID factory per the
// `TestArch_HandlersInjectIDFactory` discipline. Production passes
// `func() reminder.ID { return reminder.ID(ids.NewV7().String()) }`;
// tests inject a deterministic counter.
func NewCreateReminderHandler(
	leads crmlead.Repository,
	reminders reminder.Repository,
	now func() time.Time,
	newReminderID func() reminder.ID,
) CreateReminderHandler {
	if leads == nil {
		panic("command: NewCreateReminderHandler leads repository required")
	}
	if reminders == nil {
		panic("command: NewCreateReminderHandler reminders repository required")
	}
	if newReminderID == nil {
		panic("command: NewCreateReminderHandler newReminderID required")
	}
	if now == nil {
		now = time.Now
	}
	return CreateReminderHandler{
		leads:         leads,
		reminders:     reminders,
		now:           now,
		newReminderID: newReminderID,
	}
}

// Handle persists the reminder + emits the V1 event via the repository's
// outbox drain.
//
// On a partial-unique-index collision the handler returns
// (result with AlreadyExisted=true + the EXISTING reminder's ID when
// resolvable, or a zero ID + AlreadyExisted=true otherwise) + a nil
// error — the subscriber / cron caller ACKs the duplicate.
func (h CreateReminderHandler) Handle(ctx context.Context, cmd CreateReminderCommand) (CreateReminderResult, error) {
	if cmd.TenantID.IsZero() {
		return CreateReminderResult{}, errors.New("crm create_reminder: tenant id required")
	}
	if cmd.LeadID.IsZero() {
		return CreateReminderResult{}, errors.New("crm create_reminder: lead id required")
	}
	if cmd.AssignedToMembershipID == "" {
		return CreateReminderResult{}, errors.New("crm create_reminder: assigned-to membership id required")
	}
	if !cmd.Type.IsValid() {
		return CreateReminderResult{}, fmt.Errorf("crm create_reminder: type %q invalid", cmd.Type)
	}
	if cmd.DueAt.IsZero() {
		return CreateReminderResult{}, errors.New("crm create_reminder: due_at required")
	}

	// Guard against typo'd lead_id at the app layer — surfaces as 404
	// instead of as a hard FK error from the partial-unique-index path.
	if _, err := h.leads.GetByID(ctx, cmd.TenantID, cmd.LeadID); err != nil {
		if errors.Is(err, crmlead.ErrNotFound) {
			return CreateReminderResult{}, ErrLeadNotFound
		}
		return CreateReminderResult{}, fmt.Errorf("crm create_reminder: load lead: %w", err)
	}

	now := h.now()
	id := h.newReminderID()
	r, err := buildReminder(id, cmd, now)
	if err != nil {
		return CreateReminderResult{}, fmt.Errorf("crm create_reminder: factory: %w", err)
	}
	if err := h.reminders.Add(ctx, r); err != nil {
		if errors.Is(err, reminder.ErrAlreadyExists) {
			// For mature-lead the scheduler can probe the existing row;
			// for callback the subscriber needs neither — the partial
			// unique guarantees AT MOST ONE pending row, so the
			// already-existed branch is the success path.
			return CreateReminderResult{AlreadyExisted: true}, nil
		}
		return CreateReminderResult{}, fmt.Errorf("crm create_reminder: persist: %w", err)
	}
	return CreateReminderResult{ReminderID: r.ID()}, nil
}

func buildReminder(id reminder.ID, cmd CreateReminderCommand, now time.Time) (*reminder.Reminder, error) {
	switch cmd.Type {
	case reminder.TypeCallback:
		return reminder.NewCallbackReminder(
			id, cmd.TenantID, cmd.LeadID,
			cmd.AssignedToMembershipID, cmd.CreatedByMembershipID,
			cmd.SourceCallLogID, cmd.DueAt, cmd.Notes, now,
		)
	case reminder.TypeMatureLead:
		return reminder.NewMatureLeadReminder(
			id, cmd.TenantID, cmd.LeadID,
			cmd.AssignedToMembershipID, cmd.DueAt, cmd.Notes, now,
		)
	case reminder.TypeManual:
		return reminder.NewManualReminder(
			id, cmd.TenantID, cmd.LeadID,
			cmd.AssignedToMembershipID, cmd.CreatedByMembershipID,
			cmd.DueAt, cmd.Notes, now,
		)
	default:
		return nil, fmt.Errorf("crm create_reminder: unhandled type %q", cmd.Type)
	}
}
