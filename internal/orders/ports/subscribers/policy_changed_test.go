package subscribers_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	companysettingsevents "github.com/leadkart/leadkart-go/internal/companysettings/integrationevents"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/app/command"
	"github.com/leadkart/leadkart-go/internal/orders/ports/subscribers"
)

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
	hs := subscribers.Handlers(nil, nil, nil, nil, sub)
	if len(hs) != 2 || hs[0].HandlerName() != subscribers.HandlerPolicyChanged || hs[1].HandlerName() != subscribers.HandlerPolicyRepublished || subscribers.HandlerPolicyChanged != "orders.subscribers.PolicyChanged" {
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
