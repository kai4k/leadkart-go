//go:build integration

// arch-test:no-timeout-needed — newE2EFixture bounds container startup; per-request
// HTTP uses t.Context() (auto-canceled on test end).

// Tenant hard delete is ONE transaction (the aggregate transition, its
// tenant_deleted outbox row and the row delete): a tenant that still has
// dependents answers 409 tenant_has_dependents and nothing changes; a tenant
// without dependents is deleted and leaves exactly one attributed outbox row.

package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// bareTenant inserts a pending tenant with no memberships, roles or other
// dependents, through the repository (the path RegisterTenant's tenant insert uses).
func bareTenant(t *testing.T, f e2eFixture, namePrefix string) tenant.ID {
	t.Helper()
	suffix := ids.NewV7().String()[24:]
	s, err := slug.New(namePrefix + "-" + suffix)
	if err != nil {
		t.Fatalf("slug: %v", err)
	}
	admin, err := email.New(namePrefix + "-" + suffix + "@e2e.test")
	if err != nil {
		t.Fatalf("email: %v", err)
	}
	id := tenant.ID(ids.NewV7().String())
	tn, err := tenant.New(id, s, namePrefix+" Pharma", namePrefix, admin, testNow)
	if err != nil {
		t.Fatalf("tenant.New: %v", err)
	}
	if err := adapters.NewTenantRepository(f.Pool, pg.NewTransactor(f.Pool)).Add(t.Context(), tn); err != nil {
		t.Fatalf("add tenant: %v", err)
	}
	return id
}

// platformTenantStatus reads one tenant through the operator list (the per-id GET
// answers a uniform 403 for an unknown tenant, so it cannot show "row gone").
func platformTenantStatus(t *testing.T, f e2eFixture, token, tenantID string) (found bool, status string) {
	t.Helper()
	r := f.authedJSON(t, http.MethodGet, "/api/v1/platform/tenants", token, nil)
	if r.status != http.StatusOK {
		t.Fatalf("list tenants: %d %s", r.status, r.body)
	}
	var lr ports.ListAllTenantsResponse
	if err := json.Unmarshal(r.body, &lr); err != nil {
		t.Fatalf("decode tenants: %v", err)
	}
	for _, tn := range lr.Tenants {
		if tn.ID == tenantID {
			return true, tn.Status
		}
	}
	return false, ""
}
func TestE2E_HardDeleteTenant_WithDependentsIsConflictAndChangesNothing(t *testing.T) {
	f := newE2EFixture(t)
	p := permission.IdentityPermissions
	v := f.registerAndLogin(t, "hdep")
	opID := "0197c4a0-7000-7000-8000-00000000b001"
	op := mintOperatorJWTExact(t, f.Issuer, f.persons, opID, []string{p.Tenants.Delete, p.Tenants.Activate, p.Platform.TenantsView}, false)
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/tenants/"+v.TenantID+"/activate", op, nil); r.status != http.StatusNoContent {
		t.Fatalf("activate: %d %s", r.status, r.body)
	}
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/tenants/"+v.TenantID+"/mark-for-deletion", op, ports.MarkTenantForDeletionRequest{Reason: "hard delete test"}); r.status != http.StatusNoContent {
		t.Fatalf("mark-for-deletion: %d %s", r.status, r.body)
	}
	before := len(outboxActFor(t, f, "tenant_deleted"))
	foundBefore, statusBefore := platformTenantStatus(t, f, op, v.TenantID)
	if !foundBefore {
		t.Fatal("tenant missing before the delete")
	}

	r := f.authedJSON(t, http.MethodDelete, "/api/v1/platform/tenants/"+v.TenantID, op, nil)
	if r.status != http.StatusConflict || decodeErrorCode(t, r.body) != "tenant_has_dependents" {
		t.Errorf("hard delete of a tenant with members: %d %s, want 409 tenant_has_dependents", r.status, r.body)
	}
	found, statusAfter := platformTenantStatus(t, f, op, v.TenantID)
	if !found || statusAfter != statusBefore || statusAfter == "deleted" {
		t.Errorf("tenant after the refused delete: found=%v state %q, want present and unchanged %q", found, statusAfter, statusBefore)
	}
	if after := len(outboxActFor(t, f, "tenant_deleted")); after != before {
		t.Errorf("a failed hard delete left %d new tenant_deleted outbox row(s), want 0", after-before)
	}
}

func TestE2E_HardDeleteTenant_WithoutDependentsDeletesAndAttributes(t *testing.T) {
	f := newE2EFixture(t)
	p := permission.IdentityPermissions
	id := bareTenant(t, f, "hfree")
	opID := "0197c4a0-7000-7000-8000-00000000b002"
	op := mintOperatorJWTExact(t, f.Issuer, f.persons, opID, []string{p.Tenants.Delete, p.Platform.TenantsView}, false)

	before := len(outboxActFor(t, f, "tenant_deleted"))
	if r := f.authedJSON(t, http.MethodDelete, "/api/v1/platform/tenants/"+string(id), op, nil); r.status != http.StatusNoContent {
		t.Fatalf("hard delete: %d %s, want 204", r.status, r.body)
	}
	if found, status := platformTenantStatus(t, f, op, string(id)); found {
		t.Errorf("tenant after hard delete still listed (status %q), want the row gone", status)
	}
	rows := outboxActFor(t, f, "tenant_deleted")
	if len(rows) != before+1 {
		t.Fatalf("tenant_deleted outbox rows: %d, want exactly one new (had %d)", len(rows), before)
	}
	assertAttributed(t, rows[len(rows)-1:], opID, "hard delete")
}
