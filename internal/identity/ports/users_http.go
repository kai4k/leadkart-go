package ports

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// ----- GetUser --------------------------------------------------------------

func handleGetUser(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseUserIDPath(w, r)
		if !ok {
			return
		}
		view, err := a.Queries.GetUser.Handle(r.Context(), query.GetUserQuery{MembershipID: id})
		switch {
		case errors.Is(err, membership.ErrNotFound):
			writeError(w, http.StatusNotFound, ErrCodeUserNotFound, "")
			return
		case err != nil:
			log.ErrorContext(r.Context(), "get user failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusOK, projectUserViewToDto(view))
	})
}

// ----- ListUsers ------------------------------------------------------------

// handleListUsers serves GET /api/v1/users with cursor pagination per
// ADR 0038. Query params:
//
//   - ?cursor=<opaque base64>  — empty / absent = first page
//   - ?page_size=<int>         — clamped to [1, 200]; default 50
//
// Returns ACTIVE memberships only (status='active'). Inactive listing
// is a future ?status=inactive endpoint.
func handleListUsers(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Tenant scope is the caller's JWT tenant_id (bridged onto ctx
		// by RequireAuth). Attempting to list a different tenant via
		// query string would have been rejected by the same JWT-bridge
		// step; we read it back here from ctx for the query payload.
		tid, ok := tenancy.FromContext(r.Context())
		if !ok || tid == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}

		cursor, err := pagination.Decode(r.URL.Query().Get("cursor"))
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidCursor,
				"cursor failed to decode; retry without it to fetch first page")
			return
		}
		pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))

		page, err := a.Queries.ListUsersPaged.Handle(r.Context(), query.ListUsersPagedQuery{
			TenantID: tenant.ID(tid),
			Cursor:   cursor,
			PageSize: pageSize,
		})
		if err != nil {
			log.ErrorContext(r.Context(), "list users failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}

		out := ListUsersResponse{
			Users:      make([]UserDto, 0, len(page.Items)),
			HasMore:    page.HasMore,
			NextCursor: page.NextCursor,
		}
		for _, v := range page.Items {
			out.Users = append(out.Users, projectUserViewToDto(v))
		}
		writeJSON(w, http.StatusOK, out)
	})
}

// ----- UpdateUserProfile ----------------------------------------------------

func handleUpdateUserProfile(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseUserIDPath(w, r)
		if !ok {
			return
		}
		var req UpdateUserProfileRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		err := a.Commands.UpdateUserProfile.Handle(r.Context(), command.UpdateUserProfileCommand{
			MembershipID:  id,
			Designation:   req.Designation,
			Department:    req.Department,
			StatusMessage: req.StatusMessage,
		})
		writeUserMutationResult(w, log, r, err)
	})
}

// ----- DeactivateUser -------------------------------------------------------

func handleDeactivateUser(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseUserIDPath(w, r)
		if !ok {
			return
		}
		var req DeactivateUserRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		err := a.Commands.DeactivateUser.Handle(r.Context(), command.DeactivateUserCommand{
			MembershipID: id,
			Reason:       req.Reason,
		})
		writeUserMutationResult(w, log, r, err)
	})
}

// ----- ReactivateUser -------------------------------------------------------

func handleReactivateUser(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseUserIDPath(w, r)
		if !ok {
			return
		}
		err := a.Commands.ReactivateUser.Handle(r.Context(), command.ReactivateUserCommand{
			MembershipID: id,
		})
		writeUserMutationResult(w, log, r, err)
	})
}

// ----- AssignUserRole -------------------------------------------------------

func handleAssignUserRole(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mid, ok := parseUserIDPath(w, r)
		if !ok {
			return
		}
		var req AssignUserRoleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		if _, err := uuid.Parse(req.RoleID); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRoleID, "role_id must be a UUID")
			return
		}
		err := a.Commands.AssignUserRole.Handle(r.Context(), command.AssignUserRoleCommand{
			MembershipID: mid,
			RoleID:       role.ID(req.RoleID),
		})
		writeUserMutationResult(w, log, r, err)
	})
}

// ----- RevokeUserRole -------------------------------------------------------

func handleRevokeUserRole(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mid, ok := parseUserIDPath(w, r)
		if !ok {
			return
		}
		raw := r.PathValue("roleId")
		if _, err := uuid.Parse(raw); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRoleID,
				"roleId path parameter must be a UUID")
			return
		}
		err := a.Commands.RevokeUserRole.Handle(r.Context(), command.RevokeUserRoleCommand{
			MembershipID: mid,
			RoleID:       role.ID(raw),
		})
		writeUserMutationResult(w, log, r, err)
	})
}

// ----- ReplaceUserPermissionOverrides ---------------------------------------

func handleReplaceUserPermissionOverrides(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mid, ok := parseUserIDPath(w, r)
		if !ok {
			return
		}
		var req ReplaceUserPermissionOverridesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		err := a.Commands.ReplaceUserPermissionOverrides.Handle(r.Context(),
			command.ReplaceUserPermissionOverridesCommand{
				MembershipID: mid,
				GrantedNames: req.Granted,
				RevokedNames: req.Revoked,
			})
		if errors.Is(err, command.ErrPermissionUnknown) {
			writeError(w, http.StatusUnprocessableEntity, ErrCodePermissionUnknown, err.Error())
			return
		}
		writeUserMutationResult(w, log, r, err)
	})
}

// ----- AssignUserManager ----------------------------------------------------

func handleAssignUserManager(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mid, ok := parseUserIDPath(w, r)
		if !ok {
			return
		}
		var req AssignUserManagerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		if _, err := uuid.Parse(req.ManagerID); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidManagerID, "manager_id must be a UUID")
			return
		}
		err := a.Commands.AssignUserManager.Handle(r.Context(), command.AssignUserManagerCommand{
			MembershipID: mid,
			ManagerID:    membership.ID(req.ManagerID),
		})
		writeUserMutationResult(w, log, r, err)
	})
}

// ----- RemoveUserManager ----------------------------------------------------

func handleRemoveUserManager(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mid, ok := parseUserIDPath(w, r)
		if !ok {
			return
		}
		err := a.Commands.RemoveUserManager.Handle(r.Context(), command.RemoveUserManagerCommand{
			MembershipID: mid,
		})
		writeUserMutationResult(w, log, r, err)
	})
}

// ----- CreateUser -----------------------------------------------------------

func handleCreateUser(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Tenant scope is the caller's JWT tenant_id (bridged onto ctx
		// by RequireAuth). The handler trusts that scope rather than
		// taking a tenant ID in the body — matches the per-tenant
		// nature of Membership creation.
		tid, ok := tenancy.FromContext(r.Context())
		if !ok || tid == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		var req CreateUserRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		addr, err := email.New(req.Email)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidEmail, err.Error())
			return
		}
		// Audit chain — caller's MembershipID stamps the new user's
		// `created_by_membership_id` (migration 20260507000008).
		// Claims are guaranteed present here by the auth middleware
		// upstream of this route; defensive fallback to zero ID
		// keeps the command deterministic if a future code path
		// invokes the handler without auth.
		callerMembership := membership.ID("")
		if claims, ok := authn.ClaimsFromContext(r.Context()); ok && claims != nil {
			callerMembership = membership.ID(claims.MembershipID)
		}
		out, err := a.Commands.CreateUser.Handle(r.Context(), command.CreateUserCommand{
			TenantID:              tenant.ID(tid),
			Email:                 addr,
			Password:              req.Password,
			FirstName:             req.FirstName,
			LastName:              req.LastName,
			CreatedByMembershipID: callerMembership,
		})
		switch {
		case errors.Is(err, command.ErrEmailHasActiveMembership):
			writeError(w, http.StatusConflict, ErrCodeEmailHasActiveMembership,
				"this email already has an active membership in another tenant")
			return
		case errors.Is(err, membership.ErrInvalid):
			writeError(w, http.StatusUnprocessableEntity, ErrCodeUserInvalid, err.Error())
			return
		case err != nil:
			log.ErrorContext(r.Context(), "create user failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusCreated, CreateUserResponse{
			PersonID:      out.PersonID.String(),
			MembershipID:  out.MembershipID.String(),
			PersonExisted: out.PersonExisted,
		})
	})
}

// ----- AnonymiseUser --------------------------------------------------------

func handleAnonymiseUser(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mid, ok := parseUserIDPath(w, r)
		if !ok {
			return
		}
		err := a.Commands.AnonymiseUser.Handle(r.Context(), command.AnonymiseUserCommand{
			MembershipID: mid,
		})
		writeUserMutationResult(w, log, r, err)
	})
}

// ----- helpers --------------------------------------------------------------

func parseUserIDPath(w http.ResponseWriter, r *http.Request) (membership.ID, bool) {
	raw := r.PathValue("userId")
	if _, err := uuid.Parse(raw); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidUserID,
			"userId path parameter must be a UUID")
		return "", false
	}
	return membership.ID(raw), true
}

// writeUserMutationResult collapses the small set of expected outcomes
// from a user-mutation command handler into HTTP responses.
//
//   - nil → 204
//   - membership.ErrNotFound / command.ErrUserNotFound → 404
//   - membership.ErrInvalid → 422
//   - else → 500 + slog.ErrorContext
func writeUserMutationResult(w http.ResponseWriter, log *slog.Logger, r *http.Request, err error) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, membership.ErrNotFound),
		errors.Is(err, command.ErrUserNotFound):
		writeError(w, http.StatusNotFound, ErrCodeUserNotFound, "")
	case errors.Is(err, membership.ErrInvalid):
		writeError(w, http.StatusUnprocessableEntity, ErrCodeUserInvalid, err.Error())
	default:
		log.ErrorContext(r.Context(), "user mutation failed", "err", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
	}
}

func projectUserViewToDto(v query.UserView) UserDto {
	return UserDto{
		MembershipID:  v.MembershipID,
		PersonID:      v.PersonID,
		TenantID:      v.TenantID,
		Email:         v.Email,
		FirstName:     v.FirstName,
		LastName:      v.LastName,
		Status:        v.Status,
		Designation:   v.Designation,
		Department:    v.Department,
		StatusMessage: v.StatusMessage,
		JoinedAt:      v.JoinedAt,
		LeftAt:        v.LeftAt,
		ReportsTo:     v.ReportsTo,
		RoleIDs:       v.RoleIDs,
	}
}
