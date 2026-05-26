// Package platformtest provides cross-cutting test doubles for the
// Platform module — UoW, Outbox, and the [TransactionalFake] contract
// the rollback-aware UoW snapshots.
//
// Per-aggregate fakes (FakeUnverifiedContactRepository,
// FakeVerificationCallRepository, FakePlatformLeadRepository,
// FakeLeadCreditRepository) live in their canonical `<aggregate>test/`
// directories per TDL Wild Workouts canon — co-located with the
// domain aggregate they fake. The aliases below preserve the
// `platformtest.FakeXRepository` import surface so existing tests stay
// green without per-call-site rewrites.
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

// FakeUnverifiedContactRepository is an alias to the canonical
// [unverifiedcontacttest.FakeRepository] — defined in the aggregate-
// co-located test package per TDL Wild Workouts canon. Re-exported
// here so existing callers continue to compile.
type FakeUnverifiedContactRepository = unverifiedcontacttest.FakeRepository

// NewFakeUnverifiedContactRepository forwards to the canonical
// constructor. Re-exported for the same reason as the type alias.
func NewFakeUnverifiedContactRepository() *FakeUnverifiedContactRepository {
	return unverifiedcontacttest.NewFakeRepository()
}

// FakeVerificationCallRepository is an alias to the canonical
// [verificationcalltest.FakeRepository].
type FakeVerificationCallRepository = verificationcalltest.FakeRepository

// NewFakeVerificationCallRepository forwards to the canonical constructor.
func NewFakeVerificationCallRepository() *FakeVerificationCallRepository {
	return verificationcalltest.NewFakeRepository()
}

// FakePlatformLeadRepository is an alias to the canonical
// [platformleadtest.FakeRepository].
type FakePlatformLeadRepository = platformleadtest.FakeRepository

// NewFakePlatformLeadRepository forwards to the canonical constructor.
func NewFakePlatformLeadRepository() *FakePlatformLeadRepository {
	return platformleadtest.NewFakeRepository()
}

// FakeLeadCreditRepository is an alias to the canonical
// [leadcredittest.FakeRepository].
type FakeLeadCreditRepository = leadcredittest.FakeRepository

// NewFakeLeadCreditRepository forwards to the canonical constructor.
func NewFakeLeadCreditRepository() *FakeLeadCreditRepository {
	return leadcredittest.NewFakeRepository()
}

// ----- FakeUnitOfWork -------------------------------------------------------

// FakeUnitOfWork runs fn synchronously + models transactional rollback
// against any registered fake. Production: Postgres rolls back the
// debit when the platformlead UPDATE returns ErrAlreadySold inside the
// same WithinTx closure. The fake must mirror that semantic — without
// it, tests would silently miss "loser was debited" regressions
// (review-pass finding H10).
//
// Usage pattern:
//
//	credits := platformtest.NewFakeLeadCreditRepository()
//	leads   := platformtest.NewFakePlatformLeadRepository()
//	uow := platformtest.NewFakeUnitOfWork(credits, leads)
//	// any fake that mutates state across WithinTx boundaries must
//	// implement TransactionalFake + be registered.
//
// The zero-value [FakeUnitOfWork] continues to work for single-write
// flows that don't need rollback semantics (no fakes registered =
// closure-only, current behaviour).
type FakeUnitOfWork struct {
	fakes []TransactionalFake
}

// TransactionalFake is the rollback-aware contract a fake repository
// implements when it wants the FakeUoW to undo its mutations on
// closure error. Snapshot captures pre-tx state; Restore puts it
// back.
type TransactionalFake interface {
	// Snapshot captures the fake's pre-tx state. Returned closure
	// MUST be safe to invoke at most once (the UoW calls it iff
	// the closure errors).
	Snapshot() (restore func())
}

// NewFakeUnitOfWork constructs a UoW that snapshots each registered
// fake at WithinTx entry + restores them on closure error.
func NewFakeUnitOfWork(fakes ...TransactionalFake) *FakeUnitOfWork {
	return &FakeUnitOfWork{fakes: fakes}
}

// WithinTx satisfies [pg.UnitOfWork]. Snapshots all registered fakes
// before fn runs; on closure error, restores every fake to its
// pre-tx state (modelling Postgres ROLLBACK).
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

// Reset clears recorded events. Useful between sub-tests.
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
)

// Quiet the unused-import linter on query types when fakes ship before
// the read-model adapter.
var _ = query.UnverifiedContactView{}
