package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
)

// RejectUnverifiedContactCommand is the Lead Agent "this contact is
// unusable" terminal-reject input.
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

// Handle drives the Reject transition; UpdateByID drains the
// RejectedEvent to the outbox in the same tx.
func (h RejectUnverifiedContactHandler) Handle(
	ctx context.Context,
	cmd RejectUnverifiedContactCommand,
) error {
	now := h.now()
	err := h.contacts.UpdateByID(ctx, cmd.ContactID, func(c *unverifiedcontact.UnverifiedContact) (bool, error) {
		// Promote from New: reject can fire without a prior call-log
		// step when the form alone is clearly unusable (e.g. test data).
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
