package notification_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/notifications/domain/notification"
	"github.com/leadkart/leadkart-go/internal/notifications/domain/notification/notificationtest"
)

func fixedNow() time.Time { return time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC) }

func sampleNewInput(t *testing.T) notification.NewInput {
	t.Helper()
	return notification.NewInput{
		ID:                    notification.ID(ids.NewV7().String()),
		TenantID:              tenant.ID(ids.NewV7().String()),
		RecipientMembershipID: membership.ID(ids.NewV7().String()),
		Category:              notification.CategoryLeadAssigned,
		Title:                 "Lead Acme Pharma assigned to you",
		Body:                  "Pune-based pharma distributor; first contact within 24h.",
		SourceModule:          "crm",
		SourceEntityType:      "crm_lead",
		SourceEntityID:        ids.NewV7().String(),
		DeepLink:              "/app/crm/leads/abc",
		Now:                   fixedNow(),
	}
}

func TestNotification_New_HappyPath(t *testing.T) {
	t.Parallel()
	n, err := notification.New(sampleNewInput(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if n.State() != notification.StateUnread {
		t.Errorf("state=%s want unread", n.State())
	}
	events := n.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events=%d want 1", len(events))
	}
	if _, ok := events[0].(notification.CreatedEvent); !ok {
		t.Errorf("event 0 type=%T", events[0])
	}
}

func TestNotification_New_RejectsInvalid(t *testing.T) {
	t.Parallel()
	base := sampleNewInput(t)
	cases := []struct {
		name string
		mod  func(*notification.NewInput)
	}{
		{"zero id", func(in *notification.NewInput) { in.ID = "" }},
		{"zero tenant", func(in *notification.NewInput) { in.TenantID = "" }},
		{"zero recipient", func(in *notification.NewInput) { in.RecipientMembershipID = "" }},
		{"bad category", func(in *notification.NewInput) { in.Category = "garbage" }},
		{"empty title", func(in *notification.NewInput) { in.Title = "   " }},
		{"oversized title", func(in *notification.NewInput) {
			in.Title = string(make([]byte, 250)) // exceeds 200-char limit
		}},
		{"zero now", func(in *notification.NewInput) { in.Now = time.Time{} }},
		{"partial source — module only", func(in *notification.NewInput) {
			in.SourceEntityType = ""
		}},
		{"partial source — type only", func(in *notification.NewInput) {
			in.SourceModule = ""
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			in := base
			c.mod(&in)
			// Oversized title test needs non-whitespace bytes — fill with 'a'.
			if c.name == "oversized title" {
				bytes := make([]byte, 250)
				for i := range bytes {
					bytes[i] = 'a'
				}
				in.Title = string(bytes)
			}
			if _, err := notification.New(in); !errors.Is(err, notification.ErrInvalid) {
				t.Errorf("want ErrInvalid, got %v", err)
			}
		})
	}
}

func TestNotification_AllOrNoneSourceFieldsAllowed(t *testing.T) {
	t.Parallel()

	// All empty — manual notification, no source row — VALID.
	{
		in := sampleNewInput(t)
		in.SourceModule = ""
		in.SourceEntityType = ""
		in.SourceEntityID = ""
		if _, err := notification.New(in); err != nil {
			t.Errorf("all-empty source: %v", err)
		}
	}

	// All set — sourced notification — VALID.
	{
		if _, err := notification.New(sampleNewInput(t)); err != nil {
			t.Errorf("all-set source: %v", err)
		}
	}
}

func TestNotification_MarkRead_Idempotent(t *testing.T) {
	t.Parallel()
	n, _ := notification.New(sampleNewInput(t))
	n.PullEvents()

	if err := n.MarkRead(fixedNow().Add(time.Hour)); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if n.State() != notification.StateRead {
		t.Errorf("state=%s", n.State())
	}
	events := n.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events=%d want 1", len(events))
	}

	// Re-read — no error, no event.
	if err := n.MarkRead(fixedNow().Add(2 * time.Hour)); err != nil {
		t.Fatalf("re-MarkRead: %v", err)
	}
	if got := len(n.PullEvents()); got != 0 {
		t.Errorf("re-MarkRead events=%d want 0", got)
	}
}

func TestNotification_DismissFromUnreadOrRead(t *testing.T) {
	t.Parallel()

	// From unread.
	{
		n, _ := notification.New(sampleNewInput(t))
		n.PullEvents()
		if err := n.Dismiss(fixedNow().Add(time.Hour)); err != nil {
			t.Fatalf("Dismiss: %v", err)
		}
		events := n.PullEvents()
		if len(events) != 1 {
			t.Fatalf("events=%d want 1", len(events))
		}
		de := events[0].(notification.DismissedEvent)
		if de.PriorState != notification.StateUnread {
			t.Errorf("PriorState=%s want unread", de.PriorState)
		}
	}

	// From read.
	{
		n, _ := notification.New(sampleNewInput(t))
		_ = n.MarkRead(fixedNow().Add(time.Hour))
		n.PullEvents()
		if err := n.Dismiss(fixedNow().Add(2 * time.Hour)); err != nil {
			t.Fatalf("Dismiss: %v", err)
		}
		events := n.PullEvents()
		de := events[0].(notification.DismissedEvent)
		if de.PriorState != notification.StateRead {
			t.Errorf("PriorState=%s want read", de.PriorState)
		}
	}
}

func TestNotification_TerminalGuards(t *testing.T) {
	t.Parallel()
	n, _ := notification.New(sampleNewInput(t))
	_ = n.Dismiss(fixedNow().Add(time.Hour))

	if err := n.MarkRead(fixedNow().Add(2 * time.Hour)); !errors.Is(err, notification.ErrInvalidTransition) {
		t.Errorf("MarkRead after Dismiss: got %v want ErrInvalidTransition", err)
	}
}

func TestFakeRepository_DedupWithinWindow(t *testing.T) {
	t.Parallel()
	repo := notificationtest.NewFakeRepository()

	in := sampleNewInput(t)
	first, _ := notification.New(in)
	if err := repo.Add(t.Context(), first); err != nil {
		t.Fatalf("first Add: %v", err)
	}

	// Same recipient + source + category, 1 minute later — DUPLICATE.
	in2 := in
	in2.ID = notification.ID(ids.NewV7().String())
	in2.Now = fixedNow().Add(1 * time.Minute)
	second, _ := notification.New(in2)
	if err := repo.Add(t.Context(), second); !errors.Is(err, notification.ErrDuplicateInDedupWindow) {
		t.Errorf("dup-in-window: got %v want ErrDuplicateInDedupWindow", err)
	}

	// Same recipient + source + category, 10 minutes later — OUTSIDE window — allowed.
	in3 := in
	in3.ID = notification.ID(ids.NewV7().String())
	in3.Now = fixedNow().Add(10 * time.Minute)
	third, _ := notification.New(in3)
	if err := repo.Add(t.Context(), third); err != nil {
		t.Errorf("outside-window: %v", err)
	}

	// Different category, same source — allowed (no dedup across categories).
	in4 := in
	in4.ID = notification.ID(ids.NewV7().String())
	in4.Category = notification.CategoryReminder
	in4.Now = fixedNow().Add(2 * time.Minute)
	fourth, _ := notification.New(in4)
	if err := repo.Add(t.Context(), fourth); err != nil {
		t.Errorf("different-category: %v", err)
	}
}

func TestFakeRepository_UnreadCount(t *testing.T) {
	t.Parallel()
	repo := notificationtest.NewFakeRepository()
	tID := tenant.ID(ids.NewV7().String())
	recipient := membership.ID(ids.NewV7().String())

	for i := range 5 {
		in := sampleNewInput(t)
		in.ID = notification.ID(ids.NewV7().String())
		in.TenantID = tID
		in.RecipientMembershipID = recipient
		in.SourceEntityID = ids.NewV7().String() // distinct → no dedup
		in.Now = fixedNow().Add(time.Duration(i) * time.Minute)
		n, _ := notification.New(in)
		if err := repo.Add(t.Context(), n); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}

	count, err := repo.UnreadCount(t.Context(), tID, recipient)
	if err != nil {
		t.Fatalf("UnreadCount: %v", err)
	}
	if count != 5 {
		t.Errorf("UnreadCount=%d want 5", count)
	}
}

func TestFakeRepository_MarkAllReadBulk(t *testing.T) {
	t.Parallel()
	repo := notificationtest.NewFakeRepository()
	tID := tenant.ID(ids.NewV7().String())
	recipient := membership.ID(ids.NewV7().String())

	for i := range 3 {
		in := sampleNewInput(t)
		in.ID = notification.ID(ids.NewV7().String())
		in.TenantID = tID
		in.RecipientMembershipID = recipient
		in.SourceEntityID = ids.NewV7().String()
		in.Now = fixedNow().Add(time.Duration(i) * time.Minute)
		n, _ := notification.New(in)
		_ = repo.Add(t.Context(), n)
	}

	affected, err := repo.MarkAllReadForRecipient(t.Context(), tID, recipient, fixedNow().Add(time.Hour).UnixNano())
	if err != nil {
		t.Fatalf("MarkAllReadForRecipient: %v", err)
	}
	if affected != 3 {
		t.Errorf("affected=%d want 3", affected)
	}

	count, _ := repo.UnreadCount(t.Context(), tID, recipient)
	if count != 0 {
		t.Errorf("UnreadCount after bulk-mark=%d want 0", count)
	}
}

func TestParseState_AndCategory(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"unread", "read", "dismissed"} {
		if _, err := notification.ParseState(ok); err != nil {
			t.Errorf("ParseState(%q): %v", ok, err)
		}
	}
	if _, err := notification.ParseState("nonsense"); !errors.Is(err, notification.ErrInvalid) {
		t.Errorf("ParseState bad: got %v want ErrInvalid", err)
	}

	for _, ok := range []string{"lead_assigned", "order_confirmed", "work_item_overdue"} {
		if _, err := notification.ParseCategory(ok); err != nil {
			t.Errorf("ParseCategory(%q): %v", ok, err)
		}
	}
	if _, err := notification.ParseCategory("garbage"); !errors.Is(err, notification.ErrInvalid) {
		t.Errorf("ParseCategory bad: got %v want ErrInvalid", err)
	}
}
