package query_test

import (
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant/tenanttest"
)

// ----- GetTenantHandler ----------------------------------------------------

func TestNewGetTenantHandler_PanicsOnNilRepo(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	_ = query.NewGetTenantHandler(nil) // arch-test:ignore-err - test fixture setup
}

func TestGetTenant_RejectsZeroID(t *testing.T) {
	t.Parallel()
	h := query.NewGetTenantHandler(tenanttest.NewFakeRepository())
	_, err := h.Handle(t.Context(), query.GetTenantQuery{})
	if err == nil {
		t.Fatal("expected error on zero tenant id")
	}
}

func TestGetTenant_NotFound(t *testing.T) {
	t.Parallel()
	h := query.NewGetTenantHandler(tenanttest.NewFakeRepository())
	_, err := h.Handle(t.Context(), query.GetTenantQuery{TenantID: testTenantID})
	if !errors.Is(err, tenant.ErrNotFound) {
		t.Fatalf("err = %v, want tenant.ErrNotFound", err)
	}
}

func TestGetTenant_HappyPath_ProjectsEveryVOField(t *testing.T) {
	t.Parallel()
	tn := newFullyPopulatedTenant(t)
	repo := tenanttest.NewFakeRepository()
	if err := repo.Add(t.Context(), tn); err != nil {
		t.Fatal(err)
	}
	h := query.NewGetTenantHandler(repo)
	got, err := h.Handle(t.Context(), query.GetTenantQuery{TenantID: tn.ID()})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// Verify every projected field is non-default.
	if got.ID != tn.ID().String() {
		t.Errorf("ID")
	}
	if got.Slug != testTenantSlugStr {
		t.Errorf("Slug = %q", got.Slug)
	}
	if got.LegalName != "Acme Pharma Pvt Ltd" {
		t.Errorf("LegalName = %q", got.LegalName)
	}
	if got.DisplayName != "Acme Pharma" {
		t.Errorf("DisplayName = %q", got.DisplayName)
	}
	if got.Status == "" {
		t.Errorf("Status empty")
	}
	if got.GSTNumber != "29ABCPE1234F1Z5" {
		t.Errorf("GST = %q", got.GSTNumber)
	}
	if got.PANNumber != "ABCPE1234F" {
		t.Errorf("PAN = %q", got.PANNumber)
	}
	if got.DrugLicenceNumber != "KA-MUM-2024-12345" {
		t.Errorf("DrugLicence = %q", got.DrugLicenceNumber)
	}
	if got.AdminPhone != "+919999999999" {
		t.Errorf("AdminPhone = %q", got.AdminPhone)
	}
	wantAddr := query.AdminAddressView{
		Street: "12 MG Road", City: "Bengaluru", District: "Bengaluru Urban",
		State: "Karnataka", StateCode: "KA", Pincode: "560001",
	}
	if diff := cmp.Diff(wantAddr, got.AdminAddress); diff != "" {
		t.Errorf("AdminAddress (-want +got)\n%s", diff)
	}
	wantPolicy := query.PasswordPolicyView{
		MinLength: 14, RequireUppercase: true, RequireLowercase: true,
		RequireDigit: true, RequireSymbol: false,
		MaxFailedAttempts: 8, LockoutMinutes: 20,
	}
	if diff := cmp.Diff(wantPolicy, got.PasswordPolicy); diff != "" {
		t.Errorf("PasswordPolicy (-want +got)\n%s", diff)
	}
	if got.Locale != "en-IN" || got.TimeZone != "Asia/Kolkata" || got.DateFormat != "DD-MMM-YYYY" || got.Currency != "INR" {
		t.Errorf("Prefs wrong: locale=%q tz=%q df=%q cur=%q", got.Locale, got.TimeZone, got.DateFormat, got.Currency)
	}
	if got.CreatedAt.IsZero() {
		t.Errorf("CreatedAt zero")
	}
}

func TestGetTenant_DefaultZeroVOsProjectAsEmpty(t *testing.T) {
	t.Parallel()
	// Plain newTenant: no statutory, contact, settings, or prefs set.
	tn := newTenant(t, testTenantID, testTenantSlugStr)
	repo := tenanttest.NewFakeRepository()
	if err := repo.Add(t.Context(), tn); err != nil {
		t.Fatal(err)
	}
	h := query.NewGetTenantHandler(repo)
	got, err := h.Handle(t.Context(), query.GetTenantQuery{TenantID: tn.ID()})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got.GSTNumber != "" || got.PANNumber != "" || got.DrugLicenceNumber != "" {
		t.Errorf("statutory non-empty: %+v", got)
	}
	if got.AdminPhone != "" {
		t.Errorf("AdminPhone = %q", got.AdminPhone)
	}
	if got.Locale != "" {
		t.Errorf("Locale = %q", got.Locale)
	}
}

// ----- GetTenantBySlugHandler ---------------------------------------------

func TestNewGetTenantBySlugHandler_PanicsOnNilRepo(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		}
	}()
	_ = query.NewGetTenantBySlugHandler(nil) // arch-test:ignore-err - test fixture setup
}

func TestGetTenantBySlug_RejectsZeroSlug(t *testing.T) {
	t.Parallel()
	h := query.NewGetTenantBySlugHandler(tenanttest.NewFakeRepository())
	_, err := h.Handle(t.Context(), query.GetTenantBySlugQuery{Slug: slug.Slug{}})
	if err == nil {
		t.Fatal("expected error on zero slug")
	}
}

func TestGetTenantBySlug_NotFound(t *testing.T) {
	t.Parallel()
	h := query.NewGetTenantBySlugHandler(tenanttest.NewFakeRepository())
	_, err := h.Handle(t.Context(), query.GetTenantBySlugQuery{Slug: mustSlug(t, "nonexistent-slug")})
	if !errors.Is(err, tenant.ErrNotFound) {
		t.Fatalf("err = %v, want tenant.ErrNotFound", err)
	}
}

func TestGetTenantBySlug_HappyPath(t *testing.T) {
	t.Parallel()
	tn := newTenant(t, testTenantID, testTenantSlugStr)
	repo := tenanttest.NewFakeRepository()
	if err := repo.Add(t.Context(), tn); err != nil {
		t.Fatal(err)
	}
	h := query.NewGetTenantBySlugHandler(repo)
	got, err := h.Handle(t.Context(), query.GetTenantBySlugQuery{Slug: mustSlug(t, testTenantSlugStr)})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got.ID != tn.ID().String() {
		t.Errorf("ID = %q", got.ID)
	}
	if got.Slug != testTenantSlugStr {
		t.Errorf("Slug = %q", got.Slug)
	}
}
