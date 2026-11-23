package permissionrequest_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

func newWithReason(reason string) (*permissionrequest.Request, error) {
	return permissionrequest.New(
		permissionrequest.ID(ids.NewV7().String()), tenant.ID(ids.NewV7().String()), membership.ID(ids.NewV7().String()),
		permission.FromConstant(permission.IdentityPermissions.Users.Create), 7, reason, fixedClock())
}

// The column checks length(reason) between 10 and 1024 characters. A 10-character
// Hindi reason is 30 bytes; a 7-character one is 21 bytes and must be a domain
// refusal, not a CHECK violation (a 500).
func TestReason_IsCountedInCharacters(t *testing.T) {
	t.Parallel()
	if _, err := newWithReason(strings.Repeat("अ", permissionrequest.MinReasonLength)); err != nil {
		t.Errorf("a %d-character reason refused: %v", permissionrequest.MinReasonLength, err)
	}
	if _, err := newWithReason(strings.Repeat("अ", permissionrequest.MinReasonLength-1)); !errors.Is(err, permissionrequest.ErrInvalidRequest) {
		t.Errorf("a %d-character reason: err = %v, want ErrInvalidRequest", permissionrequest.MinReasonLength-1, err)
	}
	if _, err := newWithReason(strings.Repeat("अ", permissionrequest.MaxReasonLength)); err != nil {
		t.Errorf("a %d-character reason refused: %v", permissionrequest.MaxReasonLength, err)
	}
	if _, err := newWithReason(strings.Repeat("अ", permissionrequest.MaxReasonLength+1)); !errors.Is(err, permissionrequest.ErrInvalidRequest) {
		t.Errorf("a %d-character reason: err = %v, want ErrInvalidRequest", permissionrequest.MaxReasonLength+1, err)
	}
}

func TestDecisionReason_IsCountedInCharacters(t *testing.T) {
	t.Parallel()
	ok, over := strings.Repeat("अ", permissionrequest.MaxDecisionReasonLength), strings.Repeat("अ", permissionrequest.MaxDecisionReasonLength+1)
	approver := membership.ID(ids.NewV7().String())
	r := validNew(t)
	if err := r.Approve(approver, over, uuid.New(), fixedClock(), fixedClock()); !errors.Is(err, permissionrequest.ErrInvalidRequest) {
		t.Errorf("approve with an over-long decision reason: err = %v", err)
	}
	if err := r.Approve(approver, ok, uuid.New(), fixedClock(), fixedClock()); err != nil {
		t.Errorf("approve with a %d-character decision reason: %v", permissionrequest.MaxDecisionReasonLength, err)
	}
	d := validNew(t)
	if err := d.Deny(approver, over, fixedClock()); !errors.Is(err, permissionrequest.ErrInvalidRequest) {
		t.Errorf("deny with an over-long decision reason: err = %v", err)
	}
	d = validNew(t)
	if err := d.Deny(approver, ok, fixedClock()); err != nil {
		t.Errorf("deny with a %d-character decision reason: %v", permissionrequest.MaxDecisionReasonLength, err)
	}
}
