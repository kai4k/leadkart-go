//go:build integration

// arch-test:no-timeout-needed — newE2EFixture bounds container startup; per-request
// HTTP uses t.Context() (auto-canceled on test end).

// X-Tenant-Id scope policy, second round (ADR 0068 D7): scoped writes are
// read-only by default, the notifications inbox ignores the header, the path
// wins on /tenants/{id} routes (also against a suspended tenant), and path-only
// operator writes are attributed like header-scoped ones.

package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/messaging/messagingtest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
	invports "github.com/leadkart/leadkart-go/internal/inventory/ports"
	notificationscommand "github.com/leadkart/leadkart-go/internal/notifications/app/command"
	"github.com/leadkart/leadkart-go/internal/notifications/domain/notification"
	nports "github.com/leadkart/leadkart-go/internal/notifications/ports"
	ordersports "github.com/leadkart/leadkart-go/internal/orders/ports"
)

// A foreign-tenant header turns every non-identity write into 403
// tenant_scope_read_only, for a super user and for a non-super operator, so
// the operator's platform-tenant membership id can never be stamped into
// another tenant's rows. Identity writes the operator scope tree uses stay
// writable.
func TestE2E_TenantScope_ForeignWritesReadOnlyByDefault(t *testing.T) {
	f := newE2EFixture(t)
	a := f.registerAndLogin(t, "rowrite")
	p := permission.IdentityPermissions
	super := f.mintPlatformTokenWith(t, nil, true)
	nonSuper := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.TenantsView, p.Platform.UsersView, p.Platform.UsersManage}, false)

	for name, tok := range map[string]string{"super user": super, "non-super operator": nonSuper} {
		q := f.doWithHeaders(t, http.MethodPost, "/api/v1/orders/quotations", tok, ordersports.CreateQuotationRequest{
			CustomerLeadID: uuid.NewString(), Items: ordersSampleItems(), Note: "must not be written",
		}, scopeHdr(a.TenantID))
		if q.status != http.StatusForbidden || decodeErrorCode(t, q.body) != "tenant_scope_read_only" {
			t.Errorf("%s: orders write under scope: %d %s, want 403 tenant_scope_read_only", name, q.status, q.body)
		}
		pr := f.doWithHeaders(t, http.MethodPost, "/api/v1/inventory/products", tok, invports.CreateProductRequest{
			SKU: "RO-001", Name: "Read Only", DosageForm: "tablet", PackSize: "10x10", HSNCode: "30049099", GSTRateBps: 1200,
		}, scopeHdr(a.TenantID))
		if pr.status != http.StatusForbidden || decodeErrorCode(t, pr.body) != "tenant_scope_read_only" {
			t.Errorf("%s: inventory write under scope: %d %s, want 403 tenant_scope_read_only", name, pr.status, pr.body)
		}
	}
	lr := f.authedJSON(t, http.MethodGet, "/api/v1/inventory/products", a.AccessToken, nil)
	var products invports.ListProductsResponse
	if err := json.Unmarshal(lr.body, &products); err != nil || lr.status != http.StatusOK || len(products.Items) != 0 {
		t.Errorf("tenant A must hold no products: %d %s", lr.status, lr.body)
	}
	role := f.doWithHeaders(t, http.MethodPost, "/api/v1/roles", super, ports.CreateRoleRequest{Name: "Writable Under Scope", HierarchyLevel: 50}, scopeHdr(a.TenantID))
	if role.status != http.StatusCreated {
		t.Errorf("super user role create under scope: %d %s", role.status, role.body)
	}
}

// The notifications inbox is the caller's own: the header is ignored there, so
// an operator reads its own inbox instead of a 403 evaluated in the scoped tenant.
func TestE2E_TenantScope_NotificationsIgnoreHeader(t *testing.T) {
	f := newE2EFixture(t)
	b := f.registerAndLogin(t, "notifscope")
	p := permission.IdentityPermissions
	op := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.TenantsView, p.Notifications.Inbox.Read}, false)

	// Tenant B's admin has one notification. Control: B's admin sees it. The
	// operator, with any header (including B's own id), sees only its own inbox.
	f.seedNotification(t, b.TenantID, b.MembershipID, "Tenant B admin only")
	own := f.authedJSON(t, http.MethodGet, "/api/v1/notifications", b.AccessToken, nil)
	var ownList nports.ListNotificationsResponse
	if err := json.Unmarshal(own.body, &ownList); err != nil || own.status != http.StatusOK || len(ownList.Items) != 1 {
		t.Fatalf("control: tenant B admin inbox %d %s, want exactly the seeded notification", own.status, own.body)
	}
	for _, hdr := range []string{b.TenantID, unknownTenantID, "not-a-uuid"} {
		r := f.doWithHeaders(t, http.MethodGet, "/api/v1/notifications", op, nil, scopeHdr(hdr))
		if r.status != http.StatusOK {
			t.Errorf("notifications with header %q: %d %s, want 200 (header ignored)", hdr, r.status, r.body)
			continue
		}
		var list nports.ListNotificationsResponse
		if err := json.Unmarshal(r.body, &list); err != nil {
			t.Fatalf("decode inbox: %v", err)
		}
		if len(list.Items) != 0 {
			t.Errorf("notifications with header %q: operator sees %d item(s) from the scoped tenant, want its own (empty) inbox: %s", hdr, len(list.Items), r.body)
		}
	}
}

// seedNotification creates one unread notification for a membership through the
// notifications command, the same path the subscribers use.
func (f e2eFixture) seedNotification(t *testing.T, tenantID, membershipID, title string) {
	t.Helper()
	notif := buildNotificationsApp(f.Pool, time.Now)
	if _, err := notif.Commands.CreateNotification.Handle(t.Context(), notificationscommand.CreateNotificationCommand{
		TenantID:              tenant.ID(tenantID),
		RecipientMembershipID: membership.ID(membershipID),
		Category:              notification.CategoryReminder,
		Title:                 title,
	}); err != nil {
		t.Fatalf("seed notification: %v", err)
	}
}

// PATCH /tenants/{id}/... on a suspended tenant: the path is authoritative, so
// the write behaves the same with and without a matching header.
func TestE2E_TenantScope_PathWriteOnSuspendedTenantIgnoresHeaderState(t *testing.T) {
	f := newE2EFixture(t)
	b := f.registerAndLogin(t, "pathsusp")
	p := permission.IdentityPermissions
	op := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.TenantsView, p.Platform.TenantsManage, p.Tenants.Suspend}, false)
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/tenants/"+b.TenantID+"/suspend", op, ports.SuspendTenantRequest{Reason: "path write test"}); r.status >= 300 {
		t.Fatalf("suspend: %d %s", r.status, r.body)
	}
	body := ports.UpdateTenantProfileRequest{LegalName: "Renamed Pharma Pvt Ltd", DisplayName: "Renamed"}
	without := f.doWithHeaders(t, http.MethodPatch, "/api/v1/tenants/"+b.TenantID+"/profile", op, body, nil)
	with := f.doWithHeaders(t, http.MethodPatch, "/api/v1/tenants/"+b.TenantID+"/profile", op, body, scopeHdr(b.TenantID))
	if without.status < 200 || without.status > 299 {
		t.Errorf("suspended tenant path write without header: %d %s, want 2xx", without.status, without.body)
	}
	if with.status < 200 || with.status > 299 {
		t.Errorf("suspended tenant path write with matching header: %d %s, want 2xx (not tenant_not_active)", with.status, with.body)
	}
}

// An anchored operator writing to tenant B through the PATH alone (no header)
// leaves the same durable attribution as a header-scoped write.
func TestE2E_TenantScope_PathOnlyWriteCarriesOperatorAttribution(t *testing.T) {
	f := newE2EFixture(t)
	b := f.registerAndLogin(t, "pathattr")
	operatorID := ids.NewV7().String()
	p := permission.IdentityPermissions
	op := mintOperatorJWTExact(t, f.Issuer, f.persons, operatorID, []string{p.Platform.TenantsView, p.Platform.TenantsManage}, false)

	r := f.authedJSON(t, http.MethodPatch, "/api/v1/tenants/"+b.TenantID+"/profile", op, ports.UpdateTenantProfileRequest{LegalName: "Path Written Pvt Ltd", DisplayName: "Path Written"})
	if r.status >= 300 {
		t.Fatalf("path-only profile update: %d %s", r.status, r.body)
	}
	found := false
	for _, row := range messagingtest.DrainOutbox(t.Context(), t, f.Pool) {
		if row.Message.Metadata.Get(messaging.HeaderTenantID) != b.TenantID || row.Message.Metadata.Get(messaging.HeaderActOperatorID) == "" {
			continue
		}
		found = true
		if got := row.Message.Metadata.Get(messaging.HeaderActOperatorID); got != operatorID {
			t.Errorf("act_operator_id = %s, want %s", got, operatorID)
		}
		if got := row.Message.Metadata.Get(messaging.HeaderActReason); got == "" {
			t.Error("act_reason must be set")
		}
	}
	if !found {
		t.Fatal("a path-only operator write into another tenant must leave an attributed outbox row")
	}
}

// A non-super operator sending an unknown tenant on a MUTATING ignored route
// (tenant registration) is neither blocked nor validated by the header.
func TestE2E_TenantScope_IgnoredRouteWriteWithNonSuperOperator(t *testing.T) {
	f := newE2EFixture(t)
	p := permission.IdentityPermissions
	op := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.TenantsCreate}, false)
	r := f.doWithHeaders(t, http.MethodPost, "/api/v1/tenants", op, ports.RegisterTenantRequest{
		Slug: "ign-write-ok", LegalName: "Ignored Header Pvt Ltd", DisplayName: "Ignored Header",
		AdminEmail: "admin@ign-write.test", AdminPassword: "Correct-Horse-Battery-9!", AdminFirstName: "Ign", AdminLastName: "Header",
	}, scopeHdr(unknownTenantID))
	if r.status != http.StatusCreated {
		t.Errorf("non-super operator tenant registration with an unknown header: %d %s, want 201", r.status, r.body)
	}
}

// Permission requests reject the header on submit, list and cancel as well.
func TestE2E_TenantScope_PermissionRequestSubmitListCancelRejectScope(t *testing.T) {
	f := newE2EFixture(t)
	b := f.registerAndLogin(t, "prscope")
	memberToken, reqID, _ := f.pendingRequestIn(t, b, "prmember")
	p := permission.IdentityPermissions
	op := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.TenantsView}, false)
	for _, rt := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/api/v1/permission-requests", ports.CreatePermissionRequestRequest{Permission: p.Meta.TenantAdmin, Reason: "needs admin access for the quarter"}},
		{http.MethodGet, "/api/v1/permission-requests", nil},
		{http.MethodPost, "/api/v1/permission-requests/" + reqID + "/cancel", nil},
	} {
		r := f.doWithHeaders(t, rt.method, rt.path, op, rt.body, scopeHdr(b.TenantID))
		if r.status != http.StatusForbidden || decodeErrorCode(t, r.body) != "tenant_scope_not_supported" {
			t.Errorf("%s %s under scope: %d %s, want 403 tenant_scope_not_supported", rt.method, rt.path, r.status, r.body)
		}
	}
	if r := f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests/"+reqID+"/cancel", memberToken, nil); r.status >= 300 {
		t.Errorf("requester cancel after refused operator attempts: %d %s", r.status, r.body)
	}
}
