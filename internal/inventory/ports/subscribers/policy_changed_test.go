package subscribers_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
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
	pol := commercepolicy.Default()
	pol.Expiry.StopMonthsBefore = 4
	evt := &companysettingsevents.PolicyChangedV1{TenantIDClaim: tid, Version: 5, Policy: pol, OccurredAtUTC: now()}

	for range 3 {
		if err := sub.Handle(t.Context(), evt); err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}
	got := store.stored[tenant.ID(tid.String())]
	if got.Version != 5 || got.Policy.Expiry.StopMonthsBefore != 4 {
		t.Fatalf("stored = %+v", got)
	}

	older := &companysettingsevents.PolicyChangedV1{TenantIDClaim: tid, Version: 4, Policy: commercepolicy.Default(), OccurredAtUTC: now()}
	if err := sub.Handle(t.Context(), older); err != nil || store.stored[tenant.ID(tid.String())].Version != 5 {
		t.Fatalf("an older event must acknowledge and change nothing: %v", err)
	}

	malformed := &companysettingsevents.PolicyChangedV1{TenantIDClaim: tid, Version: 9, OccurredAtUTC: now()}
	if err := sub.Handle(t.Context(), malformed); err == nil {
		t.Fatal("an event whose policy does not validate must error (retry, dead letter), not be stored")
	}
}

func TestHandlers_RegistersThePolicyChangedHandlerUnderItsStableName(t *testing.T) {
	t.Parallel()
	sub := subscribers.NewPolicyChangedSubscriber(command.NewApplyPolicyHandler(tenantUoW{}, &recordingStore{}), silentLog())
	hs := subscribers.Handlers(nil, nil, nil, sub)
	if len(hs) != 1 || hs[0].HandlerName() != subscribers.HandlerPolicyChanged || subscribers.HandlerPolicyChanged != "inventory.subscribers.PolicyChanged" {
		t.Fatalf("handlers = %v", hs)
	}
}
