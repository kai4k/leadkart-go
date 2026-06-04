package ports

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/crm/app"
	"github.com/leadkart/leadkart-go/internal/crm/app/command"
	"github.com/leadkart/leadkart-go/internal/crm/app/query"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// handleCreateReminder is POST /api/v1/crm/leads/{leadId}/reminders.
// Creates a manual reminder against a lead. Gated by crm.leads.manage.
func handleCreateReminder(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		leadID, ok := parseLeadID(w, r)
		if !ok {
			return
		}
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		var req CreateReminderRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidBody, "request body is not valid JSON")
			return
		}
		if _, err := uuid.Parse(req.AssignedToMembershipID); err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidMembershipID, "assigned_to_membership_id must be a UUID")
			return
		}
		if req.DueAt.IsZero() {
			writeError(w, http.StatusUnprocessableEntity, errCodeInvalidDueAt, "due_at is required (RFC 3339)")
			return
		}
		out, err := a.Commands.CreateReminder.Handle(r.Context(), command.CreateReminderCommand{
			TenantID:               tid,
			LeadID:                 leadID,
			AssignedToMembershipID: req.AssignedToMembershipID,
			CreatedByMembershipID:  c.MembershipID,
			Type:                   reminder.TypeManual,
			DueAt:                  req.DueAt,
			Notes:                  req.Notes,
		})
		switch {
		case errors.Is(err, command.ErrLeadNotFound):
			writeError(w, http.StatusNotFound, errCodeLeadNotFound, "")
			return
		case errors.Is(err, reminder.ErrInvalid):
			writeError(w, http.StatusUnprocessableEntity, errCodeInvalidDueAt, err.Error())
			return
		case err != nil:
			log.ErrorContext(r.Context(), "crm: create reminder", "err", err)
			writeError(w, http.StatusInternalServerError, errCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusCreated, CreateReminderResponse{ReminderID: out.ReminderID.String()})
	})
}

// handleListReminders is GET /api/v1/crm/reminders. Cursor-paginated
// pending reminders for the caller's tenant. Callers without
// crm.leads.read_all see only their own assignee bucket per ADR 0060
// + Auth0 read:any canon (mirrors the leads list rule).
func handleListReminders(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
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
		filter := reminder.PendingFilter{}
		if t := strings.TrimSpace(params.Get("type")); t != "" {
			parsed, perr := reminder.ParseType(t)
			if perr != nil {
				writeError(w, http.StatusBadRequest, errCodeInvalidStage, perr.Error())
				return
			}
			filter.Type = parsed
		}
		if assignee := strings.TrimSpace(params.Get("assignee")); assignee != "" {
			if _, perr := uuid.Parse(assignee); perr != nil {
				writeError(w, http.StatusBadRequest, errCodeInvalidMembershipID, "assignee must be a UUID")
				return
			}
			filter.AssigneeMembershipID = assignee
		}
		if leadParam := strings.TrimSpace(params.Get("lead_id")); leadParam != "" {
			if _, perr := uuid.Parse(leadParam); perr != nil {
				writeError(w, http.StatusBadRequest, errCodeInvalidLeadID, "lead_id must be a UUID")
				return
			}
			filter.LeadID = crmlead.ID(leadParam)
		}

		selfFilter := ""
		if !c.IsSuperUser && !c.IsPlatform &&
			!slices.Contains(c.Permissions, permission.IdentityPermissions.Crm.Leads.ReadAll) {
			selfFilter = c.MembershipID
		}
		// Mirror of the leads-list privilege-probe guard (reviewer H8):
		// caller without read_all + an explicit assignee= that doesn't
		// match their own membership is a probe.
		if selfFilter != "" && filter.AssigneeMembershipID != "" && filter.AssigneeMembershipID != selfFilter {
			writeError(w, http.StatusForbidden, errCodeForbidden,
				"caller lacks crm.leads.read_all; ?assignee must equal the caller's own membership")
			return
		}

		page, err := a.Queries.ListPendingReminders.Handle(r.Context(), query.ListPendingRemindersQuery{
			TenantID: tid, Cursor: cursor, PageSize: pageSize, Filter: filter, SelfFilter: selfFilter,
		})
		if err != nil {
			log.ErrorContext(r.Context(), "crm: list reminders", "err", err)
			writeError(w, http.StatusInternalServerError, errCodeInternalError, "")
			return
		}
		out := ListRemindersResponse{
			Items:      make([]ReminderDto, 0, len(page.Items)),
			HasMore:    page.HasMore,
			NextCursor: page.NextCursor,
		}
		for _, rem := range page.Items {
			out.Items = append(out.Items, reminderToDto(rem))
		}
		writeJSON(w, http.StatusOK, out)
	})
}

// handleMarkReminderSent is POST /api/v1/crm/reminders/{reminderId}/sent.
func handleMarkReminderSent(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		id, ok := parseReminderID(w, r)
		if !ok {
			return
		}
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		err := a.Commands.MarkReminderSent.Handle(r.Context(), command.MarkReminderSentCommand{
			TenantID: tid, ReminderID: id, MarkedByMembershipID: c.MembershipID,
		})
		mapReminderMutationErr(w, log, r, err, "mark reminder sent")
	})
}

// handleCancelReminder is POST /api/v1/crm/reminders/{reminderId}/cancel.
func handleCancelReminder(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		id, ok := parseReminderID(w, r)
		if !ok {
			return
		}
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		var req CancelReminderRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidBody, "request body is not valid JSON")
			return
		}
		if strings.TrimSpace(req.Reason) == "" {
			writeError(w, http.StatusUnprocessableEntity, errCodeReasonRequired, "reason is required for cancel")
			return
		}
		err := a.Commands.CancelReminder.Handle(r.Context(), command.CancelReminderCommand{
			TenantID: tid, ReminderID: id, CancelledByMembershipID: c.MembershipID, Reason: req.Reason,
		})
		mapReminderMutationErr(w, log, r, err, "cancel reminder")
	})
}

// ----- Helpers --------------------------------------------------------------

func parseReminderID(w http.ResponseWriter, r *http.Request) (reminder.ID, bool) {
	raw := r.PathValue("reminderId")
	if _, err := uuid.Parse(raw); err != nil {
		writeError(w, http.StatusBadRequest, errCodeInvalidReminderID, "reminderId path parameter must be a UUID")
		return "", false
	}
	return reminder.ID(raw), true
}

func mapReminderMutationErr(w http.ResponseWriter, log *slog.Logger, r *http.Request, err error, op string) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, command.ErrReminderNotFound):
		writeError(w, http.StatusNotFound, errCodeReminderNotFound, "")
	case errors.Is(err, command.ErrReminderTerminal):
		writeError(w, http.StatusConflict, errCodeReminderTerminal, "")
	case errors.Is(err, reminder.ErrInvalid):
		writeError(w, http.StatusUnprocessableEntity, errCodeReasonRequired, err.Error())
	default:
		log.ErrorContext(r.Context(), "crm: "+op, "err", err)
		writeError(w, http.StatusInternalServerError, errCodeInternalError, "")
	}
}

func reminderToDto(v query.ReminderView) ReminderDto {
	return ReminderDto{
		ID:                       v.ID,
		TenantID:                 v.TenantID,
		LeadID:                   v.LeadID,
		AssignedToMembershipID:   v.AssignedToMembershipID,
		CreatedByMembershipID:    v.CreatedByMembershipID,
		SourceCallLogID:          v.SourceCallLogID,
		Type:                     v.Type,
		State:                    v.State,
		DueAt:                    v.DueAt,
		Notes:                    v.Notes,
		SentAt:                   v.SentAt,
		MarkedSentByMembershipID: v.MarkedSentByMembershipID,
		CancelledAt:              v.CancelledAt,
		CancelledByMembershipID:  v.CancelledByMembershipID,
		CancelReason:             v.CancelReason,
		CreatedAt:                v.CreatedAt,
	}
}
