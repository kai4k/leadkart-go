package ports

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
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
		views, err := a.Queries.ListUsers.Handle(r.Context(), query.ListUsersQuery{
			TenantID: tenant.ID(tid),
		})
		if err != nil {
			log.ErrorContext(r.Context(), "list users failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		out := ListUsersResponse{Users: make([]UserDto, 0, len(views))}
		for _, v := range views {
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
