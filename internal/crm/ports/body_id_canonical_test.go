package ports

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Ids in a request body or query reach the repository/command in canonical
// form, or the request is refused: a padded id, a wrapped id and the nil UUID
// are 400, never trimmed and never forwarded raw.
func TestListLeads_AssigneeFilterIsCanonical(t *testing.T) {
	t.Parallel()
	canon := uuid.NewString()
	for name, spelled := range map[string]string{
		"canonical": canon, "uppercase": strings.ToUpper(canon), "urn": "urn:uuid:" + canon,
		"braces": "{" + canon + "}", "hyphenless": strings.ReplaceAll(canon, "-", ""),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo := &recordingLeadsRepo{}
			h := handleListLeads(silentLog(), newH8App(repo))
			req := httptest.NewRequestWithContext(h8Ctx(t.Context(), h8Claims(uuid.NewString(), true)),
				http.MethodGet, "/api/v1/crm/leads?assignee="+spelled, http.NoBody)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || repo.last.AssigneeMembershipID != canon {
				t.Fatalf("status %d, assignee reaching the repository = %q, want 200 and %q", rec.Code, repo.last.AssigneeMembershipID, canon)
			}
		})
	}
}

func TestCrmIDs_PaddedWrappedAndNilAreRejected(t *testing.T) {
	t.Parallel()
	id := uuid.NewString()
	ctx := func(t *testing.T) *http.Request {
		t.Helper()
		return httptest.NewRequestWithContext(h8Ctx(t.Context(), h8Claims(uuid.NewString(), true)), http.MethodGet, "/", http.NoBody)
	}
	for name, bad := range map[string]string{
		"leading space": "%20" + id, "trailing space": id + "%20", "wrapped": "%20" + id + "%20",
		"nil uuid": "00000000-0000-0000-0000-000000000000",
	} {
		repo := &recordingLeadsRepo{}
		rec := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(h8Ctx(t.Context(), h8Claims(uuid.NewString(), true)), http.MethodGet, "/api/v1/crm/leads?assignee="+bad, http.NoBody)
		handleListLeads(silentLog(), newH8App(repo)).ServeHTTP(rec, r)
		if rec.Code != http.StatusBadRequest || repo.calls != 0 {
			t.Errorf("leads ?assignee %s: %d (repo calls %d), want 400 and no repository call", name, rec.Code, repo.calls)
		}
		for _, q := range []string{"assignee", "lead_id"} {
			rec = httptest.NewRecorder()
			r = httptest.NewRequestWithContext(h8Ctx(t.Context(), h8Claims(uuid.NewString(), true)), http.MethodGet, "/api/v1/crm/reminders?"+q+"="+bad, http.NoBody)
			handleListReminders(silentLog(), newH8App(repo)).ServeHTTP(rec, r)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("reminders ?%s %s: %d %s, want 400", q, name, rec.Code, rec.Body.String())
			}
		}
		// Bodies: an empty Application proves the request is refused before any command runs.
		decoded := strings.NewReplacer("%20", " ").Replace(bad)
		for label, h := range map[string]struct {
			handler http.Handler
			body    string
			param   string
		}{
			"assign lead":     {handleAssignLead(silentLog(), newH8App(repo)), `{"assignee_membership_id":"` + decoded + `"}`, "leadId"},
			"create reminder": {handleCreateReminder(silentLog(), newH8App(repo)), `{"assigned_to_membership_id":"` + decoded + `","due_at":"2030-01-01T00:00:00Z"}`, "leadId"},
		} {
			r = ctx(t)
			r.Body = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(h.body)).Body
			r.SetPathValue(h.param, id)
			rec = httptest.NewRecorder()
			h.handler.ServeHTTP(rec, r)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s body %s: %d %s, want 400", label, name, rec.Code, rec.Body.String())
			}
		}
	}
}
