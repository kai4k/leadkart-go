//go:build integration

// GET /api/v1/tenants/by-slug/{slug} enumeration-safety matrix.
//
// Slugs are human-readable + guessable (the company name); the
// canonical security property is that callers who lack access see
// 404, indistinguishable from "slug does not exist". GitHub / Stripe
// / Auth0 canon for natural-key resource lookups; codified in ADR 0044.
//
// The matrix:
//
//   | Caller              | Slug exists, theirs | Others slug | Missing slug | Bad slug |
//   |---------------------|---------------------|-------------|--------------|----------|
//   | Tenant admin (A)    | 200 + TenantDto     | 404         | 404          | 400      |
//   | Platform operator   | 200 + TenantDto     | 200         | 404          | 400      |
//   | Unauthenticated     | 401 (RequireFreshStamp short-circuits) |  |          |          |
//
// Each row is a separate test for failure-isolation. Tests run in
// parallel via testcontainers-per-test (default).

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// TestE2E_TenantBySlug_OwnSlug_Returns200 baseline happy path.
// Tenant admin reads their own tenant by slug.
func TestE2E_TenantBySlug_OwnSlug_Returns200(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "acme")

	resp := f.authedJSON(t, http.MethodGet,
		"/api/v1/tenants/by-slug/"+tenantA.Slug, tenantA.AccessToken, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("GET own slug: status %d, want 200; body=%s", resp.status, resp.body)
	}
	var dto ports.TenantDto
	if err := json.Unmarshal(resp.body, &dto); err != nil {
		t.Fatalf("decode TenantDto: %v", err)
	}
	if dto.ID != tenantA.TenantID {
		t.Errorf("tenant.ID = %q, want %q", dto.ID, tenantA.TenantID)
	}
	if dto.Slug != tenantA.Slug {
		t.Errorf("tenant.Slug = %q, want %q", dto.Slug, tenantA.Slug)
	}
}

// TestE2E_TenantBySlug_OthersSlug_Returns404 the load-bearing
// security property. Tenant A admin probes Tenant B slug, must 404
// (not 403 which would confirm "tenant exists, you cannot see it"
// to the attacker; 404 is indistinguishable from "does not exist").
//
// ADR 0044 enumeration safety. GitHub / Stripe / Auth0 canon.
func TestE2E_TenantBySlug_OthersSlug_Returns404(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "acme")
	tenantB := f.registerAndLogin(t, "globex")

	resp := f.authedJSON(t, http.MethodGet,
		"/api/v1/tenants/by-slug/"+tenantB.Slug, tenantA.AccessToken, nil)
	if resp.status != http.StatusNotFound {
		t.Errorf("GET cross-tenant slug: status %d, want 404 (enumeration-safe); body=%s",
			resp.status, resp.body)
	}
	er := decodeError(t, resp.body)
	if er.Error != ports.ErrCodeTenantNotFound {
		t.Errorf("error code: got %q, want %q", er.Error, ports.ErrCodeTenantNotFound)
	}
	// Critical: body MUST be identical to "slug does not exist" case
	// (no Message difference) otherwise the attacker can distinguish
	// "real but private" from "does not exist", breaking enumeration
	// safety.
	if len(er.Message) != 0 {
		t.Errorf("message: got %q, want empty (no info leak about slug existence)", er.Message)
	}
}

// TestE2E_TenantBySlug_MissingSlug_Returns404 paired with the
// previous test: when the slug genuinely does not exist, response
// shape MUST be identical to "exists but no access". Together these
// two tests prove enumeration safety.
func TestE2E_TenantBySlug_MissingSlug_Returns404(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "acme")

	probe := "nonexistent-tenant-" + ids.NewV7().String()[:8]
	resp := f.authedJSON(t, http.MethodGet,
		"/api/v1/tenants/by-slug/"+probe, tenantA.AccessToken, nil)
	if resp.status != http.StatusNotFound {
		t.Errorf("GET missing slug: status %d, want 404; body=%s", resp.status, resp.body)
	}
	er := decodeError(t, resp.body)
	if er.Error != ports.ErrCodeTenantNotFound {
		t.Errorf("error code: got %q, want %q", er.Error, ports.ErrCodeTenantNotFound)
	}
	if len(er.Message) != 0 {
		t.Errorf("message: got %q, want empty (must match cross-tenant 404 byte-for-byte)", er.Message)
	}
}

// TestE2E_TenantBySlug_InvalidSlug_Returns400 malformed slug (bad
// chars / too long / reserved) returns 400 invalid_slug. Distinct
// from the 404 path because "invalid format" is a client bug that
// should surface as a client error, not security-hide.
func TestE2E_TenantBySlug_InvalidSlug_Returns400(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "acme")

	// URL-encoded space passes the path-param decoder but fails
	// slug.New (slug VO rejects spaces).
	resp := f.authedJSON(t, http.MethodGet,
		"/api/v1/tenants/by-slug/has%20space", tenantA.AccessToken, nil)
	if resp.status != http.StatusBadRequest {
		t.Errorf("GET invalid slug: status %d, want 400; body=%s", resp.status, resp.body)
	}
	er := decodeError(t, resp.body)
	if er.Error != ports.ErrCodeInvalidSlug {
		t.Errorf("error code: got %q, want %q", er.Error, ports.ErrCodeInvalidSlug)
	}
}

// TestE2E_TenantBySlug_PlatformOperator_SeesAnySlug operators bypass
// the same-tenant gate (ADR 0039). Probing any real slug returns the
// full DTO; probing a non-existent slug still returns 404.
func TestE2E_TenantBySlug_PlatformOperator_SeesAnySlug(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "acme")
	tenantB := f.registerAndLogin(t, "globex")
	platformTok := f.mintPlatformToken(t, "")

	// Operator reads tenant A by slug.
	respA := f.authedJSON(t, http.MethodGet,
		"/api/v1/tenants/by-slug/"+tenantA.Slug, platformTok, nil)
	if respA.status != http.StatusOK {
		t.Fatalf("operator GET tenant A by slug: status %d body=%s", respA.status, respA.body)
	}
	var dtoA ports.TenantDto
	if err := json.Unmarshal(respA.body, &dtoA); err != nil {
		t.Fatalf("decode A: %v", err)
	}
	if dtoA.ID != tenantA.TenantID {
		t.Errorf("tenant A ID mismatch: got %q want %q", dtoA.ID, tenantA.TenantID)
	}

	// Same operator reads tenant B by slug.
	respB := f.authedJSON(t, http.MethodGet,
		"/api/v1/tenants/by-slug/"+tenantB.Slug, platformTok, nil)
	if respB.status != http.StatusOK {
		t.Fatalf("operator GET tenant B by slug: status %d body=%s", respB.status, respB.body)
	}
	var dtoB ports.TenantDto
	if err := json.Unmarshal(respB.body, &dtoB); err != nil {
		t.Fatalf("decode B: %v", err)
	}
	if dtoB.ID != tenantB.TenantID {
		t.Errorf("tenant B ID mismatch: got %q want %q", dtoB.ID, tenantB.TenantID)
	}

	// Operator probes non-existent slug, still 404 even for operators.
	missing := "nonexistent-" + ids.NewV7().String()[:8]
	respMiss := f.authedJSON(t, http.MethodGet,
		"/api/v1/tenants/by-slug/"+missing, platformTok, nil)
	if respMiss.status != http.StatusNotFound {
		t.Errorf("operator GET missing slug: status %d, want 404; body=%s", respMiss.status, respMiss.body)
	}
}

// TestE2E_TenantBySlug_Unauthenticated_Returns401 no bearer token
// means the RequireFreshStamp middleware short-circuits before the
// handler runs. Sanity check that the middleware is wired on this
// route.
func TestE2E_TenantBySlug_Unauthenticated_Returns401(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "acme")

	req, _ := http.NewRequest(http.MethodGet, f.URL+"/api/v1/tenants/by-slug/"+tenantA.Slug, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET no-token: status %d, want 401", resp.StatusCode)
	}
}

// TestE2E_TenantBySlug_ResponseShapesIdentical the load-bearing
// proof that 404-cross-tenant and 404-missing-slug are byte-for-byte
// identical. If they diverge, the attacker can use response-shape
// diffing to distinguish "real but private" from "does not exist",
// breaking ADR 0044.
//
// Specifically asserts:
//   - status code identical
//   - body bytes identical (no length / whitespace / field-order leak)
func TestE2E_TenantBySlug_ResponseShapesIdentical(t *testing.T) {
	f := newE2EFixture(t)
	tenantA := f.registerAndLogin(t, "acme")
	tenantB := f.registerAndLogin(t, "globex")

	crossTenant := f.authedJSON(t, http.MethodGet,
		"/api/v1/tenants/by-slug/"+tenantB.Slug, tenantA.AccessToken, nil)

	missing := f.authedJSON(t, http.MethodGet,
		"/api/v1/tenants/by-slug/nonexistent-"+ids.NewV7().String()[:8],
		tenantA.AccessToken, nil)

	if crossTenant.status != missing.status {
		t.Errorf("status mismatch breaks enumeration safety: cross-tenant=%d missing=%d",
			crossTenant.status, missing.status)
	}
	if !bytes.Equal(crossTenant.body, missing.body) {
		t.Errorf("body mismatch breaks enumeration safety:\n  cross-tenant: %s\n  missing:      %s",
			crossTenant.body, missing.body)
	}
}
