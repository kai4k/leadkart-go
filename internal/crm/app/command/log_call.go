package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
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
//
// CallbackWindow is the optional caller-supplied window the contact
// asked to be called back in per BRD §4.5. Zero when no callback was
// requested. Non-zero values fan out to the persisted row + the
// CallLogged integration event so the Reminder slice's CallLogged
// subscriber can mint a callback reminder due at Start.
type LogCallCommand struct {
	TenantID             tenant.ID
	LeadID               crmlead.ID
	Outcome              calllog.Outcome
	Notes                string
	LoggedByMembershipID string
	CallbackWindow       calllog.CallbackWindow
	// OnlyAssignedTo, when set, limits the command to a lead assigned to this
	// membership: any other lead does not exist for the caller (ErrLeadNotFound).
	// Empty means unrestricted (read_all callers, operators, system callers).
	OnlyAssignedTo string
}

// LogCallResult returns the new call-log ID for the caller.
type LogCallResult struct {
	CallID calllog.ID
}

// LogCallHandler runs the log-a-call flow. The call log is append-only; the
// lead is only guarded (scope, stage) inside the same tenant transaction.
type LogCallHandler struct {
	leads     crmlead.Repository
	calls     calllog.Repository
	uow       pg.TenantUnitOfWork
	now       func() time.Time
	newCallID func() calllog.ID
}

// NewLogCallHandler wires the handler.
//
// newCallID is the CallLog ID factory per the
// `TestArch_HandlersInjectIDFactory` discipline. Production passes
// `func() calllog.ID { return calllog.ID(ids.NewV7().String()) }`;
// tests inject a deterministic counter so the minted ID is pinnable.
func NewLogCallHandler(leads crmlead.Repository, calls calllog.Repository, uow pg.TenantUnitOfWork, now func() time.Time, newCallID func() calllog.ID) LogCallHandler {
	if leads == nil {
		panic("command: NewLogCallHandler leads repository required")
	}
	if calls == nil {
		panic("command: NewLogCallHandler calls repository required")
	}
	if uow == nil {
		panic("command: NewLogCallHandler uow required")
	}
	if newCallID == nil {
		panic("command: NewLogCallHandler newCallID required")
	}
	if now == nil {
		now = time.Now
	}
	return LogCallHandler{leads: leads, calls: calls, uow: uow, now: now, newCallID: newCallID}
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
	// The lead's scope, its stage and the call-log write are ONE tenant
	// transaction: the scope check runs inside UpdateByID (which locks the lead
	// row), so a concurrent reassignment cannot slip between the check and the
	// write. A lead outside the caller's scope does not exist for the caller.
	var callID calllog.ID
	err := h.uow.WithinTenantTx(ctx, cmd.TenantID.String(), func(ctx context.Context) error {
		if err := h.leads.UpdateByID(ctx, cmd.TenantID, cmd.LeadID, func(l *crmlead.CrmLead) (bool, error) {
			if !leadInScope(l, cmd.OnlyAssignedTo) {
				return false, crmlead.ErrNotFound
			}
			if l.Stage().IsTerminal() {
				return false, crmlead.ErrTerminal
			}
			return false, nil // a guard, nothing to persist
		}); err != nil {
			return err
		}
		c, err := calllog.NewWithCallback(
			h.newCallID(),
			cmd.TenantID,
			cmd.LeadID,
			cmd.Outcome,
			cmd.Notes,
			cmd.LoggedByMembershipID,
			h.now(),
			cmd.CallbackWindow,
		)
		if err != nil {
			return fmt.Errorf("crm log_call: factory: %w", err)
		}
		if err := h.calls.Add(ctx, c); err != nil {
			return fmt.Errorf("crm log_call: persist: %w", err)
		}
		callID = c.ID()
		return nil
	})
	switch {
	case errors.Is(err, crmlead.ErrNotFound):
		return LogCallResult{}, ErrLeadNotFound
	case errors.Is(err, crmlead.ErrTerminal):
		return LogCallResult{}, ErrLeadTerminal
	case err != nil:
		return LogCallResult{}, err
	}
	return LogCallResult{CallID: callID}, nil
}
