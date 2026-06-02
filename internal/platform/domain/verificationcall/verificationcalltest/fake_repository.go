// Package verificationcalltest provides an in-memory [verificationcall.Repository]
// for app-layer and integration tests that need a VerificationCall store without Postgres.
// Each test constructs its own [FakeRepository]; no shared state, no sync (TDL canon).
package verificationcalltest

import (
	"context"
	"slices"

	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
	"github.com/leadkart/leadkart-go/internal/platform/domain/verificationcall"
)

// FakeRepository is the in-memory [verificationcall.Repository]. Construct via [NewFakeRepository].
type FakeRepository struct {
	// Store holds calls in insertion order; ListByContact sorts newest-first on read.
	Store []*verificationcall.VerificationCall

	// DrainedEvents accumulates events pulled from each call at Add time.
	DrainedEvents []verificationcall.Event
}

// NewFakeRepository returns an empty call repository. Each test must construct its own instance.
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{}
}

// Compile-time interface conformance gate.
var _ verificationcall.Repository = (*FakeRepository)(nil)

// Add appends c to the store and drains its events into DrainedEvents, mirroring the adapter's outbox drain on commit.
func (r *FakeRepository) Add(_ context.Context, c *verificationcall.VerificationCall) error {

	r.Store = append(r.Store, c)
	r.DrainedEvents = append(r.DrainedEvents, c.PullEvents()...)
	return nil
}

// ListByContact returns all calls for contactID sorted newest-first (logged_at DESC).
func (r *FakeRepository) ListByContact(
	_ context.Context, contactID unverifiedcontact.ID,
) ([]*verificationcall.VerificationCall, error) {

	var out []*verificationcall.VerificationCall
	for _, c := range r.Store {
		if c.ContactID() == contactID {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b *verificationcall.VerificationCall) int {
		return b.LoggedAt().Compare(a.LoggedAt()) // logged_at DESC
	})
	return out, nil
}
