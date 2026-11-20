//go:build integration

// arch-test:no-timeout-needed — newE2EFixtureAt → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).

// Hierarchy round 5, commit 2 (ADR 0069): the approver queue says what the
// caller can approve; an approval never outlives the approver's own grant;
// concurrent approvals for one requester all land.

package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func (f e2eFixture) approverQueue(t *testing.T, token string) ports.ListPermissionRequestsResponse {
	t.Helper()
	r := f.authedJSON(t, http.MethodGet, "/api/v1/permission-requests?role=approver&page_size=100", token, nil)
	var out ports.ListPermissionRequestsResponse
	if r.status != http.StatusOK || json.Unmarshal(r.body, &out) != nil {
		t.Fatalf("approver queue: %d %s", r.status, r.body)
	}
	return out
}

func TestE2E_ApproverQueue_CanApproveIsWhatApproveAnswers(t *testing.T) {
	w := newPRE2E(t)
	f := w.f
	p := permission.IdentityPermissions
	e, eTok := w.exec(t)
	// held by the Team Lead (approvable) and not held (listed, but not approvable)
	for _, perm := range []string{p.Tasks.WorkItems.Reassign, p.Roles.Update, p.Users.Create} {
		if sub, _ := f.requestElevation(t, eTok, perm); sub.status != http.StatusCreated {
			t.Fatalf("submit %s: %d %s", perm, sub.status, sub.body)
		}
	}
	q := f.approverQueue(t, w.tlTok)
	if len(q.Requests) != 3 {
		t.Fatalf("the Team Lead's queue holds %d requests, want 3", len(q.Requests))
	}
	var yes, no int
	for _, it := range q.Requests {
		if it.CanApprove == nil {
			t.Fatalf("queue item %s has no can_approve", it.Permission)
		}
		r := w.decide(t, "approve", w.tlTok, it.ID)
		if *it.CanApprove {
			yes++
			if r.status != http.StatusNoContent {
				t.Errorf("%s: can_approve true but approve answered %d %s", it.Permission, r.status, r.body)
			}
		} else {
			no++
			f.wantForbidden(t, it.Permission+": can_approve false", r, http.StatusForbidden, "grant_exceeds_authority")
		}
	}
	if yes != 1 || no != 2 {
		t.Errorf("approvable %d, not approvable %d; want 1 and 2", yes, no)
	}

	// The own-requests list carries no can_approve; the executive's queue is empty.
	// The approvals above ended the executive's old token (stamp evicted at commit).
	eTok = f.loginToken(t, e.email, e.password)
	mine := f.authedJSON(t, http.MethodGet, "/api/v1/permission-requests", eTok, nil)
	var own ports.ListPermissionRequestsResponse
	if err := json.Unmarshal(mine.body, &own); err != nil || len(own.Requests) == 0 || own.Requests[0].CanApprove != nil {
		t.Errorf("own requests: %d %s, want items without can_approve", mine.status, mine.body)
	}
	if got := f.approverQueue(t, eTok); len(got.Requests) != 0 {
		t.Errorf("an executive's approver queue holds %d requests, want 0", len(got.Requests))
	}
}

// TestE2E_PermissionRequests_ApprovalNeverOutlivesTheApproversGrant: the Team
// Lead holds crm.leads.assign only through a 3-day approved override; the grant
// they approve for a report expires no later.
func TestE2E_PermissionRequests_ApprovalNeverOutlivesTheApproversGrant(t *testing.T) {
	w := newPRE2E(t)
	f := w.f
	assign := permission.IdentityPermissions.Crm.Leads.Assign
	// the owner approves the Team Lead's own request for 3 days (day-to-day name: approved without holding it)
	sub, tlReq := f.requestElevationFor(t, w.tlTok, assign, 3)
	if sub.status != http.StatusCreated {
		t.Fatalf("Team Lead requests %s: %d %s", assign, sub.status, sub.body)
	}
	if r := w.decide(t, "approve", w.owner.AccessToken, tlReq); r.status != http.StatusNoContent {
		t.Fatalf("owner approves the Team Lead's request: %d %s", r.status, r.body)
	}
	f.evictStamp(t, w.tl.personID)
	w.tlTok = f.loginToken(t, w.tl.email, w.tl.password)

	_, eTok := w.exec(t)
	sub, reqID := f.requestElevationFor(t, eTok, assign, 90)
	if sub.status != http.StatusCreated {
		t.Fatalf("exec requests %s for 90 days: %d %s", assign, sub.status, sub.body)
	}
	if r := w.decide(t, "approve", w.tlTok, reqID); r.status != http.StatusNoContent {
		t.Fatalf("Team Lead approves: %d %s", r.status, r.body)
	}
	get := func(id string) ports.PermissionRequestDto {
		var dto ports.PermissionRequestDto
		r := f.authedJSON(t, http.MethodGet, "/api/v1/permission-requests/"+id, w.owner.AccessToken, nil)
		if err := json.Unmarshal(r.body, &dto); err != nil || r.status != http.StatusOK {
			t.Fatalf("get request %s: %d %s", id, r.status, r.body)
		}
		return dto
	}
	approver, report := get(tlReq), get(reqID)
	if report.ExpiresAt.IsZero() || approver.ExpiresAt.IsZero() {
		t.Fatalf("missing expiry: approver %v, report %v", approver.ExpiresAt, report.ExpiresAt)
	}
	if report.ExpiresAt.After(approver.ExpiresAt) {
		t.Errorf("the report's grant expires %v, after the approver's own %v", report.ExpiresAt, approver.ExpiresAt)
	}
	if report.ExpiresAt.After(time.Now().Add(4 * 24 * time.Hour)) {
		t.Errorf("the 90-day request was approved until %v", report.ExpiresAt)
	}
}

func (f e2eFixture) requestElevationFor(t *testing.T, token, perm string, days int) (httpResp, string) {
	t.Helper()
	r := f.authedJSON(t, http.MethodPost, "/api/v1/permission-requests", token,
		ports.CreatePermissionRequestRequest{Permission: perm, Reason: elevationReason, DurationDays: days})
	var out ports.CreatePermissionRequestResponse
	if r.status == http.StatusCreated {
		if err := json.Unmarshal(r.body, &out); err != nil {
			t.Fatalf("decode request: %v", err)
		}
	}
	return r, out.RequestID
}

// TestE2E_PermissionRequests_ConcurrentApprovalsForOneRequesterAllLand: two
// approvals for ONE requester with DIFFERENT permissions, at once, many times:
// both grants are present afterwards (each approval writes on the row as it is
// when it takes the lock, never on a stale copy).
func TestE2E_PermissionRequests_ConcurrentApprovalsForOneRequesterAllLand(t *testing.T) {
	w := newPRE2E(t)
	f := w.f
	p := permission.IdentityPermissions
	names := []string{p.Tasks.WorkItems.Reassign, p.Tasks.WorkItems.Manage}
	for round := range 10 {
		e, eTok := w.exec(t)
		ids := make([]string, len(names))
		for i, n := range names {
			sub, id := f.requestElevation(t, eTok, n)
			if sub.status != http.StatusCreated {
				t.Fatalf("round %d: submit %s: %d %s", round, n, sub.status, sub.body)
			}
			ids[i] = id
		}
		var wg sync.WaitGroup
		results := make([]httpResp, len(ids))
		start := make(chan struct{})
		for i := range ids {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				results[i] = w.decide(t, "approve", w.tlTok, ids[i])
			}()
		}
		close(start)
		wg.Wait()
		for i, r := range results {
			if r.status != http.StatusNoContent {
				t.Fatalf("round %d: approve %s: %d %s", round, names[i], r.status, r.body)
			}
		}
		f.evictStamp(t, e.personID)
		perms, _ := tokenClaims(t, f.loginToken(t, e.email, e.password))["permission"].([]any)
		for _, n := range names {
			if !slices.Contains(perms, any(n)) {
				t.Fatalf("round %d: the requester lost the grant of %s; token carries %v", round, n, perms)
			}
		}
	}
}
