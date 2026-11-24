package subscribers_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	companysettingsevents "github.com/leadkart/leadkart-go/internal/companysettings/integrationevents"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/app/command"
	"github.com/leadkart/leadkart-go/internal/inventory/ports/subscribers"
)

func now() time.Time { return time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) }

func silentLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type tenantUoW struct{}

func (tenantUoW) WithinTenantTx(ctx context.Context, _ string, fn func(context.Context) error) error {
	return fn(ctx)
}

type recordingStore struct {
	stored map[tenant.ID]commercepolicy.Versioned
}

func (s *recordingStore) StoreIfNewer(_ context.Context, id tenant.ID, p commercepolicy.Versioned) (bool, error) {
	if cur, ok := s.stored[id]; ok && cur.Version >= p.Version {
		return false, nil
	}
	s.stored[id] = p
	return true, nil
}

func TestPolicyChangedSubscriber_AppliesTheWholePolicyAndReplayIsHarmless(t *testing.T) {
	t.Parallel()
	store := &recordingStore{stored: map[tenant.ID]commercepolicy.Versioned{}}
	sub := subscribers.NewPolicyChangedSubscriber(command.NewApplyPolicyHandler(tenantUoW{}, store), silentLog())
	tid := uuid.New()
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tid.String()))
	pol := commercepolicy.Default()
	pol.Expiry.StopMonthsBefore = 4
	evt := &companysettingsevents.PolicyChangedV1{TenantIDClaim: tid, Version: 5, Policy: pol, OccurredAtUTC: now()}

	for range 3 {
		if err := sub.Handle(ctx, evt); err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}
	got := store.stored[tenant.ID(tid.String())]
	if got.Version != 5 || got.Policy.Expiry.StopMonthsBefore != 4 {
		t.Fatalf("stored = %+v", got)
	}

	older := &companysettingsevents.PolicyChangedV1{TenantIDClaim: tid, Version: 4, Policy: commercepolicy.Default(), OccurredAtUTC: now()}
	if err := sub.Handle(ctx, older); err != nil || store.stored[tenant.ID(tid.String())].Version != 5 {
		t.Fatalf("an older event must acknowledge and change nothing: %v", err)
	}

	malformed := &companysettingsevents.PolicyChangedV1{TenantIDClaim: tid, Version: 9, OccurredAtUTC: now()}
	if err := sub.Handle(ctx, malformed); err == nil {
		t.Fatal("an event whose policy does not validate must error (retry, dead letter), not be stored")
	}
}

func TestHandlers_RegistersThePolicyChangedHandlerUnderItsStableName(t *testing.T) {
	t.Parallel()
	sub := subscribers.NewPolicyChangedSubscriber(command.NewApplyPolicyHandler(tenantUoW{}, &recordingStore{}), silentLog())
	hs := subscribers.Handlers(nil, nil, sub)
	if len(hs) != 2 || hs[0].HandlerName() != subscribers.HandlerPolicyChanged || hs[1].HandlerName() != subscribers.HandlerPolicyRepublished || subscribers.HandlerPolicyChanged != "inventory.subscribers.PolicyChanged" {
		t.Fatalf("handlers = %v", hs)
	}
}

// L1: the tenant of the envelope decides; a payload naming another tenant is
// refused and nothing is stored under either.
func TestPolicyChangedSubscriber_RefusesAPayloadThatNamesAnotherTenant(t *testing.T) {
	t.Parallel()
	store := &recordingStore{stored: map[tenant.ID]commercepolicy.Versioned{}}
	sub := subscribers.NewPolicyChangedSubscriber(command.NewApplyPolicyHandler(tenantUoW{}, store), silentLog())
	envelope, payload := uuid.New(), uuid.New()
	evt := &companysettingsevents.PolicyChangedV1{TenantIDClaim: payload, Version: 1 << 62, Policy: commercepolicy.Default(), OccurredAtUTC: now()}

	err := sub.Handle(tenancy.WithID(t.Context(), tenancy.ID(envelope.String())), evt)
	if err == nil || !messaging.IsNonRetryable(err) {
		t.Fatalf("a mismatched tenant must be a permanent refusal (dead letter), got %v", err)
	}
	if len(store.stored) != 0 {
		t.Fatalf("nothing may be stored: %v", store.stored)
	}
	if err := sub.Handle(t.Context(), evt); err == nil {
		t.Fatal("a message with no envelope tenant must be refused")
	}
}

func TestPolicyChangedSubscriber_AppliesARepublishUnderItsOwnHandler(t *testing.T) {
	t.Parallel()
	store := &recordingStore{stored: map[tenant.ID]commercepolicy.Versioned{}}
	sub := subscribers.NewPolicyChangedSubscriber(command.NewApplyPolicyHandler(tenantUoW{}, store), silentLog())
	tid := uuid.New()
	evt := &companysettingsevents.PolicyRepublishedV1{TenantIDClaim: tid, Version: 3, Policy: commercepolicy.Default(), OccurredAtUTC: now()}
	if err := sub.HandleRepublished(tenancy.WithID(t.Context(), tenancy.ID(tid.String())), evt); err != nil {
		t.Fatal(err)
	}
	if store.stored[tenant.ID(tid.String())].Version != 3 {
		t.Fatalf("stored = %+v", store.stored)
	}
}

type recordingEnqueuer struct {
	calls []uuid.UUID
	err   error
}

func (r *recordingEnqueuer) EnqueueExpiryRescan(_ context.Context, id uuid.UUID) error {
	r.calls = append(r.calls, id)
	return r.err
}

// Review S1: a stored newer version ENQUEUES the tenant rescan (the handler never scans);
// an older or duplicate event enqueues nothing; a failing enqueue never fails the event
// (the replica is stored; the daily run is the safety net).
func TestPolicyChangedSubscriber_EnqueuesTheRescanOnlyWhenANewerVersionIsStored(t *testing.T) {
	t.Parallel()
	store := &recordingStore{stored: map[tenant.ID]commercepolicy.Versioned{}}
	enq := &recordingEnqueuer{}
	sub := subscribers.NewPolicyChangedSubscriber(command.NewApplyPolicyHandler(tenantUoW{}, store), silentLog()).WithExpiryRescan(enq)
	tid := uuid.New()
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tid.String()))
	evt := &companysettingsevents.PolicyChangedV1{TenantIDClaim: tid, Version: 3, Policy: commercepolicy.Default(), OccurredAtUTC: now()}

	if err := sub.Handle(ctx, evt); err != nil || len(enq.calls) != 1 || enq.calls[0] != tid {
		t.Fatalf("a stored version: err %v calls %v", err, enq.calls)
	}
	if err := sub.Handle(ctx, evt); err != nil || len(enq.calls) != 1 {
		t.Fatalf("a duplicate must enqueue nothing: %v", enq.calls)
	}
	enq.err = errors.New("queue down")
	newer := &companysettingsevents.PolicyChangedV1{TenantIDClaim: tid, Version: 4, Policy: commercepolicy.Default(), OccurredAtUTC: now()}
	if err := sub.Handle(ctx, newer); err != nil {
		t.Fatalf("a failing enqueue must not fail the event: %v", err)
	}
	if store.stored[tenant.ID(tid.String())].Version != 4 {
		t.Fatal("the replica must be stored even when the enqueue fails")
	}
}
