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
// counterpart. Caught at CI time by adapter-side tests.
var ErrUnknown = errors.New("platform.integrationevents: unrecognised domain event")

// FromDomainEvent translates ANY recognised Platform domain event into
// its canonical integration event. Used by repository adapters via the
// shared drain helper.
//
// Returns (nil, nil) to deliberately suppress an event (e.g.
// PlatformLead.VerifiedEvent — emitted via the dedicated handler-driven
// path so the LeadSnapshot can be populated from the aggregate's Form
// VO; not via this mechanical mapper).
//
// Returns ([wrapped] ErrUnknown) for unknown types.
//
//nolint:cyclop // Switch dispatcher — one case per recognised domain event.
// Cyclomatic complexity scales with catalogue size by definition.
func FromDomainEvent(d any) (Event, error) {
	switch e := d.(type) {

	// ----- UnverifiedContact -------------------------------------------

	case unverifiedcontact.CreatedEvent:
		return UnverifiedContactCreatedV1{
			ContactID:             mustParseUUID(e.ContactID.String()),
			CreatedAt:             e.CreatedAt.UTC(),
			CreatedByMembershipID: mustParseUUID(e.CreatedByMembershipID.String()),
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
			CallID:               mustParseUUID(e.CallID.String()),
			ContactID:            mustParseUUID(e.ContactID.String()),
			OutcomeCode:          string(e.OutcomeCode),
			LoggedAt:             e.LoggedAt.UTC(),
			LoggedByMembershipID: mustParseUUID(e.LoggedByMembershipID.String()),
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
			TenantIDValue:          mustParseUUID(e.TenantID.String()),
			AdjustmentID:           newAdjustmentID(),
			DeltaCredits:           e.Delta,
			NewBalanceCredits:      e.NewBalance,
			Reason:                 e.Reason,
			AdjustedAt:             e.AdjustedAt.UTC(),
			AdjustedByMembershipID: mustParseUUID(e.AdjustedByMembershipID.String()),
		}, nil
	}

	return nil, fmt.Errorf("%w: %T", ErrUnknown, d)
}

// mustParseUUID converts a string UUID into [uuid.UUID]. Panics on
// invalid input — domain IDs are always valid UUIDv7 strings at this
// boundary (the aggregate factories generate them via ids.NewV7).
// Panic is the right escalation: malformed input here means
// in-process corruption, not user error.
func mustParseUUID(s string) uuid.UUID {
	u, err := uuid.Parse(s)
	if err != nil {
		panic(fmt.Sprintf("platform.integrationevents: malformed UUID %q: %v", s, err))
	}
	return u
}

// newAdjustmentID returns a fresh UUIDv7 for LeadCreditAdjustedV1's
// natural-key field. Decoupled into a variable so tests can swap a
// deterministic source.
//
//nolint:gochecknoglobals // test seam — swappable in tests via package-level assignment.
var newAdjustmentID = func() uuid.UUID {
	u, err := uuid.NewV7()
	if err != nil {
		panic(fmt.Sprintf("platform.integrationevents: NewV7: %v", err))
	}
	return u
}
