// Package crmleadtest provides the in-memory FakeRepository implementing
// [crmlead.Repository]. Used by app-layer handler tests + downstream
// integration scenarios that need a working CrmLead store without a
// Postgres dependency.
//
// TDL CANON (ThreeDotsLabs Wild Workouts + "Go with the Domain"):
//
//   - The fake lives in a sibling package <aggregate>test/, co-located
//     with the domain aggregate it implements an interface from. Same
//     visibility surface as the aggregate itself; no special test-side
//     plumbing.
//   - The fake is a FAITHFUL implementation of [crmlead.Repository] —
//     not a mock-with-canned-responses. It honors every contract
//     guarantee: ErrNotFound on missing IDs, source-purchase-id
//     idempotency lookup (mirrors the subscriber path's at-most-once
//     check), and append-only drain of aggregate events into a
//     per-lead emitted-events slice for assertions.
//   - Single-test-owner pattern: each test creates its OWN
//     FakeRepository via [NewFakeRepository] — no shared mutable state
//     across tests. t.Parallel is naturally safe because no two tests
//     share the same fake instance. This is TDL canon: fakes don't
//     need sync primitives because they're per-test, and putting sync
//     in domain-co-located test packages would trip
//     TestArch_NoGoroutinesInDomain (domain layer is concurrency-free
//     by design — Bryan Mills "Rethinking Concurrency Patterns").
//
// Why fakes, not mocks: per TDL "Go with the Domain" ch. 8, mocks
// couple the test to the call-pattern of the SUT (Subject Under Test);
// fakes couple to the CONTRACT. Refactoring the SUT to use the
// interface differently breaks mock-tests but leaves fake-tests
// green. The contract is the load-bearing thing.
package crmleadtest

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
)

// FakeRepository is the in-memory implementation of
// [crmlead.Repository]. Zero-value-NOT-usable — construct via
// [NewFakeRepository] so the internal maps are initialised.
// Single-test-owner: do NOT share one instance across tests; each test
// creates its own.
type FakeRepository struct {
	// ByID is the live lead index. CrmLeads are not row-level soft-
	// deleted at slice 1; every Add lands here.
	ByID map[crmlead.ID]*crmlead.CrmLead

	// ByPurchase is the (source_purchase_id) → ID index. Mirrors the
	// adapter's `GetBySourcePurchaseID` index lookup — the subscriber
	// path uses it to detect at-most-once-per-purchase ingestion.
	ByPurchase map[string]crmlead.ID

	// EmittedEventsByLead captures the per-lead events drained via
	// PullEvents on each Add / committed UpdateByID. Tests assert on
	// the recorded slice to verify the integration-event surface.
	EmittedEventsByLead map[crmlead.ID][]crmlead.Event
}

// NewFakeRepository returns an empty in-memory CrmLead repository.
// Single-test-owner — each test should construct its own instance;
// do NOT share one fake across parallel tests (no internal sync).
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{
		ByID:                map[crmlead.ID]*crmlead.CrmLead{},
		ByPurchase:          map[string]crmlead.ID{},
		EmittedEventsByLead: map[crmlead.ID][]crmlead.Event{},
	}
}

// Compile-time interface conformance gate. Drift in
// [crmlead.Repository] (a method renamed, signature changed) breaks
// at build time before any test runs.
var _ crmlead.Repository = (*FakeRepository)(nil)

// Add persists a brand-new CrmLead. Returns a generic "already exists"
// error on ID collision (programmer-error path — UUIDv7 generation
// guarantees no collisions in practice). Returns a
// "source_purchase_id collision" error when an existing lead already
// references the same purchase — mirrors the adapter's UNIQUE-violation
// surface for the (source_purchase_id) partial unique index that
// underpins the subscriber's at-most-once guard.
//
// Drains aggregate events via PullEvents into
// [FakeRepository.EmittedEventsByLead] so tests can assert on the
// integration-event shape without a real outbox.
func (r *FakeRepository) Add(_ context.Context, l *crmlead.CrmLead) error {

	if _, exists := r.ByID[l.ID()]; exists {
		return errors.New("fake: lead already exists")
	}
	if l.SourcePurchaseID() != "" {
		if _, dup := r.ByPurchase[l.SourcePurchaseID()]; dup {
			return errors.New("fake: source_purchase_id collision")
		}
		r.ByPurchase[l.SourcePurchaseID()] = l.ID()
	}
	r.ByID[l.ID()] = l
	r.EmittedEventsByLead[l.ID()] = append(r.EmittedEventsByLead[l.ID()], l.PullEvents()...)
	return nil
}

// UpdateByID loads, mutates via fn, then either persists (commit=true)
// or rolls back (commit=false / err). Returns [crmlead.ErrNotFound]
// when the row doesn't exist.
//
// On commit, drains the aggregate's events into
// [FakeRepository.EmittedEventsByLead]. On abort, drains-and-discards
// to keep the aggregate's internal event buffer clean (mirrors the
// adapter which only writes-to-outbox when the surrounding tx commits).
//
// The fake doesn't deep-copy the lead before passing to fn; the caller
// observes mutations even if it returns (false, nil). This mirrors the
// pg adapter's behavior — both rely on the aggregate's invariants
// being re-checked at persist time, not snapshot-rollback.
func (r *FakeRepository) UpdateByID(_ context.Context, id crmlead.ID, fn func(*crmlead.CrmLead) (bool, error)) error {

	l, ok := r.ByID[id]
	if !ok {
		return crmlead.ErrNotFound
	}
	persist, err := fn(l)
	if err != nil {
		return err
	}
	if persist {
		r.EmittedEventsByLead[id] = append(r.EmittedEventsByLead[id], l.PullEvents()...)
	} else {
		// Drain emitted-but-not-persisted to keep state clean.
		_ = l.PullEvents()
	}
	return nil
}

// GetByID returns the lead or [crmlead.ErrNotFound].
func (r *FakeRepository) GetByID(_ context.Context, id crmlead.ID) (*crmlead.CrmLead, error) {

	l, ok := r.ByID[id]
	if !ok {
		return nil, crmlead.ErrNotFound
	}
	return l, nil
}

// GetBySourcePurchaseID returns the lead minted from the supplied
// Platform purchase event or [crmlead.ErrNotFound] when no such lead
// exists. Underpins the subscriber's at-most-once-per-purchase
// idempotency check.
func (r *FakeRepository) GetBySourcePurchaseID(_ context.Context, purchaseID string) (*crmlead.CrmLead, error) {

	id, ok := r.ByPurchase[purchaseID]
	if !ok {
		return nil, crmlead.ErrNotFound
	}
	return r.ByID[id], nil
}

// ListPage returns the first pageSize leads in map-iteration order
// (unspecified). Slice 1 unit tests don't exercise the sort tuple +
// keyset cursor logic — those are covered by adapter integration
// tests against the actual `created_at DESC, id DESC` index.
func (r *FakeRepository) ListPage(_ context.Context, _ crmlead.ListFilter, _ pagination.Cursor, pageSize int) (pagination.Page[*crmlead.CrmLead], error) {

	out := make([]*crmlead.CrmLead, 0, len(r.ByID))
	for _, l := range r.ByID {
		out = append(out, l)
	}
	if pageSize > 0 && len(out) > pageSize {
		out = out[:pageSize]
	}
	return pagination.Page[*crmlead.CrmLead]{Items: out, HasMore: false}, nil
}
