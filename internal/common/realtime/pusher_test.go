package realtime_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/realtime"
	"github.com/leadkart/leadkart-go/internal/common/realtime/realtimetest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

func TestNoopPusher_DoesNotPanic(t *testing.T) {
	t.Parallel()
	p := realtime.NoopPusher{}
	tID := tenant.ID(ids.NewV7().String())
	mID := membership.ID(ids.NewV7().String())
	env, err := realtime.NewEnvelope("test.foo.v1", map[string]string{"k": "v"})
	require.NoError(t, err)

	// The point of the noop is that BOTH methods accept-and-discard
	// without crashing — that's the assertion (require.NotPanics is
	// the canonical "I assert this is a no-op" pattern).
	require.NotPanics(t, func() {
		p.PushToMembership(t.Context(), tID, mID, env)
	})
	require.NotPanics(t, func() {
		p.PushToTenant(t.Context(), tID, env)
	})
}

func TestFakePusher_RecordsMembershipPush(t *testing.T) {
	t.Parallel()
	p := realtimetest.NewFakePusher()
	tID := tenant.ID(ids.NewV7().String())
	mID := membership.ID(ids.NewV7().String())
	env, err := realtime.NewEnvelope("notifications.created.v1", map[string]int{"count": 42})
	require.NoError(t, err)

	p.PushToMembership(t.Context(), tID, mID, env)

	if len(p.Pushes) != 1 {
		t.Fatalf("pushes=%d want 1", len(p.Pushes))
	}
	got := p.Pushes[0]
	if got.TenantID != tID {
		t.Errorf("tenant mismatch")
	}
	if got.ToMembership != mID {
		t.Errorf("membership mismatch")
	}
	if got.Envelope.Event != "notifications.created.v1" {
		t.Errorf("event=%s", got.Envelope.Event)
	}
}

func TestFakePusher_RecordsTenantPush(t *testing.T) {
	t.Parallel()
	p := realtimetest.NewFakePusher()
	tID := tenant.ID(ids.NewV7().String())
	env := realtime.Envelope{Event: "identity.user_hierarchy_cascaded.v1"}

	p.PushToTenant(t.Context(), tID, env)

	if len(p.Pushes) != 1 {
		t.Fatalf("pushes=%d want 1", len(p.Pushes))
	}
	got := p.Pushes[0]
	if got.ToMembership != "" {
		t.Errorf("tenant-wide push: ToMembership=%s want empty", got.ToMembership)
	}
	if got.TenantID != tID {
		t.Errorf("tenant mismatch")
	}
}

func TestFakePusher_CountByEvent(t *testing.T) {
	t.Parallel()
	p := realtimetest.NewFakePusher()
	tID := tenant.ID(ids.NewV7().String())

	for range 3 {
		p.PushToTenant(t.Context(), tID, realtime.Envelope{Event: "a.v1"})
	}
	for range 2 {
		p.PushToTenant(t.Context(), tID, realtime.Envelope{Event: "b.v1"})
	}

	if got := p.CountByEvent("a.v1"); got != 3 {
		t.Errorf("a.v1 count=%d want 3", got)
	}
	if got := p.CountByEvent("b.v1"); got != 2 {
		t.Errorf("b.v1 count=%d want 2", got)
	}
	if got := p.CountByEvent("c.v1"); got != 0 {
		t.Errorf("c.v1 count=%d want 0", got)
	}
}

func TestFakePusher_Reset(t *testing.T) {
	t.Parallel()
	p := realtimetest.NewFakePusher()
	tID := tenant.ID(ids.NewV7().String())

	p.PushToTenant(t.Context(), tID, realtime.Envelope{Event: "before.v1"})
	if len(p.Pushes) != 1 {
		t.Fatalf("setup: pushes=%d", len(p.Pushes))
	}

	p.Reset()
	if len(p.Pushes) != 0 {
		t.Errorf("after Reset: pushes=%d want 0", len(p.Pushes))
	}

	p.PushToTenant(t.Context(), tID, realtime.Envelope{Event: "after.v1"})
	if len(p.Pushes) != 1 {
		t.Errorf("post-reset push: pushes=%d want 1", len(p.Pushes))
	}
}
