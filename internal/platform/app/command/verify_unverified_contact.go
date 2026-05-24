package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/platform/domain/platformlead"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
	"github.com/leadkart/leadkart-go/internal/platform/integrationevents"
)

// ErrContactAlreadyTerminal is surfaced by the verify handler when the
// contact is already in a terminal state (Verified or Rejected). For
// Verified the aggregate is idempotent; the handler still refuses to
// emit a SECOND LeadVerifiedV1 + create a SECOND PlatformLead. HTTP
// layer maps to 409 (Conflict).
var ErrContactAlreadyTerminal = errors.New("verify: contact already in terminal state")

// VerifyUnverifiedContactCommand carries the input for the Lead Agent
// "promote this contact to a marketplace PlatformLead" use case.
type VerifyUnverifiedContactCommand struct {
	ContactID  unverifiedcontact.ID
	VerifiedBy unverifiedcontact.MembershipID
}

// VerifyUnverifiedContactResult holds the new PlatformLead's ID.
type VerifyUnverifiedContactResult struct {
	PlatformLeadID platformlead.ID
}

// VerifyUnverifiedContactHandler is the load-bearing handler — runs
// the contact + lead writes in ONE UoW tx, then emits a tailored
// LeadVerifiedV1 carrying the full LeadSnapshot built from the
// contact's form VO. The mechanical mapper suppresses the platformlead
// domain VerifiedEvent precisely so this handler can drive the wire
// shape.
type VerifyUnverifiedContactHandler struct {
	uow          pg.UnitOfWork
	contacts     unverifiedcontact.Repository
	leads        platformlead.Repository
	outboxEnq    OutboxEnqueuer
	now          func() time.Time
}

// NewVerifyUnverifiedContactHandler wires the handler.
func NewVerifyUnverifiedContactHandler(
	uow pg.UnitOfWork,
	contacts unverifiedcontact.Repository,
	leads platformlead.Repository,
	outboxEnq OutboxEnqueuer,
	now func() time.Time,
) VerifyUnverifiedContactHandler {
	return VerifyUnverifiedContactHandler{
		uow: uow, contacts: contacts, leads: leads, outboxEnq: outboxEnq, now: now,
	}
}

// Handle is the contact→lead promotion. Steps inside ONE tx:
//
//  1. Generate the new PlatformLead ID.
//  2. Update the contact (load + MarkVerified + persist) — joins the tx.
//  3. Re-load the contact to get the verified form (UpdateByID returns
//     no aggregate handle; the load happens inline via GetByID under
//     the same tx ctx).
//  4. Construct + persist the PlatformLead from the contact's form +
//     ID generated in step 1.
//  5. Enqueue LeadVerifiedV1 directly with the LeadSnapshot.
func (h VerifyUnverifiedContactHandler) Handle(
	ctx context.Context,
	cmd VerifyUnverifiedContactCommand,
) (VerifyUnverifiedContactResult, error) {
	if cmd.VerifiedBy.IsZero() {
		return VerifyUnverifiedContactResult{}, errors.New("verify: verifiedBy required")
	}

	leadID := platformlead.ID(ids.NewV7().String())
	now := h.now()

	err := h.uow.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context) error {
		// Step 0: load the contact under the tx so we can guard
		// against double-verify / verify-after-reject BEFORE
		// fabricating a fresh leadID + emitting a second
		// LeadVerifiedV1 (which would create a phantom marketplace
		// row downstream). H11 — review-pass.
		existing, err := h.contacts.GetByID(ctx, cmd.ContactID)
		if err != nil {
			if errors.Is(err, unverifiedcontact.ErrNotFound) {
				return ErrContactNotFound
			}
			return fmt.Errorf("load contact: %w", err)
		}
		switch existing.State() {
		case unverifiedcontact.StateVerified, unverifiedcontact.StateRejected:
			// Terminal state — refuse the request rather than
			// idempotent-no-op (which would still fabricate a fresh
			// leadID + emit LeadVerifiedV1 a second time).
			return ErrContactAlreadyTerminal
		case unverifiedcontact.StateNew, unverifiedcontact.StateInCall, unverifiedcontact.StateBusy:
			// proceed
		}

		// Step 1: load + transition the contact. Aggregate enforces
		// the state-machine guard a second time (defense-in-depth).
		err = h.contacts.UpdateByID(ctx, cmd.ContactID, func(c *unverifiedcontact.UnverifiedContact) (bool, error) {
			// Ensure InCall first — verify requires it. If the contact
			// is in New (caller skipped the call-log step), promote
			// to InCall first so the next transition is legal.
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

		// Step 2: re-load the contact under the same tx ctx so we can
		// pull the form VO + the verified-by/at fields.
		c, err := h.contacts.GetByID(ctx, cmd.ContactID)
		if err != nil {
			return fmt.Errorf("reload contact: %w", err)
		}

		// Step 3: construct + persist the PlatformLead.
		lead, err := platformlead.NewFromUnverifiedContact(
			leadID, cmd.ContactID, c.Form(), cmd.VerifiedBy, now,
		)
		if err != nil {
			return fmt.Errorf("construct lead: %w", err)
		}
		if err := h.leads.Add(ctx, lead); err != nil {
			return fmt.Errorf("persist lead: %w", err)
		}

		// Step 4: emit LeadVerifiedV1 with the full LeadSnapshot.
		// UUIDs travel the wire as strings per ADR 0059 frozen brief.
		// Drains in the SAME tx via OutboxEnqueuer.
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
