package reminder_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Pinned IDs used across the table-driven tests below. UUIDv7-shaped so
// uuid.Parse accepts them but the actual layout doesn't matter — the
// aggregate validation only checks parseability.
const (
	testReminderID = reminder.ID("01923400-0000-7000-8000-aaaaaaaa0001")
	testTenantID   = tenant.ID("01923400-0000-7000-8000-aaaaaaaa0002")
	testLeadID     = crmlead.ID("01923400-0000-7000-8000-aaaaaaaa0003")
	testAssignee   = "01923400-0000-7000-8000-aaaaaaaa0004"
	testCreator    = "01923400-0000-7000-8000-aaaaaaaa0005"
	testCallLogID  = "01923400-0000-7000-8000-aaaaaaaa0006"
)

func pinnedNow() time.Time {
	return time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC)
}

func pinnedDueAt() time.Time {
	return time.Date(2026, 6, 3, 11, 0, 0, 0, time.UTC)
}

// ----- Factory: NewCallbackReminder -----------------------------------------

func TestNewCallbackReminder_HappyPath(t *testing.T) {
	t.Parallel()
	r, err := reminder.NewCallbackReminder(
		testReminderID, testTenantID, testLeadID,
		testAssignee, testCreator, testCallLogID,
		pinnedDueAt(), "called back", pinnedNow(),
	)
	if err != nil {
		t.Fatalf("NewCallbackReminder: %v", err)
	}
	if r.ID() != testReminderID {
		t.Fatalf("ID: %q", r.ID())
	}
	if r.Type() != reminder.TypeCallback {
		t.Fatalf("Type: %q", r.Type())
	}
	if r.State() != reminder.StatePending {
		t.Fatalf("State: %q", r.State())
	}
	if r.SourceCallLogID() != testCallLogID {
		t.Fatalf("SourceCallLogID: %q", r.SourceCallLogID())
	}
	if !r.DueAt().Equal(pinnedDueAt()) {
		t.Fatalf("DueAt: %v", r.DueAt())
	}
	evs := r.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("events: %d", len(evs))
	}
	if _, ok := evs[0].(reminder.CreatedEvent); !ok {
		t.Fatalf("event: %T", evs[0])
	}
}

func TestNewCallbackReminder_RequiresSourceCallLogID(t *testing.T) {
	t.Parallel()
	_, err := reminder.NewCallbackReminder(
		testReminderID, testTenantID, testLeadID,
		testAssignee, testCreator, "",
		pinnedDueAt(), "", pinnedNow(),
	)
	if !errors.Is(err, reminder.ErrInvalid) {
		t.Fatalf("want ErrInvalid for empty SourceCallLogID, got %v", err)
	}
}

func TestNewCallbackReminder_RejectsMalformedSourceCallLogID(t *testing.T) {
	t.Parallel()
	_, err := reminder.NewCallbackReminder(
		testReminderID, testTenantID, testLeadID,
		testAssignee, testCreator, "not-a-uuid",
		pinnedDueAt(), "", pinnedNow(),
	)
	if !errors.Is(err, reminder.ErrInvalid) {
		t.Fatalf("want ErrInvalid, got %v", err)
	}
}

// ----- Factory: NewMatureLeadReminder ---------------------------------------

func TestNewMatureLeadReminder_HappyPath(t *testing.T) {
	t.Parallel()
	r, err := reminder.NewMatureLeadReminder(
		testReminderID, testTenantID, testLeadID,
		testAssignee, pinnedDueAt(), "3 months no reorder", pinnedNow(),
	)
	if err != nil {
		t.Fatalf("NewMatureLeadReminder: %v", err)
	}
	if r.Type() != reminder.TypeMatureLead {
		t.Fatalf("Type: %q", r.Type())
	}
	if r.SourceCallLogID() != "" {
		t.Fatalf("mature-lead should not carry source_call_log_id, got %q", r.SourceCallLogID())
	}
	if r.CreatedByMembershipID() != "" {
		t.Fatalf("mature-lead should be system-created (empty CreatedBy), got %q", r.CreatedByMembershipID())
	}
}

// ----- Factory: NewManualReminder -------------------------------------------

func TestNewManualReminder_HappyPath(t *testing.T) {
	t.Parallel()
	r, err := reminder.NewManualReminder(
		testReminderID, testTenantID, testLeadID,
		testAssignee, testCreator, pinnedDueAt(), "follow up", pinnedNow(),
	)
	if err != nil {
		t.Fatalf("NewManualReminder: %v", err)
	}
	if r.Type() != reminder.TypeManual {
		t.Fatalf("Type: %q", r.Type())
	}
	if r.CreatedByMembershipID() != testCreator {
		t.Fatalf("CreatedBy: %q", r.CreatedByMembershipID())
	}
}

func TestNewManualReminder_RequiresCreatedBy(t *testing.T) {
	t.Parallel()
	_, err := reminder.NewManualReminder(
		testReminderID, testTenantID, testLeadID,
		testAssignee, "", pinnedDueAt(), "", pinnedNow(),
	)
	if !errors.Is(err, reminder.ErrInvalid) {
		t.Fatalf("want ErrInvalid for empty CreatedBy, got %v", err)
	}
}

// ----- Factory: shared invariant gates --------------------------------------

func TestFactory_RejectsZeroID(t *testing.T) {
	t.Parallel()
	_, err := reminder.NewMatureLeadReminder(
		"", testTenantID, testLeadID,
		testAssignee, pinnedDueAt(), "", pinnedNow(),
	)
	if !errors.Is(err, reminder.ErrInvalid) {
		t.Fatalf("want ErrInvalid for empty ID, got %v", err)
	}
}

func TestFactory_RejectsMalformedID(t *testing.T) {
	t.Parallel()
	_, err := reminder.NewMatureLeadReminder(
		reminder.ID("not-a-uuid"), testTenantID, testLeadID,
		testAssignee, pinnedDueAt(), "", pinnedNow(),
	)
	if !errors.Is(err, reminder.ErrInvalid) {
		t.Fatalf("want ErrInvalid for malformed ID, got %v", err)
	}
}

func TestFactory_RejectsZeroTenantID(t *testing.T) {
	t.Parallel()
	_, err := reminder.NewMatureLeadReminder(
		testReminderID, tenant.ID(""), testLeadID,
		testAssignee, pinnedDueAt(), "", pinnedNow(),
	)
	if !errors.Is(err, reminder.ErrInvalid) {
		t.Fatalf("want ErrInvalid for empty TenantID, got %v", err)
	}
}

func TestFactory_RejectsZeroLeadID(t *testing.T) {
	t.Parallel()
	_, err := reminder.NewMatureLeadReminder(
		testReminderID, testTenantID, crmlead.ID(""),
		testAssignee, pinnedDueAt(), "", pinnedNow(),
	)
	if !errors.Is(err, reminder.ErrInvalid) {
		t.Fatalf("want ErrInvalid for empty LeadID, got %v", err)
	}
}

func TestFactory_RejectsEmptyAssignee(t *testing.T) {
	t.Parallel()
	_, err := reminder.NewMatureLeadReminder(
		testReminderID, testTenantID, testLeadID,
		"", pinnedDueAt(), "", pinnedNow(),
	)
	if !errors.Is(err, reminder.ErrInvalid) {
		t.Fatalf("want ErrInvalid for empty assignee, got %v", err)
	}
}

func TestFactory_RejectsZeroDueAt(t *testing.T) {
	t.Parallel()
	_, err := reminder.NewMatureLeadReminder(
		testReminderID, testTenantID, testLeadID,
		testAssignee, time.Time{}, "", pinnedNow(),
	)
	if !errors.Is(err, reminder.ErrInvalid) {
		t.Fatalf("want ErrInvalid for zero DueAt, got %v", err)
	}
}

func TestFactory_RejectsZeroNow(t *testing.T) {
	t.Parallel()
	_, err := reminder.NewMatureLeadReminder(
		testReminderID, testTenantID, testLeadID,
		testAssignee, pinnedDueAt(), "", time.Time{},
	)
	if !errors.Is(err, reminder.ErrInvalid) {
		t.Fatalf("want ErrInvalid for zero now, got %v", err)
	}
}

func TestFactory_RejectsOverlongNotes(t *testing.T) {
	t.Parallel()
	notes := strings.Repeat("x", 2001)
	_, err := reminder.NewMatureLeadReminder(
		testReminderID, testTenantID, testLeadID,
		testAssignee, pinnedDueAt(), notes, pinnedNow(),
	)
	if !errors.Is(err, reminder.ErrInvalid) {
		t.Fatalf("want ErrInvalid for overlong notes, got %v", err)
	}
}

// ----- State machine: MarkSent ----------------------------------------------

func TestMarkSent_HappyPath(t *testing.T) {
	t.Parallel()
	r := mustNewPending(t)
	_ = r.PullEvents() // drain CreatedEvent
	if err := r.MarkSent(testCreator, pinnedNow().Add(time.Hour)); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}
	if r.State() != reminder.StateSent {
		t.Fatalf("State: %q", r.State())
	}
	if r.SentAt().IsZero() {
		t.Fatal("SentAt should be set")
	}
	evs := r.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("events: %d", len(evs))
	}
	if _, ok := evs[0].(reminder.MarkedSentEvent); !ok {
		t.Fatalf("event: %T", evs[0])
	}
}

func TestMarkSent_RejectsAlreadySent(t *testing.T) {
	t.Parallel()
	r := mustNewPending(t)
	_ = r.PullEvents()
	if err := r.MarkSent(testCreator, pinnedNow().Add(time.Hour)); err != nil {
		t.Fatalf("first MarkSent: %v", err)
	}
	err := r.MarkSent(testCreator, pinnedNow().Add(2*time.Hour))
	if !errors.Is(err, reminder.ErrConflict) {
		t.Fatalf("want ErrConflict on double MarkSent, got %v", err)
	}
}

func TestMarkSent_RejectsCancelled(t *testing.T) {
	t.Parallel()
	r := mustNewPending(t)
	_ = r.PullEvents()
	if err := r.Cancel(testCreator, "user changed mind", pinnedNow().Add(time.Hour)); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	_ = r.PullEvents()
	err := r.MarkSent(testCreator, pinnedNow().Add(2*time.Hour))
	if !errors.Is(err, reminder.ErrConflict) {
		t.Fatalf("want ErrConflict on MarkSent after Cancel, got %v", err)
	}
}

func TestMarkSent_RequiresActor(t *testing.T) {
	t.Parallel()
	r := mustNewPending(t)
	_ = r.PullEvents()
	if err := r.MarkSent("", pinnedNow().Add(time.Hour)); !errors.Is(err, reminder.ErrInvalid) {
		t.Fatalf("want ErrInvalid for empty markedBy, got %v", err)
	}
}

// ----- State machine: Cancel ------------------------------------------------

func TestCancel_HappyPath(t *testing.T) {
	t.Parallel()
	r := mustNewPending(t)
	_ = r.PullEvents()
	if err := r.Cancel(testCreator, "duplicate of #42", pinnedNow().Add(time.Hour)); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if r.State() != reminder.StateCancelled {
		t.Fatalf("State: %q", r.State())
	}
	if r.CancelReason() != "duplicate of #42" {
		t.Fatalf("CancelReason: %q", r.CancelReason())
	}
	evs := r.PullEvents()
	if len(evs) != 1 {
		t.Fatalf("events: %d", len(evs))
	}
	if _, ok := evs[0].(reminder.CancelledEvent); !ok {
		t.Fatalf("event: %T", evs[0])
	}
}

func TestCancel_RequiresReason(t *testing.T) {
	t.Parallel()
	r := mustNewPending(t)
	_ = r.PullEvents()
	err := r.Cancel(testCreator, "   ", pinnedNow().Add(time.Hour))
	if !errors.Is(err, reminder.ErrInvalid) {
		t.Fatalf("want ErrInvalid for empty reason, got %v", err)
	}
}

func TestCancel_RejectsOverlongReason(t *testing.T) {
	t.Parallel()
	r := mustNewPending(t)
	_ = r.PullEvents()
	err := r.Cancel(testCreator, strings.Repeat("x", 1001), pinnedNow().Add(time.Hour))
	if !errors.Is(err, reminder.ErrInvalid) {
		t.Fatalf("want ErrInvalid for overlong reason, got %v", err)
	}
}

func TestCancel_RejectsAfterSent(t *testing.T) {
	t.Parallel()
	r := mustNewPending(t)
	_ = r.PullEvents()
	if err := r.MarkSent(testCreator, pinnedNow().Add(time.Hour)); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}
	_ = r.PullEvents()
	err := r.Cancel(testCreator, "too late", pinnedNow().Add(2*time.Hour))
	if !errors.Is(err, reminder.ErrConflict) {
		t.Fatalf("want ErrConflict on Cancel after Sent, got %v", err)
	}
}

func TestCancel_RejectsDoubleCancel(t *testing.T) {
	t.Parallel()
	r := mustNewPending(t)
	_ = r.PullEvents()
	if err := r.Cancel(testCreator, "first", pinnedNow().Add(time.Hour)); err != nil {
		t.Fatalf("first cancel: %v", err)
	}
	_ = r.PullEvents()
	err := r.Cancel(testCreator, "second", pinnedNow().Add(2*time.Hour))
	if !errors.Is(err, reminder.ErrConflict) {
		t.Fatalf("want ErrConflict on double cancel, got %v", err)
	}
}

// ----- Enum parse -----------------------------------------------------------

func TestParseType_Catalogue(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"callback", "mature_lead", "manual"} {
		got, err := reminder.ParseType(raw)
		if err != nil {
			t.Fatalf("ParseType(%q): %v", raw, err)
		}
		if got.String() != raw {
			t.Fatalf("ParseType(%q) -> %q", raw, got)
		}
	}
}

func TestParseType_Unknown(t *testing.T) {
	t.Parallel()
	_, err := reminder.ParseType("invented")
	if !errors.Is(err, reminder.ErrInvalid) {
		t.Fatalf("ParseType unknown: %v", err)
	}
}

func TestParseState_Catalogue(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"pending", "sent", "cancelled"} {
		got, err := reminder.ParseState(raw)
		if err != nil {
			t.Fatalf("ParseState(%q): %v", raw, err)
		}
		if got.String() != raw {
			t.Fatalf("ParseState(%q) -> %q", raw, got)
		}
	}
}

func TestParseState_Unknown(t *testing.T) {
	t.Parallel()
	_, err := reminder.ParseState("invented")
	if !errors.Is(err, reminder.ErrInvalid) {
		t.Fatalf("ParseState unknown: %v", err)
	}
}

// ----- Helpers --------------------------------------------------------------

func mustNewPending(t *testing.T) *reminder.Reminder {
	t.Helper()
	r, err := reminder.NewManualReminder(
		testReminderID, testTenantID, testLeadID,
		testAssignee, testCreator, pinnedDueAt(), "ping the prospect", pinnedNow(),
	)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return r
}
