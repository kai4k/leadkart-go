package command_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/app/command"
)

// fakeTenantUoW runs fn directly; the tenant the handler asked for is recorded.
type fakeTenantUoW struct{ tenants []string }

func (u *fakeTenantUoW) WithinTenantTx(ctx context.Context, tenantID string, fn func(context.Context) error) error {
	u.tenants = append(u.tenants, tenantID)
	return fn(ctx)
}

// versionGuardStore is the replica's rule in memory: store only a newer version.
type versionGuardStore struct {
	byTenant map[tenant.ID]commercepolicy.Versioned
	err      error
}

func (s *versionGuardStore) StoreIfNewer(_ context.Context, id tenant.ID, p commercepolicy.Versioned) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	if cur, ok := s.byTenant[id]; ok && cur.Version >= p.Version {
		return false, nil
	}
	s.byTenant[id] = p
	return true, nil
}

func TestApplyPolicyHandler_StoresNewerAndIgnoresReplayAndReorder(t *testing.T) {
	t.Parallel()
	store := &versionGuardStore{byTenant: map[tenant.ID]commercepolicy.Versioned{}}
	uow := &fakeTenantUoW{}
	h := command.NewApplyPolicyHandler(uow, store)
	tid := tenant.ID(uuid.NewString())
	three := commercepolicy.Default()
	three.Expiry.StopMonthsBefore = 3

	apply := func(version int64, p commercepolicy.Policy) command.ApplyPolicyResult {
		t.Helper()
		out, err := h.Handle(t.Context(), command.ApplyPolicyCommand{TenantID: tid, Version: version, Policy: p})
		if err != nil {
			t.Fatalf("apply v%d: %v", version, err)
		}
		return out
	}
	if !apply(2, three).Stored {
		t.Fatal("the first version must be stored")
	}
	if apply(2, three).Stored {
		t.Fatal("a replayed version must store nothing")
	}
	if apply(1, commercepolicy.Default()).Stored || store.byTenant[tid].Version != 2 {
		t.Fatal("an older version arriving late must not roll the replica back")
	}
	if !apply(3, commercepolicy.Default()).Stored || store.byTenant[tid].Version != 3 {
		t.Fatal("a newer version must be stored")
	}
	if len(uow.tenants) != 4 || uow.tenants[0] != tid.String() {
		t.Fatalf("every apply runs in the tenant's own transaction: %v", uow.tenants)
	}
}

func TestApplyPolicyHandler_RefusesWhatItWouldNotStore(t *testing.T) {
	t.Parallel()
	store := &versionGuardStore{byTenant: map[tenant.ID]commercepolicy.Versioned{}}
	h := command.NewApplyPolicyHandler(&fakeTenantUoW{}, store)
	tid := tenant.ID(uuid.NewString())

	bad := commercepolicy.Default()
	bad.Expiry.StopMonthsBefore = -1
	for name, cmd := range map[string]command.ApplyPolicyCommand{
		"no tenant":         {Version: 1, Policy: commercepolicy.Default()},
		"version zero":      {TenantID: tid, Version: 0, Policy: commercepolicy.Default()},
		"an invalid policy": {TenantID: tid, Version: 1, Policy: bad},
		"an empty policy":   {TenantID: tid, Version: 1},
	} {
		if _, err := h.Handle(t.Context(), cmd); err == nil {
			t.Errorf("%s must be refused (retry, then dead letter), never stored", name)
		}
	}
	if len(store.byTenant) != 0 {
		t.Fatal("nothing invalid may reach the replica")
	}
	store.err = errors.New("db down")
	if _, err := h.Handle(t.Context(), command.ApplyPolicyCommand{TenantID: tid, Version: 1, Policy: commercepolicy.Default()}); err == nil {
		t.Fatal("a store failure must surface so the event is retried")
	}
}
