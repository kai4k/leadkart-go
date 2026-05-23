// http_listleads_h8_test.go — focused white-box test for reviewer H8:
// when SelfFilter is active (caller LACKS crm.leads.read_all), a
// caller-supplied ?assignee=<other> MUST 403. Same-membership /
// absent assignee MUST flow through to the repository (200).

package ports

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/crm/app"
	cmdquery "github.com/leadkart/leadkart-go/internal/crm/app/query"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// recordingLeadsRepo records every ListPage call so the test can assert
// whether the handler short-circuited (recorded.calls == 0 on 403) or
// dispatched (recorded.calls == 1 on 200).
type recordingLeadsRepo struct {
	mu    sync.Mutex
	calls int
	last  crmlead.ListFilter
}

func (r *recordingLeadsRepo) Add(context.Context, *crmlead.CrmLead) error  { panic("unused") }
func (r *recordingLeadsRepo) GetByID(context.Context, crmlead.ID) (*crmlead.CrmLead, error) {
	panic("unused")
}
func (r *recordingLeadsRepo) GetBySourcePurchaseID(context.Context, string) (*crmlead.CrmLead, error) {
	panic("unused")
}
func (r *recordingLeadsRepo) UpdateByID(context.Context, crmlead.ID, func(*crmlead.CrmLead) (bool, error)) error {
	panic("unused")
}
func (r *recordingLeadsRepo) ListPage(_ context.Context, f crmlead.ListFilter, _ pagination.Cursor, _ int) (pagination.Page[*crmlead.CrmLead], error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.last = f
	return pagination.Page[*crmlead.CrmLead]{}, nil
}

func silentLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newH8App(repo *recordingLeadsRepo) app.Application {
	return app.Application{
		Commands: app.Commands{},
		Queries: app.Queries{
			ListLeads: cmdquery.NewListLeadsHandler(repo),
			GetLead:   cmdquery.NewGetLeadHandler(repo),
		},
	}
}

// h8Claims builds a minimally-populated *jwt.Claims for the
// authenticated context used by the ListLeads handler. The handler
// only reads IsPlatform / IsSuperUser / Permissions / MembershipID.
func h8Claims(membershipID string, hasReadAll bool) *jwt.Claims {
	perms := []string{}
	if hasReadAll {
		perms = append(perms, permission.IdentityPermissions.Crm.Leads.ReadAll)
	}
	return &jwt.Claims{
		MembershipID: membershipID,
		TenantID:     uuid.NewString(),
		TenantSlug:   "acme",
		Permissions:  perms,
	}
}

// TestHandleListLeads_H8_SelfFilterWithOtherAssignee_403 pins the H8
// fix: a caller lacking read_all who passes ?assignee=<other> MUST
// 403, NOT silently get an empty page from the SQL AND-intersection.
func TestHandleListLeads_H8_SelfFilterWithOtherAssignee_403(t *testing.T) {
	t.Parallel()
	repo := &recordingLeadsRepo{}
	a := newH8App(repo)
	h := handleListLeads(silentLog(), a)

	selfMembership := uuid.NewString()
	otherMembership := uuid.NewString()

	req := httptest.NewRequestWithContext(
		authn.WithClaims(t.Context(), h8Claims(selfMembership, false /*hasReadAll*/)),
		http.MethodGet, "/api/v1/crm/leads?assignee="+otherMembership, nil,
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if repo.calls != 0 {
		t.Fatalf("repository invoked despite 403 short-circuit: calls=%d", repo.calls)
	}
	var er errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &er); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if er.Error != errCodeForbidden {
		t.Fatalf("error code: got %q, want %q", er.Error, errCodeForbidden)
	}
}

// TestHandleListLeads_H8_SelfFilterWithSelfAssignee_200 covers the
// harmless case: ?assignee=<self> when SelfFilter active. Same value
// in both predicates; SQL collapses. Handler MUST dispatch.
func TestHandleListLeads_H8_SelfFilterWithSelfAssignee_200(t *testing.T) {
	t.Parallel()
	repo := &recordingLeadsRepo{}
	a := newH8App(repo)
	h := handleListLeads(silentLog(), a)

	selfMembership := uuid.NewString()

	req := httptest.NewRequestWithContext(
		authn.WithClaims(t.Context(), h8Claims(selfMembership, false /*hasReadAll*/)),
		http.MethodGet, "/api/v1/crm/leads?assignee="+selfMembership, nil,
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if repo.calls != 1 {
		t.Fatalf("repository call count: got %d, want 1", repo.calls)
	}
}

// TestHandleListLeads_H8_ReadAllBypassesGate covers the platform-tier
// case: caller HAS crm.leads.read_all, ?assignee=<other> is legitimate
// (e.g. manager filters a teammate's caseload). MUST dispatch (200).
func TestHandleListLeads_H8_ReadAllBypassesGate(t *testing.T) {
	t.Parallel()
	repo := &recordingLeadsRepo{}
	a := newH8App(repo)
	h := handleListLeads(silentLog(), a)

	selfMembership := uuid.NewString()
	otherMembership := uuid.NewString()

	req := httptest.NewRequestWithContext(
		authn.WithClaims(t.Context(), h8Claims(selfMembership, true /*hasReadAll*/)),
		http.MethodGet, "/api/v1/crm/leads?assignee="+otherMembership, nil,
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if repo.calls != 1 {
		t.Fatalf("repository call count: got %d, want 1", repo.calls)
	}
	if repo.last.AssigneeMembershipID != otherMembership {
		t.Fatalf("assignee filter forwarded: got %q, want %q",
			repo.last.AssigneeMembershipID, otherMembership)
	}
}
