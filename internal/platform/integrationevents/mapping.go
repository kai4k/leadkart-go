package integrationevents

import (
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit"
	"github.com/leadkart/leadkart-go/internal/platform/domain/platformlead"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
	"github.com/leadkart/leadkart-go/internal/platform/domain/verificationcall"
)

// ErrUnknown surfaces from FromDomainEvent when the input is not a
// recognised domain event in this module — usually means a new
// aggregate method emitted an event without wiring the integration
// counterpart. Caught at CI time by adapter-side tests + the
// TestArch_FromDomainEvent_HandlesAllRegisteredDomainEvents arch test.
var ErrUnknown = errors.New("platform.integrationevents: unrecognised domain event")

// FromDomainEvent enforces the event-suppression contract — READ
// BEFORE TOUCHING.
//
// `FromDomainEvent` recognises every Platform domain-event TYPE the
// arch test [TestArch_FromDomainEvent_HandlesAllRegisteredDomainEvents]
// enumerates. For each input the mapper returns one of:
//
//  1. (Event, nil)   — translate this domain event into an integration
//     event; outbox writer persists it.
//  2. (nil,   nil)   — INTENTIONAL suppression. The handler emits the
//     corresponding integration event directly because
//     the wire shape needs data the domain event does
//     NOT carry (e.g. LeadSnapshot, which lives on the
//     PlatformLead aggregate's Form VO; the domain
//     VerifiedEvent only carries IDs + timestamps).
//     Skipping the mechanical mapper here prevents
//     a duplicate emit when the handler runs.
//  3. (nil,   err)   — typed `ErrUnknown` wrap. New domain event added
//     without a mapper case — arch test fails CI.
//
// Adding a new domain event:
//   - ALWAYS add a case here (or the arch test fails).
//   - To map it: build the integration event + `return (event, nil)`.
//   - To suppress it: `return nil, nil` + a comment explaining WHY
//     (handler emits directly? audit-only via outbox row itself? etc.)
//
// Cyclomatic complexity scales with catalogue size by definition.
//
//nolint:cyclop // Switch dispatcher — one case per recognised domain event.
func FromDomainEvent(d any) (Event, error) {
	switch e := d.(type) {

	// ----- UnverifiedContact -------------------------------------------

	case unverifiedcontact.CreatedEvent:
		return UnverifiedContactCreatedV1{
			ContactID:             e.ContactID.String(),
			CreatedAt:             e.CreatedAt.UTC(),
			CreatedByMembershipID: e.CreatedByMembershipID.String(),
			MobileE164:            e.MobileE164,
		}, nil

	case unverifiedcontact.CallStartedEvent:
		// No integration event in Slice 1 — the verification-call row's
		// LoggedEvent carries the same information for downstream
		// consumers. Suppress without error.
		return nil, nil //nolint:nilnil // intentional suppression — caller skips on nil event

	case unverifiedcontact.VerifiedEvent:
		// LeadVerifiedV1 needs the LeadSnapshot which lives on the
		// PlatformLead aggregate, not on the contact's domain event.
		// The handler emits LeadVerifiedV1 directly inside the verify
		// flow (see app/command/verify_unverified_contact.go).
		// Suppress here to avoid double-emit.
		return nil, nil //nolint:nilnil // intentional suppression — handler emits directly

	case unverifiedcontact.RejectedEvent:
		// Rejection has no integration event in Slice 1 — captured
		// in audit log via the outbox row itself. Suppress.
		return nil, nil //nolint:nilnil // intentional suppression — audit-only

	case unverifiedcontact.MarkedBusyEvent:
		// Marked-busy is handled by the verification-call's
		// LoggedEvent (outcome=busy carries the callback window).
		// Suppress.
		return nil, nil //nolint:nilnil // intentional suppression — call-log event covers this

	// ----- VerificationCall --------------------------------------------

	case verificationcall.LoggedEvent:
		return VerificationCallLoggedV1{
			CallID:               e.CallID.String(),
			ContactID:            e.ContactID.String(),
			OutcomeCode:          string(e.OutcomeCode),
			LoggedAt:             e.LoggedAt.UTC(),
			LoggedByMembershipID: e.LoggedByMembershipID.String(),
		}, nil

	// ----- PlatformLead ------------------------------------------------

	case platformlead.VerifiedEvent:
		// Emitted by the handler (with snapshot) — see comment on
		// unverifiedcontact.VerifiedEvent above. Mechanical mapper
		// suppresses.
		return nil, nil //nolint:nilnil // intentional suppression — handler emits directly

	case platformlead.PurchasedEvent:
		// Same pattern — LeadPurchasedV1 needs the LeadSnapshot which
		// the handler loads + supplies. Suppress here.
		return nil, nil //nolint:nilnil // intentional suppression — handler emits directly

	// ----- LeadCredit --------------------------------------------------

	case leadcredit.AdjustedEvent:
		// LeadCreditAdjustedV1's AdjustmentID is a fresh UUIDv7 per
		// emit — generated here, not on the domain event (the
		// aggregate doesn't know wire IDs). Caller (the handler) does
		// NOT supply it; this mapper is the canonical source.
		//
		// Non-determinism note: retrying the same domain event yields
		// a fresh AdjustmentID each time. Downstream consumers that
		// need idempotency MUST dedup on the outbox envelope ID
		// (Wolverine inbox `MessageId` = outbox row UUIDv7), NOT on
		// AdjustmentID. AdjustmentID is the forensic "which credit
		// adjustment was this" anchor; envelope ID is the
		// "have I already processed this delivery" anchor.
		return LeadCreditAdjustedV1{
			TenantID:               e.TenantID.String(),
			AdjustmentID:           newAdjustmentID(),
			DeltaCredits:           e.Delta,
			NewBalanceCredits:      e.NewBalance,
			Reason:                 e.Reason,
			AdjustedAt:             e.AdjustedAt.UTC(),
			AdjustedByMembershipID: e.AdjustedByMembershipID.String(),
		}, nil
	}

	return nil, fmt.Errorf("%w: %T", ErrUnknown, d)
}

// newAdjustmentID returns a fresh UUIDv7 string for
// LeadCreditAdjustedV1's natural-key field. Decoupled into a variable
// so tests can swap a deterministic source.
//
//nolint:gochecknoglobals // test seam — swappable in tests via package-level assignment.
var newAdjustmentID = func() string {
	u, err := uuid.NewV7()
	if err != nil {
		// uuid.NewV7 only errors when the crypto/rand source is
		// broken — fail loudly, this is an init-time-class failure.
		panic(fmt.Sprintf("platform.integrationevents: NewV7: %v", err))
	}
	return u.String()
}
