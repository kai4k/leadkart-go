package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/crm/domain/calllog"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// LogCallCommand carries a call-log creation request. The handler
// verifies the parent lead exists + is not terminal (a converted /
// lost lead refuses new call logs — final dispositions are final).
//
// TenantID is the caller's tenant scope (TDL canon per ADR 0062 —
// flows through as an explicit value, not via ctx-tenancy).
type LogCallCommand struct {
	TenantID             tenant.ID
	LeadID               crmlead.ID
	Outcome              calllog.Outcome
	Notes                string
	LoggedByMembershipID string
}

// LogCallResult returns the new call-log ID for the caller.
type LogCallResult struct {
	CallID calllog.ID
}

// LogCallHandler runs the log-a-call flow. Append-only; no UpdateByID.
type LogCallHandler struct {
	leads     crmlead.Repository
	calls     calllog.Repository
	now       func() time.Time
	newCallID func() calllog.ID
}

// NewLogCallHandler wires the handler.
//
// newCallID is the CallLog ID factory per the
// `TestArch_HandlersInjectIDFactory` discipline. Production passes
// `func() calllog.ID { return calllog.ID(ids.NewV7().String()) }`;
// tests inject a deterministic counter so the minted ID is pinnable.
func NewLogCallHandler(leads crmlead.Repository, calls calllog.Repository, now func() time.Time, newCallID func() calllog.ID) LogCallHandler {
	if leads == nil {
		panic("command: NewLogCallHandler leads repository required")
	}
	if calls == nil {
		panic("command: NewLogCallHandler calls repository required")
	}
	if newCallID == nil {
		panic("command: NewLogCallHandler newCallID required")
	}
	if now == nil {
		now = time.Now
	}
	return LogCallHandler{leads: leads, calls: calls, now: now, newCallID: newCallID}
}

// Handle persists the call log + emits the V1 event via the repository's
// outbox drain.
func (h LogCallHandler) Handle(ctx context.Context, cmd LogCallCommand) (LogCallResult, error) {
	if cmd.TenantID.IsZero() {
		return LogCallResult{}, errors.New("crm log_call: tenant id required")
	}
	if cmd.LeadID.IsZero() {
		return LogCallResult{}, errors.New("crm log_call: lead id required")
	}
	if cmd.LoggedByMembershipID == "" {
		return LogCallResult{}, errors.New("crm log_call: logged-by membership id required")
	}
	// Load the lead first — gates against terminal leads + verifies the
	// caller's tenant scope actually owns this lead (RLS-equivalent
	// rejection at the repository layer).
	lead, err := h.leads.GetByID(ctx, cmd.TenantID, cmd.LeadID)
	if err != nil {
		if errors.Is(err, crmlead.ErrNotFound) {
			return LogCallResult{}, ErrLeadNotFound
		}
		return LogCallResult{}, fmt.Errorf("crm log_call: load lead: %w", err)
	}
	if lead.Stage().IsTerminal() {
		return LogCallResult{}, ErrLeadTerminal
	}

	c, err := calllog.New(
		h.newCallID(),
		cmd.TenantID,
		cmd.LeadID,
		cmd.Outcome,
		cmd.Notes,
		cmd.LoggedByMembershipID,
		h.now(),
	)
	if err != nil {
		return LogCallResult{}, fmt.Errorf("crm log_call: factory: %w", err)
	}
	if err := h.calls.Add(ctx, c); err != nil {
		return LogCallResult{}, fmt.Errorf("crm log_call: persist: %w", err)
	}
	return LogCallResult{CallID: c.ID()}, nil
}
