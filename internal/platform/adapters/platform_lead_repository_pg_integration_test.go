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

package adapters_test

import (
	"time"
	"context"
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/platform/adapters"
	"github.com/leadkart/leadkart-go/internal/platform/domain/platformlead"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
)

// seedPlatformLead inserts a contact row first (FK target) + a
// PlatformLead linked to it. Returns the lead ID.
func seedPlatformLead(t *testing.T, leadRepo *adapters.PlatformLeadRepository, contactRepo *adapters.UnverifiedContactRepository, tx *pg.Transactor) platformlead.ID {
	t.Helper()
	leadID := platformlead.ID(ids.NewV7().String())
	agentID := unverifiedcontact.MembershipID(ids.NewV7().String())
	contactID := unverifiedcontact.ID(ids.NewV7().String())

	// First insert the parent contact so the FK on platform_leads.
	// source_contact_id is satisfiable.
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

// TestPlatformLeadRepository_Add_RoundTripsViaGetByID — write + read.
func TestPlatformLeadRepository_Add_RoundTripsViaGetByID(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_ = ctx // arch-test:integration-timeout-anchor
	pool := platformPool(t)
	tx := pg.NewTransactor(pool)
	repo := adapters.NewPlatformLeadRepository(pool, tx)
	contactRepo := adapters.NewUnverifiedContactRepository(pool, tx)

	leadID := seedPlatformLead(t, repo, contactRepo, tx)
	err := tx.WithinTx(t.Context(), pg.TxScopePlatform, func(ctx context.Context) error {
		got, err := repo.GetByID(ctx, leadID)
		if err != nil {
			return err
		}
		if got.ID() != leadID {
			t.Errorf("ID round-trip: got %q want %q", got.ID(), leadID)
		}
		if !got.IsAvailable() {
			t.Error("expected IsAvailable=true on freshly-verified lead")
		}
		// FULL form round-trip on GetByID — including PII (this read
		// path is used by the buyer post-purchase, so PII availability
		// here is correct).
		if got.Form().Email() != "ops@acme.test" {
			t.Errorf("Email round-trip: got %q want ops@acme.test", got.Form().Email())
		}
		if got.Form().GstNumber() != "27AAAAA0000A1Z5" {
			t.Errorf("GstNumber round-trip: got %q", got.Form().GstNumber())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read tx: %v", err)
	}
}

// TestPlatformLeadRepository_MarketplaceBrowse_OmitsPII — H12. The
// browse SELECT explicitly omits email / gst_number / pan_number /
// mobile_e164 / street to prevent a future `SELECT *` drift from
// leaking PII through the DTO mapper. The returned aggregate's
// PII-field accessors MUST return the empty string (no row data
// scanned in).
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

	// Browse runs under TxScopePlatform too (any authenticated tenant
	// can browse; the SELECT policy allows unsold rows to all). Use
	// a tenant scope to confirm a real cross-tenant browse pulls the
	// row without PII.
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
	// PII assertions — H12. Form().Email() / GstNumber() / PanNumber()
	// / MobileE164() / Street() MUST be empty because the browse
	// SELECT does not fetch those columns.
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
	// Non-PII fields ARE populated.
	if row.Form().ContactName() == "" {
		t.Error("ContactName missing — needed for marketplace card")
	}
	if row.Form().City() != "Pune" {
		t.Errorf("City: got %q want Pune", row.Form().City())
	}
}

// TestPlatformLeadRepository_UpdateByID_Purchase_RoundTrips —
// purchase flow drives sold_to_tenant_id + sold_at; reload sees the
// transition.
func TestPlatformLeadRepository_UpdateByID_Purchase_RoundTrips(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_ = ctx // arch-test:integration-timeout-anchor
	pool := platformPool(t)
	tx := pg.NewTransactor(pool)
	repo := adapters.NewPlatformLeadRepository(pool, tx)
	contactRepo := adapters.NewUnverifiedContactRepository(pool, tx)

	leadID := seedPlatformLead(t, repo, contactRepo, tx)
	buyerTenant := platformlead.TenantID(ids.NewV7().String())
	buyerMember := unverifiedcontact.MembershipID(ids.NewV7().String())

	err := tx.WithinTx(t.Context(), pg.TxScopePlatform, func(ctx context.Context) error {
		return repo.UpdateByID(ctx, leadID, func(l *platformlead.PlatformLead) (bool, error) {
			return true, l.Purchase(buyerTenant, buyerMember, 50000, nowUTC())
		})
	})
	if err != nil {
		t.Fatalf("purchase: %v", err)
	}

	err = tx.WithinTx(t.Context(), pg.TxScopePlatform, func(ctx context.Context) error {
		got, err := repo.GetByID(ctx, leadID)
		if err != nil {
			return err
		}
		if got.IsAvailable() {
			t.Error("expected IsAvailable=false post-purchase")
		}
		if got.SoldToTenantID() != buyerTenant {
			t.Errorf("SoldToTenantID: got %q want %q", got.SoldToTenantID(), buyerTenant)
		}
		if got.AmountPaisa() != 50000 {
			t.Errorf("AmountPaisa: got %d want 50000", got.AmountPaisa())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
}

// TestPlatformLeadRepository_GetByID_ReturnsErrNotFound — sentinel
// shape.
func TestPlatformLeadRepository_GetByID_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_ = ctx // arch-test:integration-timeout-anchor
	pool := platformPool(t)
	tx := pg.NewTransactor(pool)
	repo := adapters.NewPlatformLeadRepository(pool, tx)

	err := tx.WithinTx(t.Context(), pg.TxScopePlatform, func(ctx context.Context) error {
		_, err := repo.GetByID(ctx, platformlead.ID(ids.NewV7().String()))
		if !errors.Is(err, platformlead.ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tx: %v", err)
	}
}
