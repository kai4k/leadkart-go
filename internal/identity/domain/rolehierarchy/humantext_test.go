package rolehierarchy_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/humantext"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/rolehierarchy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

func TestNew_RefusesAReasonWithDirectionOverride(t *testing.T) {
	t.Parallel()
	_, err := rolehierarchy.New(
		rolehierarchy.ID(ids.NewV7().String()), tenant.ID(ids.NewV7().String()),
		role.ID(ids.NewV7().String()), role.ID(ids.NewV7().String()), membership.ID(""),
		"manager reorg "+string(rune(0x202e))+"requires this link", fixedClock())
	var he *humantext.Error
	if !errors.Is(err, rolehierarchy.ErrInvalidReason) || !errors.As(err, &he) || he.Field != "reason" {
		t.Fatalf("err = %v, want ErrInvalidReason + humantext.Error for reason", err)
	}
}
