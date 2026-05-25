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
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/rolehierarchy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// ----- GetRole --------------------------------------------------------------

func handleGetRole(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := tenancy.FromContext(r.Context())
		if !ok || tid == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		id, ok := parseRoleIDPath(w, r)
		if !ok {
			return
		}
		view, err := a.Queries.GetRole.Handle(r.Context(), query.GetRoleQuery{
			TenantID: tenant.ID(tid),
			RoleID:   id,
		})
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
		var parent role.ID
		if req.ParentRoleID != "" {
			if _, perr := uuid.Parse(req.ParentRoleID); perr != nil {
				writeError(w, http.StatusBadRequest, ErrCodeInvalidRoleID,
					"parent_role_id must be a UUID")
				return
			}
			parent = role.ID(req.ParentRoleID)
		}
		actor := actorMembershipFromContext(r)
		out, err := a.Commands.CreateRole.Handle(r.Context(), command.CreateRoleCommand{
			TenantID:          tenant.ID(tid),
			Name:              req.Name,
			HierarchyLevel:    req.HierarchyLevel,
			ParentRoleID:      parent,
			ActorMembershipID: actor,
		})
		switch {
		case errors.Is(err, command.ErrRoleNameTaken):
			writeError(w, http.StatusConflict, ErrCodeRoleNameTaken,
				"a role with this name already exists")
			return
		case errors.Is(err, rolehierarchy.ErrCycle):
			writeError(w, http.StatusUnprocessableEntity, ErrCodeRoleHierarchyCycle,
				"parent_role_id creates a cycle in the role hierarchy")
			return
		case errors.Is(err, rolehierarchy.ErrCrossTenant):
			writeError(w, http.StatusUnprocessableEntity, ErrCodeRoleHierarchyCrossTenant,
				"parent_role_id belongs to a different tenant")
			return
		case errors.Is(err, rolehierarchy.ErrSelfReference):
			writeError(w, http.StatusBadRequest, ErrCodeRoleHierarchySelfReference,
				"a role cannot be its own parent")
			return
		case errors.Is(err, rolehierarchy.ErrEdgeAlreadyExists):
			writeError(w, http.StatusConflict, ErrCodeRoleHierarchyEdgeExists,
				"role already has an active parent edge")
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

// ----- SetRoleParent (ADR 0058 — Wave 9.4) ----------------------------------

func handleSetRoleParent(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := tenancy.FromContext(r.Context())
		if !ok || tid == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		id, ok := parseRoleIDPath(w, r)
		if !ok {
			return
		}
		var req SetRoleParentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		var newParent role.ID
		if req.ParentRoleID != nil && *req.ParentRoleID != "" {
			if _, perr := uuid.Parse(*req.ParentRoleID); perr != nil {
				writeError(w, http.StatusBadRequest, ErrCodeInvalidRoleID,
					"parent_role_id must be a UUID or null")
				return
			}
			newParent = role.ID(*req.ParentRoleID)
		}
		actor := actorMembershipFromContext(r)
		err := a.Commands.SetRoleParent.Handle(r.Context(), command.SetRoleParentCommand{
			TenantID:          tenant.ID(tid),
			RoleID:            id,
			NewParentID:       newParent,
			ActorMembershipID: actor,
			Reason:            req.Reason,
		})
		writeRoleMutationResult(w, log, r, err)
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
		tid, ok := tenancy.FromContext(r.Context())
		if !ok || tid == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		err := a.Commands.UpdateRole.Handle(r.Context(), command.UpdateRoleCommand{
			TenantID:       tenant.ID(tid),
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
		tid, ok := tenancy.FromContext(r.Context())
		if !ok || tid == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		err := a.Commands.ReplaceRolePermissions.Handle(r.Context(),
			command.ReplaceRolePermissionsCommand{
				TenantID:        tenant.ID(tid),
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
		tid, ok := tenancy.FromContext(r.Context())
		if !ok || tid == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		err := a.Commands.GrantRolePermission.Handle(r.Context(),
			command.GrantRolePermissionCommand{
				TenantID:       tenant.ID(tid),
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
		tid, ok := tenancy.FromContext(r.Context())
		if !ok || tid == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		err := a.Commands.RevokeRolePermission.Handle(r.Context(),
			command.RevokeRolePermissionCommand{
				TenantID:       tenant.ID(tid),
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
		tid, tidOk := tenancy.FromContext(r.Context())
		if !tidOk || tid == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		err := a.Commands.DeleteRole.Handle(r.Context(), command.DeleteRoleCommand{
			TenantID:  tenant.ID(tid),
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

// actorMembershipFromContext extracts the caller's MembershipID from
// the JWT claims so command handlers can populate audit columns.
// Returns zero when no claims OR no membership claim is present
// (system / bootstrap paths). HTTP callers under RequireFreshStamp
// always have one.
func actorMembershipFromContext(r *http.Request) membership.ID {
	c, ok := authn.ClaimsFromContext(r.Context())
	if !ok {
		return membership.ID("")
	}
	return membership.ID(c.MembershipID)
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
	case errors.Is(err, rolehierarchy.ErrCycle):
		writeError(w, http.StatusUnprocessableEntity, ErrCodeRoleHierarchyCycle,
			"parent_role_id creates a cycle in the role hierarchy")
	case errors.Is(err, rolehierarchy.ErrCrossTenant):
		writeError(w, http.StatusUnprocessableEntity, ErrCodeRoleHierarchyCrossTenant,
			"parent_role_id belongs to a different tenant")
	case errors.Is(err, rolehierarchy.ErrSelfReference):
		writeError(w, http.StatusBadRequest, ErrCodeRoleHierarchySelfReference,
			"a role cannot be its own parent")
	case errors.Is(err, rolehierarchy.ErrEdgeAlreadyExists):
		writeError(w, http.StatusConflict, ErrCodeRoleHierarchyEdgeExists,
			"role already has an active parent edge")
	case errors.Is(err, rolehierarchy.ErrInvalidReason):
		writeError(w, http.StatusUnprocessableEntity, ErrCodeRoleHierarchyInvalidReason,
			"reason must be 10-1024 characters when supplied")
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
		ParentRoleID:    v.ParentRoleID,
	}
}
