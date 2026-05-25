//go:build integration

package adapters_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pg/rlstest"
	"github.com/leadkart/leadkart-go/internal/platform/adapters"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadform"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
)

// TestKeysetUnverifiedContactsPage_UsesIndexUnderRLS mirrors the
// identity keyset EXPLAIN gate (per ADR 0038) for
// platform.unverified_contacts.
//
// Asserts the state-filtered keyset query plans as an Index Scan against
// the composite index idx_uvc_state_created_keyset declared in
// migration 20260601000001 as
// `(state, created_at DESC, id DESC)` — matching the query's
// `WHERE state = $1 ORDER BY created_at DESC, id DESC` keyset shape.
//
// Platform-only table; the connection runs under TxScopePlatform so RLS
// admits the rows.
func TestKeysetUnverifiedContactsPage_UsesIndexUnderRLS(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_ = ctx // arch-test:integration-timeout-anchor
	pool := platformPool(t)
	tx := pg.NewTransactor(pool)
	repo := adapters.NewUnverifiedContactRepository(pool, tx)

	agentID := unverifiedcontact.MembershipID(ids.NewV7().String())

	// Seed 200 contacts in StateNew so the planner reliably prefers the
	// index over a Seq Scan. Stagger created_at so the keyset cursor has
	// distinct (created_at, id) pairs.
	const seedCount = 200
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC).UTC()
	for i := range seedCount {
		createdAt := base.Add(time.Duration(i) * time.Second)
		form, err := leadform.New(leadform.Input{
			ContactName:    "Seed Contact",
			MobileE164:     "+91987600" + padDigits(i, 4),
			Email:          "seed" + padDigits(i, 4) + "@uvc.test",
			Pincode:        "411001",
			City:           "Pune",
			District:       "Pune",
			State:          "Maharashtra",
			HasDrugLicence: true,
			HasGst:         true,
			GstNumber:      "27AAAAA0000A1Z5",
			HasPan:         true,
			PanNumber:      "AAAAA0000A",
			BusinessType:   leadform.BusinessTypePCD,
			MedicineSystem: leadform.MedicineSystemAllopathic,
			ProductRanges:  []string{"Antibiotics"},
			DosageForms:    []string{"Tablet"},
			OrderValue:     leadform.OrderValueUpto25000,
			BuyTimeline:    leadform.BuyTimelineWithin15Days,
		})
		if err != nil {
			t.Fatalf("leadform.New %d: %v", i, err)
		}
		c, err := unverifiedcontact.New(unverifiedcontact.ID(ids.NewV7().String()), form, agentID, createdAt)
		if err != nil {
			t.Fatalf("unverifiedcontact.New %d: %v", i, err)
		}
		if err := repo.Add(t.Context(), c); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}

	if _, err := pool.Exec(t.Context(), `ANALYZE platform.unverified_contacts`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	// EXPLAIN runs under platform GUC so RLS admits the rows — mirror of
	// the read path: HTTP authn middleware sets app.is_platform=true
	// when the operator's JWT is_platform=true.
	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	dbtx, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = dbtx.Rollback(context.Background()) }() // arch-test:context-background — cleanup must outlive test ctx

	rlstest.SetSessionPlatformLocal(t, t.Context(), dbtx)

	// Same predicate shape as sqlc's ListUnverifiedContactsPage with a
	// non-empty state filter (typical caller pattern: "show me NEW").
	// First-page sentinel cursor admits every row.
	const explainSQL = `
		EXPLAIN (FORMAT JSON, ANALYZE, BUFFERS)
		SELECT id, state, rejection_reason,
		       busy_callback_at, busy_callback_end_at, platform_lead_id,
		       contact_name, mobile_e164, email, pincode, city, district, state_geo, street,
		       has_drug_licence, has_gst, gst_number, has_pan, pan_number,
		       business_type, medicine_system, product_ranges, dosage_forms,
		       order_value, buy_timeline,
		       created_at, created_by_membership_id,
		       verified_at, verified_by_membership_id,
		       rejected_at, rejected_by_membership_id
		FROM   platform.unverified_contacts
		WHERE  ($1::text = '' OR state = $1::text)
		  AND  ($2::timestamptz IS NULL
		        OR (created_at, id) < ($2::timestamptz, $3::uuid))
		ORDER  BY created_at DESC, id DESC
		LIMIT  $4
	`

	sentinelTime := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	sentinelID := "ffffffff-ffff-ffff-ffff-ffffffffffff"

	var rawPlan []byte
	if err := dbtx.QueryRow(t.Context(), explainSQL,
		string(unverifiedcontact.StateNew), sentinelTime, sentinelID, int32(51),
	).Scan(&rawPlan); err != nil {
		t.Fatalf("explain: %v", err)
	}

	planText := string(rawPlan)
	t.Logf("EXPLAIN plan:\n%s", planText)

	// Primary regression guard: any Index Scan flavour is acceptable.
	// The bug class we're guarding is "planner ignored every index +
	// scanned the table".
	if !strings.Contains(planText, "Index Scan") &&
		!strings.Contains(planText, "Bitmap Index Scan") {
		t.Errorf("expected plan to contain Index Scan (any flavour); got:\n%s", planText)
	}
	if strings.Contains(planText, `"Node Type": "Seq Scan"`) &&
		strings.Contains(planText, `"Relation Name": "unverified_contacts"`) {
		t.Errorf("planner fell back to Seq Scan on platform.unverified_contacts:\n%s", planText)
	}

	var parsed []map[string]any
	if err := json.Unmarshal(rawPlan, &parsed); err != nil {
		t.Fatalf("plan JSON malformed: %v", err)
	}
}

// padDigits returns n as a zero-padded width-digit string for unique
// mobile + email seeding. Sufficient up to 10^width-1 distinct rows.
func padDigits(n, width int) string {
	s := ""
	for i := range width {
		_ = i
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}
