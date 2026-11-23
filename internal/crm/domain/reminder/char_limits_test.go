package reminder_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/textlimit"
	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder"
)

// M2: reminder notes (2000) and the cancel reason (1000) are limited in
// CHARACTERS (CHECK on length()); the domain used to count bytes and refuse
// valid Hindi from 667 / 334 letters.
func TestReminder_TextLimitsCountCharactersNotBytes(t *testing.T) {
	t.Parallel()
	dev := func(n int) string { return strings.Repeat("क", n) }
	if _, err := reminder.NewManualReminder(testReminderID, testTenantID, testLeadID, testAssignee, testCreator, pinnedDueAt(), dev(2000), pinnedNow()); err != nil {
		t.Fatalf("2000 Devanagari characters of notes must be accepted: %v", err)
	}
	_, err := reminder.NewManualReminder(testReminderID, testTenantID, testLeadID, testAssignee, testCreator, pinnedDueAt(), dev(2001), pinnedNow())
	if te, ok := textlimit.Of(err); !errors.Is(err, reminder.ErrInvalid) || !ok || te.Code() != "notes_too_long" || te.Unit != textlimit.UnitChars {
		t.Fatalf("2001 characters of notes must be refused in characters, got %v", err)
	}

	if err := mustNewPending(t).Cancel(testCreator, dev(1000), pinnedNow().Add(time.Hour)); err != nil {
		t.Fatalf("a cancel reason of 1000 Devanagari characters must be accepted: %v", err)
	}
	err = mustNewPending(t).Cancel(testCreator, dev(1001), pinnedNow().Add(time.Hour))
	if te, ok := textlimit.Of(err); !errors.Is(err, reminder.ErrInvalid) || !ok || te.Code() != "reason_too_long" || te.Unit != textlimit.UnitChars {
		t.Fatalf("1001 characters of cancel reason must be refused in characters, got %v", err)
	}
}
