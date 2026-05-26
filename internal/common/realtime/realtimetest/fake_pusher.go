// Package realtimetest provides the in-memory FakePusher implementing
// [realtime.Pusher]. Records every push so unit tests can assert on
// "what got pushed to whom".
package realtimetest

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/common/realtime"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Push is one recorded delivery. ToMembership zero means the push
// was tenant-wide.
type Push struct {
	TenantID     tenant.ID
	ToMembership membership.ID
	Envelope     realtime.Envelope
}

// FakePusher records every PushTo* call. Tests inspect [FakePusher.Pushes]
// to assert event ordering, recipient targeting, envelope shape.
// Zero-value-NOT-usable — construct via [NewFakePusher].
//
// Single-test-owner: each test creates its OWN instance. No internal
// sync — concurrency-free domain-subtree per the project canon.
type FakePusher struct {
	Pushes []Push
}

// NewFakePusher returns an empty fake.
func NewFakePusher() *FakePusher { return &FakePusher{} }

// Compile-time interface conformance.
var _ realtime.Pusher = (*FakePusher)(nil)

// PushToMembership records the per-recipient push.
func (f *FakePusher) PushToMembership(_ context.Context, tenantID tenant.ID, m membership.ID, env realtime.Envelope) {
	f.Pushes = append(f.Pushes, Push{TenantID: tenantID, ToMembership: m, Envelope: env})
}

// PushToTenant records the tenant-wide push (ToMembership stays zero).
func (f *FakePusher) PushToTenant(_ context.Context, tenantID tenant.ID, env realtime.Envelope) {
	f.Pushes = append(f.Pushes, Push{TenantID: tenantID, Envelope: env})
}

// Reset clears recorded pushes. Useful between subtests within one
// fixture.
func (f *FakePusher) Reset() { f.Pushes = nil }

// CountByEvent returns the number of recorded pushes with the given
// EventName. Convenience for table-driven assertion.
func (f *FakePusher) CountByEvent(name realtime.EventName) int {
	var n int
	for _, p := range f.Pushes {
		if p.Envelope.Event == name {
			n++
		}
	}
	return n
}
