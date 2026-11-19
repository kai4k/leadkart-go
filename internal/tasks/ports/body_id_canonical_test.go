package ports

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
	"github.com/leadkart/leadkart-go/internal/tasks/app"
	"github.com/leadkart/leadkart-go/internal/tasks/app/command"
	"github.com/leadkart/leadkart-go/internal/tasks/app/query"
	"github.com/leadkart/leadkart-go/internal/tasks/domain/workitem"
	"github.com/leadkart/leadkart-go/internal/tasks/domain/workitem/workitemtest"
)

func canonicalTestCtx(t *testing.T, c *jwt.Claims) *http.Request {
	t.Helper()
	ctx := tenancy.WithID(authn.WithClaims(t.Context(), c), tenancy.ID(c.TenantID))
	return httptest.NewRequestWithContext(ctx, http.MethodGet, "/", http.NoBody)
}

// ?assignee and ?batch_id reach the repository in canonical form, whatever the
// spelling; a padded, wrapped or nil id is a 400 and the repository is never called.
func TestListWorkItems_IDFiltersAreCanonical(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	canon := uuid.NewString()
	run := func(t *testing.T, target string) (*httptest.ResponseRecorder, *recordingWorkItems) {
		t.Helper()
		repo := &recordingWorkItems{}
		a := app.Application{Queries: app.Queries{ListWorkItems: query.NewListWorkItemsHandler(repo)}}
		rec := httptest.NewRecorder()
		r := canonicalTestCtx(t, operatorClaims(uuid.NewString(), true))
		r.URL.RawQuery = strings.SplitN(target, "?", 2)[1]
		handleListWorkItems(log, a).ServeHTTP(rec, r)
		return rec, repo
	}
	for name, spelled := range map[string]string{"uppercase": strings.ToUpper(canon), "urn": "urn:uuid:" + canon, "braces": "{" + canon + "}", "hyphenless": strings.ReplaceAll(canon, "-", "")} {
		rec, repo := run(t, "/x?assignee="+spelled+"&batch_id="+spelled)
		if rec.Code != http.StatusOK || repo.last.AssignedToMembershipID != canon || repo.last.BatchID != canon {
			t.Errorf("%s: %d assignee=%q batch=%q, want 200 and %q for both", name, rec.Code, repo.last.AssignedToMembershipID, repo.last.BatchID, canon)
		}
	}
	for name, bad := range map[string]string{"leading": "%20" + canon, "trailing": canon + "%20", "wrapped": "%20" + canon + "%20", "nil": "00000000-0000-0000-0000-000000000000"} {
		for _, q := range []string{"assignee", "batch_id"} {
			rec, repo := run(t, "/x?"+q+"="+bad)
			if rec.Code != http.StatusBadRequest || repo.calls != 0 {
				t.Errorf("?%s %s: %d (repo calls %d), want 400 and no repository call", q, name, rec.Code, repo.calls)
			}
		}
	}
}

// Body ids that are not exactly one non-nil UUID are refused before any command
// runs (the Application is empty: reaching a command would panic).
func TestTaskBodyIDs_PaddedWrappedAndNilAreRejected(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	id := uuid.NewString()
	for name, bad := range map[string]string{"leading": " " + id, "trailing": id + " ", "wrapped": " " + id + " ", "nil": "00000000-0000-0000-0000-000000000000"} {
		create := `{"type":"follow_up","assigned_to_membership_id":"` + bad + `","title":"t","due_at":"2030-01-01T00:00:00Z"}`
		createBatch := `{"type":"follow_up","assigned_to_membership_id":"` + id + `","batch_id":"` + bad + `","title":"t","due_at":"2030-01-01T00:00:00Z"}`
		reassign := `{"new_assignee_membership_id":"` + bad + `"}`
		for label, tc := range map[string]struct {
			h    http.Handler
			body string
		}{
			"create (assignee)": {handleCreateWorkItem(log, app.Application{}), create},
			"create (batch)":    {handleCreateWorkItem(log, app.Application{}), createBatch},
			"reassign":          {handleReassignWorkItem(log, app.Application{}), reassign},
		} {
			r := canonicalTestCtx(t, operatorClaims(uuid.NewString(), true))
			r.Body = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(tc.body)).Body
			r.SetPathValue("workItemId", id)
			rec := httptest.NewRecorder()
			tc.h.ServeHTTP(rec, r)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s with %s id: %d %s, want 400", label, name, rec.Code, rec.Body.String())
			}
		}
	}
}

// ----- canonical value reaches the command ----------------------------------

type recordingMemberships struct {
	mu   sync.Mutex
	seen []string
}

func (m *recordingMemberships) ExistsActiveInTenant(_ context.Context, _ tenant.ID, membershipID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen = append(m.seen, membershipID)
	return true, nil
}

// everyoneVisible makes every membership a subordinate, so the hierarchy gate
// passes for whichever canonical id the command was handed.
type everyoneVisible struct{ ids []string }

func (h everyoneVisible) ListSubordinateMembershipIDs(context.Context, tenant.ID, string) ([]string, error) {
	return h.ids, nil
}

func TestTaskBodyIDs_CanonicalValueReachesTheCommand(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	assignee, batch, newAssignee := uuid.NewString(), uuid.NewString(), uuid.NewString()
	claims := operatorClaims(uuid.NewString(), true)
	repo := workitemtest.NewFakeRepository()
	members := &recordingMemberships{}
	now := func() time.Time { return time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC) }
	a := app.Application{Commands: app.Commands{
		CreateWorkItem:   command.NewCreateWorkItemHandler(repo, members, now, func() workitem.ID { return workitem.ID(uuid.NewString()) }),
		ReassignWorkItem: command.NewReassignWorkItemHandler(repo, everyoneVisible{ids: []string{newAssignee}}, members, now),
	}}
	post := func(h http.Handler, body, pathID string) *httptest.ResponseRecorder {
		r := canonicalTestCtx(t, claims)
		r.Method = http.MethodPost
		r.Body = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(body)).Body
		if pathID != "" {
			r.SetPathValue("workItemId", pathID)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	create := `{"type":"follow_up","assigned_to_membership_id":"` + strings.ToUpper(assignee) + `","batch_id":"` + strings.ToUpper(batch) + `","title":"t","due_at":"2030-01-01T00:00:00Z"}`
	rec := post(handleCreateWorkItem(log, a), create, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created *workitem.WorkItem
	for _, w := range repo.ByID {
		created = w
	}
	if created.AssignedToMembershipID() != assignee || created.BatchID() != batch {
		t.Fatalf("create stored assignee=%q batch=%q, want canonical %q / %q", created.AssignedToMembershipID(), created.BatchID(), assignee, batch)
	}
	if len(members.seen) != 1 || members.seen[0] != assignee {
		t.Fatalf("membership probe saw %v, want [%s]", members.seen, assignee)
	}

	rec = post(handleReassignWorkItem(log, a), `{"new_assignee_membership_id":"`+strings.ToUpper(newAssignee)+`","reason":"cover"}`, created.ID().String())
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reassign: %d %s", rec.Code, rec.Body.String())
	}
	if got := repo.ByID[created.ID()].AssignedToMembershipID(); got != newAssignee {
		t.Fatalf("reassign stored assignee %q, want canonical %q", got, newAssignee)
	}
}
