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
		// Step 1: load + transition the contact. Idempotent on the
		// aggregate side — re-running this handler against an already-
		// verified contact short-circuits (state machine no-op).
		err := h.contacts.UpdateByID(ctx, cmd.ContactID, func(c *unverifiedcontact.UnverifiedContact) (bool, error) {
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
		// Drains in the SAME tx via OutboxEnqueuer.
		ev := integrationevents.LeadVerifiedV1{
			PlatformLeadID:         leadIDUUID(leadID),
			VerifiedAt:             now.UTC(),
			VerifiedByMembershipID: membershipUUID(cmd.VerifiedBy),
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
