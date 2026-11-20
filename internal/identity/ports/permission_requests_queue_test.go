package ports_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership/membershiptest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest/permissionrequesttest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role/roletest"
)

// A caller whose claims build no valid actor (here: a malformed membership id)
// reaches the approver queue as the zero actor; the command refuses it and the
// route answers 403 forbidden, not a 500.
func TestApproverQueue_ZeroActorAnswers403(t *testing.T) {
	t.Parallel()
	var a app.Application
	a.Commands.ListDecidablePermissionRequests = command.NewListDecidablePermissionRequestsHandler(inlineUoW{},
		permissionrequesttest.NewFakeRepository(), membershiptest.NewFakeRepository(), roletest.NewFakeRepository(), bodyPersons{}, func() time.Time { return bodyNow })
	c := azClaims()
	c.MembershipID = "not-a-uuid"
	rec := azServeApp(t, a, c, "GET", "/api/v1/permission-requests?role=approver", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d %s, want 403", rec.Code, rec.Body.String())
	}
}
