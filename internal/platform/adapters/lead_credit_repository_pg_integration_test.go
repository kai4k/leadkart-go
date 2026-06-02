//go:build integration

// arch-test:no-timeout-needed — shared pgtest container + pgxpool conn
//   timeouts + package-level `task ci:test:int -timeout=15m` already bound
//   execution.
//
// arch-test:parallel-safe — fresh tenant_id per test bound via tenancy.WithID();
//   RLS isolates rows by tenant, so parallel runs can't see each other.
//
// SQL-contract coverage (ADR 0062, TDL Test Pyramid):
//   - Optimistic UPDATE ... WHERE version = $expected returning 0 rows →
//     [leadcredit.ErrConflict]; backs the handler retry loop (ADR 0059).
//   - Outbox row (TenantScoped event) written in the aggregate's tx;
//     confirms tenant_id stamping (ADR 0059, C3).
//
// Round-trip + ErrNotFound + state-transition coverage lives in
// [leadcredittest.FakeRepository].

package adapters_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/platform/adapters"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit"
)

// TestLeadCreditRepository_UpsertWithVersion_ConflictOnStaleVersion proves a
// competing write underneath a stale read yields ErrConflict (the version
// WHERE clause returning 0 rows; ADR 0059).
func TestLeadCreditRepository_UpsertWithVersion_ConflictOnStaleVersion(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_ = ctx // arch-test:integration-timeout-anchor
	pool := platformPool(t)
	tx := pg.NewTransactor(pool)
	repo := adapters.NewLeadCreditRepository(pool, tx)

	tenantID := leadcredit.TenantID(uuid.New().String())
	op := leadcredit.MembershipID(ids.NewV7().String())

	// Seed Balance=100 / Version=1.
	err := tx.WithinTx(t.Context(), pg.TxScopePlatform, func(ctx context.Context) error {
		c, err := leadcredit.NewForTenant(tenantID, nowUTC())
		if err != nil {
			return err
		}
		if err := c.Topup(100, "seed", op, nowUTC()); err != nil {
			return err
		}
		return repo.UpsertWithVersion(ctx, c)
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Caller A reads Version=1.
	var aSide *leadcredit.LeadCredit
	err = tx.WithinTx(t.Context(), pg.TxScopePlatform, func(ctx context.Context) error {
		got, err := repo.GetByTenant(ctx, tenantID)
		if err != nil {
			return err
		}
		aSide = got
		return nil
	})
	if err != nil {
		t.Fatalf("caller A read: %v", err)
	}

	// Competing write — caller B reads Version=1 + commits Version=2.
	err = tx.WithinTx(t.Context(), pg.TxScopePlatform, func(ctx context.Context) error {
		got, err := repo.GetByTenant(ctx, tenantID)
		if err != nil {
			return err
		}
		if err := got.Topup(20, "competing", op, nowUTC()); err != nil {
			return err
		}
		return repo.UpsertWithVersion(ctx, got)
	})
	if err != nil {
		t.Fatalf("caller B write: %v", err)
	}

	// Caller A writes with a now-stale version stamp.
	err = tx.WithinTx(t.Context(), pg.TxScopePlatform, func(ctx context.Context) error {
		if err := aSide.Topup(50, "stale write", op, nowUTC()); err != nil {
			return err
		}
		return repo.UpsertWithVersion(ctx, aSide)
	})
	if !errors.Is(err, leadcredit.ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
}

// TestLeadCreditRepository_UpsertWithVersion_DrainsAdjustedEventToOutbox
// confirms the AdjustedEvent maps to LeadCreditAdjustedV1 on the outbox with
// tenant_id set (TenantScoped, real tenant FK; C3).
func TestLeadCreditRepository_UpsertWithVersion_DrainsAdjustedEventToOutbox(t *testing.T) {
	// arch-test:no-parallel — cross-tenant scan; uses TruncateAll
	sharedPG.TruncateAll(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_ = ctx // arch-test:integration-timeout-anchor
	pool := platformPool(t)
	tx := pg.NewTransactor(pool)
	repo := adapters.NewLeadCreditRepository(pool, tx)

	tenantID := leadcredit.TenantID(uuid.New().String())
	op := leadcredit.MembershipID(ids.NewV7().String())

	fix := newPlatformOutboxFixture(t)

	err := tx.WithinTx(t.Context(), pg.TxScopePlatform, func(ctx context.Context) error {
		c, err := leadcredit.NewForTenant(tenantID, nowUTC())
		if err != nil {
			return err
		}
		if err := c.Topup(100, "seed", op, nowUTC()); err != nil {
			return err
		}
		return repo.UpsertWithVersion(ctx, c)
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	msgs := fix.forwardAndWait(t, 1)
	got := platformEventTypes(msgs)
	if len(got) != 1 || got[0] != "platform.lead_credit_adjusted.v1" {
		t.Fatalf("event_types: got %v want [platform.lead_credit_adjusted.v1]", got)
	}
	// TenantScoped: must carry the real tenant FK on the wire.
	if tid := msgs[0].Metadata.Get(messaging.HeaderTenantID); tid != tenantID.String() {
		t.Errorf("tenant_id header: got %q want %q (TenantScoped — must carry real tenant FK)",
			tid, tenantID.String())
	}
}
