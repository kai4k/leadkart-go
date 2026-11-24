package ports

import (
	"encoding/json"
	"errors"
	"github.com/leadkart/leadkart-go/internal/common/lifecycle"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/leadkart/leadkart-go/internal/common/humantext"
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
		status, ok := lifecycle.ParseStatusFilter(r.URL.Query().Get("status"))
		if !ok {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidStatusFilter,
				"status must be one of active, inactive, all")
			return
		}
		res, err := a.Queries.ListRoles.Handle(r.Context(), query.ListRolesQuery{
			TenantID: tenant.ID(tid),
			Status:   status,
		})
		if err != nil {
			log.ErrorContext(r.Context(), "list roles failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		counts := res.Counts
		out := ListRolesResponse{Roles: make([]RoleDto, 0, len(res.Roles)), StatusCounts: &counts}
		for _, v := range res.Roles {
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
		if !decodeFullBody(w, r, &req) {
			return
		}
		if !validRoleLevel(w, req.HierarchyLevel) {
			return
		}
		var parent role.ID
		if req.ParentRoleID != "" {
			canon, ok := authn.CanonicalID(req.ParentRoleID)
			if !ok {
				writeError(w, http.StatusBadRequest, ErrCodeInvalidRoleID,
					"parent_role_id must be a UUID")
				return
			}
			parent = role.ID(canon)
		}
		actor := actorMembershipFromContext(r)
		out, err := a.Commands.CreateRole.Handle(r.Context(), command.CreateRoleCommand{
			TenantID:          tenant.ID(tid),
			Name:              req.Name,
			HierarchyLevel:    req.HierarchyLevel,
			ParentRoleID:      parent,
			ActorMembershipID: actor,
			Actor:             authn.AuthorityFrom(r.Context()),
		})
		switch {
		case writeAuthorityError(w, err):
			return
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
			writeInvalid(w, ErrCodeRoleInvalid, err)
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
		if !decodeFullBody(w, r, &req) {
			return
		}
		var newParent role.ID
		if req.ParentRoleID != nil && *req.ParentRoleID != "" {
			parentID, pok := authn.CanonicalID(*req.ParentRoleID)
			if !pok {
				writeError(w, http.StatusBadRequest, ErrCodeInvalidRoleID,
					"parent_role_id must be a UUID or null")
				return
			}
			newParent = role.ID(parentID)
		}
		actor := actorMembershipFromContext(r)
		err := a.Commands.SetRoleParent.Handle(r.Context(), command.SetRoleParentCommand{
			TenantID:          tenant.ID(tid),
			RoleID:            id,
			NewParentID:       newParent,
			ActorMembershipID: actor,
			Reason:            req.Reason,
			Actor:             authn.AuthorityFrom(r.Context()),
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
		if req.Name == "" && req.HierarchyLevel == nil {
			// {} , null or garbage: nothing the route recognises, so nothing to change.
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "the body must carry a name or a hierarchy_level")
			return
		}
		hl := -1
		if req.HierarchyLevel != nil {
			if !validRoleLevel(w, *req.HierarchyLevel) {
				return
			}
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
			Actor:          authn.AuthorityFrom(r.Context()),
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
		// A missing or null "permissions" would decode to nil and clear the role;
		// an explicit [] is the only way to say "clear all".
		if !decodeFullBody(w, r, &req) {
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
				Actor:           authn.AuthorityFrom(r.Context()),
			})
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
				Actor:          authn.AuthorityFrom(r.Context()),
			})
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
				Actor:          authn.AuthorityFrom(r.Context()),
			})
		writeRoleMutationResult(w, log, r, err)
	})
}

// ----- Deactivate / reactivate a role (ADR 0073: a role is never deleted) -----

// handleChangeRoleStatus serves POST .../deactivate (reactivate=false) and .../reactivate
// (reactivate=true): 200 with the updated role, idempotent.
func handleChangeRoleStatus(log *slog.Logger, a app.Application, reactivate bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseRoleIDPath(w, r)
		if !ok {
			return
		}
		// ChangedBy comes from the JWT Subject claim: the caller's PersonID.
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
		cmd := command.ChangeRoleStatusCommand{
			TenantID:  tenant.ID(tid),
			RoleID:    id,
			ChangedBy: c.Subject,
			Actor:     authn.AuthorityFrom(r.Context()),
		}
		var err error
		if reactivate {
			err = a.Commands.ReactivateRole.Handle(r.Context(), cmd)
		} else {
			err = a.Commands.DeactivateRole.Handle(r.Context(), cmd)
		}
		if err != nil {
			writeRoleMutationResult(w, log, r, err)
			return
		}
		view, err := a.Queries.GetRole.Handle(r.Context(), query.GetRoleQuery{TenantID: cmd.TenantID, RoleID: id})
		if err != nil {
			log.ErrorContext(r.Context(), "read role after status change failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusOK, projectRoleViewToDto(view))
	})
}

// ----- helpers --------------------------------------------------------------

// validRoleLevel: a created or re-levelled role is in [1, 99] (0 is the Owner
// tier, reserved to system roles). Writes 422 role_invalid and returns false
// otherwise.
func validRoleLevel(w http.ResponseWriter, level int) bool {
	if level < role.HierarchyLevelOwner+1 || level > role.HierarchyLevelMax {
		writeError(w, http.StatusUnprocessableEntity, ErrCodeRoleInvalid, "hierarchy_level must be between 1 and 99")
		return false
	}
	return true
}

func parseRoleIDPath(w http.ResponseWriter, r *http.Request) (role.ID, bool) {
	id, ok := authn.CanonicalID(r.PathValue("roleId"))
	if !ok {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRoleID,
			"roleId path parameter must be a UUID")
		return "", false
	}
	return role.ID(id), true
}

// actorMembershipFromContext extracts the caller's MembershipID from
// the JWT claims so command handlers can populate audit columns.
// Returns zero when no claims, an operator header scope (see authn.ActorMembershipID), OR no membership claim is present
// (system / bootstrap paths). HTTP callers under RequireFreshStamp
// always have one.
func actorMembershipFromContext(r *http.Request) membership.ID {
	return membership.ID(authn.ActorMembershipID(r.Context()))
}

func writeRoleMutationResult(w http.ResponseWriter, log *slog.Logger, r *http.Request, err error) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case writeAuthorityError(w, err):
		// ADR 0069 policy refusal, written by the shared translator.
	case errors.Is(err, command.ErrRoleInUse):
		var inUse *command.RoleInUseError
		holders := 0
		if errors.As(err, &inUse) {
			holders = inUse.ActiveHolders
		}
		msg := "the role is still held by " + strconv.Itoa(holders) + " active member(s); reassign them first"
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Type: problemType(ErrCodeRoleInUse), Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
			Detail: msg, Error: ErrCodeRoleInUse, Message: msg,
			Errors: map[string][]string{"active_holders": {strconv.Itoa(holders)}},
		})
	case errors.Is(err, command.ErrPermissionUnknown):
		writeError(w, http.StatusUnprocessableEntity, ErrCodePermissionUnknown, err.Error())
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
	case errors.Is(err, rolehierarchy.ErrInvalidReason) && errors.Is(err, humantext.ErrInvalidCharacters):
		writeInvalid(w, ErrCodeRoleHierarchyInvalidReason, err)
	case errors.Is(err, rolehierarchy.ErrInvalidReason):
		writeError(w, http.StatusUnprocessableEntity, ErrCodeRoleHierarchyInvalidReason,
			"reason must be 10-1024 characters when supplied")
	case errors.Is(err, role.ErrInvalid):
		writeInvalid(w, ErrCodeRoleInvalid, err)
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
		IsActive:        v.IsActive,
	}
}
