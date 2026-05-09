package command_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	commonemail "github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/clock"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// fakeTenantRepo is the minimum [tenant.Repository] surface the
// tenant-update / lifecycle handlers exercise.
type fakeTenantRepo struct {
	tenants map[tenant.ID]*tenant.Tenant
}

func newFakeTenantRepo() *fakeTenantRepo {
	return &fakeTenantRepo{tenants: make(map[tenant.ID]*tenant.Tenant)}
}

func (r *fakeTenantRepo) Add(_ context.Context, t *tenant.Tenant) error {
	r.tenants[t.ID()] = t
	return nil
}
func (r *fakeTenantRepo) UpdateByID(_ context.Context, id tenant.ID, fn func(*tenant.Tenant) (bool, error)) error {
	t, ok := r.tenants[id]
	if !ok {
		return tenant.ErrNotFound
	}
	commit, err := fn(t)
	if err != nil {
		return err
	}
	_ = commit
	return nil
}
func (r *fakeTenantRepo) GetByID(_ context.Context, id tenant.ID) (*tenant.Tenant, error) {
	t, ok := r.tenants[id]
	if !ok {
		return nil, tenant.ErrNotFound
	}
	return t, nil
}
func (r *fakeTenantRepo) GetBySlug(_ context.Context, _ slug.Slug) (*tenant.Tenant, error) {
	return nil, tenant.ErrNotFound
}

func (r *fakeTenantRepo) ListAll(_ context.Context) ([]*tenant.Tenant, error) {
	out := make([]*tenant.Tenant, 0, len(r.tenants))
	for _, t := range r.tenants {
		out = append(out, t)
	}
	return out, nil
}

func (r *fakeTenantRepo) HardDeleteRow(_ context.Context, id tenant.ID) error {
	delete(r.tenants, id)
	return nil
}

var _ tenant.Repository = (*fakeTenantRepo)(nil)

func newTenant(t *testing.T) *tenant.Tenant {
	t.Helper()
	clock.Set(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC))
	t.Cleanup(clock.Reset)

	tenantSlug, err := slug.New("acme-pharma")
	if err != nil {
		t.Fatalf("slug.New: %v", err)
	}
	addr, _ := commonemail.New("admin@acme.test")
	tn, err := tenant.New(
		tenant.ID("11111111-1111-1111-1111-111111111111"),
		tenantSlug, "Acme Pharma Pvt Ltd", "Acme Pharma", addr,
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
	_ = repo.Add(t.Context(), tn)

	h := command.NewUpdateTenantProfileHandler(repo)
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
	h := command.NewUpdateTenantProfileHandler(repo)
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
	_ = repo.Add(t.Context(), tn)
	h := command.NewUpdateTenantProfileHandler(repo)
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
	_ = repo.Add(t.Context(), tn)
	h := command.NewUpdateTenantStatutoryHandler(repo)
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
	_ = repo.Add(t.Context(), tn)
	h := command.NewUpdateTenantSettingsHandler(repo)
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
	if err := tn.Activate(); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	tn.PullEvents()
	_ = repo.Add(t.Context(), tn)

	h := command.NewSuspendTenantHandler(repo, newFakeMembershipRepo())
	err := h.Handle(t.Context(), command.SuspendTenantCommand{TenantID: tn.ID()})
	if !errors.Is(err, tenant.ErrInvalid) {
		t.Fatalf("err = %v, want wraps tenant.ErrInvalid (empty reason)", err)
	}
}

func TestSuspendTenant_Succeeds(t *testing.T) {
	t.Parallel()
	repo := newFakeTenantRepo()
	tn := newTenant(t)
	if err := tn.Activate(); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	tn.PullEvents()
	_ = repo.Add(t.Context(), tn)
	h := command.NewSuspendTenantHandler(repo, newFakeMembershipRepo())
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
	_ = tn.Activate()
	tn.PullEvents()
	_ = repo.Add(t.Context(), tn)
	h := command.NewActivateTenantHandler(repo)
	if err := h.Handle(t.Context(), command.ActivateTenantCommand{TenantID: tn.ID()}); err != nil {
		t.Fatalf("Handle (already active): %v", err)
	}
}

func TestMarkTenantForDeletion_HappyPath(t *testing.T) {
	t.Parallel()
	repo := newFakeTenantRepo()
	tn := newTenant(t)
	_ = tn.Activate()
	tn.PullEvents()
	_ = repo.Add(t.Context(), tn)

	h := command.NewMarkTenantForDeletionHandler(repo, newFakeMembershipRepo())
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
	_ = tn.Activate()
	_ = tn.MarkForDeletion("test-reason")
	tn.PullEvents()
	_ = repo.Add(t.Context(), tn)

	h := command.NewRestoreTenantHandler(repo)
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
	h := command.NewRestoreTenantHandler(repo)
	err := h.Handle(t.Context(), command.RestoreTenantCommand{
		TenantID: tenant.ID("99999999-9999-9999-9999-999999999999"),
	})
	if !errors.Is(err, command.ErrTenantNotFound) {
		t.Fatalf("err = %v, want ErrTenantNotFound", err)
	}
}
