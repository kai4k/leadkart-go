package jobs_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/crm/app/command"
	"github.com/leadkart/leadkart-go/internal/crm/app/jobs"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead/crmleadtest"
	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder"
	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder/remindertest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// pinnedNow gives the worker a deterministic clock so the cutoff +
// due_at + emitted-event timestamps are stable across runs.
func pinnedNow() time.Time { return time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC) }

func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubScanner is the test-side [jobs.LeadScanner]. Returns whatever
// the test seeds via SetCandidates; returns an error when SetErr is
// non-nil.
type stubScanner struct {
	candidates []jobs.MatureLeadCandidate
	err        error
}

func (s *stubScanner) MatureLeads(_ context.Context, _ time.Time, _ int) ([]jobs.MatureLeadCandidate, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.candidates, nil
}

// seedConvertedLead inserts a CrmLead in the supplied tenant + drives
// it through StageContacted → ... → StageConverted so the
// CreateReminderHandler's lead-existence probe succeeds.
func seedConvertedLead(t *testing.T, leads *crmleadtest.FakeRepository, tid tenant.ID, assignee string) crmlead.ID {
	t.Helper()
	l, err := crmlead.New(
		crmlead.ID(ids.NewV7().String()),
		tid,
		crmlead.Profile{ContactName: "Mature", PhoneE164: "+919812345678"},
		"01923400-0000-7000-8000-cccccccc0001",
		pinnedNow(),
	)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := l.Assign(assignee, "01923400-0000-7000-8000-cccccccc0002", "first", pinnedNow()); err != nil {
		t.Fatalf("seed assign: %v", err)
	}
	if err := leads.Add(t.Context(), l); err != nil {
		t.Fatalf("seed Add: %v", err)
	}
	return l.ID()
}

func TestMatureLeadScanWorker_CreatesReminders(t *testing.T) {
	t.Parallel()
	leads := crmleadtest.NewFakeRepository()
	reminders := remindertest.NewFakeRepository()
	tid := tenant.ID("01923400-0000-7000-8000-aaaaaaaa0001")
	assignee := "01923400-0000-7000-8000-aaaaaaaa0010"
	leadA := seedConvertedLead(t, leads, tid, assignee)
	leadB := seedConvertedLead(t, leads, tid, assignee)

	create := command.NewCreateReminderHandler(leads, reminders, pinnedNow,
		func() reminder.ID { return reminder.ID(ids.NewV7().String()) })

	scanner := &stubScanner{candidates: []jobs.MatureLeadCandidate{
		{TenantID: tid, LeadID: leadA, AssignedToMembershipID: assignee, ConvertedAt: pinnedNow().Add(-100 * 24 * time.Hour)},
		{TenantID: tid, LeadID: leadB, AssignedToMembershipID: assignee, ConvertedAt: pinnedNow().Add(-100 * 24 * time.Hour)},
	}}
	w := jobs.NewMatureLeadScanWorker(scanner, create, silentLog(), pinnedNow)
	if err := w.Work(t.Context(), &river.Job[jobs.MatureLeadScanJob]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(reminders.ByID) != 2 {
		t.Fatalf("reminders: %d (want 2)", len(reminders.ByID))
	}
	for _, r := range reminders.ByID {
		if r.Type() != reminder.TypeMatureLead {
			t.Fatalf("type: %q", r.Type())
		}
		if r.AssignedToMembershipID() != assignee {
			t.Fatalf("assignee: %q", r.AssignedToMembershipID())
		}
	}
}

func TestMatureLeadScanWorker_DuplicateIsIdempotent(t *testing.T) {
	t.Parallel()
	leads := crmleadtest.NewFakeRepository()
	reminders := remindertest.NewFakeRepository()
	tid := tenant.ID("01923400-0000-7000-8000-aaaaaaaa0001")
	assignee := "01923400-0000-7000-8000-aaaaaaaa0010"
	leadA := seedConvertedLead(t, leads, tid, assignee)

	create := command.NewCreateReminderHandler(leads, reminders, pinnedNow,
		func() reminder.ID { return reminder.ID(ids.NewV7().String()) })

	scanner := &stubScanner{candidates: []jobs.MatureLeadCandidate{
		{TenantID: tid, LeadID: leadA, AssignedToMembershipID: assignee, ConvertedAt: pinnedNow().Add(-100 * 24 * time.Hour)},
	}}
	w := jobs.NewMatureLeadScanWorker(scanner, create, silentLog(), pinnedNow)
	if err := w.Work(t.Context(), &river.Job[jobs.MatureLeadScanJob]{}); err != nil {
		t.Fatalf("first Work: %v", err)
	}
	// Re-run the scan — the partial-unique-pending gate should make
	// this idempotent. (The fake repository mirrors the SQL adapter's
	// partial-unique behavior; see remindertest.FakeRepository.Add.)
	if err := w.Work(t.Context(), &river.Job[jobs.MatureLeadScanJob]{}); err != nil {
		t.Fatalf("second Work: %v", err)
	}
	if len(reminders.ByID) != 1 {
		t.Fatalf("reminders: %d (want 1 after re-run)", len(reminders.ByID))
	}
}

func TestMatureLeadScanWorker_ScannerErrorPropagates(t *testing.T) {
	t.Parallel()
	leads := crmleadtest.NewFakeRepository()
	reminders := remindertest.NewFakeRepository()
	create := command.NewCreateReminderHandler(leads, reminders, pinnedNow,
		func() reminder.ID { return reminder.ID(ids.NewV7().String()) })
	scanner := &stubScanner{err: errors.New("db down")}
	w := jobs.NewMatureLeadScanWorker(scanner, create, silentLog(), pinnedNow)
	err := w.Work(t.Context(), &river.Job[jobs.MatureLeadScanJob]{})
	if err == nil {
		t.Fatal("want scanner error to propagate to River")
	}
}

func TestMatureLeadScanWorker_LeadDisappearedIsLoggedNotErrored(t *testing.T) {
	t.Parallel()
	leads := crmleadtest.NewFakeRepository()
	reminders := remindertest.NewFakeRepository()
	tid := tenant.ID("01923400-0000-7000-8000-aaaaaaaa0001")
	assignee := "01923400-0000-7000-8000-aaaaaaaa0010"

	create := command.NewCreateReminderHandler(leads, reminders, pinnedNow,
		func() reminder.ID { return reminder.ID(ids.NewV7().String()) })

	// Candidate references a lead that does NOT exist in the leads
	// fake (race between scan + reminder write). Worker logs + skips.
	scanner := &stubScanner{candidates: []jobs.MatureLeadCandidate{
		{TenantID: tid, LeadID: crmlead.ID("01923400-0000-7000-8000-aaaaaaaa0099"), AssignedToMembershipID: assignee, ConvertedAt: pinnedNow().Add(-100 * 24 * time.Hour)},
	}}
	w := jobs.NewMatureLeadScanWorker(scanner, create, silentLog(), pinnedNow)
	if err := w.Work(t.Context(), &river.Job[jobs.MatureLeadScanJob]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(reminders.ByID) != 0 {
		t.Fatalf("no reminders should be minted on missing-lead; got %d", len(reminders.ByID))
	}
}
