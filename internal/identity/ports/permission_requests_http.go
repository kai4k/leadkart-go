package ports

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// Wave 9.1e — Permission-elevation approval workflow HTTP handlers
// per ADR 0055.
//
// Authorization shape:
//   - All routes require RequireFreshStamp (auth) — population of
//     membership_id from JWT is mandatory for every operation.
//   - Submit (POST .../permission-requests): caller IS the requester.
//   - Approve/Deny: handler-inline check (caller IS requester's manager
//     OR caller's JWT carries is_platform=true).
//   - Cancel: handler-inline check (caller IS the requester; mismatch
//     collapses to 404 per ADR 0044 enumeration-safety canon).
//   - List my requests / pending-for-approver: caller IS the membership
//     in the relevant role.
//
// All routes are tenant-scoped: per ADR 0062 the TenantID is extracted
// from the request's tenancy context and threaded explicitly through
// Commands + Queries to the repository layer (no ctx-tenancy GUC
// smuggling).

// ----- POST /api/v1/permission-requests ------------------------------------

func handleCreatePermissionRequest(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok || c.MembershipID == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		tid, ok := tenancy.FromContext(r.Context())
		if !ok || tid == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}

		var req CreatePermissionRequestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}

		perm, err := permission.TryFromConstant(req.Permission)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, ErrCodePermissionUnknown, err.Error())
			return
		}

		out, err := a.Commands.RequestPermissionElevation.Handle(r.Context(),
			command.RequestPermissionElevationCommand{
				TenantID:              tenant.ID(tid),
				RequesterMembershipID: membership.ID(c.MembershipID),
				Permission:            perm,
				DurationDays:          req.DurationDays,
				Reason:                req.Reason,
			})
		switch {
		case errors.Is(err, command.ErrUserNotFound):
			writeError(w, http.StatusNotFound, ErrCodeUserNotFound, "")
			return
		case errors.Is(err, command.ErrPermissionRequestPendingExists):
			writeError(w, http.StatusConflict, ErrCodePermissionRequestPendingExists,
				"a pending request for this permission already exists")
			return
		case errors.Is(err, permissionrequest.ErrInvalidRequest),
			errors.Is(err, permissionrequest.ErrInvalidDuration),
			errors.Is(err, permissionrequest.ErrInvalidPermission):
			writeError(w, http.StatusUnprocessableEntity, ErrCodePermissionRequestInvalid, err.Error())
			return
		case err != nil:
			log.ErrorContext(r.Context(), "create permission request failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}

		resp := CreatePermissionRequestResponse{
			RequestID: out.RequestID.String(),
			Status:    string(permissionrequest.StatePending),
		}
		if !out.ApproverMembershipID.IsZero() {
			resp.ApproverMembershipID = out.ApproverMembershipID.String()
		}
		writeJSON(w, http.StatusCreated, resp)
	})
}

// ----- GET /api/v1/permission-requests (?role=requester|approver) ---------

func handleListPermissionRequests(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok || c.MembershipID == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		tid, ok := tenancy.FromContext(r.Context())
		if !ok || tid == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}

		role := r.URL.Query().Get("role")
		if role == "" {
			role = "requester" // default — caller's own history.
		}
		if role != "requester" && role != "approver" {
			writeError(w, http.StatusBadRequest, ErrCodePermissionRequestRoleQuery,
				"role must be one of: requester, approver")
			return
		}

		cursor, err := pagination.Decode(r.URL.Query().Get("cursor"))
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidCursor, err.Error())
			return
		}
		pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))

		var page pagination.Page[query.PermissionRequestView]
		var qerr error
		switch role {
		case "approver":
			page, qerr = a.Queries.ListPendingForApprover.Handle(r.Context(),
				query.ListPendingForApproverQuery{
					TenantID:             tenant.ID(tid),
					ApproverMembershipID: membership.ID(c.MembershipID),
					Cursor:               cursor,
					PageSize:             pageSize,
				})
		default: // requester
			page, qerr = a.Queries.ListMyPermissionRequests.Handle(r.Context(),
				query.ListMyPermissionRequestsQuery{
					TenantID:              tenant.ID(tid),
					RequesterMembershipID: membership.ID(c.MembershipID),
					Cursor:                cursor,
					PageSize:              pageSize,
				})
		}
		if qerr != nil {
			log.ErrorContext(r.Context(), "list permission requests failed", "err", qerr, "role", role)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}

		out := ListPermissionRequestsResponse{
			Requests:   make([]PermissionRequestDto, 0, len(page.Items)),
			HasMore:    page.HasMore,
			NextCursor: page.NextCursor,
		}
		for _, v := range page.Items {
			out.Requests = append(out.Requests, projectPermissionRequestViewToDto(v))
		}
		writeJSON(w, http.StatusOK, out)
	})
}

// ----- GET /api/v1/permission-requests/{requestId} -------------------------

func handleGetPermissionRequest(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok || c.MembershipID == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		tid, ok := tenancy.FromContext(r.Context())
		if !ok || tid == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		id, ok := parsePermissionRequestIDPath(w, r)
		if !ok {
			return
		}
		view, err := a.Queries.GetPermissionRequest.Handle(r.Context(),
			query.GetPermissionRequestQuery{TenantID: tenant.ID(tid), RequestID: id})
		switch {
		case errors.Is(err, permissionrequest.ErrNotFound):
			writeError(w, http.StatusNotFound, ErrCodePermissionRequestNotFound, "")
			return
		case err != nil:
			log.ErrorContext(r.Context(), "get permission request failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		// Caller must be either the requester, the named approver, OR
		// a Platform operator. Cross-caller reads collapse to 404 per
		// ADR 0044 enumeration-safety canon.
		isPlatform := c.IsPlatform
		isRequester := view.RequesterMembershipID == c.MembershipID
		isApprover := view.ApproverMembershipID != "" && view.ApproverMembershipID == c.MembershipID
		if !isPlatform && !isRequester && !isApprover {
			writeError(w, http.StatusNotFound, ErrCodePermissionRequestNotFound, "")
			return
		}
		writeJSON(w, http.StatusOK, projectPermissionRequestViewToDto(view))
	})
}

// ----- POST /api/v1/permission-requests/{requestId}/approve ---------------

func handleApprovePermissionRequest(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok || c.MembershipID == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		tid, ok := tenancy.FromContext(r.Context())
		if !ok || tid == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		id, ok := parsePermissionRequestIDPath(w, r)
		if !ok {
			return
		}
		var req ApprovePermissionRequestRequest
		// Body is OPTIONAL on Approve. Decode best-effort; EOF on empty
		// body is fine.
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&req)
		}
		err := a.Commands.ApprovePermissionRequest.Handle(r.Context(),
			command.ApprovePermissionRequestCommand{
				TenantID:             tenant.ID(tid),
				RequestID:            id,
				ApproverMembershipID: membership.ID(c.MembershipID),
				IsPlatformOperator:   c.IsPlatform,
				DecisionReason:       req.DecisionReason,
			})
		switch {
		case errors.Is(err, command.ErrPermissionRequestNotFound):
			writeError(w, http.StatusNotFound, ErrCodePermissionRequestNotFound, "")
			return
		case errors.Is(err, command.ErrPermissionRequestNotPending):
			writeError(w, http.StatusConflict, ErrCodePermissionRequestNotPending, "")
			return
		case errors.Is(err, command.ErrPermissionRequestSelfApproval):
			writeError(w, http.StatusUnprocessableEntity, ErrCodePermissionRequestSelfApproval, "")
			return
		case errors.Is(err, command.ErrPermissionRequestForbidden):
			writeError(w, http.StatusForbidden, ErrCodePermissionRequestForbidden, "")
			return
		case errors.Is(err, permissionrequest.ErrInvalidRequest):
			writeError(w, http.StatusUnprocessableEntity, ErrCodePermissionRequestInvalid, err.Error())
			return
		case err != nil:
			log.ErrorContext(r.Context(), "approve permission request failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// ----- POST /api/v1/permission-requests/{requestId}/deny ------------------

func handleDenyPermissionRequest(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok || c.MembershipID == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		tid, ok := tenancy.FromContext(r.Context())
		if !ok || tid == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		id, ok := parsePermissionRequestIDPath(w, r)
		if !ok {
			return
		}
		var req DenyPermissionRequestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		err := a.Commands.DenyPermissionRequest.Handle(r.Context(),
			command.DenyPermissionRequestCommand{
				TenantID:             tenant.ID(tid),
				RequestID:            id,
				ApproverMembershipID: membership.ID(c.MembershipID),
				IsPlatformOperator:   c.IsPlatform,
				DecisionReason:       req.DecisionReason,
			})
		switch {
		case errors.Is(err, command.ErrPermissionRequestNotFound):
			writeError(w, http.StatusNotFound, ErrCodePermissionRequestNotFound, "")
			return
		case errors.Is(err, command.ErrPermissionRequestNotPending):
			writeError(w, http.StatusConflict, ErrCodePermissionRequestNotPending, "")
			return
		case errors.Is(err, command.ErrPermissionRequestSelfApproval):
			writeError(w, http.StatusUnprocessableEntity, ErrCodePermissionRequestSelfApproval, "")
			return
		case errors.Is(err, command.ErrPermissionRequestForbidden):
			writeError(w, http.StatusForbidden, ErrCodePermissionRequestForbidden, "")
			return
		case errors.Is(err, permissionrequest.ErrInvalidRequest):
			writeError(w, http.StatusUnprocessableEntity, ErrCodePermissionRequestInvalid, err.Error())
			return
		case err != nil:
			log.ErrorContext(r.Context(), "deny permission request failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// ----- POST /api/v1/permission-requests/{requestId}/cancel ----------------

func handleCancelPermissionRequest(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok || c.MembershipID == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		tid, ok := tenancy.FromContext(r.Context())
		if !ok || tid == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		id, ok := parsePermissionRequestIDPath(w, r)
		if !ok {
			return
		}
		err := a.Commands.CancelPermissionRequest.Handle(r.Context(),
			command.CancelPermissionRequestCommand{
				TenantID:              tenant.ID(tid),
				RequestID:             id,
				RequesterMembershipID: membership.ID(c.MembershipID),
			})
		switch {
		case errors.Is(err, command.ErrPermissionRequestNotFound):
			writeError(w, http.StatusNotFound, ErrCodePermissionRequestNotFound, "")
			return
		case errors.Is(err, command.ErrPermissionRequestNotPending):
			writeError(w, http.StatusConflict, ErrCodePermissionRequestNotPending, "")
			return
		case err != nil:
			log.ErrorContext(r.Context(), "cancel permission request failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// ----- helpers --------------------------------------------------------------

// parsePermissionRequestIDPath extracts + validates {requestId} from
// the URL. Writes the 400 response itself on failure and returns
// (zero, false).
func parsePermissionRequestIDPath(w http.ResponseWriter, r *http.Request) (permissionrequest.ID, bool) {
	raw := r.PathValue("requestId")
	if _, err := uuid.Parse(raw); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidPermissionRequestID,
			"requestId path parameter must be a UUID")
		return "", false
	}
	return permissionrequest.ID(raw), true
}

// projectPermissionRequestViewToDto maps the query-view onto the wire
// DTO. Pure shape translation — no logic.
func projectPermissionRequestViewToDto(v query.PermissionRequestView) PermissionRequestDto {
	return PermissionRequestDto{
		ID:                    v.ID,
		TenantID:              v.TenantID,
		RequesterMembershipID: v.RequesterMembershipID,
		Permission:            v.Permission,
		DurationDays:          v.DurationDays,
		Reason:                v.Reason,
		State:                 v.State,
		ApproverMembershipID:  v.ApproverMembershipID,
		DecidedAt:             v.DecidedAt,
		DecisionReason:        v.DecisionReason,
		ExpiresAt:             v.ExpiresAt,
		CreatedAt:             v.CreatedAt,
		UpdatedAt:             v.UpdatedAt,
	}
}
