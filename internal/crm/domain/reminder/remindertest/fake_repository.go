// Package remindertest provides the in-memory FakeRepository
// implementing [reminder.Repository] per TDL canon (ThreeDotsLabs Wild
// Workouts + "Go with the Domain" ch. 8 — fakes, not mocks).
//
// Co-located with the aggregate (`<aggregate>test/`) per the rule
// enforced by TestArch_TDL_*. Single-test-owner — each test creates its
// OWN [FakeRepository] via [NewFakeRepository]; t.Parallel is naturally
// safe because no two tests share the same fake instance.
//
// Contract fidelity: the fake mirrors the SQL adapter's behavior for
// every assertion the unit tests rely on:
//
//   - Cross-tenant reads return [reminder.ErrNotFound].
//   - Add returns [reminder.ErrAlreadyExists] when the partial unique
//     index would fire (TypeCallback + (tenant, source_call_log_id)
//     while pending; TypeMatureLead + (tenant, lead_id) while pending).
//   - UpdateByID drains aggregate events into the per-reminder
//     EmittedEvents slice for assertions.
package remindertest

import (
	"cmp"
	"context"
	"slices"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// FakeRepository is the in-memory implementation of
// [reminder.Repository]. Zero-value-NOT-usable — construct via
// [NewFakeRepository] so the internal maps are initialised.
// Single-test-owner — do NOT share one instance across parallel tests
// (no internal sync, per the domain-subtree concurrency-free rule).
type FakeRepository struct {
	// ByID is the live reminder index.
	ByID map[reminder.ID]*reminder.Reminder

	// EmittedEventsByReminder captures the per-reminder events drained
	// via PullEvents on each Add / committed UpdateByID. Tests assert
	// on this slice to verify the integration-event surface.
	EmittedEventsByReminder map[reminder.ID][]reminder.Event
}

// NewFakeRepository returns an empty in-memory reminder repository.
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{
		ByID:                    map[reminder.ID]*reminder.Reminder{},
		EmittedEventsByReminder: map[reminder.ID][]reminder.Event{},
	}
}

// Compile-time interface conformance gate.
var _ reminder.Repository = (*FakeRepository)(nil)

// Add persists a brand-new reminder. Returns
// [reminder.ErrAlreadyExists] when the partial unique indices would
// fire — mirrors the adapter's SQLSTATE 23505 translation.
//
// Drains aggregate events via PullEvents into [EmittedEventsByReminder].
func (r *FakeRepository) Add(_ context.Context, rem *reminder.Reminder) error {
	if _, exists := r.ByID[rem.ID()]; exists {
		return reminder.ErrAlreadyExists
	}
	// Partial unique gates — pending only.
	if rem.State() == reminder.StatePending {
		switch rem.Type() {
		case reminder.TypeCallback:
			for _, existing := range r.ByID {
				if existing.TenantID() != rem.TenantID() {
					continue
				}
				if existing.Type() != reminder.TypeCallback {
					continue
				}
				if existing.State() != reminder.StatePending {
					continue
				}
				if existing.SourceCallLogID() == "" {
					continue
				}
				if existing.SourceCallLogID() == rem.SourceCallLogID() {
					return reminder.ErrAlreadyExists
				}
			}
		case reminder.TypeMatureLead:
			for _, existing := range r.ByID {
				if existing.TenantID() != rem.TenantID() {
					continue
				}
				if existing.Type() != reminder.TypeMatureLead {
					continue
				}
				if existing.State() != reminder.StatePending {
					continue
				}
				if existing.LeadID() == rem.LeadID() {
					return reminder.ErrAlreadyExists
				}
			}
		case reminder.TypeManual:
			// No partial unique on manual reminders — users may set
			// multiple manual reminders against the same lead.
		}
	}
	r.ByID[rem.ID()] = rem
	r.EmittedEventsByReminder[rem.ID()] = append(r.EmittedEventsByReminder[rem.ID()], rem.PullEvents()...)
	return nil
}

// UpdateByID loads (scoped to tenantID), mutates via fn, then either
// persists (commit=true) or rolls back (commit=false / err). Returns
// [reminder.ErrNotFound] when the row doesn't exist OR belongs to a
// different tenant.
func (r *FakeRepository) UpdateByID(_ context.Context, tenantID tenant.ID, id reminder.ID, fn func(*reminder.Reminder) (bool, error)) error {
	rem, ok := r.ByID[id]
	if !ok {
		return reminder.ErrNotFound
	}
	if rem.TenantID() != tenantID {
		return reminder.ErrNotFound
	}
	persist, err := fn(rem)
	if err != nil {
		return err
	}
	if persist {
		r.EmittedEventsByReminder[id] = append(r.EmittedEventsByReminder[id], rem.PullEvents()...)
	} else {
		_ = rem.PullEvents()
	}
	return nil
}

// GetByID returns the reminder from the supplied tenant or
// [reminder.ErrNotFound].
func (r *FakeRepository) GetByID(_ context.Context, tenantID tenant.ID, id reminder.ID) (*reminder.Reminder, error) {
	rem, ok := r.ByID[id]
	if !ok {
		return nil, reminder.ErrNotFound
	}
	if rem.TenantID() != tenantID {
		return nil, reminder.ErrNotFound
	}
	return rem, nil
}

// ListPagePending returns the pending reminders matching filter, sorted
// by (due_at ASC, id ASC) — same sort tuple as the adapter's keyset
// index. PageSize is honored but cursor pagination is simplified (no
// keyset arithmetic at the fake layer; adapter integration tests cover
// the real keyset path).
func (r *FakeRepository) ListPagePending(_ context.Context, tenantID tenant.ID, filter reminder.PendingFilter, _ pagination.Cursor, pageSize int) (pagination.Page[*reminder.Reminder], error) {
	out := make([]*reminder.Reminder, 0, len(r.ByID))
	for _, rem := range r.ByID {
		if rem.TenantID() != tenantID {
			continue
		}
		if rem.State() != reminder.StatePending {
			continue
		}
		if filter.AssigneeMembershipID != "" && rem.AssignedToMembershipID() != filter.AssigneeMembershipID {
			continue
		}
		if filter.Type != "" && rem.Type() != filter.Type {
			continue
		}
		if !filter.LeadID.IsZero() && rem.LeadID() != filter.LeadID {
			continue
		}
		out = append(out, rem)
	}
	slices.SortFunc(out, func(a, b *reminder.Reminder) int {
		if a.DueAt().Equal(b.DueAt()) {
			return cmp.Compare(a.ID(), b.ID())
		}
		if a.DueAt().Before(b.DueAt()) {
			return -1
		}
		return 1
	})
	if pageSize > 0 && len(out) > pageSize {
		out = out[:pageSize]
	}
	return pagination.Page[*reminder.Reminder]{Items: out, HasMore: false}, nil
}

// FindPendingMatureForLead returns the open mature-lead reminder for
// the supplied lead, or [reminder.ErrNotFound].
func (r *FakeRepository) FindPendingMatureForLead(_ context.Context, tenantID tenant.ID, leadID crmlead.ID) (*reminder.Reminder, error) {
	for _, rem := range r.ByID {
		if rem.TenantID() != tenantID {
			continue
		}
		if rem.LeadID() != leadID {
			continue
		}
		if rem.Type() != reminder.TypeMatureLead {
			continue
		}
		if rem.State() != reminder.StatePending {
			continue
		}
		return rem, nil
	}
	return nil, reminder.ErrNotFound
}
