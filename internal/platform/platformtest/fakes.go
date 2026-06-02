// Package platformtest provides cross-cutting Platform test doubles: UoW,
// Outbox, and the [TransactionalFake] contract the rollback-aware UoW snapshots.
//
// Per-aggregate fakes live in their canonical `<aggregate>test/` directories
// (TDL Wild Workouts canon, co-located with the aggregate). The aliases below
// preserve the `platformtest.FakeXRepository` import surface for existing tests.
package platformtest

import (
	"context"
	"sync"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/platform/app/query"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit/leadcredittest"
	"github.com/leadkart/leadkart-go/internal/platform/domain/platformlead/platformleadtest"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact/unverifiedcontacttest"
	"github.com/leadkart/leadkart-go/internal/platform/domain/verificationcall/verificationcalltest"
	"github.com/leadkart/leadkart-go/internal/platform/integrationevents"
)

// ----- per-aggregate fake aliases ------------------------------------------

// FakeUnverifiedContactRepository aliases the canonical
// [unverifiedcontacttest.FakeRepository], re-exported for existing callers.
type FakeUnverifiedContactRepository = unverifiedcontacttest.FakeRepository

// NewFakeUnverifiedContactRepository forwards to the canonical constructor.
func NewFakeUnverifiedContactRepository() *FakeUnverifiedContactRepository {
	return unverifiedcontacttest.NewFakeRepository()
}

// FakeVerificationCallRepository aliases the canonical
// [verificationcalltest.FakeRepository].
type FakeVerificationCallRepository = verificationcalltest.FakeRepository

// NewFakeVerificationCallRepository forwards to the canonical constructor.
func NewFakeVerificationCallRepository() *FakeVerificationCallRepository {
	return verificationcalltest.NewFakeRepository()
}

// FakePlatformLeadRepository aliases the canonical
// [platformleadtest.FakeRepository].
type FakePlatformLeadRepository = platformleadtest.FakeRepository

// NewFakePlatformLeadRepository forwards to the canonical constructor.
func NewFakePlatformLeadRepository() *FakePlatformLeadRepository {
	return platformleadtest.NewFakeRepository()
}

// FakeLeadCreditRepository aliases the canonical
// [leadcredittest.FakeRepository].
type FakeLeadCreditRepository = leadcredittest.FakeRepository

// NewFakeLeadCreditRepository forwards to the canonical constructor.
func NewFakeLeadCreditRepository() *FakeLeadCreditRepository {
	return leadcredittest.NewFakeRepository()
}

// ----- FakeUnitOfWork -------------------------------------------------------

// FakeUnitOfWork runs fn synchronously and models transactional rollback
// against any registered fake. Production Postgres rolls back the debit when
// the platformlead UPDATE returns ErrAlreadySold in the same WithinTx closure;
// the fake must mirror that or tests miss "loser was debited" regressions
// (finding H10).
//
// Usage:
//
//	credits := platformtest.NewFakeLeadCreditRepository()
//	leads   := platformtest.NewFakePlatformLeadRepository()
//	uow := platformtest.NewFakeUnitOfWork(credits, leads)
//
// Any fake mutating state across WithinTx boundaries must implement
// TransactionalFake and be registered. The zero value works for single-write
// flows that don't need rollback (no fakes registered = closure-only).
type FakeUnitOfWork struct {
	fakes []TransactionalFake
}

// TransactionalFake is the rollback-aware contract a fake implements so the
// FakeUoW can undo its mutations on closure error.
type TransactionalFake interface {
	// Snapshot captures pre-tx state. The returned closure restores it and
	// is called at most once (only when the closure errors).
	Snapshot() (restore func())
}

// NewFakeUnitOfWork returns a UoW that snapshots each registered fake at
// WithinTx entry and restores them on closure error.
func NewFakeUnitOfWork(fakes ...TransactionalFake) *FakeUnitOfWork {
	return &FakeUnitOfWork{fakes: fakes}
}

// WithinTx satisfies [pg.UnitOfWork]: snapshots all registered fakes before fn,
// and on closure error restores them (modelling Postgres ROLLBACK).
func (u *FakeUnitOfWork) WithinTx(ctx context.Context, _ pg.TxScope, fn func(ctx context.Context) error) error {
	restores := make([]func(), 0, len(u.fakes))
	for _, f := range u.fakes {
		restores = append(restores, f.Snapshot())
	}
	err := fn(ctx)
	if err != nil {
		for _, r := range restores {
			r()
		}
	}
	return err
}

// ----- FakeOutbox ----------------------------------------------------------

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

// Reset clears recorded events between sub-tests.
func (f *FakeOutbox) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Events = nil
}

// ----- compile-time conformance assertions ---------------------------------

var (
	_ pg.UnitOfWork     = (*FakeUnitOfWork)(nil)
	_ TransactionalFake = (*FakeLeadCreditRepository)(nil)
	_ TransactionalFake = (*FakePlatformLeadRepository)(nil)
	_ TransactionalFake = (*FakeUnverifiedContactRepository)(nil)
)

// Pins the query import while fakes ship ahead of the read-model adapter.
var _ = query.UnverifiedContactView{}
