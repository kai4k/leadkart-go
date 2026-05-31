package ports_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
	"github.com/leadkart/leadkart-go/internal/platform/app"
	"github.com/leadkart/leadkart-go/internal/platform/app/command"
	"github.com/leadkart/leadkart-go/internal/platform/app/query"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadform"
	"github.com/leadkart/leadkart-go/internal/platform/domain/platformlead"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
	"github.com/leadkart/leadkart-go/internal/platform/domain/verificationcall"
	"github.com/leadkart/leadkart-go/internal/platform/platformtest"
	"github.com/leadkart/leadkart-go/internal/platform/ports"
)

// fakeVerifier returns its embedded claims (or err) for any token.
type fakeVerifier struct {
	claims *jwt.Claims
	err    error
}

func (f *fakeVerifier) Verify(_ string) (*jwt.Claims, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.claims, nil
}

// alwaysFresh is a stamp validator that always reports fresh.
type alwaysFresh struct{}

func (alwaysFresh) IsFresh(_ context.Context, _, _ string) (bool, error) { return true, nil }

func silentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// buildSampleForm returns a valid leadform.Form for seeding.
func buildSampleForm(t *testing.T) (leadform.Form, error) {
	t.Helper()
	return leadform.New(leadform.Input{
		ContactName:    "Acme Pharma",
		MobileE164:     "+919876543210",
		Pincode:        "411001",
		City:           "Pune",
		District:       "Pune",
		State:          "Maharashtra",
		BusinessType:   leadform.BusinessTypePCD,
		MedicineSystem: leadform.MedicineSystemAllopathic,
		OrderValue:     leadform.OrderValueUpto25000,
		BuyTimeline:    leadform.BuyTimelineWithin15Days,
	})
}

// platformClaims builds Claims that satisfy RequirePlatform (is_platform=true,
// slug=platform) plus perms. Subject + SecurityStamp set so freshness guards pass.
func platformClaims(perms []string) *jwt.Claims {
	c := &jwt.Claims{
		TenantID:      ids.NewV7().String(),
		TenantSlug:    authn.PlatformTenantSlug,
		MembershipID:  ids.NewV7().String(),
		SecurityStamp: ids.NewV7().String(),
		IsPlatform:    true,
		Permissions:   perms,
	}
	c.Subject = ids.NewV7().String()
	return c
}

// tenantClaims builds Claims for a regular tenant user; RequirePlatform must refuse.
func tenantClaims(perms []string) *jwt.Claims {
	c := &jwt.Claims{
		TenantID:      ids.NewV7().String(),
		TenantSlug:    "acme-pharma",
		MembershipID:  ids.NewV7().String(),
		SecurityStamp: ids.NewV7().String(),
		IsPlatform:    false,
		Permissions:   perms,
	}
	c.Subject = ids.NewV7().String()
	return c
}

// buildApp wires a platform Application over in-memory fakes — enough to
// exercise the HTTP layer's wire shapes and middleware composition.
func buildApp(t *testing.T) (app.Application, *platformtest.FakeUnverifiedContactRepository, *platformtest.FakeLeadCreditRepository) {
	t.Helper()
	contacts := platformtest.NewFakeUnverifiedContactRepository()
	leads := platformtest.NewFakePlatformLeadRepository()
	credits := platformtest.NewFakeLeadCreditRepository()
	outbox := platformtest.NewFakeOutbox()
	calls := platformtest.NewFakeVerificationCallRepository()
	uow := platformtest.NewFakeUnitOfWork(credits, leads)
	now := func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) }

	return app.Application{
		Commands: app.Commands{
			CreateUnverifiedContact: command.NewCreateUnverifiedContactHandler(contacts, now, func() unverifiedcontact.ID { return unverifiedcontact.ID(ids.NewV7().String()) }),
			LogVerificationCall:     command.NewLogVerificationCallHandler(uow, calls, contacts, now, func() verificationcall.ID { return verificationcall.ID(ids.NewV7().String()) }),
			VerifyUnverifiedContact: command.NewVerifyUnverifiedContactHandler(uow, contacts, leads, outbox, now, func() platformlead.ID { return platformlead.ID(ids.NewV7().String()) }),
			RejectUnverifiedContact: command.NewRejectUnverifiedContactHandler(contacts, now),
			PurchaseLead:            command.NewPurchaseLeadHandler(uow, leads, credits, outbox, now, func() string { return ids.NewV7().String() }),
			TopupLeadCredits:        command.NewTopupLeadCreditsHandler(uow, credits, now),
		},
		Queries: app.Queries{
			BrowseMarketplace:    query.NewBrowseMarketplaceHandler(leads),
			GetLeadCreditBalance: query.NewGetLeadCreditBalanceHandler(credits),
		},
	}, contacts, credits
}

func wireMux(t *testing.T, claims *jwt.Claims, a app.Application) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	ports.AddRoutes(mux, silentLog(), a, &fakeVerifier{claims: claims}, alwaysFresh{})
	return mux
}

func doRequest(t *testing.T, mux *http.ServeMux, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequestWithContext(t.Context(), method, path, rdr)
	req.Header.Set("Authorization", "Bearer xxx")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestHandleCreate_Platform_OK — happy path for a Platform-tier operator (C2).
func TestHandleCreate_Platform_OK(t *testing.T) {
	t.Parallel()

	claims := platformClaims([]string{permission.IdentityPermissions.PlatformUnverifiedContacts.Manage})
	a, _, _ := buildApp(t)
	mux := wireMux(t, claims, a)

	body := ports.CreateUnverifiedContactRequest{
		ContactName:    "Acme Pharma",
		MobileE164:     "+919876543210",
		Pincode:        "411001",
		City:           "Pune",
		District:       "Pune",
		State:          "Maharashtra",
		BusinessType:   "PCD",
		MedicineSystem: "Allopathic",
		OrderValue:     "Upto25000",
		BuyTimeline:    "Within15Days",
	}
	rec := doRequest(t, mux, "POST", "/api/v1/platform/unverified-contacts", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s want 201", rec.Code, rec.Body.String())
	}
	var out ports.CreateUnverifiedContactResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if out.ContactID == "" {
		t.Error("ContactID missing")
	}
}

// TestHandleCreate_Tenant_RefusedByRequirePlatform — C5+C2: a tenant role with
// the right permission is still refused (is_platform=false).
func TestHandleCreate_Tenant_RefusedByRequirePlatform(t *testing.T) {
	t.Parallel()

	claims := tenantClaims([]string{permission.IdentityPermissions.PlatformUnverifiedContacts.Manage})
	a, _, _ := buildApp(t)
	mux := wireMux(t, claims, a)

	body := ports.CreateUnverifiedContactRequest{
		ContactName: "Should Be Refused",
		MobileE164:  "+919876543210", Pincode: "411001", City: "Pune", District: "Pune", State: "Maharashtra",
		BusinessType: "PCD", MedicineSystem: "Allopathic",
		OrderValue: "Upto25000", BuyTimeline: "Within15Days",
	}
	rec := doRequest(t, mux, "POST", "/api/v1/platform/unverified-contacts", body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (RequirePlatform), got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleListUnverifiedContacts_Tenant_Refused — LIST exposes the whole
// pipeline; a tenant must not reach it (C5).
func TestHandleListUnverifiedContacts_Tenant_Refused(t *testing.T) {
	t.Parallel()

	claims := tenantClaims([]string{permission.IdentityPermissions.PlatformUnverifiedContacts.Manage})
	a, _, _ := buildApp(t)
	mux := wireMux(t, claims, a)

	rec := doRequest(t, mux, "GET", "/api/v1/platform/unverified-contacts", nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleTopupLeadCredits_Tenant_Refused — C5: a tenant can't top up even
// with the topup permission; RequirePlatform is the load-bearing gate.
func TestHandleTopupLeadCredits_Tenant_Refused(t *testing.T) {
	t.Parallel()

	claims := tenantClaims([]string{permission.IdentityPermissions.PlatformLeadCredits.Topup})
	a, _, _ := buildApp(t)
	mux := wireMux(t, claims, a)

	body := ports.TopupLeadCreditsRequest{
		TenantID: ids.NewV7().String(),
		Delta:    100,
		Reason:   "test",
	}
	rec := doRequest(t, mux, "POST", "/api/v1/platform/lead-credits/topup", body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleTopupLeadCredits_Platform_OK — happy path for a Platform operator.
func TestHandleTopupLeadCredits_Platform_OK(t *testing.T) {
	t.Parallel()

	claims := platformClaims([]string{permission.IdentityPermissions.PlatformLeadCredits.Topup})
	a, _, credits := buildApp(t)
	mux := wireMux(t, claims, a)

	tenantID := ids.NewV7().String()
	body := ports.TopupLeadCreditsRequest{
		TenantID: tenantID,
		Delta:    250,
		Reason:   "Q3 marketing budget",
	}
	rec := doRequest(t, mux, "POST", "/api/v1/platform/lead-credits/topup", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s want 200", rec.Code, rec.Body.String())
	}
	var out ports.TopupLeadCreditsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.NewBalance != 250 {
		t.Errorf("NewBalance=%d want 250", out.NewBalance)
	}
	c, err := credits.GetByTenant(t.Context(), leadcredit.TenantID(tenantID))
	if err != nil {
		t.Fatalf("GetByTenant: %v", err)
	}
	if c.Balance() != 250 {
		t.Errorf("stored Balance=%d want 250", c.Balance())
	}
}

// TestHandlePurchaseLead_Tenant_HappyPath — purchase is a tenant action;
// confirms RequirePlatform is NOT on the purchase route.
func TestHandlePurchaseLead_Tenant_HappyPath(t *testing.T) {
	t.Parallel()

	claims := tenantClaims([]string{permission.IdentityPermissions.PlatformMarketplace.Purchase})

	contacts := platformtest.NewFakeUnverifiedContactRepository()
	leads := platformtest.NewFakePlatformLeadRepository()
	credits := platformtest.NewFakeLeadCreditRepository()
	outbox := platformtest.NewFakeOutbox()
	calls := platformtest.NewFakeVerificationCallRepository()
	uow := platformtest.NewFakeUnitOfWork(credits, leads)
	now := func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) }

	// Seed a lead and credit balance for the caller's tenant.
	agentID := unverifiedcontact.MembershipID(ids.NewV7().String())
	contactID := unverifiedcontact.ID(ids.NewV7().String())
	leadID := platformlead.ID(ids.NewV7().String())
	form, err := buildSampleForm(t)
	if err != nil {
		t.Fatalf("form: %v", err)
	}
	l, err := platformlead.NewFromUnverifiedContact(leadID, contactID, form, agentID, now())
	if err != nil {
		t.Fatalf("seed lead: %v", err)
	}
	if err := leads.Add(t.Context(), l); err != nil {
		t.Fatalf("seed persist: %v", err)
	}

	tenantID := leadcredit.TenantID(claims.TenantID)
	cr, err := leadcredit.NewForTenant(tenantID, now())
	if err != nil {
		t.Fatalf("ctor credit: %v", err)
	}
	if err := cr.Topup(10, "seed", leadcredit.MembershipID(claims.MembershipID), now()); err != nil {
		t.Fatalf("topup: %v", err)
	}
	if err := credits.UpsertWithVersion(t.Context(), cr); err != nil {
		t.Fatalf("persist credit: %v", err)
	}

	a := app.Application{
		Commands: app.Commands{
			CreateUnverifiedContact: command.NewCreateUnverifiedContactHandler(contacts, now, func() unverifiedcontact.ID { return unverifiedcontact.ID(ids.NewV7().String()) }),
			LogVerificationCall:     command.NewLogVerificationCallHandler(uow, calls, contacts, now, func() verificationcall.ID { return verificationcall.ID(ids.NewV7().String()) }),
			VerifyUnverifiedContact: command.NewVerifyUnverifiedContactHandler(uow, contacts, leads, outbox, now, func() platformlead.ID { return platformlead.ID(ids.NewV7().String()) }),
			RejectUnverifiedContact: command.NewRejectUnverifiedContactHandler(contacts, now),
			PurchaseLead:            command.NewPurchaseLeadHandler(uow, leads, credits, outbox, now, func() string { return ids.NewV7().String() }),
			TopupLeadCredits:        command.NewTopupLeadCreditsHandler(uow, credits, now),
		},
		Queries: app.Queries{
			BrowseMarketplace:    query.NewBrowseMarketplaceHandler(leads),
			GetLeadCreditBalance: query.NewGetLeadCreditBalanceHandler(credits),
		},
	}
	mux := wireMux(t, claims, a)

	body := ports.PurchaseLeadRequest{AmountPaisa: 50000}
	rec := doRequest(t, mux, "POST", "/api/v1/platform/marketplace/leads/"+leadID.String()+"/purchase", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s want 201", rec.Code, rec.Body.String())
	}
	var out ports.PurchaseLeadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if out.PurchaseID == "" {
		t.Error("PurchaseID missing")
	}
	if out.PlatformLeadID != leadID.String() {
		t.Errorf("PlatformLeadID=%q want %q", out.PlatformLeadID, leadID)
	}
}

// TestHandleVerify_AlreadyTerminal_409 — H11+C2: ErrContactAlreadyTerminal
// maps to 409 with the right error code.
func TestHandleVerify_AlreadyTerminal_409(t *testing.T) {
	t.Parallel()

	claims := platformClaims([]string{permission.IdentityPermissions.PlatformUnverifiedContacts.Manage})

	// Seed a contact already in a terminal state.
	contacts := platformtest.NewFakeUnverifiedContactRepository()
	leads := platformtest.NewFakePlatformLeadRepository()
	credits := platformtest.NewFakeLeadCreditRepository()
	outbox := platformtest.NewFakeOutbox()
	calls := platformtest.NewFakeVerificationCallRepository()
	uow := platformtest.NewFakeUnitOfWork(credits, leads)
	now := func() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) }

	form, err := buildSampleForm(t)
	if err != nil {
		t.Fatalf("form: %v", err)
	}
	agentID := unverifiedcontact.MembershipID(ids.NewV7().String())
	cID := unverifiedcontact.ID(ids.NewV7().String())
	c, err := unverifiedcontact.New(cID, form, agentID, now())
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := c.StartCall(now()); err != nil {
		t.Fatalf("start call: %v", err)
	}
	if err := c.MarkRejected("test", agentID, now()); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if err := contacts.Add(t.Context(), c); err != nil {
		t.Fatalf("add: %v", err)
	}

	a := app.Application{
		Commands: app.Commands{
			CreateUnverifiedContact: command.NewCreateUnverifiedContactHandler(contacts, now, func() unverifiedcontact.ID { return unverifiedcontact.ID(ids.NewV7().String()) }),
			LogVerificationCall:     command.NewLogVerificationCallHandler(uow, calls, contacts, now, func() verificationcall.ID { return verificationcall.ID(ids.NewV7().String()) }),
			VerifyUnverifiedContact: command.NewVerifyUnverifiedContactHandler(uow, contacts, leads, outbox, now, func() platformlead.ID { return platformlead.ID(ids.NewV7().String()) }),
			RejectUnverifiedContact: command.NewRejectUnverifiedContactHandler(contacts, now),
			PurchaseLead:            command.NewPurchaseLeadHandler(uow, leads, credits, outbox, now, func() string { return ids.NewV7().String() }),
			TopupLeadCredits:        command.NewTopupLeadCreditsHandler(uow, credits, now),
		},
		Queries: app.Queries{
			BrowseMarketplace:    query.NewBrowseMarketplaceHandler(leads),
			GetLeadCreditBalance: query.NewGetLeadCreditBalanceHandler(credits),
		},
	}
	mux := wireMux(t, claims, a)

	rec := doRequest(t, mux, "POST", "/api/v1/platform/unverified-contacts/"+cID.String()+"/verify", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s want 409", rec.Code, rec.Body.String())
	}
	var out ports.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, rec.Body.String())
	}
	if out.Error != ports.ErrCodeContactAlreadyTerminal {
		t.Errorf("error=%q want %q", out.Error, ports.ErrCodeContactAlreadyTerminal)
	}
}

// keeps errors imported when tests are sliced
var _ = errors.New
