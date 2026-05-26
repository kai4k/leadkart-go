package realtime

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// NoopPusher is the zero-effect implementation. Wired into the worker
// composition root (the worker has no client connections to push to)
// + into test fixtures that don't exercise the real-time surface.
//
// Per Mat Ryer canon — no implicit fallback. Production HTTP host
// wires the concrete WebsocketPusher explicitly; NoopPusher is opt-in.
type NoopPusher struct{}

// Compile-time interface conformance.
var _ Pusher = NoopPusher{}

// PushToMembership is a no-op.
func (NoopPusher) PushToMembership(_ context.Context, _ tenant.ID, _ membership.ID, _ Envelope) {
}

// PushToTenant is a no-op.
func (NoopPusher) PushToTenant(_ context.Context, _ tenant.ID, _ Envelope) {}
