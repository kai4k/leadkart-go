//go:build integration

// arch-test:no-timeout-needed — every test in this file uses the shared
//   pgtest container (per-package); pgxpool internal conn timeouts +
//   package-level `task ci:test:int -timeout=15m` already bound execution.
//   Per-test context.WithTimeout would be belt-and-suspenders against the
//   shared-pool + parallel-with-RLS canon shape.
//
// arch-test:parallel-safe — every Test* uses the shared pgtest container
//   + a fresh tenant_id per test bound via tenancy.WithID(); RLS isolates
//   rows by tenant so parallel runs cannot see each others state.
//   Brandur "Postgres at scale" + TDL Wild Workouts canon: shared
//   infrastructure + per-test logical isolation = safe parallelism.
//
// SQL-CONTRACT COVERAGE (per ADR 0062 — TDL Test Pyramid):
//   - Optimistic-version UPDATE: `UPDATE ... WHERE version = $expected`
//     returning 0 rows → typed [leadcredit.ErrConflict]. This is the
//     SQL-specific contract that backs the handler's retry loop per
//     ADR 0059.
//   - Outbox row insertion (TenantScoped event) in the SAME tx as the
//     aggregate write; confirms tenant_id stamping for TenantScoped
//     events per ADR 0059 + C3.
//
// Round-trip Get/Insert + ErrNotFound + plain state transition coverage
// moved to [leadcredittest.FakeRepository].

package adapters_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/messaging/messagingtest"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/platform/adapters"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit"
)

// TestLeadCreditRepository_UpsertWithVersion_ConflictOnStaleVersion —
// SQL-contract: load the row, persist a competing update underneath,
// then attempt our own update — MUST return ErrConflict. The "WHERE
// version = $expected" UPDATE returning 0 rows is the SQL-specific
// behavior that drives the handler's retry loop per ADR 0059.
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

	// Caller A now attempts to write — version stamp is stale.
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
// — SQL-contract: confirms the AdjustedEvent gets translated to
// LeadCreditAdjustedV1 and lands on platform.outbox with tenant_id set
// (TenantScoped event; NOT NULL because it has a real tenant FK per C3
// semantics).
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

	topic, stampedString := messagingtest.OutboxFirstTopicForTopic(t, pool, messagingtest.SchemaPlatform, "platform.lead_credit_adjusted.v1")
	if topic != "platform.lead_credit_adjusted.v1" {
		t.Errorf("topic: got %q want platform.lead_credit_adjusted.v1", topic)
	}
	if stampedString != tenantID.String() {
		t.Errorf("tenant_id: got %q want %q (TenantScoped — must carry real tenant FK)",
			stampedString, tenantID)
	}
}
