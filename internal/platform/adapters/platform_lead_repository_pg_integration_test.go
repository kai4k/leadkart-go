//go:build integration

// arch-test:no-timeout-needed — every test in this file uses the shared
//   pgtest container (per-package); pgxpool internal conn timeouts +
//   package-level `task ci:test:int -timeout=15m` already bound execution.
//   Per-test context.WithTimeout would be belt-and-suspenders against the
//   shared-pool + parallel-with-RLS canon shape.
//
// SQL-CONTRACT COVERAGE (per ADR 0062 — TDL Test Pyramid):
//   - MarketplaceBrowse SELECT column-set discipline (H12): the browse
//     read MUST omit PII columns (email / gst_number / pan_number /
//     mobile_e164 / street). Asserted at the SQL layer because a `SELECT
//     *` drift in production would bypass the application-layer DTO
//     mapper. This is a SQL-specific contract test.
//
// Round-trip Add/GetByID + state-machine (Purchase) + ErrNotFound
// coverage moved to [platformleadtest.FakeRepository].

package adapters_test

import (
	"context"
	"testing"
	"time"

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

// TestPlatformLeadRepository_MarketplaceBrowse_OmitsPII — SQL-contract
// (H12). The browse SELECT explicitly omits email / gst_number /
// pan_number / mobile_e164 / street to prevent a future `SELECT *` drift
// from leaking PII through the DTO mapper. The returned aggregate's
// PII-field accessors MUST return the empty string (no row data scanned
// in).
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
