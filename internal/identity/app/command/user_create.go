package command

import (
	"context"
	"errors"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/app/service"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// CreateUserCommand carries the validated input for adding a user
// to the caller's tenant. Mirrors [service.CreateUserCommand] shape;
// the handler is a thin pass-through.
type CreateUserCommand struct {
	TenantID  tenant.ID
	Email     email.Address
	Password  string
	FirstName string
	LastName  string
}

// CreateUserResult is the wire-friendly outcome of CreateUser.
type CreateUserResult struct {
	PersonID      person.ID
	MembershipID  membership.ID
	PersonExisted bool
}

// CreateUserHandler dispatches to the [service.UserOnboardingService].
type CreateUserHandler struct {
	onboarding *service.UserOnboardingService
}

// NewCreateUserHandler wires the handler.
func NewCreateUserHandler(svc *service.UserOnboardingService) CreateUserHandler {
	if svc == nil {
		panic("command: NewCreateUserHandler service required")
	}
	return CreateUserHandler{onboarding: svc}
}

// Handle dispatches to the service. Translates the service-layer
// result into the public command result shape.
func (h CreateUserHandler) Handle(ctx context.Context, cmd CreateUserCommand) (CreateUserResult, error) {
	out, err := h.onboarding.CreateUser(ctx, service.CreateUserCommand{
		TenantID:  cmd.TenantID,
		Email:     cmd.Email,
		Password:  cmd.Password,
		FirstName: cmd.FirstName,
		LastName:  cmd.LastName,
	})
	if err != nil {
		if errors.Is(err, service.ErrEmailHasActiveMembership) {
			return CreateUserResult{}, ErrEmailHasActiveMembership
		}
		return CreateUserResult{}, fmt.Errorf("create_user: %w", err)
	}
	return CreateUserResult{
		PersonID:      out.PersonID,
		MembershipID:  out.MembershipID,
		PersonExisted: out.PersonExisted,
	}, nil
}
