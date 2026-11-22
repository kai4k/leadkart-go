package query

import (
	"context"
	"errors"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// PasswordRulesView is the policy as a member sees it: the rules a new password
// must satisfy, under the names of the 422 rule codes. MaxLength is a size bound in
// bytes; MinLength counts characters.
type PasswordRulesView struct {
	MinLength         int
	MaxLength         int
	RequiresUppercase bool
	RequiresLowercase bool
	RequiresDigit     bool
	RequiresSymbol    bool
}

func rulesOf(p tenant.PasswordPolicy) PasswordRulesView {
	return PasswordRulesView{
		MinLength:         p.MinLength(),
		MaxLength:         tenant.MaxPasswordBytes,
		RequiresUppercase: p.RequireUppercase(),
		RequiresLowercase: p.RequireLowercase(),
		RequiresDigit:     p.RequireDigit(),
		RequiresSymbol:    p.RequireSymbol(),
	}
}

// GetOwnPasswordPolicyHandler resolves the password policy that binds the caller:
// the policy of the caller's OWN tenant (the tenant of the token, never an
// operator's acted-on scope), the same rule change-password applies. It is read
// fresh and not cached with the capabilities bundle: a tenant admin may change the
// policy at any moment and a rule list that lags the server is a wrong message on a
// form.
type GetOwnPasswordPolicyHandler struct {
	tenants tenant.Repository
}

// NewGetOwnPasswordPolicyHandler wires the handler.
func NewGetOwnPasswordPolicyHandler(tenants tenant.Repository) GetOwnPasswordPolicyHandler {
	if tenants == nil {
		panic("query: NewGetOwnPasswordPolicyHandler tenants repository required")
	}
	return GetOwnPasswordPolicyHandler{tenants: tenants}
}

// Handle returns the policy of tenant id, or the default policy when id is zero or
// the tenant row is gone (the same fallback change-password uses). Any other read
// failure is an error, never a silent fallback to a different rule.
func (h GetOwnPasswordPolicyHandler) Handle(ctx context.Context, id tenant.ID) (PasswordRulesView, error) {
	if h.tenants == nil || id.IsZero() {
		return rulesOf(tenant.DefaultPasswordPolicy()), nil
	}
	tn, err := h.tenants.GetByID(ctx, id)
	switch {
	case err == nil:
		return rulesOf(tn.Settings().EffectivePasswordPolicy()), nil
	case errors.Is(err, tenant.ErrNotFound):
		return rulesOf(tenant.DefaultPasswordPolicy()), nil
	default:
		return PasswordRulesView{}, fmt.Errorf("get_own_password_policy: load tenant: %w", err)
	}
}
