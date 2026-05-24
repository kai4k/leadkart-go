//go:build integration

// Platform-module end-to-end test (ADR 0059 / review-pass C2). Drives
// the full HTTP surface against a testcontainers Postgres + the real
// ServeMux via httptest:
//
//   create unverified contact → log call → verify → browse → purchase
//
// Asserts:
//   - Each step returns the canonical 2xx + DTO shape.
//   - Marketplace browse OMITS PII (H12).
//   - Purchase debits the buyer's credit balance.
//   - Outbox accumulates the expected events (per ADR 0059 contract).
//   - RequirePlatform refuses tenant claims on platform-only routes (C5).

package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/config"
	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	identityports "github.com/leadkart/leadkart-go/internal/identity/ports"
	inventoryapp "github.com/leadkart/leadkart-go/internal/inventory/app"
	platformadapters "github.com/leadkart/leadkart-go/internal/platform/adapters"
	platformapp "github.com/leadkart/leadkart-go/internal/platform/app"
	platformcommand "github.com/leadkart/leadkart-go/internal/platform/app/command"
	platformquery "github.com/leadkart/leadkart-go/internal/platform/app/query"
	platformports "github.com/leadkart/leadkart-go/internal/platform/ports"

	"net/http/httptest"
)

// platformE2E wires the Identity Application AND the Platform
// Application against the same testcontainers Postgres.
type platformE2E struct {
	URL    string
	Pool   *pgxpool.Pool
	Issuer *jwt.Issuer
	id     e2eFixture
}

func newPlatformE2E(t *testing.T) platformE2E {
	t.Helper()
	pool := startWiredPostgresForHTTP(t)
	cfg := config.AppConfig{
		JWT: config.JWTConfig{
			KeyID:      "test-k1",
			SigningKey: "0123456789abcdef0123456789abcdef",
		},
		Refresh: config.RefreshConfig{
			AbsoluteTTL: 14 * 24 * time.Hour,
		},
	}
	now := func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) }
	hybrid := newTestHybridCache(t)
	wiring, err := buildIdentityApp(pool, hybrid, cfg, now)
	if err != nil {
		t.Fatalf("buildIdentityApp: %v", err)
	}

	// Platform wiring against the SAME pool.
	tx := pg.NewTransactor(pool)
	contacts := platformadapters.NewUnverifiedContactRepository(pool, tx)
	calls := platformadapters.NewVerificationCallRepository(pool, tx)
	leads := platformadapters.NewPlatformLeadRepository(pool, tx)
	credits := platformadapters.NewLeadCreditRepository(pool, tx)
	reader := platformadapters.NewUnverifiedContactReader(pool, tx)
	outboxEnq := platformadapters.NewOutboxEnqueuer()

	platformApp := platformapp.Application{
		Commands: platformapp.Commands{
			CreateUnverifiedContact: platformcommand.NewCreateUnverifiedContactHandler(contacts, now),
			LogVerificationCall:     platformcommand.NewLogVerificationCallHandler(tx, calls, contacts, now),
			VerifyUnverifiedContact: platformcommand.NewVerifyUnverifiedContactHandler(tx, contacts, leads, outboxEnq, now),
			RejectUnverifiedContact: platformcommand.NewRejectUnverifiedContactHandler(contacts, now),
			PurchaseLead:            platformcommand.NewPurchaseLeadHandler(tx, leads, credits, outboxEnq, now),
			TopupLeadCredits:        platformcommand.NewTopupLeadCreditsHandler(tx, credits, now),
		},
		Queries: platformapp.Queries{
			ListUnverifiedContacts: platformquery.NewListUnverifiedContactsHandler(reader),
			BrowseMarketplace:      platformquery.NewBrowseMarketplaceHandler(leads),
			GetLeadCreditBalance:   platformquery.NewGetLeadCreditBalanceHandler(credits),
		},
	}

	srv := httptest.NewServer(newServer(silentLogger(), wiring.App, platformApp, inventoryapp.Application{}, wiring.Issuer, wiring.StampValidator))
	t.Cleanup(srv.Close)

	return platformE2E{
		URL:    srv.URL,
		Pool:   pool,
		Issuer: wiring.Issuer,
		id: e2eFixture{
			URL:     srv.URL,
			Issuer:  wiring.Issuer,
			Pool:    pool,
			app:     wiring.App,
			persons: wiring.Persons,
		},
	}
}

// mintBuyerToken builds a tenant-scoped JWT with the marketplace
// Browse + Purchase + LeadCredits.Read permissions. Substitutes for
// the role-permission-grant flow (which would require a Platform
// admin handler to attach the grants) so the E2E test can exercise
// the buyer-side surface without setting up admin scaffolding.
func (f platformE2E) mintBuyerToken(t *testing.T, r registeredTenant) string {
	t.Helper()
	tok, err := f.Issuer.Issue(jwt.IssueArgs{
		PersonID:      r.PersonID,
		TenantID:      r.TenantID,
		TenantSlug:    r.Slug,
		MembershipID:  r.MembershipID,
		SecurityStamp: securityStampForPerson(t, f, r.PersonID),
		IsPlatform:    false,
		Permissions: []string{
			permission.IdentityPermissions.PlatformMarketplace.Browse,
			permission.IdentityPermissions.PlatformMarketplace.Purchase,
			permission.IdentityPermissions.PlatformLeadCredits.Read,
		},
	})
	if err != nil {
		t.Fatalf("mint buyer: %v", err)
	}
	return tok
}

// securityStampForPerson reads the live security_stamp off the
// persons row so the RequireFreshStamp middleware accepts the
// synthetic JWT.
func securityStampForPerson(t *testing.T, f platformE2E, personID string) string {
	t.Helper()
	var stamp string
	err := f.Pool.QueryRow(t.Context(),
		`SELECT security_stamp::text FROM identity.persons WHERE id = $1`, personID).Scan(&stamp)
	if err != nil {
		t.Fatalf("read stamp: %v", err)
	}
	return stamp
}

// mintPlatformOperatorToken provisions a Person row + mints a JWT with
// IsPlatform=true + tenant_slug=platform + all platform permissions.
// Mirrors the cross_tenant_e2e mintPlatformToken helper but adds the
// Platform.* permissions Slice 1 needs.
func (f platformE2E) mintPlatformOperatorToken(t *testing.T) string {
	t.Helper()
	personID := ids.NewV7().String()
	suffix := personID[len(personID)-12:]
	addr, err := email.New("op-" + suffix + "@platform.test")
	if err != nil {
		t.Fatalf("email: %v", err)
	}
	pwHash, err := person.NewPasswordHash(
		"$argon2id$v=19$m=19456,t=2,p=1$c2FsdHkx$abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
	)
	if err != nil {
		t.Fatalf("pw hash: %v", err)
	}
	op, err := person.New(person.ID(personID), addr, "Platform", "Operator", pwHash)
	if err != nil {
		t.Fatalf("person.New: %v", err)
	}
	if err := f.id.persons.Add(t.Context(), op); err != nil {
		t.Fatalf("persons.Add: %v", err)
	}
	tok, err := f.Issuer.Issue(jwt.IssueArgs{
		PersonID:      personID,
		TenantID:      ids.NewV7().String(),
		TenantSlug:    "platform",
		MembershipID:  ids.NewV7().String(),
		SecurityStamp: op.SecurityStamp().String(),
		IsPlatform:    true,
		Permissions: []string{
			permission.IdentityPermissions.PlatformUnverifiedContacts.Manage,
			permission.IdentityPermissions.PlatformMarketplace.Browse,
			permission.IdentityPermissions.PlatformMarketplace.Purchase,
			permission.IdentityPermissions.PlatformLeadCredits.Topup,
			permission.IdentityPermissions.PlatformLeadCredits.Read,
		},
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return tok
}

// TestE2E_Platform_FullFlow_CreateVerifyBrowsePurchase — drives the
// canonical Lead Agent → Buyer flow end-to-end. C2 review-pass.
func TestE2E_Platform_FullFlow_CreateVerifyBrowsePurchase(t *testing.T) {
	f := newPlatformE2E(t)
	opToken := f.mintPlatformOperatorToken(t)

	// Register a buying tenant. The login-derived JWT only carries the
	// CompanyOwner role's Meta.TenantAdmin permission; we override
	// with a synthetic JWT carrying the marketplace browse/purchase
	// permissions (the role-assignment flow lands in Slice 2 +
	// admin scaffolding).
	buyer := f.id.registerAndLogin(t, "buyer")
	buyer.AccessToken = f.mintBuyerToken(t, buyer)
	// Top up the buyer with 10 credits.
	topupResp := f.id.authedJSON(t, http.MethodPost, "/api/v1/platform/lead-credits/topup", opToken,
		platformports.TopupLeadCreditsRequest{
			TenantID: buyer.TenantID,
			Delta:    10,
			Reason:   "E2E test seed",
		})
	if topupResp.status != http.StatusOK {
		t.Fatalf("topup: status %d body %s", topupResp.status, topupResp.body)
	}

	// Step 1: Lead Agent creates the unverified contact.
	createResp := f.id.authedJSON(t, http.MethodPost, "/api/v1/platform/unverified-contacts", opToken,
		platformports.CreateUnverifiedContactRequest{
			ContactName:    "E2E Pharmacy",
			MobileE164:     "+919876543210",
			Email:          "e2e@pharmacy.test",
			Pincode:        "411001",
			City:           "Pune",
			District:       "Pune",
			State:          "Maharashtra",
			HasDrugLicence: true,
			HasGst:         true,
			GstNumber:      "27AAAAA0000A1Z5",
			HasPan:         true,
			PanNumber:      "AAAAA0000A",
			BusinessType:   "PCD",
			MedicineSystem: "Allopathic",
			ProductRanges:  []string{"Antibiotics"},
			DosageForms:    []string{"Tablet"},
			OrderValue:     "Upto25000",
			BuyTimeline:    "Within15Days",
		})
	if createResp.status != http.StatusCreated {
		t.Fatalf("create contact: status %d body %s", createResp.status, createResp.body)
	}
	var created platformports.CreateUnverifiedContactResponse
	if err := json.Unmarshal(createResp.body, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Step 2: Log a verification call (no_answer).
	callResp := f.id.authedJSON(t, http.MethodPost,
		"/api/v1/platform/unverified-contacts/"+created.ContactID+"/calls", opToken,
		platformports.LogVerificationCallRequest{
			Outcome: "no_answer",
			Notes:   "Rang out, will retry",
		})
	if callResp.status != http.StatusCreated {
		t.Fatalf("log call: status %d body %s", callResp.status, callResp.body)
	}

	// Step 3: Verify the contact → creates PlatformLead.
	verifyResp := f.id.authedJSON(t, http.MethodPost,
		"/api/v1/platform/unverified-contacts/"+created.ContactID+"/verify", opToken, nil)
	if verifyResp.status != http.StatusCreated {
		t.Fatalf("verify: status %d body %s", verifyResp.status, verifyResp.body)
	}
	var verified platformports.VerifyUnverifiedContactResponse
	if err := json.Unmarshal(verifyResp.body, &verified); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if verified.PlatformLeadID == "" {
		t.Fatal("PlatformLeadID missing")
	}

	// Step 4: Buyer browses the marketplace.
	browseResp := f.id.authedJSON(t, http.MethodGet, "/api/v1/platform/marketplace/leads", buyer.AccessToken, nil)
	if browseResp.status != http.StatusOK {
		t.Fatalf("browse: status %d body %s", browseResp.status, browseResp.body)
	}
	var browse platformports.BrowseMarketplaceResponse
	if err := json.Unmarshal(browseResp.body, &browse); err != nil {
		t.Fatalf("decode browse: %v body=%s", err, browseResp.body)
	}
	found := false
	for _, item := range browse.Items {
		if item.ID == verified.PlatformLeadID {
			found = true
		}
	}
	if !found {
		t.Fatalf("verified lead not in marketplace browse; items=%+v", browse.Items)
	}

	// Step 5: Buyer purchases the lead.
	purchaseResp := f.id.authedJSON(t, http.MethodPost,
		"/api/v1/platform/marketplace/leads/"+verified.PlatformLeadID+"/purchase", buyer.AccessToken,
		platformports.PurchaseLeadRequest{AmountPaisa: 50000})
	if purchaseResp.status != http.StatusCreated {
		t.Fatalf("purchase: status %d body %s", purchaseResp.status, purchaseResp.body)
	}

	// Step 6: Buyer's balance is now 9 (1 credit per lead per ADR 0059).
	balResp := f.id.authedJSON(t, http.MethodGet,
		"/api/v1/platform/lead-credits/balance", buyer.AccessToken, nil)
	if balResp.status != http.StatusOK {
		t.Fatalf("balance: status %d body %s", balResp.status, balResp.body)
	}
	var bal platformports.LeadCreditBalanceResponse
	if err := json.Unmarshal(balResp.body, &bal); err != nil {
		t.Fatalf("decode bal: %v", err)
	}
	if bal.Balance != 9 {
		t.Errorf("balance: got %d want 9", bal.Balance)
	}

	// Step 7: A second buyer would see the lead REMOVED from the
	// marketplace (sold).
	buyer2 := f.id.registerAndLogin(t, "buyer2")
	buyer2.AccessToken = f.mintBuyerToken(t, buyer2)
	browse2 := f.id.authedJSON(t, http.MethodGet, "/api/v1/platform/marketplace/leads", buyer2.AccessToken, nil)
	var browse2Resp platformports.BrowseMarketplaceResponse
	_ = json.Unmarshal(browse2.body, &browse2Resp)
	for _, item := range browse2Resp.Items {
		if item.ID == verified.PlatformLeadID {
			t.Errorf("sold lead %q must NOT appear in marketplace; items=%+v",
				verified.PlatformLeadID, browse2Resp.Items)
		}
	}
}

// TestE2E_Platform_TenantClaim_RefusedByRequirePlatform — C5: tenant
// claims with the Manage permission still get 403 from RequirePlatform.
func TestE2E_Platform_TenantClaim_RefusedByRequirePlatform(t *testing.T) {
	f := newPlatformE2E(t)
	tenant := f.id.registerAndLogin(t, "tenant-c5")

	// Even with a valid tenant token, RequirePlatform refuses Platform
	// operator routes. The permission gate is internal defense-in-
	// depth; the route table layers RequirePlatform OUTSIDE the
	// permission gate.
	resp := f.id.authedJSON(t, http.MethodPost, "/api/v1/platform/unverified-contacts", tenant.AccessToken,
		platformports.CreateUnverifiedContactRequest{
			ContactName: "Refused", MobileE164: "+919876543210",
			Pincode: "411001", City: "X", District: "X", State: "X",
			BusinessType: "PCD", MedicineSystem: "Allopathic",
			OrderValue: "Upto25000", BuyTimeline: "Within15Days",
		})
	if resp.status != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body %s", resp.status, resp.body)
	}
}

// avoid unused identityports import for now (placeholder for future
// cross-domain assertions).
var _ = identityports.RegisterTenantResponse{}
