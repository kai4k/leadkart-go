// Package app holds the Identity Application facade.
//
// Per TDL Wild Workouts canonical layout: an Application{Commands,
// Queries} struct aggregates concrete handler structs as fields. HTTP
// + future gRPC + future CLI ports call `app.Commands.X.Handle(...)`
// directly — no service interface, no mediator.
//
// Composition root (cmd/api/main.go) builds an Application by wiring
// concrete adapters into each handler's constructor + assembling the
// facade. Tests typically construct a partial Application with fakes
// for the handlers under test.
package app

import (
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
)

// Application is the Identity facade. Every external port (HTTP,
// future gRPC, event subscribers) takes an Application and dispatches
// directly into its handler fields.
type Application struct {
	Commands Commands
	Queries  Queries
}

// Commands aggregates all Identity command handlers. One field per
// use case. New use cases extend this struct, never reach into a
// shared service abstraction.
type Commands struct {
	RegisterTenant       command.RegisterTenantHandler
	Login                command.LoginHandler
	Refresh              command.RefreshHandler
	Logout               command.LogoutHandler
	ChangePassword       command.ChangePasswordHandler
	RevokeSession        command.RevokeSessionHandler
	RevokeAllSessions    command.RevokeAllSessionsHandler
	RequestPasswordReset command.RequestPasswordResetHandler
	ConfirmPasswordReset command.ConfirmPasswordResetHandler
	RequestEmailChange   command.RequestEmailChangeHandler
	ConfirmEmailChange   command.ConfirmEmailChangeHandler

	// Tenant management.
	UpdateTenantProfile            command.UpdateTenantProfileHandler
	UpdateTenantStatutory          command.UpdateTenantStatutoryHandler
	UpdateTenantAdminContact       command.UpdateTenantAdminContactHandler
	UpdateTenantSettings           command.UpdateTenantSettingsHandler
	UpdateTenantDisplayPreferences command.UpdateTenantDisplayPreferencesHandler
	SuspendTenant                  command.SuspendTenantHandler
	ActivateTenant                 command.ActivateTenantHandler
	MarkTenantForDeletion          command.MarkTenantForDeletionHandler
	RestoreTenant                  command.RestoreTenantHandler

	// User (Membership) management.
	UpdateUserProfile              command.UpdateUserProfileHandler
	DeactivateUser                 command.DeactivateUserHandler
	ReactivateUser                 command.ReactivateUserHandler
	AssignUserRole                 command.AssignUserRoleHandler
	RevokeUserRole                 command.RevokeUserRoleHandler
	ReplaceUserPermissionOverrides command.ReplaceUserPermissionOverridesHandler
	AssignUserManager              command.AssignUserManagerHandler
	RemoveUserManager              command.RemoveUserManagerHandler
	CreateUser                     command.CreateUserHandler
	AnonymiseUser                  command.AnonymiseUserHandler

	// Role management.
	CreateRole              command.CreateRoleHandler
	UpdateRole              command.UpdateRoleHandler
	DeleteRole              command.DeleteRoleHandler
	ReplaceRolePermissions  command.ReplaceRolePermissionsHandler
	GrantRolePermission     command.GrantRolePermissionHandler
	RevokeRolePermission    command.RevokeRolePermissionHandler

	// Platform — cross-tenant Person + tenant ops.
	GlobalSuspendPerson         command.GlobalSuspendPersonHandler
	LiftPersonGlobalSuspension  command.LiftPersonGlobalSuspensionHandler
	AnonymisePerson             command.AnonymisePersonHandler
	UpdatePersonProfile         command.UpdatePersonProfileHandler
	HardDeleteTenant            command.HardDeleteTenantHandler
}

// Queries aggregates all Identity query handlers. Read-side only — no
// state mutation. Mirrors the [Commands] composition shape.
type Queries struct {
	ListSessions          query.ListSessionsHandler
	GetTenant             query.GetTenantHandler
	GetUser               query.GetUserHandler
	ListUsers             query.ListUsersHandler
	GetRole               query.GetRoleHandler
	ListRoles             query.ListRolesHandler
	GetPerson             query.GetPersonHandler
	ListPersonMemberships query.ListPersonMembershipsHandler
	ListAllTenants        query.ListAllTenantsHandler
}
