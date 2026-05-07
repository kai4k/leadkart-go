package ports

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// ----- ListAllTenants (Platform) --------------------------------------------

func handleListAllTenants(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		views, err := a.Queries.ListAllTenants.Handle(r.Context(), query.ListAllTenantsQuery{})
		if err != nil {
			log.ErrorContext(r.Context(), "list all tenants failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		out := ListAllTenantsResponse{Tenants: make([]TenantDto, 0, len(views))}
		for _, v := range views {
			out.Tenants = append(out.Tenants, projectViewToDto(v))
		}
		writeJSON(w, http.StatusOK, out)
	})
}

// ----- HardDeleteTenant -----------------------------------------------------

func handleHardDeleteTenant(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseTenantIDPath(w, r)
		if !ok {
			return
		}
		err := a.Commands.HardDeleteTenant.Handle(r.Context(), command.HardDeleteTenantCommand{
			TenantID: id,
		})
		writeTenantMutationResult(w, log, r, err)
	})
}

// ----- GetPerson ------------------------------------------------------------

func handleGetPerson(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePersonIDPath(w, r)
		if !ok {
			return
		}
		view, err := a.Queries.GetPerson.Handle(r.Context(), query.GetPersonQuery{PersonID: id})
		switch {
		case errors.Is(err, person.ErrNotFound):
			writeError(w, http.StatusNotFound, ErrCodePersonNotFound, "")
			return
		case err != nil:
			log.ErrorContext(r.Context(), "get person failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusOK, projectPersonViewToDto(view))
	})
}

// ----- ListPersonMemberships -----------------------------------------------

func handleListPersonMemberships(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePersonIDPath(w, r)
		if !ok {
			return
		}
		views, err := a.Queries.ListPersonMemberships.Handle(r.Context(),
			query.ListPersonMembershipsQuery{PersonID: id})
		switch {
		case errors.Is(err, person.ErrNotFound):
			writeError(w, http.StatusNotFound, ErrCodePersonNotFound, "")
			return
		case err != nil:
			log.ErrorContext(r.Context(), "list person memberships failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		out := ListPersonMembershipsResponse{Memberships: make([]UserDto, 0, len(views))}
		for _, v := range views {
			out.Memberships = append(out.Memberships, projectUserViewToDto(v))
		}
		writeJSON(w, http.StatusOK, out)
	})
}

// ----- GlobalSuspendPerson --------------------------------------------------

func handleGlobalSuspendPerson(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePersonIDPath(w, r)
		if !ok {
			return
		}
		var req GlobalSuspendPersonRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		err := a.Commands.GlobalSuspendPerson.Handle(r.Context(),
			command.GlobalSuspendPersonCommand{
				PersonID: id,
				Reason:   req.Reason,
			})
		writePersonMutationResult(w, log, r, err)
	})
}

// ----- LiftPersonGlobalSuspension -------------------------------------------

func handleLiftPersonGlobalSuspension(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePersonIDPath(w, r)
		if !ok {
			return
		}
		err := a.Commands.LiftPersonGlobalSuspension.Handle(r.Context(),
			command.LiftPersonGlobalSuspensionCommand{PersonID: id})
		writePersonMutationResult(w, log, r, err)
	})
}

// ----- AnonymisePerson (direct, Person-level) -------------------------------

func handleAnonymisePerson(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePersonIDPath(w, r)
		if !ok {
			return
		}
		err := a.Commands.AnonymisePerson.Handle(r.Context(),
			command.AnonymisePersonCommand{PersonID: id})
		writePersonMutationResult(w, log, r, err)
	})
}

// ----- UpdatePersonProfile (Platform direct) --------------------------------

func handleUpdatePersonProfile(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parsePersonIDPath(w, r)
		if !ok {
			return
		}
		var req UpdatePersonProfileRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		err := a.Commands.UpdatePersonProfile.Handle(r.Context(),
			command.UpdatePersonProfileCommand{
				PersonID:  id,
				FirstName: req.FirstName,
				LastName:  req.LastName,
			})
		writePersonMutationResult(w, log, r, err)
	})
}

// ----- helpers --------------------------------------------------------------

func parsePersonIDPath(w http.ResponseWriter, r *http.Request) (person.ID, bool) {
	raw := r.PathValue("personId")
	if _, err := uuid.Parse(raw); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidPersonID,
			"personId path parameter must be a UUID")
		return "", false
	}
	return person.ID(raw), true
}

// writePersonMutationResult collapses Person command outcomes:
//
//   - nil → 204
//   - person.ErrNotFound / command.ErrPersonNotFound → 404
//   - person.ErrInvalid (wrapped) → 422
//   - else → 500 + slog.ErrorContext
func writePersonMutationResult(w http.ResponseWriter, log *slog.Logger, r *http.Request, err error) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, person.ErrNotFound),
		errors.Is(err, command.ErrPersonNotFound):
		writeError(w, http.StatusNotFound, ErrCodePersonNotFound, "")
	case errors.Is(err, person.ErrInvalid):
		writeError(w, http.StatusUnprocessableEntity, ErrCodePersonInvalid, err.Error())
	default:
		log.ErrorContext(r.Context(), "person mutation failed", "err", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
	}
}

func projectPersonViewToDto(v query.PersonView) PersonDto {
	return PersonDto{
		ID:                     v.ID,
		Email:                  v.Email,
		FirstName:              v.FirstName,
		LastName:               v.LastName,
		IsActive:               v.IsActive,
		IsAnonymised:           v.IsAnonymised,
		IsGloballySuspended:    v.IsGloballySuspended,
		GlobalSuspensionReason: v.GlobalSuspensionReason,
		GloballySuspendedAt:    v.GloballySuspendedAt,
		CreatedAt:              v.CreatedAt,
		AnonymisedAt:           v.AnonymisedAt,
	}
}

// ----- CreateImpersonationSession -------------------------------------------

func handleCreateImpersonationSession(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Operator identity comes from the JWT Subject claim — the
		// session is bound to the operator who created it; subsequent
		// per-request use must match. Never trust a body-supplied
		// operator id.
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok || strings.TrimSpace(c.Subject) == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		var req CreateImpersonationSessionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		if _, err := uuid.Parse(req.TargetTenantID); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidTenantID,
				"target_tenant_id must be a UUID")
			return
		}
		var dur time.Duration
		if req.DurationMinutes > 0 {
			dur = time.Duration(req.DurationMinutes) * time.Minute
		}
		out, err := a.Commands.CreateImpersonationSession.Handle(r.Context(),
			command.CreateImpersonationSessionCommand{
				OperatorID:     c.Subject,
				TargetTenantID: tenant.ID(req.TargetTenantID),
				Reason:         req.Reason,
				Duration:       dur,
			})
		if errors.Is(err, command.ErrImpersonationInvalid) {
			writeError(w, http.StatusUnprocessableEntity, ErrCodeImpersonationInvalid, err.Error())
			return
		}
		if err != nil {
			log.ErrorContext(r.Context(), "create impersonation session failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusCreated, CreateImpersonationSessionResponse{
			SessionID:    out.SessionID,
			ExpiresAtUTC: out.ExpiresAtUTC,
		})
	})
}

// ----- EndImpersonationSession ----------------------------------------------

func handleEndImpersonationSession(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok || strings.TrimSpace(c.Subject) == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		raw := r.PathValue("sessionId")
		if _, err := uuid.Parse(raw); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidSessionID,
				"sessionId path parameter must be a UUID")
			return
		}
		err := a.Commands.EndImpersonationSession.Handle(r.Context(),
			command.EndImpersonationSessionCommand{
				OperatorID: c.Subject,
				SessionID:  raw,
			})
		if err != nil {
			log.ErrorContext(r.Context(), "end impersonation session failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// ----- ListImpersonationSessions --------------------------------------------

func handleListImpersonationSessions(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok || strings.TrimSpace(c.Subject) == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		views, err := a.Queries.ListImpersonationSessions.Handle(r.Context(),
			query.ListImpersonationSessionsQuery{OperatorID: c.Subject})
		if err != nil {
			log.ErrorContext(r.Context(), "list impersonation sessions failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		out := ListImpersonationSessionsResponse{
			Sessions: make([]ImpersonationSessionDto, 0, len(views)),
		}
		for _, v := range views {
			out.Sessions = append(out.Sessions, ImpersonationSessionDto{
				SessionID:      v.SessionID,
				OperatorID:     v.OperatorID,
				TargetTenantID: v.TargetTenantID,
				Reason:         v.Reason,
				CreatedAt:      v.CreatedAt,
				ExpiresAt:      v.ExpiresAt,
			})
		}
		writeJSON(w, http.StatusOK, out)
	})
}

// ----- PlatformStats --------------------------------------------------------

func handlePlatformStats(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		view, err := a.Queries.PlatformStats.Handle(r.Context())
		if err != nil {
			log.ErrorContext(r.Context(), "platform stats failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusOK, PlatformStatsResponse{
			TenantsTotal:      view.TenantsTotal,
			TenantsActive:     view.TenantsActive,
			TenantsSuspended:  view.TenantsSuspended,
			PersonsTotal:      view.PersonsTotal,
			MembershipsActive: view.MembershipsActive,
		})
	})
}

// _ guards the unused import stub when only some handlers are wired.
var _ = tenant.ID("")
