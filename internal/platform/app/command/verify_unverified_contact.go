package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/platform/domain/platformlead"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
	"github.com/leadkart/leadkart-go/internal/platform/integrationevents"
)

// ErrContactAlreadyTerminal signals the contact is already Verified or
// Rejected. The handler refuses rather than emit a second
// LeadVerifiedV1 / create a second PlatformLead; HTTP maps to 409.
var ErrContactAlreadyTerminal = errors.New("verify: contact already in terminal state")

// VerifyUnverifiedContactCommand is the Lead Agent "promote this contact
// to a marketplace PlatformLead" input.
type VerifyUnverifiedContactCommand struct {
	ContactID  unverifiedcontact.ID
	VerifiedBy unverifiedcontact.MembershipID
}

// VerifyUnverifiedContactResult holds the new PlatformLead's ID.
type VerifyUnverifiedContactResult struct {
	PlatformLeadID platformlead.ID
}

// VerifyUnverifiedContactHandler writes the contact and the new lead in
// one UoW tx, then emits LeadVerifiedV1 with the full snapshot from the
// contact's form VO. The mechanical mapper suppresses the platformlead
// VerifiedEvent so this handler drives the wire shape.
type VerifyUnverifiedContactHandler struct {
	uow       pg.UnitOfWork
	contacts  unverifiedcontact.Repository
	leads     platformlead.Repository
	outboxEnq OutboxEnqueuer
	now       func() time.Time
	newLeadID func() platformlead.ID
}

// NewVerifyUnverifiedContactHandler wires the handler.
//
// newLeadID is injected per TestArch_HandlersInjectIDFactory; tests
// inject a deterministic counter for pinnable IDs.
func NewVerifyUnverifiedContactHandler(
	uow pg.UnitOfWork,
	contacts unverifiedcontact.Repository,
	leads platformlead.Repository,
	outboxEnq OutboxEnqueuer,
	now func() time.Time,
	newLeadID func() platformlead.ID,
) VerifyUnverifiedContactHandler {
	if newLeadID == nil {
		panic("command: NewVerifyUnverifiedContactHandler newLeadID required")
	}
	if now == nil {
		now = time.Now
	}
	return VerifyUnverifiedContactHandler{
		uow: uow, contacts: contacts, leads: leads, outboxEnq: outboxEnq,
		now: now, newLeadID: newLeadID,
	}
}

// Handle promotes a contact to a PlatformLead in one tx: guard against
// double-verify, transition the contact, build the lead from its form,
// and enqueue LeadVerifiedV1 with the snapshot.
func (h VerifyUnverifiedContactHandler) Handle(
	ctx context.Context,
	cmd VerifyUnverifiedContactCommand,
) (VerifyUnverifiedContactResult, error) {
	if cmd.VerifiedBy.IsZero() {
		return VerifyUnverifiedContactResult{}, errors.New("verify: verifiedBy required")
	}

	leadID := h.newLeadID()
	now := h.now()

	err := h.uow.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context) error {
		// Guard against double-verify / verify-after-reject before
		// minting a fresh leadID + second LeadVerifiedV1 (which would
		// create a phantom marketplace row). H11 — review-pass.
		existing, err := h.contacts.GetByID(ctx, cmd.ContactID)
		if err != nil {
			if errors.Is(err, unverifiedcontact.ErrNotFound) {
				return ErrContactNotFound
			}
			return fmt.Errorf("load contact: %w", err)
		}
		switch existing.State() {
		case unverifiedcontact.StateVerified, unverifiedcontact.StateRejected:
			// Refuse rather than no-op: a no-op would still mint a
			// fresh leadID and re-emit LeadVerifiedV1.
			return ErrContactAlreadyTerminal
		case unverifiedcontact.StateNew, unverifiedcontact.StateInCall, unverifiedcontact.StateBusy:
			// proceed
		}

		// Transition the contact; the aggregate re-checks the guard
		// (defense-in-depth).
		err = h.contacts.UpdateByID(ctx, cmd.ContactID, func(c *unverifiedcontact.UnverifiedContact) (bool, error) {
			// Verify requires InCall; promote from New if the caller
			// skipped the call-log step.
			if c.State() == unverifiedcontact.StateNew {
				if err := c.StartCall(now); err != nil {
					return false, err
				}
			}
			return true, c.MarkVerified(leadID.String(), cmd.VerifiedBy, now)
		})
		if err != nil {
			if errors.Is(err, unverifiedcontact.ErrNotFound) {
				return ErrContactNotFound
			}
			return fmt.Errorf("transition contact: %w", err)
		}

		// Re-load under the same tx to pull the form VO.
		c, err := h.contacts.GetByID(ctx, cmd.ContactID)
		if err != nil {
			return fmt.Errorf("reload contact: %w", err)
		}

		lead, err := platformlead.NewFromUnverifiedContact(
			leadID, cmd.ContactID, c.Form(), cmd.VerifiedBy, now,
		)
		if err != nil {
			return fmt.Errorf("construct lead: %w", err)
		}
		if err := h.leads.Add(ctx, lead); err != nil {
			return fmt.Errorf("persist lead: %w", err)
		}

		// Emit LeadVerifiedV1 in the same tx via OutboxEnqueuer. UUIDs
		// ride the wire as strings (ADR 0059).
		ev := integrationevents.LeadVerifiedV1{
			PlatformLeadID:         leadID.String(),
			VerifiedAt:             now.UTC(),
			VerifiedByMembershipID: cmd.VerifiedBy.String(),
			LeadSnapshot:           integrationevents.SnapshotFromForm(c.Form()),
		}
		if err := h.outboxEnq.EnqueueInTx(ctx, ev); err != nil {
			return fmt.Errorf("enqueue lead-verified: %w", err)
		}
		return nil
	})
	if err != nil {
		return VerifyUnverifiedContactResult{}, err
	}
	return VerifyUnverifiedContactResult{PlatformLeadID: leadID}, nil
}
