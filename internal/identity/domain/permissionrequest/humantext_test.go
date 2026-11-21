package permissionrequest_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/humantext"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

var rlo = string(rune(0x202e))

func requireInvalid(t *testing.T, err error, field string) {
	t.Helper()
	var he *humantext.Error
	if !errors.Is(err, permissionrequest.ErrInvalidRequest) || !errors.As(err, &he) || he.Field != field {
		t.Fatalf("err = %v, want ErrInvalidRequest + humantext.Error for %q", err, field)
	}
}

func TestNew_RefusesAReasonWithDirectionOverride(t *testing.T) {
	t.Parallel()
	_, err := permissionrequest.New(
		permissionrequest.ID(ids.NewV7().String()), tenant.ID(ids.NewV7().String()), membership.ID(ids.NewV7().String()),
		permission.FromConstant(permission.IdentityPermissions.Users.Create), 7,
		"need to onboard "+rlo+"5 users for the sales drive", fixedClock())
	requireInvalid(t, err, "reason")
}

func TestApproveAndDeny_RefuseADecisionReasonWithControlCharacters(t *testing.T) {
	t.Parallel()
	approver := membership.ID(ids.NewV7().String())
	requireInvalid(t, validNew(t).Approve(approver, "ok"+rlo+"fine", uuid.New(), fixedClock(), fixedClock()), "decision_reason")
	requireInvalid(t, validNew(t).Deny(approver, "scope\ntoo broad", fixedClock()), "decision_reason")
}
