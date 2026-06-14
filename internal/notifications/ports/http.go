package ports

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
	"github.com/leadkart/leadkart-go/internal/notifications/app"
	"github.com/leadkart/leadkart-go/internal/notifications/app/command"
	"github.com/leadkart/leadkart-go/internal/notifications/app/query"
	"github.com/leadkart/leadkart-go/internal/notifications/domain/notification"
)

// tenantFromContext extracts the caller's tenant ID from ctx (bound by authn).
func tenantFromContext(r *http.Request) (tenant.ID, bool) {
	tid, ok := tenancy.FromContext(r.Context())
	if !ok || tid == "" {
		return tenant.ID(""), false
	}
	return tenant.ID(tid), true
}

// AddRoutes registers Notifications HTTP handlers (Mat Ryer 2024 NewServer).
//
// Routes (all under /api/v1/notifications/, recipient = caller's membership):
//
//	GET  /api/v1/notifications                          inbox (cursor-paginated)
//	GET  /api/v1/notifications/unread-count             badge count
//	POST /api/v1/notifications/read-all                 bulk mark-read
//	POST /api/v1/notifications/{notificationId}/read    mark one read
//	POST /api/v1/notifications/{notificationId}/dismiss dismiss one
//
// Per-handler permission gates: read = notifications.inbox.read; the
// state-change actions = notifications.inbox.manage. Recipient scoping (you see
// only YOUR inbox) is enforced in the query, not by permission.
func AddRoutes(mux *http.ServeMux, log *slog.Logger, a app.Application, verifier authn.Verifier, stampValidator authn.StampValidator) {
	if verifier == nil || stampValidator == nil {
		return
	}
	read := authn.RequirePermission(verifier, stampValidator, permission.IdentityPermissions.Notifications.Inbox.Read)
	manage := authn.RequirePermission(verifier, stampValidator, permission.IdentityPermissions.Notifications.Inbox.Manage)

	mux.Handle("GET /api/v1/notifications", read(handleListInbox(log, a)))
	mux.Handle("GET /api/v1/notifications/unread-count", read(handleUnreadCount(log, a)))
	mux.Handle("POST /api/v1/notifications/read-all", manage(handleMarkAllRead(log, a)))
	mux.Handle("POST /api/v1/notifications/{notificationId}/read", manage(handleMarkRead(log, a)))
	mux.Handle("POST /api/v1/notifications/{notificationId}/dismiss", manage(handleDismiss(log, a)))
}

// ----- Handlers -------------------------------------------------------------

func handleListInbox(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recipient, tid, ok := callerContext(w, r)
		if !ok {
			return
		}
		params := r.URL.Query()
		cursor, err := pagination.Decode(params.Get("cursor"))
		if err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidCursor, err.Error())
			return
		}
		pageSize := 0
		if raw := strings.TrimSpace(params.Get("page_size")); raw != "" {
			if n, perr := strconv.Atoi(raw); perr == nil {
				pageSize = n
			}
		}
		var filter notification.ListFilter
		if s := strings.TrimSpace(params.Get("state")); s != "" {
			st, perr := notification.ParseState(s)
			if perr != nil {
				writeError(w, http.StatusBadRequest, errCodeInvalidState, perr.Error())
				return
			}
			filter.State = st
		}
		if s := strings.TrimSpace(params.Get("category")); s != "" {
			cat, perr := notification.ParseCategory(s)
			if perr != nil {
				writeError(w, http.StatusBadRequest, errCodeInvalidCategory, perr.Error())
				return
			}
			filter.Category = cat
		}
		page, err := a.Queries.ListInbox.Handle(r.Context(), query.ListInboxQuery{
			TenantID:              tid,
			RecipientMembershipID: recipient,
			Filter:                filter,
			Cursor:                cursor,
			PageSize:              pageSize,
		})
		if err != nil {
			log.ErrorContext(r.Context(), "notifications: list inbox", "err", err)
			writeError(w, http.StatusInternalServerError, errCodeInternalError, "")
			return
		}
		out := ListNotificationsResponse{
			Items:      make([]NotificationDto, 0, len(page.Items)),
			HasMore:    page.HasMore,
			NextCursor: page.NextCursor,
		}
		for _, v := range page.Items {
			out.Items = append(out.Items, viewToDto(v))
		}
		writeJSON(w, http.StatusOK, out)
	})
}

func handleUnreadCount(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recipient, tid, ok := callerContext(w, r)
		if !ok {
			return
		}
		n, err := a.Queries.UnreadCount.Handle(r.Context(), query.UnreadCountQuery{
			TenantID:              tid,
			RecipientMembershipID: recipient,
		})
		if err != nil {
			log.ErrorContext(r.Context(), "notifications: unread count", "err", err)
			writeError(w, http.StatusInternalServerError, errCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusOK, UnreadCountResponse{UnreadCount: n})
	})
}

func handleMarkRead(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recipient, tid, ok := callerContext(w, r)
		if !ok {
			return
		}
		id, ok := parseNotificationID(w, r)
		if !ok {
			return
		}
		err := a.Commands.MarkRead.Handle(r.Context(), command.MarkNotificationReadCommand{
			TenantID:              tid,
			NotificationID:        id,
			RecipientMembershipID: recipient,
		})
		mapMutationErr(w, log, r, err, "mark read")
	})
}

func handleDismiss(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recipient, tid, ok := callerContext(w, r)
		if !ok {
			return
		}
		id, ok := parseNotificationID(w, r)
		if !ok {
			return
		}
		err := a.Commands.Dismiss.Handle(r.Context(), command.DismissNotificationCommand{
			TenantID:              tid,
			NotificationID:        id,
			RecipientMembershipID: recipient,
		})
		mapMutationErr(w, log, r, err, "dismiss")
	})
}

func handleMarkAllRead(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recipient, tid, ok := callerContext(w, r)
		if !ok {
			return
		}
		res, err := a.Commands.MarkAllRead.Handle(r.Context(), command.MarkAllReadCommand{
			TenantID:              tid,
			RecipientMembershipID: recipient,
		})
		if err != nil {
			log.ErrorContext(r.Context(), "notifications: mark all read", "err", err)
			writeError(w, http.StatusInternalServerError, errCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusOK, MarkAllReadResponse{MarkedRead: res.MarkedRead})
	})
}

// ----- Helpers --------------------------------------------------------------

// callerContext resolves the caller's recipient membership + tenant, writing a
// 401 and returning ok=false when either is missing. Returns the membership id
// (not the raw claims) so this package needs no identity/app dependency.
func callerContext(w http.ResponseWriter, r *http.Request) (membership.ID, tenant.ID, bool) {
	c, ok := authn.ClaimsFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
		return "", "", false
	}
	tid, ok := tenantFromContext(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
		return "", "", false
	}
	return membership.ID(c.MembershipID), tid, true
}

func parseNotificationID(w http.ResponseWriter, r *http.Request) (notification.ID, bool) {
	raw := r.PathValue("notificationId")
	if _, err := uuid.Parse(raw); err != nil {
		writeError(w, http.StatusBadRequest, errCodeInvalidNotificationID, "notificationId must be a UUID")
		return "", false
	}
	return notification.ID(raw), true
}

func mapMutationErr(w http.ResponseWriter, log *slog.Logger, r *http.Request, err error, op string) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, notification.ErrNotFound):
		writeError(w, http.StatusNotFound, errCodeNotificationNotFound, "")
	case errors.Is(err, notification.ErrInvalidTransition):
		writeError(w, http.StatusConflict, errCodeInvalidTransition, err.Error())
	default:
		log.ErrorContext(r.Context(), "notifications: "+op, "err", err)
		writeError(w, http.StatusInternalServerError, errCodeInternalError, "")
	}
}

func viewToDto(v query.NotificationView) NotificationDto {
	return NotificationDto{
		ID:               v.ID,
		Category:         v.Category,
		Title:            v.Title,
		Body:             v.Body,
		State:            v.State,
		SourceModule:     v.SourceModule,
		SourceEntityType: v.SourceEntityType,
		SourceEntityID:   v.SourceEntityID,
		DeepLink:         v.DeepLink,
		CreatedAt:        v.CreatedAtUTC,
		ReadAt:           v.ReadAtUTC,
		DismissedAt:      v.DismissedAtUTC,
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{
		Type:    "https://leadkart.api/errors/" + code,
		Title:   http.StatusText(status),
		Status:  status,
		Detail:  message,
		Error:   code,
		Message: message,
	})
}
