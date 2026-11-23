package jobs_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/leadkart/leadkart-go/internal/companysettings/app/command"
	"github.com/leadkart/leadkart-go/internal/companysettings/app/jobs"
)

type fakeLister struct {
	ids   []uuid.UUID // sorted
	err   error
	pages int
}

func (l *fakeLister) ListStoredTenants(_ context.Context, after uuid.UUID, limit int) ([]uuid.UUID, error) {
	l.pages++
	if l.err != nil {
		return nil, l.err
	}
	var out []uuid.UUID
	for _, id := range l.ids {
		if after != uuid.Nil && id.String() <= after.String() {
			continue
		}
		out = append(out, id)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

type fakeRepublisher struct {
	seen    []string
	failFor map[string]error
}

func (r *fakeRepublisher) Handle(_ context.Context, cmd command.RepublishCommand) error {
	r.seen = append(r.seen, cmd.TenantID.String())
	return r.failFor[cmd.TenantID.String()]
}

func silent() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func sortedIDs(n int) []uuid.UUID {
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = uuid.New()
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int {
		switch {
		case a.String() < b.String():
			return -1
		case a.String() > b.String():
			return 1
		}
		return 0
	})
	return ids
}

func TestRepublishWorker_ReannouncesEveryStoredTenantAcrossPages(t *testing.T) {
	t.Parallel()
	lister := &fakeLister{ids: sortedIDs(jobs.PageSize + 7)}
	rep := &fakeRepublisher{}
	w := jobs.NewRepublishWorker(lister, rep, silent())
	if err := w.Work(t.Context(), &river.Job[jobs.RepublishJob]{}); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if len(rep.seen) != jobs.PageSize+7 || lister.pages != 2 {
		t.Fatalf("republished %d tenants over %d pages, want %d over 2", len(rep.seen), lister.pages, jobs.PageSize+7)
	}
}

func TestRepublishWorker_OneTenantFailingDoesNotStopTheSweepButFailsTheJob(t *testing.T) {
	t.Parallel()
	ids := sortedIDs(3)
	lister := &fakeLister{ids: ids}
	rep := &fakeRepublisher{failFor: map[string]error{ids[1].String(): errors.New("boom")}}
	w := jobs.NewRepublishWorker(lister, rep, silent())
	err := w.Work(t.Context(), &river.Job[jobs.RepublishJob]{})
	if err == nil {
		t.Fatal("a failed tenant must fail the job so river retries it")
	}
	if len(rep.seen) != 3 {
		t.Fatalf("the sweep stopped at the failure: %v", rep.seen)
	}
}

func TestRepublishWorker_ListFailureFailsTheJobAndKindIsStable(t *testing.T) {
	t.Parallel()
	w := jobs.NewRepublishWorker(&fakeLister{err: errors.New("db down")}, &fakeRepublisher{}, silent())
	if err := w.Work(t.Context(), &river.Job[jobs.RepublishJob]{}); err == nil {
		t.Fatal("a listing failure must fail the job")
	}
	if got := (jobs.RepublishJob{}).Kind(); got != "companysettings.republish_policies" {
		t.Fatalf("kind = %q: it is a river-stable name", got)
	}
}
