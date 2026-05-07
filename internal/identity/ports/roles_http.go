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
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// ----- GetRole --------------------------------------------------------------

func handleGetRole(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseRoleIDPath(w, r)
		if !ok {
			return
		}
		view, err := a.Queries.GetRole.Handle(r.Context(), query.GetRoleQuery{RoleID: id})
		switch {
		case errors.Is(err, role.ErrNotFound):
			writeError(w, http.StatusNotFound, ErrCodeRoleNotFound, "")
			return
		case err != nil:
			log.ErrorContext(r.Context(), "get role failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusOK, projectRoleViewToDto(view))
	})
}

// ----- ListRoles ------------------------------------------------------------

func handleListRoles(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := tenancy.FromContext(r.Context())
		if !ok || tid == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		views, err := a.Queries.ListRoles.Handle(r.Context(), query.ListRolesQuery{
			TenantID: tenant.ID(tid),
		})
		if err != nil {
			log.ErrorContext(r.Context(), "list roles failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		out := ListRolesResponse{Roles: make([]RoleDto, 0, len(views))}
		for _, v := range views {
			out.Roles = append(out.Roles, projectRoleViewToDto(v))
		}
		writeJSON(w, http.StatusOK, out)
	})
}

// ----- CreateRole -----------------------------------------------------------

func handleCreateRole(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := tenancy.FromContext(r.Context())
		if !ok || tid == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		var req CreateRoleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		out, err := a.Commands.CreateRole.Handle(r.Context(), command.CreateRoleCommand{
			TenantID:       tenant.ID(tid),
			Name:           req.Name,
			HierarchyLevel: req.HierarchyLevel,
		})
		switch {
		case errors.Is(err, command.ErrRoleNameTaken):
			writeError(w, http.StatusConflict, ErrCodeRoleNameTaken,
				"a role with this name already exists")
			return
		case errors.Is(err, role.ErrInvalid):
			writeError(w, http.StatusUnprocessableEntity, ErrCodeRoleInvalid, err.Error())
			return
		case err != nil:
			log.ErrorContext(r.Context(), "create role failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusCreated, CreateRoleResponse{RoleID: out.RoleID.String()})
	})
}

// ----- UpdateRole -----------------------------------------------------------

func handleUpdateRole(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseRoleIDPath(w, r)
		if !ok {
			return
		}
		var req UpdateRoleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		hl := -1
		if req.HierarchyLevel != nil {
			hl = *req.HierarchyLevel
		}
		err := a.Commands.UpdateRole.Handle(r.Context(), command.UpdateRoleCommand{
			RoleID:         id,
			Name:           req.Name,
			HierarchyLevel: hl,
		})
		writeRoleMutationResult(w, log, r, err)
	})
}

// ----- ReplaceRolePermissions -----------------------------------------------

func handleReplaceRolePermissions(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseRoleIDPath(w, r)
		if !ok {
			return
		}
		var req ReplaceRolePermissionsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		err := a.Commands.ReplaceRolePermissions.Handle(r.Context(),
			command.ReplaceRolePermissionsCommand{
				RoleID:          id,
				PermissionNames: req.Permissions,
			})
		if errors.Is(err, command.ErrPermissionUnknown) {
			writeError(w, http.StatusUnprocessableEntity, ErrCodePermissionUnknown, err.Error())
			return
		}
		writeRoleMutationResult(w, log, r, err)
	})
}

// ----- GrantRolePermission --------------------------------------------------

func handleGrantRolePermission(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseRoleIDPath(w, r)
		if !ok {
			return
		}
		var req RolePermissionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		err := a.Commands.GrantRolePermission.Handle(r.Context(),
			command.GrantRolePermissionCommand{
				RoleID:         id,
				PermissionName: req.Permission,
			})
		if errors.Is(err, command.ErrPermissionUnknown) {
			writeError(w, http.StatusUnprocessableEntity, ErrCodePermissionUnknown, err.Error())
			return
		}
		writeRoleMutationResult(w, log, r, err)
	})
}

// ----- RevokeRolePermission -------------------------------------------------

func handleRevokeRolePermission(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseRoleIDPath(w, r)
		if !ok {
			return
		}
		var req RolePermissionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		err := a.Commands.RevokeRolePermission.Handle(r.Context(),
			command.RevokeRolePermissionCommand{
				RoleID:         id,
				PermissionName: req.Permission,
			})
		if errors.Is(err, command.ErrPermissionUnknown) {
			writeError(w, http.StatusUnprocessableEntity, ErrCodePermissionUnknown, err.Error())
			return
		}
		writeRoleMutationResult(w, log, r, err)
	})
}

// ----- DeleteRole -----------------------------------------------------------

func handleDeleteRole(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseRoleIDPath(w, r)
		if !ok {
			return
		}
		// DeletedBy comes from the JWT Subject claim — caller's PersonID.
		c, claimsOk := authn.ClaimsFromContext(r.Context())
		if !claimsOk {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		err := a.Commands.DeleteRole.Handle(r.Context(), command.DeleteRoleCommand{
			RoleID:    id,
			DeletedBy: c.Subject,
		})
		writeRoleMutationResult(w, log, r, err)
	})
}

// ----- helpers --------------------------------------------------------------

func parseRoleIDPath(w http.ResponseWriter, r *http.Request) (role.ID, bool) {
	raw := r.PathValue("roleId")
	if _, err := uuid.Parse(raw); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRoleID,
			"roleId path parameter must be a UUID")
		return "", false
	}
	return role.ID(raw), true
}

func writeRoleMutationResult(w http.ResponseWriter, log *slog.Logger, r *http.Request, err error) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, role.ErrNotFound),
		errors.Is(err, command.ErrRoleNotFound):
		writeError(w, http.StatusNotFound, ErrCodeRoleNotFound, "")
	case errors.Is(err, role.ErrNameTaken),
		errors.Is(err, command.ErrRoleNameTaken):
		writeError(w, http.StatusConflict, ErrCodeRoleNameTaken,
			"a role with this name already exists")
	case errors.Is(err, role.ErrInvalid):
		writeError(w, http.StatusUnprocessableEntity, ErrCodeRoleInvalid, err.Error())
	default:
		log.ErrorContext(r.Context(), "role mutation failed", "err", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
	}
}

func projectRoleViewToDto(v query.RoleView) RoleDto {
	return RoleDto{
		ID:              v.ID,
		TenantID:        v.TenantID,
		Name:            v.Name,
		IsSystemDefault: v.IsSystemDefault,
		IsSuperAdmin:    v.IsSuperAdmin,
		HierarchyLevel:  v.HierarchyLevel,
		Permissions:     v.Permissions,
		CreatedAt:       v.CreatedAt,
	}
}
