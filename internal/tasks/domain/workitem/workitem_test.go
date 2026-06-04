package workitem_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/tasks/domain/workitem"
)

// fixedNow is the deterministic instant test fixtures pass to domain
// factories per the clock-injection refactor + Khorikov §8 (tests
// pin time; never derive from a wall-clock call).
var fixedNow = time.Date(2026, 5, 26, 10, 0, 0, 0, time.UTC)

// newID returns a fresh UUIDv4 string for use as a workitem.ID /
// membership ID in tests. UUIDv7 is for production B-tree locality;
// v4 is fine for test pinning.
func newID(t *testing.T) string {
	t.Helper()
	return uuid.New().String()
}

func sampleParams(t *testing.T, now time.Time) workitem.NewParams {
	t.Helper()
	return workitem.NewParams{
		ID:                     workitem.ID(newID(t)),
		TenantID:               tenant.ID(newID(t)),
		Type:                   workitem.TypeManual,
		Priority:               workitem.PriorityMedium,
		Title:                  "Follow up with Pharma X",
		Description:            "Quoted yesterday, expect a reply today.",
		AssignedToMembershipID: newID(t),
		AssignedByMembershipID: newID(t),
		CreatedByMembershipID:  newID(t),
		DueAt:                  now.Add(2 * time.Hour),
		Now:                    now,
	}
}

func TestNewManual_Valid(t *testing.T) {
	t.Parallel()
	now := fixedNow
	w, err := workitem.NewManual(sampleParams(t, now))
	require.NoError(t, err)
	require.Equal(t, workitem.StatePending, w.State())
	require.Equal(t, workitem.TypeManual, w.Type())
	require.Equal(t, workitem.PriorityMedium, w.Priority())

	evs := w.PullEvents()
	require.Len(t, evs, 1)
	created, ok := evs[0].(workitem.CreatedEvent)
	require.True(t, ok)
	require.Equal(t, w.ID(), created.WorkItemID)
	require.Equal(t, workitem.TypeManual, created.Type)
	require.Empty(t, created.SourceModule)
}

func TestNewManual_DefaultsPriorityWhenEmpty(t *testing.T) {
	t.Parallel()
	now := fixedNow
	p := sampleParams(t, now)
	p.Priority = ""
	w, err := workitem.NewManual(p)
	require.NoError(t, err)
	require.Equal(t, workitem.PriorityMedium, w.Priority())
}

func TestNewManual_RejectsInvalidInputs(t *testing.T) {
	t.Parallel()
	now := fixedNow
	cases := []struct {
		name  string
		mut   func(p *workitem.NewParams)
		match string
	}{
		{"empty id", func(p *workitem.NewParams) { p.ID = "" }, "id"},
		{"bad uuid id", func(p *workitem.NewParams) { p.ID = "not-a-uuid" }, "id"},
		{"empty tenant", func(p *workitem.NewParams) { p.TenantID = "" }, "tenant"},
		{"empty assignee", func(p *workitem.NewParams) { p.AssignedToMembershipID = "" }, "assigned_to"},
		{"bad uuid assignee", func(p *workitem.NewParams) { p.AssignedToMembershipID = "x" }, "assigned_to"},
		{"empty assigner", func(p *workitem.NewParams) { p.AssignedByMembershipID = "" }, "assigned_by"},
		{"empty creator", func(p *workitem.NewParams) { p.CreatedByMembershipID = "" }, "created_by"},
		{"empty title", func(p *workitem.NewParams) { p.Title = "  " }, "title"},
		{"oversize title", func(p *workitem.NewParams) { p.Title = strings.Repeat("a", 201) }, "title"},
		{"oversize description", func(p *workitem.NewParams) { p.Description = strings.Repeat("a", 4001) }, "description"},
		{"bad type", func(p *workitem.NewParams) { p.Type = "weird" }, "type"},
		{"bad priority", func(p *workitem.NewParams) { p.Priority = "epic" }, "priority"},
		{"zero due_at", func(p *workitem.NewParams) { p.DueAt = time.Time{} }, "due_at"},
		{"zero now", func(p *workitem.NewParams) { p.Now = time.Time{} }, "now"},
		{"bad batch id", func(p *workitem.NewParams) { p.BatchID = "not-uuid" }, "batch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := sampleParams(t, now)
			tc.mut(&p)
			_, err := workitem.NewManual(p)
			require.Error(t, err)
			require.ErrorIs(t, err, workitem.ErrInvalid)
			require.Contains(t, err.Error(), tc.match)
		})
	}
}

func TestNewAutoCreated_RequiresSource(t *testing.T) {
	t.Parallel()
	now := fixedNow
	p := workitem.AutoCreateParams{
		ID:                     workitem.ID(newID(t)),
		TenantID:               tenant.ID(newID(t)),
		Type:                   workitem.TypeCallbackReminder,
		Priority:               workitem.PriorityHigh,
		Title:                  "Callback: Pharma Y",
		AssignedToMembershipID: newID(t),
		AssignedByMembershipID: newID(t),
		CreatedByMembershipID:  newID(t),
		DueAt:                  now.Add(time.Hour),
		Now:                    now,
		// Source intentionally empty
	}
	_, err := workitem.NewAutoCreated(p)
	require.ErrorIs(t, err, workitem.ErrInvalid)
}

func TestNewAutoCreated_Valid(t *testing.T) {
	t.Parallel()
	now := fixedNow
	p := workitem.AutoCreateParams{
		ID:                     workitem.ID(newID(t)),
		TenantID:               tenant.ID(newID(t)),
		Type:                   workitem.TypeCallbackReminder,
		Priority:               workitem.PriorityHigh,
		Title:                  "Callback: Pharma Y",
		AssignedToMembershipID: newID(t),
		AssignedByMembershipID: newID(t),
		CreatedByMembershipID:  newID(t),
		DueAt:                  now.Add(time.Hour),
		Now:                    now,
		Source: workitem.Source{
			Module:     "crm",
			EntityType: "call_log",
			EntityID:   newID(t),
		},
	}
	w, err := workitem.NewAutoCreated(p)
	require.NoError(t, err)
	require.Equal(t, "crm", w.Source().Module)
	require.False(t, w.Source().IsZero())
}

func TestStart_Transitions(t *testing.T) {
	t.Parallel()
	now := fixedNow
	w := mustNewManual(t, now)
	_ = w.PullEvents()
	actor := newID(t)

	require.NoError(t, w.Start(actor, now))
	require.Equal(t, workitem.StateInProgress, w.State())
	evs := w.PullEvents()
	require.Len(t, evs, 1)
	_, ok := evs[0].(workitem.StartedEvent)
	require.True(t, ok)
}

func TestStart_IdempotentOnSelf(t *testing.T) {
	t.Parallel()
	now := fixedNow
	w := mustNewManual(t, now)
	_ = w.PullEvents()
	actor := newID(t)
	require.NoError(t, w.Start(actor, now))
	_ = w.PullEvents()
	require.NoError(t, w.Start(actor, now))
	require.Empty(t, w.PullEvents()) // no event on idempotent re-start
}

func TestStart_RejectsTerminal(t *testing.T) {
	t.Parallel()
	now := fixedNow
	w := mustNewManual(t, now)
	actor := newID(t)
	require.NoError(t, w.Complete(actor, now))

	err := w.Start(actor, now)
	require.ErrorIs(t, err, workitem.ErrConflict)
}

func TestStart_RejectsOverdue(t *testing.T) {
	t.Parallel()
	now := fixedNow
	w := mustNewManual(t, now)
	require.NoError(t, w.MarkOverdue(now))

	err := w.Start(newID(t), now)
	require.ErrorIs(t, err, workitem.ErrConflict)
}

func TestComplete_Transitions(t *testing.T) {
	t.Parallel()
	now := fixedNow
	w := mustNewManual(t, now)
	_ = w.PullEvents()
	actor := newID(t)
	require.NoError(t, w.Complete(actor, now))
	require.Equal(t, workitem.StateCompleted, w.State())
	require.False(t, w.CompletedAt().IsZero())
	evs := w.PullEvents()
	require.Len(t, evs, 1)
}

func TestComplete_FromInProgress(t *testing.T) {
	t.Parallel()
	now := fixedNow
	w := mustNewManual(t, now)
	actor := newID(t)
	require.NoError(t, w.Start(actor, now))
	require.NoError(t, w.Complete(actor, now))
	require.Equal(t, workitem.StateCompleted, w.State())
}

func TestComplete_FromOverdue(t *testing.T) {
	t.Parallel()
	now := fixedNow
	w := mustNewManual(t, now)
	require.NoError(t, w.MarkOverdue(now))
	require.NoError(t, w.Complete(newID(t), now))
	require.Equal(t, workitem.StateCompleted, w.State())
}

func TestComplete_IdempotentOnSelf(t *testing.T) {
	t.Parallel()
	now := fixedNow
	w := mustNewManual(t, now)
	actor := newID(t)
	require.NoError(t, w.Complete(actor, now))
	_ = w.PullEvents()
	require.NoError(t, w.Complete(actor, now))
	require.Empty(t, w.PullEvents())
}

func TestComplete_RejectsCancelled(t *testing.T) {
	t.Parallel()
	now := fixedNow
	w := mustNewManual(t, now)
	require.NoError(t, w.Cancel(newID(t), "dup", now))
	err := w.Complete(newID(t), now)
	require.ErrorIs(t, err, workitem.ErrConflict)
}

func TestCancel_RequiresReason(t *testing.T) {
	t.Parallel()
	now := fixedNow
	w := mustNewManual(t, now)
	err := w.Cancel(newID(t), "  ", now)
	require.ErrorIs(t, err, workitem.ErrInvalid)
}

func TestCancel_Transitions(t *testing.T) {
	t.Parallel()
	now := fixedNow
	w := mustNewManual(t, now)
	_ = w.PullEvents()
	actor := newID(t)
	require.NoError(t, w.Cancel(actor, "no longer relevant", now))
	require.Equal(t, workitem.StateCancelled, w.State())
	require.Equal(t, "no longer relevant", w.CancellationReason())
	evs := w.PullEvents()
	require.Len(t, evs, 1)
	cancel, ok := evs[0].(workitem.CancelledEvent)
	require.True(t, ok)
	require.Equal(t, "no longer relevant", cancel.Reason)
}

func TestCancel_StrictIdempotent(t *testing.T) {
	t.Parallel()
	now := fixedNow
	w := mustNewManual(t, now)
	actor := newID(t)
	require.NoError(t, w.Cancel(actor, "dup", now))
	_ = w.PullEvents()
	require.NoError(t, w.Cancel(actor, "dup", now)) // same reason → no-op
	require.Empty(t, w.PullEvents())

	err := w.Cancel(actor, "different reason", now)
	require.ErrorIs(t, err, workitem.ErrConflict)
}

func TestCancel_RejectsCompleted(t *testing.T) {
	t.Parallel()
	now := fixedNow
	w := mustNewManual(t, now)
	require.NoError(t, w.Complete(newID(t), now))
	err := w.Cancel(newID(t), "too late", now)
	require.ErrorIs(t, err, workitem.ErrConflict)
}

func TestMarkOverdue_FromPending(t *testing.T) {
	t.Parallel()
	now := fixedNow
	w := mustNewManual(t, now)
	_ = w.PullEvents()
	require.NoError(t, w.MarkOverdue(now))
	require.Equal(t, workitem.StateOverdue, w.State())
	evs := w.PullEvents()
	require.Len(t, evs, 1)
	_, ok := evs[0].(workitem.OverdueEvent)
	require.True(t, ok)
}

func TestMarkOverdue_FromInProgress(t *testing.T) {
	t.Parallel()
	now := fixedNow
	w := mustNewManual(t, now)
	require.NoError(t, w.Start(newID(t), now))
	_ = w.PullEvents()
	require.NoError(t, w.MarkOverdue(now))
	require.Equal(t, workitem.StateOverdue, w.State())
}

func TestMarkOverdue_IdempotentSelfAndTerminal(t *testing.T) {
	t.Parallel()
	now := fixedNow
	w := mustNewManual(t, now)
	require.NoError(t, w.MarkOverdue(now))
	_ = w.PullEvents()
	require.NoError(t, w.MarkOverdue(now)) // already overdue
	require.Empty(t, w.PullEvents())

	w2 := mustNewManual(t, now)
	require.NoError(t, w2.Complete(newID(t), now))
	_ = w2.PullEvents()
	require.NoError(t, w2.MarkOverdue(now)) // completed: silent no-op
	require.Empty(t, w2.PullEvents())
}

func TestReassign_Transitions(t *testing.T) {
	t.Parallel()
	now := fixedNow
	w := mustNewManual(t, now)
	_ = w.PullEvents()
	newAssignee := newID(t)
	actor := newID(t)
	require.NoError(t, w.Reassign(newAssignee, actor, "balancing load", now))
	require.Equal(t, newAssignee, w.AssignedToMembershipID())
	evs := w.PullEvents()
	require.Len(t, evs, 1)
	r, ok := evs[0].(workitem.ReassignedEvent)
	require.True(t, ok)
	require.Equal(t, newAssignee, r.NewAssigneeMembershipID)
}

func TestReassign_IdempotentSelf(t *testing.T) {
	t.Parallel()
	now := fixedNow
	w := mustNewManual(t, now)
	current := w.AssignedToMembershipID()
	_ = w.PullEvents()
	require.NoError(t, w.Reassign(current, newID(t), "", now))
	require.Empty(t, w.PullEvents())
}

func TestReassign_RejectsTerminal(t *testing.T) {
	t.Parallel()
	now := fixedNow
	w := mustNewManual(t, now)
	require.NoError(t, w.Complete(newID(t), now))
	err := w.Reassign(newID(t), newID(t), "", now)
	require.ErrorIs(t, err, workitem.ErrConflict)
}

func TestUnmarshalFromDB_PreservesFields(t *testing.T) {
	t.Parallel()
	now := fixedNow
	snap := workitem.Snapshot{
		ID:                     workitem.ID(newID(t)),
		TenantID:               tenant.ID(newID(t)),
		Type:                   workitem.TypeFollowUp,
		Priority:               workitem.PriorityHigh,
		State:                  workitem.StateInProgress,
		Title:                  "Snap",
		AssignedToMembershipID: newID(t),
		AssignedByMembershipID: newID(t),
		DueAt:                  now.Add(time.Hour),
		CreatedAt:              now,
		CreatedByMembershipID:  newID(t),
	}
	w := workitem.UnmarshalFromDB(snap)
	require.Equal(t, snap.ID, w.ID())
	require.Equal(t, snap.State, w.State())
	require.Equal(t, snap.Priority, w.Priority())
	require.Empty(t, w.PullEvents()) // rehydration emits no event
}

func TestSource_ValidateMixed(t *testing.T) {
	t.Parallel()
	now := fixedNow
	// partial source → invalid
	p := workitem.AutoCreateParams{
		ID:                     workitem.ID(newID(t)),
		TenantID:               tenant.ID(newID(t)),
		Type:                   workitem.TypeCallbackReminder,
		Priority:               workitem.PriorityHigh,
		Title:                  "X",
		AssignedToMembershipID: newID(t),
		AssignedByMembershipID: newID(t),
		CreatedByMembershipID:  newID(t),
		DueAt:                  now.Add(time.Hour),
		Now:                    now,
		Source: workitem.Source{
			Module:     "crm",
			EntityType: "", // partial
			EntityID:   newID(t),
		},
	}
	_, err := workitem.NewAutoCreated(p)
	require.ErrorIs(t, err, workitem.ErrInvalid)
}

func TestParsers(t *testing.T) {
	t.Parallel()
	tp, err := workitem.ParseType("manual")
	require.NoError(t, err)
	require.Equal(t, workitem.TypeManual, tp)

	_, err = workitem.ParseType("nope")
	require.ErrorIs(t, err, workitem.ErrInvalid)

	pr, err := workitem.ParsePriority("")
	require.NoError(t, err)
	require.Equal(t, workitem.PriorityMedium, pr)

	st, err := workitem.ParseState("overdue")
	require.NoError(t, err)
	require.Equal(t, workitem.StateOverdue, st)
}

// mustNewManual is the test helper that fails fast on unexpected
// invariant violations during setup. Pinned to a fixed `now` so tests
// remain deterministic.
func mustNewManual(t *testing.T, now time.Time) *workitem.WorkItem {
	t.Helper()
	w, err := workitem.NewManual(sampleParams(t, now))
	require.NoError(t, err)
	return w
}
