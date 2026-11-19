package ports_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/impersonation"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// An operator who has lost platform.impersonation.manage can still close the
// session they opened; nobody can close someone else's, and a principal that is
// not a platform operator cannot reach the route at all.
func TestEndImpersonationSession_OwnSessionNeedsNoPermission(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	operator := func(subject string, perms ...string) *jwt.Claims {
		c := azClaims(perms...)
		c.TenantSlug, c.IsPlatform, c.Subject = authn.PlatformTenantSlug, true, subject
		return c
	}
	const owner, other = "0190a000-0000-7000-8000-0000000000a1", "0190a000-0000-7000-8000-0000000000a2"
	setup := func(t *testing.T) (app.Application, *adapters.ImpersonationInMemoryStore, string) {
		t.Helper()
		store := adapters.NewImpersonationInMemoryStore(func() time.Time { return now })
		sess, err := impersonation.NewSession(owner, "22222222-2222-4222-8222-222222222222", "investigate-customer-issue-ticket-1234", 30*time.Minute, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Put(t.Context(), sess); err != nil {
			t.Fatal(err)
		}
		var a app.Application
		a.Commands.EndImpersonationSession = command.NewEndImpersonationSessionHandler(store)
		return a, store, sess.ID()
	}
	exists := func(t *testing.T, store *adapters.ImpersonationInMemoryStore, id string) bool {
		t.Helper()
		_, err := store.Get(t.Context(), id)
		return err == nil
	}

	t.Run("owner without the permission ends it", func(t *testing.T) {
		t.Parallel()
		a, store, id := setup(t)
		if rec := azServeApp(t, a, operator(owner), "DELETE", "/api/v1/platform/impersonation/sessions/"+id, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("status %d %s, want 204", rec.Code, rec.Body.String())
		}
		if exists(t, store, id) {
			t.Error("session survived its owner ending it")
		}
	})
	t.Run("owner with the permission ends it", func(t *testing.T) {
		t.Parallel()
		a, store, id := setup(t)
		if rec := azServeApp(t, a, operator(owner, permission.IdentityPermissions.Platform.ImpersonationManage), "DELETE", "/api/v1/platform/impersonation/sessions/"+id, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("status %d, want 204", rec.Code)
		}
		if exists(t, store, id) {
			t.Error("session survived")
		}
	})
	t.Run("another operator cannot end it", func(t *testing.T) {
		t.Parallel()
		a, store, id := setup(t)
		for name, c := range map[string]*jwt.Claims{
			"no permission":   operator(other),
			"with permission": operator(other, permission.IdentityPermissions.Platform.ImpersonationManage),
		} {
			if rec := azServeApp(t, a, c, "DELETE", "/api/v1/platform/impersonation/sessions/"+id, ""); rec.Code != http.StatusNoContent {
				t.Errorf("%s: status %d, want the idempotent 204", name, rec.Code)
			}
			if !exists(t, store, id) {
				t.Fatalf("%s: another operator ended the session", name)
			}
		}
	})
	t.Run("a tenant principal cannot reach the route", func(t *testing.T) {
		t.Parallel()
		a, store, id := setup(t)
		tenantPrincipal := azClaims(permission.IdentityPermissions.Meta.TenantAdmin, permission.IdentityPermissions.Platform.ImpersonationManage)
		tenantPrincipal.Subject = owner
		if rec := azServeApp(t, a, tenantPrincipal, "DELETE", "/api/v1/platform/impersonation/sessions/"+id, ""); rec.Code != http.StatusForbidden {
			t.Errorf("status %d, want 403", rec.Code)
		}
		if !exists(t, store, id) {
			t.Error("a tenant principal ended a platform session")
		}
	})
}
