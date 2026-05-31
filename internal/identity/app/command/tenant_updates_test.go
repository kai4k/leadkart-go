package command_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership/membershiptest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant/tenanttest"
)

// membershipsWithSuperAdmin overrides the shared fake's
// HasActiveSuperAdmin (which always conservatively returns false) so
// tenant-lifecycle tests can exercise the platform-tenant-deletion
// guard from [tenant_lifecycle.go ensureNotPlatformTenant]. `hasErr`
// triggers the generic-error wrap path; `hasFlag` triggers the
// ErrPlatformTenantUndeletable arm.
type membershipsWithSuperAdmin struct {
	*membershiptest.FakeRepository
	hasFlag bool
	hasErr  error
}

func (m *membershipsWithSuperAdmin) HasActiveSuperAdmin(_ context.Context, _ tenant.ID) (bool, error) {
	if m.hasErr != nil {
		return false, m.hasErr
	}
	return m.hasFlag, nil
}

// platformTenantMembers returns a membership repo that reports
// HasActiveSuperAdmin=true — i.e. the supplied tenant holds a
// SuperAdmin role-holder + is therefore the platform tenant.
func platformTenantMembers() *membershipsWithSuperAdmin {
	return &membershipsWithSuperAdmin{
		FakeRepository: membershiptest.NewFakeRepository(),
		hasFlag:        true,
	}
}

// boomingPlatformGuard returns a membership repo whose
// HasActiveSuperAdmin call fails. Exercises the wrapped
// `"platform-tenant guard: %w"` branch.
func boomingPlatformGuard(err error) *membershipsWithSuperAdmin {
	return &membershipsWithSuperAdmin{
		FakeRepository: membershiptest.NewFakeRepository(),
		hasErr:         err,
	}
}

// _ = membership.ID compile-time guard.
var _ = membership.ID("")

// The tenant-side fake lives in internal/identity/domain/tenant/tenanttest/
// per TDL Wild Workouts canon — co-located with the aggregate it
// fakes. newFakeTenantRepo is preserved as a one-line alias so existing
// tests don't need rewriting.
func newFakeTenantRepo() *tenanttest.FakeRepository { return tenanttest.NewFakeRepository() }

func newTenant(t *testing.T) *tenant.Tenant {
	t.Helper()

	tenantSlug, err := slug.New("acme-pharma")
	if err != nil {
		t.Fatalf("slug.New: %v", err)
	}
	addr, _ := email.New("admin@acme.test")
	tn, err := tenant.New(
		tenant.ID("11111111-1111-1111-1111-111111111111"),
		tenantSlug, "Acme Pharma Pvt Ltd", "Acme Pharma", addr,
		testNow,
	)
	if err != nil {
		t.Fatalf("tenant.Register: %v", err)
	}
	tn.PullEvents() // discard registration event for cleaner per-test assertions
	return tn
}

func TestUpdateTenantProfile_Succeeds(t *testing.T) {
	t.Parallel()
	repo := newFakeTenantRepo()
	tn := newTenant(t)
	_ = repo.Add(t.Context(), tn) // arch-test:ignore-err - test fixture setup

	h := command.NewUpdateTenantProfileHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.UpdateTenantProfileCommand{
		TenantID:    tn.ID(),
		LegalName:   "Acme Pharma Limited",
		DisplayName: "Acme",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if tn.LegalName() != "Acme Pharma Limited" {
		t.Errorf("LegalName = %q", tn.LegalName())
	}
	if tn.DisplayName() != "Acme" {
		t.Errorf("DisplayName = %q", tn.DisplayName())
	}
}

func TestUpdateTenantProfile_NotFound(t *testing.T) {
	t.Parallel()
	repo := newFakeTenantRepo()
	h := command.NewUpdateTenantProfileHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.UpdateTenantProfileCommand{
		TenantID:    tenant.ID("99999999-9999-9999-9999-999999999999"),
		LegalName:   "x",
		DisplayName: "x",
	})
	if !errors.Is(err, tenant.ErrNotFound) {
		t.Fatalf("err = %v, want tenant.ErrNotFound", err)
	}
}

func TestUpdateTenantProfile_AggregateRejection_WrapsErrInvalid(t *testing.T) {
	t.Parallel()
	repo := newFakeTenantRepo()
	tn := newTenant(t)
	_ = repo.Add(t.Context(), tn) // arch-test:ignore-err - test fixture setup
	h := command.NewUpdateTenantProfileHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.UpdateTenantProfileCommand{
		TenantID:    tn.ID(),
		LegalName:   "",
		DisplayName: "x",
	})
	if !errors.Is(err, tenant.ErrInvalid) {
		t.Fatalf("err = %v, want wraps tenant.ErrInvalid", err)
	}
}

func TestUpdateTenantStatutory_RejectsBadGST(t *testing.T) {
	t.Parallel()
	repo := newFakeTenantRepo()
	tn := newTenant(t)
	_ = repo.Add(t.Context(), tn) // arch-test:ignore-err - test fixture setup
	h := command.NewUpdateTenantStatutoryHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.UpdateTenantStatutoryCommand{
		TenantID:  tn.ID(),
		GSTNumber: "not-a-gst",
	})
	if err == nil || !strings.Contains(err.Error(), "gst") {
		t.Fatalf("err = %v, want gst rejection", err)
	}
}

func TestUpdateTenantSettings_Succeeds(t *testing.T) {
	t.Parallel()
	repo := newFakeTenantRepo()
	tn := newTenant(t)
	_ = repo.Add(t.Context(), tn) // arch-test:ignore-err - test fixture setup
	h := command.NewUpdateTenantSettingsHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.UpdateTenantSettingsCommand{
		TenantID:          tn.ID(),
		MinLength:         12,
		RequireUppercase:  true,
		RequireLowercase:  true,
		RequireDigit:      true,
		RequireSymbol:     false,
		MaxFailedAttempts: 5,
		LockoutMinutes:    30,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := tn.Settings().PasswordPolicy().MinLength(); got != 12 {
		t.Errorf("MinLength = %d, want 12", got)
	}
}

func TestSuspendTenant_RequiresReason(t *testing.T) {
	t.Parallel()
	repo := newFakeTenantRepo()
	tn := newTenant(t)
	if err := tn.Activate(testNow); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	tn.PullEvents()
	_ = repo.Add(t.Context(), tn) // arch-test:ignore-err - test fixture setup

	h := command.NewSuspendTenantHandler(repo, newFakeMembershipRepo(), func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.SuspendTenantCommand{TenantID: tn.ID()})
	if !errors.Is(err, tenant.ErrInvalid) {
		t.Fatalf("err = %v, want wraps tenant.ErrInvalid (empty reason)", err)
	}
}

func TestSuspendTenant_Succeeds(t *testing.T) {
	t.Parallel()
	repo := newFakeTenantRepo()
	tn := newTenant(t)
	if err := tn.Activate(testNow); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	tn.PullEvents()
	_ = repo.Add(t.Context(), tn) // arch-test:ignore-err - test fixture setup
	h := command.NewSuspendTenantHandler(repo, newFakeMembershipRepo(), func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.SuspendTenantCommand{
		TenantID: tn.ID(),
		Reason:   "billing-overdue-30d",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if tn.Status() != tenant.StatusSuspended {
		t.Errorf("Status = %v, want Suspended", tn.Status())
	}
}

func TestActivateTenant_Idempotent(t *testing.T) {
	t.Parallel()
	repo := newFakeTenantRepo()
	tn := newTenant(t)
	_ = tn.Activate(testNow) // arch-test:ignore-err - test fixture setup
	tn.PullEvents()
	_ = repo.Add(t.Context(), tn) // arch-test:ignore-err - test fixture setup
	h := command.NewActivateTenantHandler(repo, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.ActivateTenantCommand{TenantID: tn.ID()}); err != nil {
		t.Fatalf("Handle (already active): %v", err)
	}
}

func TestMarkTenantForDeletion_HappyPath(t *testing.T) {
	t.Parallel()
	repo := newFakeTenantRepo()
	tn := newTenant(t)
	_ = tn.Activate(testNow) // arch-test:ignore-err - test fixture setup
	tn.PullEvents()
	_ = repo.Add(t.Context(), tn) // arch-test:ignore-err - test fixture setup

	h := command.NewMarkTenantForDeletionHandler(repo, newFakeMembershipRepo(), func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.MarkTenantForDeletionCommand{
		TenantID: tn.ID(),
		Reason:   "operator: tenant-requested-closure",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if tn.Status() != tenant.StatusPendingDeletion {
		t.Errorf("Status = %v, want PendingDeletion", tn.Status())
	}
}

func TestRestoreTenant_FromPendingDeletion(t *testing.T) {
	t.Parallel()
	repo := newFakeTenantRepo()
	tn := newTenant(t)
	_ = tn.Activate(testNow)                       // arch-test:ignore-err - test fixture setup
	_ = tn.MarkForDeletion("test-reason", testNow) // arch-test:ignore-err - test fixture setup
	tn.PullEvents()
	_ = repo.Add(t.Context(), tn) // arch-test:ignore-err - test fixture setup

	h := command.NewRestoreTenantHandler(repo, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.RestoreTenantCommand{TenantID: tn.ID()}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if tn.Status() != tenant.StatusActive {
		t.Errorf("Status = %v, want Active", tn.Status())
	}
}

func TestRestoreTenant_NotFound(t *testing.T) {
	t.Parallel()
	repo := newFakeTenantRepo()
	h := command.NewRestoreTenantHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.RestoreTenantCommand{
		TenantID: tenant.ID("99999999-9999-9999-9999-999999999999"),
	})
	if !errors.Is(err, command.ErrTenantNotFound) {
		t.Fatalf("err = %v, want ErrTenantNotFound", err)
	}
}

// ----- Lifecycle handlers: input + guard + state-machine coverage --------
//
// Table-driven for the trivial input-validation arms; named tests for
// the higher-signal guard / aggregate-rejection / propagation branches.

// TestTenantLifecycle_InputRejections — every lifecycle handler rejects
// zero TenantID at the boundary BEFORE touching any repo. Mirrors the
// guard pattern in change_password / create_user.
func TestTenantLifecycle_InputRejections(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		fn   func() error
	}{
		{"Suspend", func() error {
			return command.NewSuspendTenantHandler(newFakeTenantRepo(), newFakeMembershipRepo(), func() time.Time { return testNow }).
				Handle(t.Context(), command.SuspendTenantCommand{Reason: "x"})
		}},
		{"Activate", func() error {
			return command.NewActivateTenantHandler(newFakeTenantRepo(), func() time.Time { return testNow }).
				Handle(t.Context(), command.ActivateTenantCommand{})
		}},
		{"MarkForDeletion", func() error {
			return command.NewMarkTenantForDeletionHandler(newFakeTenantRepo(), newFakeMembershipRepo(), func() time.Time { return testNow }).
				Handle(t.Context(), command.MarkTenantForDeletionCommand{Reason: "x"})
		}},
		{"Restore", func() error {
			return command.NewRestoreTenantHandler(newFakeTenantRepo(), func() time.Time { return testNow }).
				Handle(t.Context(), command.RestoreTenantCommand{})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := c.fn()
			if err == nil {
				t.Fatal("expected error for zero TenantID, got nil")
			}
		})
	}
}

// TestSuspendTenant_PlatformTenant_RejectsBeforeTransition exercises
// the deletion-guard arm: if the tenant holds an active SuperAdmin
// role-holder, Suspend short-circuits with ErrPlatformTenantUndeletable
// BEFORE any aggregate transition fires.
func TestSuspendTenant_PlatformTenant_RejectsBeforeTransition(t *testing.T) {
	t.Parallel()
	repo := newFakeTenantRepo()
	tn := newTenant(t)
	_ = tn.Activate(testNow) // arch-test:ignore-err - test fixture setup
	tn.PullEvents()
	_ = repo.Add(t.Context(), tn) // arch-test:ignore-err - test fixture setup

	h := command.NewSuspendTenantHandler(repo, platformTenantMembers(), func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.SuspendTenantCommand{
		TenantID: tn.ID(),
		Reason:   "ops-test",
	})
	if !errors.Is(err, command.ErrPlatformTenantUndeletable) {
		t.Fatalf("err = %v, want ErrPlatformTenantUndeletable", err)
	}
	// Defense-in-depth: short-circuit means the aggregate stays Active.
	if tn.Status() != tenant.StatusActive {
		t.Errorf("Status = %v, want Active (guard must short-circuit before transition)", tn.Status())
	}
}

// TestSuspendTenant_GuardErrorWrapped exercises the wrapped-error path:
// a generic HasActiveSuperAdmin failure surfaces as
// `"platform-tenant guard: %w"` — operators MUST see the underlying
// driver error, not a silently-swallowed guard.
func TestSuspendTenant_GuardErrorWrapped(t *testing.T) {
	t.Parallel()
	repo := newFakeTenantRepo()
	tn := newTenant(t)
	_ = tn.Activate(testNow) // arch-test:ignore-err - test fixture setup
	tn.PullEvents()
	_ = repo.Add(t.Context(), tn) // arch-test:ignore-err - test fixture setup

	h := command.NewSuspendTenantHandler(repo, boomingPlatformGuard(errBoom), func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.SuspendTenantCommand{
		TenantID: tn.ID(),
		Reason:   "ops-test",
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want chain includes errBoom", err)
	}
	if errors.Is(err, command.ErrPlatformTenantUndeletable) {
		t.Fatal("guard driver error must NOT collapse to ErrPlatformTenantUndeletable")
	}
}

// TestSuspendTenant_AggregateRejection_WrongState exercises the
// wrap-around-aggregate-error branch. Aggregate refuses Suspend on
// PendingDeletion → handler wraps with "suspend_tenant: %w".
func TestSuspendTenant_AggregateRejection_WrongState(t *testing.T) {
	t.Parallel()
	repo := newFakeTenantRepo()
	tn := newTenant(t)
	_ = tn.Activate(testNow)                         // arch-test:ignore-err
	_ = tn.MarkForDeletion("ops-test-init", testNow) // arch-test:ignore-err
	tn.PullEvents()
	_ = repo.Add(t.Context(), tn) // arch-test:ignore-err

	h := command.NewSuspendTenantHandler(repo, newFakeMembershipRepo(), func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.SuspendTenantCommand{
		TenantID: tn.ID(),
		Reason:   "billing-overdue",
	})
	if !errors.Is(err, tenant.ErrInvalid) {
		t.Fatalf("err = %v, want wraps tenant.ErrInvalid (pending deletion)", err)
	}
}

// TestActivateTenant_AggregateRejection_FromDeleted — Deleted is
// terminal; Activate refuses with wrapped ErrInvalid.
func TestActivateTenant_AggregateRejection_FromDeleted(t *testing.T) {
	t.Parallel()
	repo := newFakeTenantRepo()
	tn := newTenant(t)
	_ = tn.Activate(testNow)                         // arch-test:ignore-err
	_ = tn.MarkForDeletion("ops-test-init", testNow) // arch-test:ignore-err
	_ = tn.HardDelete(testNow)                       // arch-test:ignore-err
	tn.PullEvents()
	_ = repo.Add(t.Context(), tn) // arch-test:ignore-err

	h := command.NewActivateTenantHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.ActivateTenantCommand{TenantID: tn.ID()})
	if !errors.Is(err, tenant.ErrInvalid) {
		t.Fatalf("err = %v, want wraps tenant.ErrInvalid (terminal)", err)
	}
}

// TestActivateTenant_NotFound — the not-found sentinel propagation.
func TestActivateTenant_NotFound(t *testing.T) {
	t.Parallel()
	repo := newFakeTenantRepo()
	h := command.NewActivateTenantHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.ActivateTenantCommand{
		TenantID: tenant.ID("99999999-9999-9999-9999-999999999999"),
	})
	if !errors.Is(err, command.ErrTenantNotFound) {
		t.Fatalf("err = %v, want ErrTenantNotFound", err)
	}
}

// TestMarkTenantForDeletion_PlatformTenant_RejectsBeforeTransition
// — same guard as Suspend; refuses to mark the platform tenant.
func TestMarkTenantForDeletion_PlatformTenant_RejectsBeforeTransition(t *testing.T) {
	t.Parallel()
	repo := newFakeTenantRepo()
	tn := newTenant(t)
	_ = tn.Activate(testNow) // arch-test:ignore-err
	tn.PullEvents()
	_ = repo.Add(t.Context(), tn) // arch-test:ignore-err

	h := command.NewMarkTenantForDeletionHandler(repo, platformTenantMembers(), func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.MarkTenantForDeletionCommand{
		TenantID: tn.ID(),
		Reason:   "ops: lifecycle test",
	})
	if !errors.Is(err, command.ErrPlatformTenantUndeletable) {
		t.Fatalf("err = %v, want ErrPlatformTenantUndeletable", err)
	}
	if tn.Status() != tenant.StatusActive {
		t.Errorf("Status = %v, want Active (guard must short-circuit)", tn.Status())
	}
}

// TestMarkTenantForDeletion_GuardErrorWrapped — generic HasActiveSuperAdmin
// failure surfaces as the wrapped guard error.
func TestMarkTenantForDeletion_GuardErrorWrapped(t *testing.T) {
	t.Parallel()
	repo := newFakeTenantRepo()
	tn := newTenant(t)
	_ = tn.Activate(testNow) // arch-test:ignore-err
	tn.PullEvents()
	_ = repo.Add(t.Context(), tn) // arch-test:ignore-err

	h := command.NewMarkTenantForDeletionHandler(repo, boomingPlatformGuard(errBoom), func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.MarkTenantForDeletionCommand{
		TenantID: tn.ID(),
		Reason:   "ops: lifecycle test",
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want chain includes errBoom", err)
	}
}

// TestMarkTenantForDeletion_ReasonMismatch_OnAlreadyPending — once a
// tenant is PendingDeletion, MarkForDeletion is idempotent ONLY when
// the supplied reason matches the existing schedule's reason. Any
// other reason is rejected with wrapped ErrInvalid (audit integrity).
func TestMarkTenantForDeletion_ReasonMismatch_OnAlreadyPending(t *testing.T) {
	t.Parallel()
	repo := newFakeTenantRepo()
	tn := newTenant(t)
	_ = tn.Activate(testNow)                                   // arch-test:ignore-err
	_ = tn.MarkForDeletion("original: ops-initiated", testNow) // arch-test:ignore-err
	tn.PullEvents()
	_ = repo.Add(t.Context(), tn) // arch-test:ignore-err

	h := command.NewMarkTenantForDeletionHandler(repo, newFakeMembershipRepo(), func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.MarkTenantForDeletionCommand{
		TenantID: tn.ID(),
		Reason:   "different: customer-requested",
	})
	if !errors.Is(err, tenant.ErrInvalid) {
		t.Fatalf("err = %v, want wraps tenant.ErrInvalid (reason mismatch)", err)
	}
}

// TestRestoreTenant_AggregateRejection_FromSuspended — Restore is legal
// only from PendingDeletion. Calling on a Suspended tenant hits the
// aggregate's default arm + wraps ErrInvalid. (Already-Active is
// idempotent per the aggregate doc, so that's NOT a rejection path.)
func TestRestoreTenant_AggregateRejection_FromSuspended(t *testing.T) {
	t.Parallel()
	repo := newFakeTenantRepo()
	tn := newTenant(t)
	_ = tn.Activate(testNow)                    // arch-test:ignore-err
	_ = tn.Suspend("ops-test-suspend", testNow) // arch-test:ignore-err
	tn.PullEvents()
	_ = repo.Add(t.Context(), tn) // arch-test:ignore-err

	h := command.NewRestoreTenantHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.RestoreTenantCommand{TenantID: tn.ID()})
	if !errors.Is(err, tenant.ErrInvalid) {
		t.Fatalf("err = %v, want wraps tenant.ErrInvalid (not pending deletion)", err)
	}
}

// TestSuspendTenant_NotFound + TestMarkForDeletion_NotFound — propagate
// ErrTenantNotFound sentinel from the repo, not raw tenant.ErrNotFound.
func TestSuspendTenant_NotFound(t *testing.T) {
	t.Parallel()
	repo := newFakeTenantRepo()
	h := command.NewSuspendTenantHandler(repo, newFakeMembershipRepo(), func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.SuspendTenantCommand{
		TenantID: tenant.ID("99999999-9999-9999-9999-999999999999"),
		Reason:   "ops",
	})
	if !errors.Is(err, command.ErrTenantNotFound) {
		t.Fatalf("err = %v, want ErrTenantNotFound", err)
	}
}

func TestMarkTenantForDeletion_NotFound(t *testing.T) {
	t.Parallel()
	repo := newFakeTenantRepo()
	h := command.NewMarkTenantForDeletionHandler(repo, newFakeMembershipRepo(), func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.MarkTenantForDeletionCommand{
		TenantID: tenant.ID("99999999-9999-9999-9999-999999999999"),
		Reason:   "ops: test",
	})
	if !errors.Is(err, command.ErrTenantNotFound) {
		t.Fatalf("err = %v, want ErrTenantNotFound", err)
	}
}
