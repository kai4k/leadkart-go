package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
	"github.com/leadkart/leadkart-go/internal/platform/domain/verificationcall"
)

// LogVerificationCallCommand carries the validated input for the Lead
// Agent "I just attempted a call" use case.
type LogVerificationCallCommand struct {
	ContactID              unverifiedcontact.ID
	Outcome                verificationcall.OutcomeCode
	Notes                  string
	CallbackWindowStartAt  time.Time
	CallbackWindowEndAt    time.Time
	LoggedBy               unverifiedcontact.MembershipID
}

// LogVerificationCallResult holds the new call-log row's ID.
type LogVerificationCallResult struct {
	CallID verificationcall.ID
}

// ErrContactNotFound is returned when the cmd.ContactID doesn't exist.
// HTTP layer maps to 404.
var ErrContactNotFound = errors.New("log verification call: contact not found")

// LogVerificationCallHandler appends a call-log row AND drives the
// contact's state machine if the outcome carries a transition (Busy →
// MarkBusy; non-transition outcomes leave the contact state alone).
//
// Verify + Reject have dedicated handlers (one HTTP call per terminal
// transition, even though both also imply an underlying call). The
// frontend pattern: call this endpoint with outcome=verified|rejected
// ONLY to record the log row, then call POST /verify or POST /reject
// to drive the terminal transition. Slice 1 treats this endpoint as
// log-only — terminal transitions don't auto-fire from outcome.
type LogVerificationCallHandler struct {
	uow      pg.UnitOfWork
	calls    verificationcall.Repository
	contacts unverifiedcontact.Repository
	now      func() time.Time
}

// NewLogVerificationCallHandler wires the handler.
func NewLogVerificationCallHandler(
	uow pg.UnitOfWork,
	calls verificationcall.Repository,
	contacts unverifiedcontact.Repository,
	now func() time.Time,
) LogVerificationCallHandler {
	return LogVerificationCallHandler{uow: uow, calls: calls, contacts: contacts, now: now}
}

// Handle persists the call + transitions the contact's state when the
// outcome implies one:
//   - First call on a New contact → transition the contact to InCall
//     so the verify/reject paths' guard passes.
//   - Busy outcome → MarkBusy with the callback window.
//   - Verified / Rejected outcomes leave the contact alone (handled
//     by the dedicated verify/reject endpoints).
//   - NoAnswer / WrongNumber outcomes also leave state alone.
func (h LogVerificationCallHandler) Handle(
	ctx context.Context,
	cmd LogVerificationCallCommand,
) (LogVerificationCallResult, error) {
	callID := verificationcall.ID(ids.NewV7().String())
	now := h.now()

	var out LogVerificationCallResult
	err := h.uow.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context) error {
		// Step 1: drive the contact's state machine if the outcome
		// implies a transition. UpdateByID loads + persists +
		// drains events in the surrounding tx.
		err := h.contacts.UpdateByID(ctx, cmd.ContactID, func(c *unverifiedcontact.UnverifiedContact) (bool, error) {
			// Ensure the contact is InCall so the call-log row's
			// "Lead Agent on the phone now" semantic matches the
			// state machine. If already InCall (re-attempt after
			// Busy or repeat call) this is a no-op via the
			// aggregate's idempotent StartCall.
			if c.State() == unverifiedcontact.StateNew || c.State() == unverifiedcontact.StateBusy {
				if err := c.StartCall(now); err != nil {
					return false, err
				}
			}
			if cmd.Outcome == verificationcall.OutcomeBusy {
				if err := c.MarkBusy(cmd.CallbackWindowStartAt, cmd.CallbackWindowEndAt, now); err != nil {
					return false, err
				}
			}
			return true, nil
		})
		if err != nil {
			if errors.Is(err, unverifiedcontact.ErrNotFound) {
				return ErrContactNotFound
			}
			return fmt.Errorf("update contact state: %w", err)
		}

		// Step 2: append the call-log row.
		call, err := verificationcall.New(
			callID, cmd.ContactID, cmd.Outcome, cmd.Notes,
			cmd.CallbackWindowStartAt, cmd.CallbackWindowEndAt,
			cmd.LoggedBy, now,
		)
		if err != nil {
			return fmt.Errorf("construct call: %w", err)
		}
		if err := h.calls.Add(ctx, call); err != nil {
			return fmt.Errorf("persist call: %w", err)
		}
		out = LogVerificationCallResult{CallID: callID}
		return nil
	})
	if err != nil {
		return LogVerificationCallResult{}, err
	}
	return out, nil
}
