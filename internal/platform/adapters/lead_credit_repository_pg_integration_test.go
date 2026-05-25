//go:build integration

package adapters_test

import (
	"time"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/messaging/messagingtest"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/platform/adapters"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit"
)

// TestLeadCreditRepository_UpsertWithVersion_HappyPath_InsertThenUpdate
// — confirms the INSERT-on-first-write + WHERE-version UPDATE on
// subsequent writes round-trip cleanly through the sqlc query.
func TestLeadCreditRepository_UpsertWithVersion_HappyPath_InsertThenUpdate(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_ = ctx // arch-test:integration-timeout-anchor
	pool := platformPool(t)
	tx := pg.NewTransactor(pool)
	repo := adapters.NewLeadCreditRepository(pool, tx)

	tenantID := leadcredit.TenantID(uuid.New().String())
	op := leadcredit.MembershipID(ids.NewV7().String())

	// First write — INSERT path.
	err := tx.WithinTx(t.Context(), pg.TxScopePlatform, func(ctx context.Context) error {
		c, err := leadcredit.NewForTenant(tenantID, nowUTC())
		if err != nil {
			return err
		}
		if err := c.Topup(100, "initial", op, nowUTC()); err != nil {
			return err
		}
		return repo.UpsertWithVersion(ctx, c)
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// Read back — Balance=100, Version=1 (post-INSERT).
	err = tx.WithinTx(t.Context(), pg.TxScopePlatform, func(ctx context.Context) error {
		got, err := repo.GetByTenant(ctx, tenantID)
		if err != nil {
			return err
		}
		if got.Balance() != 100 {
			t.Errorf("balance: got %d want 100", got.Balance())
		}
		// INSERT path persists with version=1 (per amended SQL) so the
		// repo's optimistic-version UPDATE path can unambiguously
		// detect "fresh aggregate" via in-memory Version==0.
		if got.Version() != 1 {
			t.Errorf("version: got %d want 1 (INSERT writes version=1)", got.Version())
		}
		// Second write — UPDATE path with WHERE version = $expected.
		if err := got.Topup(50, "second", op, nowUTC()); err != nil {
			return err
		}
		return repo.UpsertWithVersion(ctx, got)
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	// Read back — Balance=150.
	err = tx.WithinTx(t.Context(), pg.TxScopePlatform, func(ctx context.Context) error {
		got, err := repo.GetByTenant(ctx, tenantID)
		if err != nil {
			return err
		}
		if got.Balance() != 150 {
			t.Errorf("balance: got %d want 150", got.Balance())
		}
		if got.Version() != 2 {
			t.Errorf("version: got %d want 2 (UPDATE bumps 1→2)", got.Version())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read after update: %v", err)
	}
}

// TestLeadCreditRepository_UpsertWithVersion_ConflictOnStaleVersion —
// load the row, persist a competing update underneath, then attempt
// our own update — MUST return ErrConflict. This is the LOAD-BEARING
// semantic per ADR 0059 that drives the handler's retry loop.
func TestLeadCreditRepository_UpsertWithVersion_ConflictOnStaleVersion(t *testing.T) {
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

// TestLeadCreditRepository_GetByTenant_ReturnsErrNotFound — typed
// sentinel propagation on a missing row.
func TestLeadCreditRepository_GetByTenant_ReturnsErrNotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_ = ctx // arch-test:integration-timeout-anchor
	pool := platformPool(t)
	tx := pg.NewTransactor(pool)
	repo := adapters.NewLeadCreditRepository(pool, tx)

	err := tx.WithinTx(t.Context(), pg.TxScopePlatform, func(ctx context.Context) error {
		_, err := repo.GetByTenant(ctx, leadcredit.TenantID(uuid.New().String()))
		if !errors.Is(err, leadcredit.ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
}

// TestLeadCreditRepository_UpsertWithVersion_DrainsAdjustedEventToOutbox
// — confirms the AdjustedEvent gets translated to LeadCreditAdjustedV1
// and lands on platform.outbox with tenant_id set (TenantScoped event;
// NOT NULL because it has a real tenant FK per C3 semantics).
func TestLeadCreditRepository_UpsertWithVersion_DrainsAdjustedEventToOutbox(t *testing.T) {
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
