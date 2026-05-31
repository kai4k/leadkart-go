package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
	"github.com/leadkart/leadkart-go/internal/platform/domain/verificationcall"
)

// LogVerificationCallCommand is the Lead Agent "I just attempted a
// call" input.
type LogVerificationCallCommand struct {
	ContactID             unverifiedcontact.ID
	Outcome               verificationcall.OutcomeCode
	Notes                 string
	CallbackWindowStartAt time.Time
	CallbackWindowEndAt   time.Time
	LoggedBy              unverifiedcontact.MembershipID
}

// LogVerificationCallResult holds the new call-log row's ID.
type LogVerificationCallResult struct {
	CallID verificationcall.ID
}

// ErrContactNotFound signals an unknown cmd.ContactID; HTTP maps to 404.
var ErrContactNotFound = errors.New("log verification call: contact not found")

// LogVerificationCallHandler appends a call-log row and drives the
// contact state machine when the outcome implies a transition (Busy →
// MarkBusy). Terminal transitions do NOT auto-fire from outcome: verify
// and reject have dedicated handlers/endpoints (Slice 1: log-only).
type LogVerificationCallHandler struct {
	uow       pg.UnitOfWork
	calls     verificationcall.Repository
	contacts  unverifiedcontact.Repository
	now       func() time.Time
	newCallID func() verificationcall.ID
}

// NewLogVerificationCallHandler wires the handler.
//
// newCallID is injected per TestArch_HandlersInjectIDFactory; tests
// inject a deterministic counter for pinnable IDs.
func NewLogVerificationCallHandler(
	uow pg.UnitOfWork,
	calls verificationcall.Repository,
	contacts unverifiedcontact.Repository,
	now func() time.Time,
	newCallID func() verificationcall.ID,
) LogVerificationCallHandler {
	if newCallID == nil {
		panic("command: NewLogVerificationCallHandler newCallID required")
	}
	if now == nil {
		now = time.Now
	}
	return LogVerificationCallHandler{
		uow: uow, calls: calls, contacts: contacts, now: now, newCallID: newCallID,
	}
}

// Handle persists the call and transitions the contact when the outcome
// implies one:
//   - first call on a New contact → InCall (so verify/reject guards pass)
//   - Busy → MarkBusy with the callback window
//   - all other outcomes leave state alone
func (h LogVerificationCallHandler) Handle(
	ctx context.Context,
	cmd LogVerificationCallCommand,
) (LogVerificationCallResult, error) {
	callID := h.newCallID()
	now := h.now()

	var out LogVerificationCallResult
	err := h.uow.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context) error {
		err := h.contacts.UpdateByID(ctx, cmd.ContactID, func(c *unverifiedcontact.UnverifiedContact) (bool, error) {
			// Move to InCall so the log row's "on the phone now"
			// semantic matches state. StartCall is idempotent.
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
