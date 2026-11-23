//go:build integration

// arch-test:no-timeout-needed — newE2EFixture bounds container startup; per-request
// HTTP uses t.Context() (auto-canceled on test end).

// Operator attribution on platform routes (ADR 0068 "Attribution"): every
// destructive platform operation leaves an outbox row carrying the operator's
// person id and a reason, with or without the X-Tenant-Id header.

package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/messaging/messagingtest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

// outboxActFor returns the (act_operator_id, act_reason) of every outbox row
// whose event_type contains eventFragment.
func outboxActFor(t *testing.T, f e2eFixture, eventFragment string) [][2]string {
	t.Helper()
	var out [][2]string
	for _, row := range messagingtest.DrainOutbox(t.Context(), t, f.Pool) {
		if strings.Contains(row.Message.Metadata.Get(messaging.HeaderEventType), eventFragment) {
			out = append(out, [2]string{
				row.Message.Metadata.Get(messaging.HeaderActOperatorID),
				row.Message.Metadata.Get(messaging.HeaderActReason),
			})
		}
	}
	return out
}

func assertAttributed(t *testing.T, rows [][2]string, operatorID, what string) {
	t.Helper()
	if len(rows) == 0 {
		t.Fatalf("%s: no outbox row written", what)
	}
	for _, r := range rows {
		if r[0] != operatorID || r[1] == "" {
			t.Errorf("%s: act_operator_id=%q act_reason=%q, want operator %s and a reason", what, r[0], r[1], operatorID)
		}
	}
}

// A header-less suspend and a header-scoped deactivation carry the operator.
func TestE2E_PlatformRoutes_DestructiveWritesCarryOperatorAttribution(t *testing.T) {
	f := newE2EFixture(t)
	p := permission.IdentityPermissions

	// arch-test:serial — subtests share one fixture and read the whole outbox.
	t.Run("suspend without header", func(t *testing.T) {
		v := f.registerAndLogin(t, "attrsusp")
		opID := "0197c4a0-7000-7000-8000-00000000a001"
		op := mintOperatorJWTExact(t, f.Issuer, f.persons, opID, []string{p.Tenants.Suspend}, false)
		if r := f.authedJSON(t, http.MethodPost, "/api/v1/tenants/"+v.TenantID+"/suspend", op, ports.SuspendTenantRequest{Reason: "attribution test"}); r.status != http.StatusNoContent {
			t.Fatalf("suspend: %d %s", r.status, r.body)
		}
		assertAttributed(t, outboxActFor(t, f, "tenant_suspended"), opID, "suspend")
	})

	// arch-test:serial — shares the fixture and the outbox with its siblings.
	t.Run("an existing header-scope claim is kept", func(t *testing.T) {
		v := f.registerAndLogin(t, "attrkeep")
		m := f.createMember(t, v, "attrkeepm") // a plain member: deactivating an owner needs platform.tenants.ownership
		opID := "0197c4a0-7000-7000-8000-00000000a004"
		op := mintOperatorJWTExact(t, f.Issuer, f.persons, opID, []string{p.Platform.UsersManage, p.Platform.TenantsView}, false)
		if r := f.doWithHeaders(t, http.MethodPost, "/api/v1/users/"+m.membershipID+"/deactivate", op, ports.DeactivateUserRequest{Reason: "attribution test"}, scopeHdr(v.TenantID)); r.status != http.StatusNoContent {
			t.Fatalf("deactivate via header: %d %s", r.status, r.body)
		}
		rows := outboxActFor(t, f, "membership_deactivated")
		found := false
		for _, r := range rows {
			if r[0] == opID {
				found = true
				if r[1] != "tenant-via-header" {
					t.Errorf("act_reason = %q, want the header-scope reason kept", r[1])
				}
			}
		}
		if !found {
			t.Fatal("no attributed deactivate row")
		}
	})
}
