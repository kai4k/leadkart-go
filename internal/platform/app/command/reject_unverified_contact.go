package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
)

// RejectUnverifiedContactCommand carries the input for the Lead Agent
// "this contact is unusable" terminal-reject use case.
type RejectUnverifiedContactCommand struct {
	ContactID  unverifiedcontact.ID
	Reason     string
	RejectedBy unverifiedcontact.MembershipID
}

// RejectUnverifiedContactHandler runs the terminal-reject transition.
type RejectUnverifiedContactHandler struct {
	contacts unverifiedcontact.Repository
	now      func() time.Time
}

// NewRejectUnverifiedContactHandler wires the handler.
func NewRejectUnverifiedContactHandler(
	contacts unverifiedcontact.Repository,
	now func() time.Time,
) RejectUnverifiedContactHandler {
	if now == nil {
		now = time.Now
	}
	return RejectUnverifiedContactHandler{contacts: contacts, now: now}
}

// Handle persists the rejected transition. UpdateByID drains the
// RejectedEvent to the outbox in the same tx.
func (h RejectUnverifiedContactHandler) Handle(
	ctx context.Context,
	cmd RejectUnverifiedContactCommand,
) error {
	now := h.now()
	err := h.contacts.UpdateByID(ctx, cmd.ContactID, func(c *unverifiedcontact.UnverifiedContact) (bool, error) {
		// Promote from New if needed — the reject endpoint is a quick
		// terminal path that doesn't always run a call-log step first
		// (operator decides "this contact is clearly unusable" from
		// the form alone, e.g. obvious test data).
		if c.State() == unverifiedcontact.StateNew {
			if err := c.StartCall(now); err != nil {
				return false, err
			}
		}
		return true, c.MarkRejected(cmd.Reason, cmd.RejectedBy, now)
	})
	if err != nil {
		if errors.Is(err, unverifiedcontact.ErrNotFound) {
			return ErrContactNotFound
		}
		return fmt.Errorf("reject unverified contact: %w", err)
	}
	return nil
}
