// Package platformtest provides in-memory test doubles for the
// Platform module — fakes per `tdd.md` "Fakes pattern". Used by
// app/command/*_test.go to exercise handlers without touching pgx.
//
// All fakes are goroutine-safe; uow + outbox propagation models the
// production shape (UoW closure runs the fn, fakes pretend they
// joined the tx by writing to their in-memory slice).
package platformtest

import (
	"context"
	"sort"
	"sync"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/platform/app/query"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit"
	"github.com/leadkart/leadkart-go/internal/platform/domain/platformlead"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
	"github.com/leadkart/leadkart-go/internal/platform/domain/verificationcall"
	"github.com/leadkart/leadkart-go/internal/platform/integrationevents"
)

// FakeUnitOfWork runs fn immediately — no real transactional semantics
// (callers' adapters in tests are in-memory). Matches the
// pg.UnitOfWork contract.
type FakeUnitOfWork struct{}

// WithinTx satisfies [pg.UnitOfWork].
func (FakeUnitOfWork) WithinTx(ctx context.Context, _ pg.TxScope, fn func(ctx context.Context) error) error {
	return fn(ctx)
}

// FakeOutbox captures enqueued events for assertions. Goroutine-safe.
type FakeOutbox struct {
	mu     sync.Mutex
	Events []integrationevents.Event
}

// NewFakeOutbox returns an empty fake.
func NewFakeOutbox() *FakeOutbox { return &FakeOutbox{} }

// EnqueueInTx satisfies the app-layer OutboxEnqueuer interface.
func (f *FakeOutbox) EnqueueInTx(_ context.Context, events ...integrationevents.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Events = append(f.Events, events...)
	return nil
}

// Reset clears recorded events. Useful between sub-tests.
func (f *FakeOutbox) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Events = nil
}

// ----- FakeUnverifiedContactRepository --------------------------------------

// FakeUnverifiedContactRepository is an in-memory store. Also captures
// pulled domain events on Add/UpdateByID so tests can assert they were
// drained.
type FakeUnverifiedContactRepository struct {
	mu             sync.Mutex
	Store          map[unverifiedcontact.ID]*unverifiedcontact.UnverifiedContact
	DrainedEvents  []unverifiedcontact.Event
}

// NewFakeUnverifiedContactRepository returns an empty fake.
func NewFakeUnverifiedContactRepository() *FakeUnverifiedContactRepository {
	return &FakeUnverifiedContactRepository{
		Store: make(map[unverifiedcontact.ID]*unverifiedcontact.UnverifiedContact),
	}
}

// Add satisfies [unverifiedcontact.Repository].
func (r *FakeUnverifiedContactRepository) Add(_ context.Context, c *unverifiedcontact.UnverifiedContact) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Store[c.ID()] = c
	r.DrainedEvents = append(r.DrainedEvents, c.PullEvents()...)
	return nil
}

// UpdateByID satisfies [unverifiedcontact.Repository]. Re-hydrates a
// fresh aggregate copy on each call so the mutator's state-machine
// transitions reflect the canonical re-load-mutate-persist shape.
func (r *FakeUnverifiedContactRepository) UpdateByID(
	_ context.Context,
	id unverifiedcontact.ID,
	updateFn func(*unverifiedcontact.UnverifiedContact) (bool, error),
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.Store[id]
	if !ok {
		return unverifiedcontact.ErrNotFound
	}
	shouldPersist, err := updateFn(c)
	if err != nil {
		return err
	}
	if !shouldPersist {
		return nil
	}
	r.DrainedEvents = append(r.DrainedEvents, c.PullEvents()...)
	return nil
}

// GetByID satisfies [unverifiedcontact.Repository].
func (r *FakeUnverifiedContactRepository) GetByID(
	_ context.Context, id unverifiedcontact.ID,
) (*unverifiedcontact.UnverifiedContact, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.Store[id]
	if !ok {
		return nil, unverifiedcontact.ErrNotFound
	}
	return c, nil
}

// ----- FakeVerificationCallRepository --------------------------------------

// FakeVerificationCallRepository is an in-memory store.
type FakeVerificationCallRepository struct {
	mu            sync.Mutex
	Store         []*verificationcall.VerificationCall
	DrainedEvents []verificationcall.Event
}

// NewFakeVerificationCallRepository returns an empty fake.
func NewFakeVerificationCallRepository() *FakeVerificationCallRepository {
	return &FakeVerificationCallRepository{}
}

// Add satisfies [verificationcall.Repository].
func (r *FakeVerificationCallRepository) Add(_ context.Context, c *verificationcall.VerificationCall) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Store = append(r.Store, c)
	r.DrainedEvents = append(r.DrainedEvents, c.PullEvents()...)
	return nil
}

// ListByContact satisfies [verificationcall.Repository].
func (r *FakeVerificationCallRepository) ListByContact(
	_ context.Context, contactID unverifiedcontact.ID,
) ([]*verificationcall.VerificationCall, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*verificationcall.VerificationCall
	for _, c := range r.Store {
		if c.ContactID() == contactID {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LoggedAt().After(out[j].LoggedAt())
	})
	return out, nil
}

// ----- FakePlatformLeadRepository ------------------------------------------

// FakePlatformLeadRepository is an in-memory store.
type FakePlatformLeadRepository struct {
	mu            sync.Mutex
	Store         map[platformlead.ID]*platformlead.PlatformLead
	DrainedEvents []platformlead.Event
}

// NewFakePlatformLeadRepository returns an empty fake.
func NewFakePlatformLeadRepository() *FakePlatformLeadRepository {
	return &FakePlatformLeadRepository{
		Store: make(map[platformlead.ID]*platformlead.PlatformLead),
	}
}

// Add satisfies [platformlead.Repository].
func (r *FakePlatformLeadRepository) Add(_ context.Context, l *platformlead.PlatformLead) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Store[l.ID()] = l
	r.DrainedEvents = append(r.DrainedEvents, l.PullEvents()...)
	return nil
}

// UpdateByID satisfies [platformlead.Repository].
func (r *FakePlatformLeadRepository) UpdateByID(
	_ context.Context,
	id platformlead.ID,
	updateFn func(*platformlead.PlatformLead) (bool, error),
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.Store[id]
	if !ok {
		return platformlead.ErrNotFound
	}
	shouldPersist, err := updateFn(l)
	if err != nil {
		return err
	}
	if !shouldPersist {
		return nil
	}
	r.DrainedEvents = append(r.DrainedEvents, l.PullEvents()...)
	return nil
}

// GetByID satisfies [platformlead.Repository].
func (r *FakePlatformLeadRepository) GetByID(
	_ context.Context, id platformlead.ID,
) (*platformlead.PlatformLead, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.Store[id]
	if !ok {
		return nil, platformlead.ErrNotFound
	}
	return l, nil
}

// MarketplaceBrowse satisfies [platformlead.Repository]. Returns ALL
// unsold leads in insertion order — Slice 1 tests don't exercise the
// filter set; the integration test suite does.
func (r *FakePlatformLeadRepository) MarketplaceBrowse(
	_ context.Context,
	_ platformlead.MarketplaceFilter,
	_ pagination.Cursor,
	pageSize int,
) ([]*platformlead.PlatformLead, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*platformlead.PlatformLead
	for _, l := range r.Store {
		if l.IsAvailable() {
			out = append(out, l)
		}
		if len(out) >= pageSize {
			break
		}
	}
	return out, nil
}

// ----- FakeLeadCreditRepository --------------------------------------------

// FakeLeadCreditRepository is an in-memory store with version checks.
type FakeLeadCreditRepository struct {
	mu            sync.Mutex
	Store         map[leadcredit.TenantID]*leadcredit.LeadCredit
	versions      map[leadcredit.TenantID]int64 // last persisted version
	DrainedEvents []leadcredit.Event
	// ForceConflictOnce flips the next UpsertWithVersion to return
	// ErrConflict. Used by retry tests.
	ForceConflictOnce bool
}

// NewFakeLeadCreditRepository returns an empty fake.
func NewFakeLeadCreditRepository() *FakeLeadCreditRepository {
	return &FakeLeadCreditRepository{
		Store:    make(map[leadcredit.TenantID]*leadcredit.LeadCredit),
		versions: make(map[leadcredit.TenantID]int64),
	}
}

// GetByTenant satisfies [leadcredit.Repository].
func (r *FakeLeadCreditRepository) GetByTenant(
	_ context.Context, id leadcredit.TenantID,
) (*leadcredit.LeadCredit, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.Store[id]
	if !ok {
		return nil, leadcredit.ErrNotFound
	}
	// Return a fresh hydration from snapshot so callers' mutations
	// don't leak across calls — same shape as a real pgx-backed read.
	snap := leadcredit.Snapshot{
		TenantID:  c.TenantID(),
		Balance:   c.Balance(),
		Version:   c.Version(),
		CreatedAt: c.CreatedAt(),
		UpdatedAt: c.UpdatedAt(),
	}
	return leadcredit.UnmarshalFromDB(snap), nil
}

// UpsertWithVersion satisfies [leadcredit.Repository] with optimistic-
// version semantics. Returns ErrConflict when ForceConflictOnce is set
// OR when the in-aggregate version doesn't match the stored version.
func (r *FakeLeadCreditRepository) UpsertWithVersion(
	_ context.Context, l *leadcredit.LeadCredit,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ForceConflictOnce {
		r.ForceConflictOnce = false
		return leadcredit.ErrConflict
	}
	stored, exists := r.versions[l.TenantID()]
	if !exists {
		// INSERT path — aggregate version must be 0.
		if l.Version() != 0 {
			return leadcredit.ErrConflict
		}
	} else if l.Version() != stored {
		return leadcredit.ErrConflict
	}
	// Persist a fresh snapshot with version+1 so subsequent reads see
	// the new state.
	snap := leadcredit.Snapshot{
		TenantID:  l.TenantID(),
		Balance:   l.Balance(),
		Version:   l.Version() + 1,
		CreatedAt: l.CreatedAt(),
		UpdatedAt: l.UpdatedAt(),
	}
	r.Store[l.TenantID()] = leadcredit.UnmarshalFromDB(snap)
	r.versions[l.TenantID()] = l.Version() + 1
	r.DrainedEvents = append(r.DrainedEvents, l.PullEvents()...)
	return nil
}

// Compile-time interface assertions — keep fakes in step with domain
// interface drift.
var (
	_ unverifiedcontact.Repository = (*FakeUnverifiedContactRepository)(nil)
	_ verificationcall.Repository  = (*FakeVerificationCallRepository)(nil)
	_ platformlead.Repository      = (*FakePlatformLeadRepository)(nil)
	_ leadcredit.Repository        = (*FakeLeadCreditRepository)(nil)
)

// Quiet the unused-import linter on query types when fakes ship before
// the read-model adapter.
var _ = query.UnverifiedContactView{}
