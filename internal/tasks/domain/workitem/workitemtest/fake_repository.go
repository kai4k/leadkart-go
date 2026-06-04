// Package workitemtest provides the in-memory FakeRepository
// implementing [workitem.Repository]. Per TDL canon — fake co-located
// with the aggregate, single-test-owner pattern, no sync primitives
// (domain subtree is concurrency-free).
package workitemtest

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/tasks/domain/workitem"
)

// FakeRepository is the in-memory implementation of
// [workitem.Repository]. Zero-value-NOT-usable — construct via
// [NewFakeRepository]. Single-test-owner: do NOT share one instance
// across tests; each test creates its own.
type FakeRepository struct {
	// ByID is the live work-item index. Soft-deleted items remain in
	// the map with their is_deleted flag mirrored on the aggregate's
	// state (the aggregate has no is_deleted bit; the fake tracks it
	// separately via the Deleted set).
	ByID map[workitem.ID]*workitem.WorkItem

	// Deleted is the soft-delete tombstone set. Items here are
	// excluded from GetByID / List* / GetOpenBySource / dashboard
	// counts (mirrors the SQL adapter's `WHERE NOT is_deleted` clause).
	Deleted map[workitem.ID]struct{}

	// EmittedEventsByID captures the per-item events drained via
	// PullEvents on each Add / committed UpdateByID. Tests assert on
	// the recorded slice to verify the integration-event surface.
	EmittedEventsByID map[workitem.ID][]workitem.Event
}

// NewFakeRepository returns an empty in-memory WorkItem repository.
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{
		ByID:              map[workitem.ID]*workitem.WorkItem{},
		Deleted:           map[workitem.ID]struct{}{},
		EmittedEventsByID: map[workitem.ID][]workitem.Event{},
	}
}

// Compile-time interface conformance gate.
var _ workitem.Repository = (*FakeRepository)(nil)

// Add persists a brand-new WorkItem. Returns a generic "already
// exists" error on ID collision (programmer-error path).
//
// Returns [workitem.ErrAlreadyExistsForSource] when an existing OPEN
// work item references the same (source.EntityType, source.EntityID)
// pair — mirrors the partial-unique-index 23505 from the SQL adapter.
func (r *FakeRepository) Add(_ context.Context, w *workitem.WorkItem) error {
	if _, exists := r.ByID[w.ID()]; exists {
		return errors.New("fake: work item id collision")
	}
	src := w.Source()
	if !src.IsZero() && w.State().IsOpen() {
		for _, existing := range r.ByID {
			if _, deleted := r.Deleted[existing.ID()]; deleted {
				continue
			}
			if existing.TenantID() != w.TenantID() {
				continue
			}
			if !existing.State().IsOpen() {
				continue
			}
			es := existing.Source()
			if es.EntityType == src.EntityType && es.EntityID == src.EntityID {
				return workitem.ErrAlreadyExistsForSource
			}
		}
	}
	r.ByID[w.ID()] = w
	r.EmittedEventsByID[w.ID()] = append(r.EmittedEventsByID[w.ID()], w.PullEvents()...)
	return nil
}

// UpdateByID mirrors the SQL adapter's UoW pattern.
func (r *FakeRepository) UpdateByID(_ context.Context, tenantID tenant.ID, id workitem.ID, fn func(*workitem.WorkItem) (bool, error)) error {
	w, ok := r.ByID[id]
	if !ok {
		return workitem.ErrNotFound
	}
	if _, deleted := r.Deleted[id]; deleted {
		return workitem.ErrNotFound
	}
	if w.TenantID() != tenantID {
		return workitem.ErrNotFound
	}
	persist, err := fn(w)
	if err != nil {
		return err
	}
	if persist {
		r.EmittedEventsByID[id] = append(r.EmittedEventsByID[id], w.PullEvents()...)
	} else {
		_ = w.PullEvents()
	}
	return nil
}

// GetByID returns the work item from the supplied tenant or
// [workitem.ErrNotFound] (including for soft-deleted rows).
func (r *FakeRepository) GetByID(_ context.Context, tenantID tenant.ID, id workitem.ID) (*workitem.WorkItem, error) {
	w, ok := r.ByID[id]
	if !ok {
		return nil, workitem.ErrNotFound
	}
	if _, deleted := r.Deleted[id]; deleted {
		return nil, workitem.ErrNotFound
	}
	if w.TenantID() != tenantID {
		return nil, workitem.ErrNotFound
	}
	return w, nil
}

// GetOpenBySource returns the single open work item for the source
// pair under the tenant scope, or [workitem.ErrNotFound] when none
// exists. The partial-unique-index invariant guarantees at-most-one.
func (r *FakeRepository) GetOpenBySource(_ context.Context, tenantID tenant.ID, entityType, entityID string) (*workitem.WorkItem, error) {
	for _, w := range r.ByID {
		if _, deleted := r.Deleted[w.ID()]; deleted {
			continue
		}
		if w.TenantID() != tenantID {
			continue
		}
		if !w.State().IsOpen() {
			continue
		}
		src := w.Source()
		if src.EntityType == entityType && src.EntityID == entityID {
			return w, nil
		}
	}
	return nil, workitem.ErrNotFound
}

// ListPage returns the first pageSize work items (scoped to tenantID,
// matching filter) sorted by (due_at DESC, id DESC). Slice 1 tests
// don't exercise the keyset cursor logic itself; adapter integration
// tests cover the SQL keyset under EXPLAIN.
func (r *FakeRepository) ListPage(_ context.Context, tenantID tenant.ID, filter workitem.ListFilter, _ pagination.Cursor, pageSize int) (pagination.Page[*workitem.WorkItem], error) {
	out := make([]*workitem.WorkItem, 0, len(r.ByID))
	for _, w := range r.ByID {
		if _, deleted := r.Deleted[w.ID()]; deleted {
			continue
		}
		if w.TenantID() != tenantID {
			continue
		}
		if !matchesFilter(w, filter) {
			continue
		}
		out = append(out, w)
	}
	slices.SortStableFunc(out, func(a, b *workitem.WorkItem) int {
		if !a.DueAt().Equal(b.DueAt()) {
			return b.DueAt().Compare(a.DueAt()) // due_at DESC
		}
		return cmp.Compare(b.ID(), a.ID()) // id DESC
	})
	if pageSize > 0 && len(out) > pageSize {
		out = out[:pageSize]
	}
	return pagination.Page[*workitem.WorkItem]{Items: out, HasMore: false}, nil
}

// ListOverdueCandidates returns up to `limit` work items in pending/
// in_progress state whose due_at < asOf. tenantID zero means all
// tenants (the scanner runs cross-tenant in production).
func (r *FakeRepository) ListOverdueCandidates(_ context.Context, tenantID tenant.ID, asOf time.Time, limit int) ([]*workitem.WorkItem, error) {
	out := []*workitem.WorkItem{}
	for _, w := range r.ByID {
		if _, deleted := r.Deleted[w.ID()]; deleted {
			continue
		}
		if !tenantID.IsZero() && w.TenantID() != tenantID {
			continue
		}
		if w.State() != workitem.StatePending && w.State() != workitem.StateInProgress {
			continue
		}
		if !w.DueAt().Before(asOf) {
			continue
		}
		out = append(out, w)
	}
	slices.SortStableFunc(out, func(a, b *workitem.WorkItem) int {
		return a.DueAt().Compare(b.DueAt()) // due_at ASC
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ListPurgeCandidates returns up to `limit` terminal work items with
// terminal timestamps < before.
func (r *FakeRepository) ListPurgeCandidates(_ context.Context, tenantID tenant.ID, before time.Time, limit int) ([]*workitem.WorkItem, error) {
	out := []*workitem.WorkItem{}
	for _, w := range r.ByID {
		if _, deleted := r.Deleted[w.ID()]; deleted {
			continue
		}
		if !tenantID.IsZero() && w.TenantID() != tenantID {
			continue
		}
		switch w.State() {
		case workitem.StateCompleted:
			if !w.CompletedAt().Before(before) {
				continue
			}
		case workitem.StateCancelled:
			if !w.CancelledAt().Before(before) {
				continue
			}
		default:
			continue
		}
		out = append(out, w)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// DeleteByID flips the soft-delete tombstone. Idempotent — already-
// deleted returns nil.
func (r *FakeRepository) DeleteByID(_ context.Context, tenantID tenant.ID, id workitem.ID) error {
	w, ok := r.ByID[id]
	if !ok {
		return workitem.ErrNotFound
	}
	if w.TenantID() != tenantID {
		return workitem.ErrNotFound
	}
	r.Deleted[id] = struct{}{}
	return nil
}

// CountDashboard mirrors the SQL CTE in the adapter. Filters by
// visibleMembershipIDs (set semantics) — empty/nil means count
// everything in the tenant.
func (r *FakeRepository) CountDashboard(_ context.Context, tenantID tenant.ID, visibleMembershipIDs []string, asOf time.Time) (workitem.DashboardCounts, error) {
	visible := map[string]struct{}{}
	for _, id := range visibleMembershipIDs {
		visible[id] = struct{}{}
	}
	hasFilter := len(visible) > 0

	asOfDay := truncateDay(asOf)
	out := workitem.DashboardCounts{}
	for _, w := range r.ByID {
		if _, deleted := r.Deleted[w.ID()]; deleted {
			continue
		}
		if w.TenantID() != tenantID {
			continue
		}
		if hasFilter {
			if _, ok := visible[w.AssignedToMembershipID()]; !ok {
				continue
			}
		}
		dueDay := truncateDay(w.DueAt())
		switch w.State() {
		case workitem.StatePending, workitem.StateInProgress:
			if w.State() == workitem.StatePending {
				out.TotalPending++
			}
			if dueDay.Equal(asOfDay) {
				out.Today++
			} else if dueDay.After(asOfDay) {
				out.Upcoming++
			}
		case workitem.StateOverdue:
			out.Overdue++
		case workitem.StateCompleted:
			if truncateDay(w.CompletedAt()).Equal(asOfDay) {
				out.CompletedToday++
			}
		}
	}
	return out, nil
}

func truncateDay(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func matchesFilter(w *workitem.WorkItem, f workitem.ListFilter) bool {
	if f.State != "" && w.State() != f.State {
		return false
	}
	if f.Type != "" && w.Type() != f.Type {
		return false
	}
	if f.Priority != "" && w.Priority() != f.Priority {
		return false
	}
	if f.SelfFilter != "" {
		if w.AssignedToMembershipID() != f.SelfFilter {
			return false
		}
	} else if f.AssignedToMembershipID != "" && w.AssignedToMembershipID() != f.AssignedToMembershipID {
		return false
	}
	if f.BatchID != "" && w.BatchID() != f.BatchID {
		return false
	}
	if !f.DueBefore.IsZero() && !w.DueAt().Before(f.DueBefore) {
		return false
	}
	if !f.DueAfter.IsZero() && !w.DueAt().After(f.DueAfter) {
		return false
	}
	return true
}
