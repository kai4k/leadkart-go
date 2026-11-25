package ports

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant/tenanttest"
)

// G2p: the platform tenant's id is the id of the tenant with slug "platform", read from the store;
// absent until that tenant exists, then cached.
func TestPlatformTenantResolver_ReadsTheTenantWithThePlatformSlug(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	repo := tenanttest.NewFakeRepository()
	add := func(slugText, id string) {
		t.Helper()
		sl, err := slug.New(slugText)
		if err != nil {
			t.Fatal(err)
		}
		admin, err := email.New("admin@" + slugText + ".test")
		if err != nil {
			t.Fatal(err)
		}
		tn, err := tenant.New(tenant.ID(id), sl, slugText+" Pvt Ltd", slugText, admin, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.Add(t.Context(), tn); err != nil {
			t.Fatal(err)
		}
	}
	resolve := NewPlatformTenantResolver(slog.New(slog.NewTextHandler(io.Discard, nil)), app.Application{Queries: app.Queries{GetTenantBySlug: query.NewGetTenantBySlugHandler(repo)}})

	add("acme", "11111111-1111-1111-1111-111111111111")
	if got := resolve(t.Context()); got != "" {
		t.Fatalf("no platform tenant yet: got %q, want empty", got)
	}
	add("platform", "22222222-2222-2222-2222-222222222222")
	if got := resolve(t.Context()); got != "22222222-2222-2222-2222-222222222222" {
		t.Fatalf("platform tenant id = %q", got)
	}
}

// failingTenants answers every slug lookup with a fixed error.
type failingTenants struct {
	tenant.Repository
	err error
}

func (f failingTenants) GetBySlug(context.Context, slug.Slug) (*tenant.Tenant, error) {
	return nil, f.err
}

// G2q i1: a lookup failure is logged once per call, with the error and nothing else; an ordinary "not
// found" (platform tenant not registered yet) stays silent. Either way the resolver answers "".
func TestPlatformTenantResolver_LogsALookupFailure(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err      error
		wantLogs int
	}{
		"database error": {err: errors.New("connection refused"), wantLogs: 1},
		"not found":      {err: tenant.ErrNotFound, wantLogs: 0},
	} {
		var buf bytes.Buffer
		log := slog.New(slog.NewTextHandler(&buf, nil))
		resolve := NewPlatformTenantResolver(log, app.Application{Queries: app.Queries{GetTenantBySlug: query.NewGetTenantBySlugHandler(failingTenants{err: tc.err})}})
		if got := resolve(t.Context()); got != "" {
			t.Errorf("%s: id = %q, want empty", name, got)
		}
		if n := strings.Count(buf.String(), "platform tenant lookup failed"); n != tc.wantLogs {
			t.Errorf("%s: %d log lines, want %d (%q)", name, n, tc.wantLogs, buf.String())
		}
	}
}
