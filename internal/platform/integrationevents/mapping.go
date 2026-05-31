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

// ErrUnknown surfaces from FromDomainEvent when the input is not a recognised
// domain event — usually a new aggregate event without a wired integration
// counterpart. Caught in CI by adapter tests and
// TestArch_FromDomainEvent_HandlesAllRegisteredDomainEvents.
var ErrUnknown = errors.New("platform.integrationevents: unrecognised domain event")

// FromDomainEvent maps a Platform domain event to its integration event. It
// recognises every type enumerated by
// [TestArch_FromDomainEvent_HandlesAllRegisteredDomainEvents] and returns:
//
//  1. (Event, nil) — translated; the outbox writer persists it.
//  2. (nil, nil)   — intentional suppression. The handler emits the integration
//     event directly because the wire shape needs data the domain event lacks
//     (e.g. LeadSnapshot, which lives on the PlatformLead Form VO while the
//     domain VerifiedEvent carries only IDs + timestamps). Suppressing here
//     avoids a duplicate emit.
//  3. (nil, ErrUnknown) — a new domain event with no mapper case; arch test fails.
//
// Adding a domain event: add a case here (the arch test enforces it). Map it
// with return (event, nil), or suppress with return nil, nil plus a WHY comment.
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
		// No Slice 1 event — the verification-call LoggedEvent carries the
		// same information downstream.
		return nil, nil //nolint:nilnil // intentional suppression — caller skips on nil event

	case unverifiedcontact.VerifiedEvent:
		// LeadVerifiedV1 needs the LeadSnapshot, which lives on PlatformLead,
		// not on this contact event. The handler emits it directly in the
		// verify flow (app/command/verify_unverified_contact.go); suppress to
		// avoid a double-emit.
		return nil, nil //nolint:nilnil // intentional suppression — handler emits directly

	case unverifiedcontact.RejectedEvent:
		// No Slice 1 event — captured in audit via the outbox row itself.
		return nil, nil //nolint:nilnil // intentional suppression — audit-only

	case unverifiedcontact.MarkedBusyEvent:
		// Covered by the verification-call LoggedEvent (outcome=busy carries
		// the callback window).
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
		// Emitted by the handler with snapshot — see unverifiedcontact.VerifiedEvent above.
		return nil, nil //nolint:nilnil // intentional suppression — handler emits directly

	case platformlead.PurchasedEvent:
		// LeadPurchasedV1 needs the LeadSnapshot the handler loads and supplies.
		return nil, nil //nolint:nilnil // intentional suppression — handler emits directly

	// ----- LeadCredit --------------------------------------------------

	case leadcredit.AdjustedEvent:
		// AdjustmentID is a fresh UUIDv7 minted here (the aggregate doesn't know
		// wire IDs); this mapper is its canonical source.
		//
		// Non-deterministic: retrying the same domain event yields a new
		// AdjustmentID. Consumers needing idempotency MUST dedup on the outbox
		// envelope ID (inbox MessageId = outbox row UUIDv7), NOT AdjustmentID.
		// AdjustmentID is the forensic "which adjustment" anchor; envelope ID is
		// the "already processed this delivery" anchor.
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

// newAdjustmentID returns a fresh UUIDv7 string for LeadCreditAdjustedV1.
// A var so tests can swap a deterministic source.
//
//nolint:gochecknoglobals // test seam — swappable in tests via package-level assignment.
var newAdjustmentID = func() string {
	u, err := uuid.NewV7()
	if err != nil {
		// NewV7 only errors when crypto/rand is broken — fail loudly.
		panic(fmt.Sprintf("platform.integrationevents: NewV7: %v", err))
	}
	return u.String()
}
