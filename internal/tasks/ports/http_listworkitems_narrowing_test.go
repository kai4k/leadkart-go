package ports

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
	"github.com/leadkart/leadkart-go/internal/tasks/app"
	"github.com/leadkart/leadkart-go/internal/tasks/app/query"
	"github.com/leadkart/leadkart-go/internal/tasks/domain/workitem"
)

// recordingWorkItems implements only ListPage; the embedded nil Repository
// panics on any other call, so the test also proves nothing else is reached.
type recordingWorkItems struct {
	workitem.Repository
	mu    sync.Mutex
	calls int
	last  workitem.ListFilter
}

func (r *recordingWorkItems) ListPage(_ context.Context, _ tenant.ID, f workitem.ListFilter, _ pagination.Cursor, _ int) (pagination.Page[*workitem.WorkItem], error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.last = f
	return pagination.Page[*workitem.WorkItem]{}, nil
}

func operatorClaims(membershipID string, super bool, perms ...string) *jwt.Claims {
	return &jwt.Claims{
		MembershipID: membershipID,
		TenantID:     uuid.NewString(),
		TenantSlug:   authn.PlatformTenantSlug,
		IsPlatform:   true,
		IsSuperUser:  super,
		Permissions:  perms,
	}
}

// Platform-tenant membership is not scope authority: a zero-permission platform
// member is narrowed to their own work items (probing another assignee is 403),
// a super user and an operator holding platform.tenants.view are not.
func TestHandleListWorkItems_PlatformMemberNarrowing(t *testing.T) {
	t.Parallel()
	view := permission.IdentityPermissions.Platform.TenantsView
	other := uuid.NewString()
	tests := []struct {
		name         string
		claims       func(self string) *jwt.Claims
		wantSelf     bool
		probeOther   int
		probeOtherOK bool
	}{
		{"zero-permission platform member is narrowed", func(m string) *jwt.Claims { return operatorClaims(m, false) }, true, http.StatusForbidden, false},
		{"super user is not narrowed", func(m string) *jwt.Claims { return operatorClaims(m, true) }, false, http.StatusOK, true},
		{"operator with platform.tenants.view is not narrowed", func(m string) *jwt.Claims { return operatorClaims(m, false, view) }, false, http.StatusOK, true},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	run := func(t *testing.T, c *jwt.Claims, target string) (*httptest.ResponseRecorder, *recordingWorkItems) {
		t.Helper()
		repo := &recordingWorkItems{}
		a := app.Application{Queries: app.Queries{ListWorkItems: query.NewListWorkItemsHandler(repo)}}
		ctx := tenancy.WithID(authn.WithClaims(t.Context(), c), tenancy.ID(c.TenantID))
		rec := httptest.NewRecorder()
		handleListWorkItems(log, a).ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody))
		return rec, repo
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			self := uuid.NewString()
			rec, repo := run(t, tc.claims(self), "/api/v1/tasks/work-items")
			if rec.Code != http.StatusOK || repo.calls != 1 {
				t.Fatalf("plain list: %d %s (repo calls %d)", rec.Code, rec.Body.String(), repo.calls)
			}
			if gotSelf := repo.last.SelfFilter == self; gotSelf != tc.wantSelf {
				t.Fatalf("SelfFilter forwarded = %q, narrowed=%v, want narrowed=%v", repo.last.SelfFilter, gotSelf, tc.wantSelf)
			}
			rec, repo = run(t, tc.claims(self), "/api/v1/tasks/work-items?assignee="+other)
			if rec.Code != tc.probeOther || (repo.calls == 1) != tc.probeOtherOK {
				t.Fatalf("?assignee=other: %d %s (repo calls %d), want %d reached=%v", rec.Code, rec.Body.String(), repo.calls, tc.probeOther, tc.probeOtherOK)
			}
		})
	}
}
