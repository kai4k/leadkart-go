//go:build integration

// arch-test:no-timeout-needed — shared pgtest container + pgxpool conn
//   timeouts + package-level `task ci:test:int -timeout=15m` already bound
//   execution.
//
// SQL-contract coverage (ADR 0062, TDL Test Pyramid):
//   - MarketplaceBrowse SELECT column-set (H12): must omit PII columns
//     (email / gst_number / pan_number / mobile_e164 / street). Asserted at
//     the SQL layer because a SELECT * drift would bypass the DTO mapper.
//
// Round-trip + state-machine + ErrNotFound coverage lives in
// [platformleadtest.FakeRepository].

package adapters_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/platform/adapters"
	"github.com/leadkart/leadkart-go/internal/platform/adapters/db"
	"github.com/leadkart/leadkart-go/internal/platform/domain/platformlead"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
)

// seedPlatformLead inserts the parent contact (FK target) then a PlatformLead
// linked to it, returning the lead ID.
func seedPlatformLead(t *testing.T, leadRepo *adapters.PlatformLeadRepository, contactRepo *adapters.UnverifiedContactRepository, tx *pg.Transactor) platformlead.ID {
	t.Helper()
	leadID := platformlead.ID(ids.NewV7().String())
	agentID := unverifiedcontact.MembershipID(ids.NewV7().String())
	contactID := unverifiedcontact.ID(ids.NewV7().String())

	// Parent contact satisfies the platform_leads.source_contact_id FK.
	c, err := unverifiedcontact.New(contactID, fixtureForm(t), agentID, nowUTC())
	if err != nil {
		t.Fatalf("contact ctor: %v", err)
	}
	err = tx.WithinTx(t.Context(), pg.TxScopePlatform, func(ctx context.Context) error {
		return contactRepo.Add(ctx, c)
	})
	if err != nil {
		t.Fatalf("seed contact: %v", err)
	}

	l, err := platformlead.NewFromUnverifiedContact(leadID, contactID, fixtureForm(t), agentID, nowUTC())
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	err = tx.WithinTx(t.Context(), pg.TxScopePlatform, func(ctx context.Context) error {
		return leadRepo.Add(ctx, l)
	})
	if err != nil {
		t.Fatalf("seed lead: %v", err)
	}
	return leadID
}

// TestPlatformLeadRepository_MarketplaceBrowse_OmitsPII asserts the browse
// SELECT omits PII (H12): the returned aggregate's PII accessors must be empty.
func TestPlatformLeadRepository_MarketplaceBrowse_OmitsPII(t *testing.T) {
	// arch-test:no-parallel — cross-tenant scan; uses TruncateAll
	sharedPG.TruncateAll(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_ = ctx // arch-test:integration-timeout-anchor
	pool := platformPool(t)
	tx := pg.NewTransactor(pool)
	repo := adapters.NewPlatformLeadRepository(pool, tx)
	contactRepo := adapters.NewUnverifiedContactRepository(pool, tx)

	leadID := seedPlatformLead(t, repo, contactRepo, tx)

	// Any authenticated tenant can browse (SELECT policy admits unsold rows).
	got, err := repo.MarketplaceBrowse(t.Context(), platformlead.MarketplaceFilter{},
		pagination.Cursor{}, 50)
	if err != nil {
		t.Fatalf("browse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 row, got %d", len(got))
	}
	row := got[0]
	if row.ID() != leadID {
		t.Errorf("ID: got %q want %q", row.ID(), leadID)
	}
	// H12: PII accessors must be empty — the browse SELECT skips those columns.
	if row.Form().Email() != "" {
		t.Errorf("Email PII leak: got %q want \"\" (H12 — marketplace browse must not expose email)", row.Form().Email())
	}
	if row.Form().GstNumber() != "" {
		t.Errorf("GstNumber PII leak: got %q want \"\" (H12)", row.Form().GstNumber())
	}
	if row.Form().PanNumber() != "" {
		t.Errorf("PanNumber PII leak: got %q want \"\" (H12)", row.Form().PanNumber())
	}
	if row.Form().MobileE164() != "" {
		t.Errorf("MobileE164 PII leak: got %q want \"\" (H12)", row.Form().MobileE164())
	}
	if row.Form().Street() != "" {
		t.Errorf("Street PII leak: got %q want \"\" (H12)", row.Form().Street())
	}
	// Non-PII fields are populated.
	if row.Form().ContactName() == "" {
		t.Error("ContactName missing — needed for marketplace card")
	}
	if row.Form().City() != "Pune" {
		t.Errorf("City: got %q want Pune", row.Form().City())
	}
}

// TestPlatformLeadRepository_Purchase_PersistsAndHydratesBuyer exercises the
// multi-buyer purchase path end-to-end (ADR 0065): UpdateByID locks the lead,
// RecordPurchase appends a buyer, the adapter INSERTs the lead_purchases row,
// and a subsequent load re-hydrates the buyer set from the DB so the
// no-double-buy guard fires.
func TestPlatformLeadRepository_Purchase_PersistsAndHydratesBuyer(t *testing.T) {
	// arch-test:no-parallel — uses TruncateAll
	sharedPG.TruncateAll(t)
	pool := platformPool(t)
	tx := pg.NewTransactor(pool)
	repo := adapters.NewPlatformLeadRepository(pool, tx)
	contactRepo := adapters.NewUnverifiedContactRepository(pool, tx)
	leadID := seedPlatformLead(t, repo, contactRepo, tx)

	tenantA := platformlead.TenantID(ids.NewV7().String())
	memberA := unverifiedcontact.MembershipID(ids.NewV7().String())
	purchaseID := ids.NewV7().String()

	err := repo.UpdateByID(t.Context(), leadID, func(l *platformlead.PlatformLead) (bool, error) {
		return true, l.RecordPurchase(purchaseID, tenantA, memberA, 50000, 6, nowUTC())
	})
	if err != nil {
		t.Fatalf("purchase: %v", err)
	}

	// Reload (FOR UPDATE hydrates the buyer set): the buyer must persist.
	err = repo.UpdateByID(t.Context(), leadID, func(l *platformlead.PlatformLead) (bool, error) {
		if !l.HasBuyer(tenantA) {
			t.Errorf("buyer not hydrated from lead_purchases")
		}
		if l.PurchaseCount() != 1 {
			t.Errorf("PurchaseCount = %d, want 1", l.PurchaseCount())
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}

	// Double-buy by the same tenant is rejected by the in-memory guard.
	err = repo.UpdateByID(t.Context(), leadID, func(l *platformlead.PlatformLead) (bool, error) {
		return true, l.RecordPurchase(ids.NewV7().String(), tenantA, memberA, 50000, 6, nowUTC())
	})
	if !errors.Is(err, platformlead.ErrAlreadyPurchased) {
		t.Fatalf("expected ErrAlreadyPurchased, got %v", err)
	}
}

// TestPlatformLeadRepository_DuplicatePurchase_UniqueViolation validates the
// UNIQUE(lead_id, tenant_id) constraint backstop (ADR 0065): two inserts for
// the same (lead, tenant) raise SQLSTATE 23505 — the race-condition guard the
// adapter translates to ErrAlreadyPurchased when the in-memory check is stale.
func TestPlatformLeadRepository_DuplicatePurchase_UniqueViolation(t *testing.T) {
	// arch-test:no-parallel — uses TruncateAll
	sharedPG.TruncateAll(t)
	pool := platformPool(t)
	tx := pg.NewTransactor(pool)
	repo := adapters.NewPlatformLeadRepository(pool, tx)
	contactRepo := adapters.NewUnverifiedContactRepository(pool, tx)
	leadID := seedPlatformLead(t, repo, contactRepo, tx)

	leadUUID, _ := uuid.Parse(leadID.String())
	tenantUUID := uuid.New()
	q := db.New(pool)

	err := tx.WithinTxPgx(t.Context(), pg.TxScopePlatform, func(ctx context.Context, pgxTx pgx.Tx) error {
		qq := q.WithTx(pgxTx)
		row := func(amount int64) db.InsertLeadPurchaseParams {
			return db.InsertLeadPurchaseParams{
				ID:                    pgconv.PgUUID(uuid.New()),
				LeadID:                pgconv.PgUUID(leadUUID),
				TenantID:              pgconv.PgUUID(tenantUUID),
				CreatedByMembershipID: pgconv.PgUUID(uuid.New()),
				AmountPaisa:           amount,
				PurchasedAt:           pgconv.PgRequiredTimestamp(nowUTC()),
			}
		}
		if e := qq.InsertLeadPurchase(ctx, row(50000)); e != nil {
			return e
		}
		return qq.InsertLeadPurchase(ctx, row(60000)) // same (lead, tenant) → 23505
	})

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pg.SQLStateUniqueViolation {
		t.Fatalf("expected 23505 unique violation, got %v", err)
	}
}
