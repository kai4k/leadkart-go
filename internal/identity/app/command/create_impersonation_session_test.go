package command_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// impSigningKey is the deterministic HS256 secret used by impersonation
// handler tests. 32+ bytes per RFC 7518 §3.2.
var impSigningKey = jwt.SigningKey{
	KeyID:  "imp-test-key",
	Secret: []byte("test-secret-bytes-test-secret-bytes-32+"),
}

func newImpersonationIssuer(t *testing.T) *jwt.Issuer {
	t.Helper()
	iss, err := jwt.NewIssuer(impSigningKey, nil, func() time.Time { return testNow })
	if err != nil {
		t.Fatalf("jwt.NewIssuer: %v", err)
	}
	return iss
}

// TestNewCreateImpersonationSessionHandler_PanicsOnNilDeps locks the
// wiring contract: each of the four deps (store, tenants, issuer,
// now) is required at composition time. Now is the only optional
// dep (nil → time.Now).
func TestNewCreateImpersonationSessionHandler_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()
	tenants := newFakeTenantRepo()
	store := adapters.NewImpersonationInMemoryStore(func() time.Time { return testNow })
	issuer := newImpersonationIssuer(t)

	cases := []struct {
		name string
		fn   func()
	}{
		{
			name: "nil store",
			fn: func() {
				_ = command.NewCreateImpersonationSessionHandler(nil, tenants, issuer, func() time.Time { return testNow })
			},
		},
		{
			name: "nil tenants",
			fn: func() {
				_ = command.NewCreateImpersonationSessionHandler(store, nil, issuer, func() time.Time { return testNow })
			},
		},
		{
			name: "nil issuer",
			fn: func() {
				_ = command.NewCreateImpersonationSessionHandler(store, tenants, nil, func() time.Time { return testNow })
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Error("expected panic on nil dep")
				}
			}()
			c.fn()
		})
	}
}

// TestCreateImpersonationSession_RejectsMissingInputs verifies the
// input-shape guards before any store / issuer call. Per ADR 0045 +
// session VO: operator id, security stamp, and target tenant id are
// all mandatory.
func TestCreateImpersonationSession_RejectsMissingInputs(t *testing.T) {
	t.Parallel()
	tenants := newFakeTenantRepo()
	store := adapters.NewImpersonationInMemoryStore(func() time.Time { return testNow })
	issuer := newImpersonationIssuer(t)
	h := command.NewCreateImpersonationSessionHandler(store, tenants, issuer, func() time.Time { return testNow })

	cases := []struct {
		name string
		cmd  command.CreateImpersonationSessionCommand
	}{
		{
			name: "missing operator id",
			cmd: command.CreateImpersonationSessionCommand{
				OperatorSecurityStamp: "stamp",
				TargetTenantID:        tenant.ID("11111111-1111-1111-1111-111111111111"),
				Reason:                "operator-investigating-issue",
			},
		},
		{
			name: "missing security stamp",
			cmd: command.CreateImpersonationSessionCommand{
				OperatorID:     "op-1",
				TargetTenantID: tenant.ID("11111111-1111-1111-1111-111111111111"),
				Reason:         "operator-investigating-issue",
			},
		},
		{
			name: "missing target tenant",
			cmd: command.CreateImpersonationSessionCommand{
				OperatorID:            "op-1",
				OperatorSecurityStamp: "stamp",
				Reason:                "operator-investigating-issue",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := h.Handle(t.Context(), c.cmd)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

// TestCreateImpersonationSession_TargetTenantNotFound_ReturnsErrImpersonationTargetMissing
// proves the resolve-target gate fires BEFORE the session record is
// created. The HTTP layer relies on the typed sentinel for 404 mapping
// per ADR 0045 + Russ Cox "Working with Errors in Go 1.13".
func TestCreateImpersonationSession_TargetTenantNotFound_ReturnsErrImpersonationTargetMissing(t *testing.T) {
	t.Parallel()
	tenants := newFakeTenantRepo() // empty — no tenant seeded
	store := adapters.NewImpersonationInMemoryStore(func() time.Time { return testNow })
	issuer := newImpersonationIssuer(t)
	h := command.NewCreateImpersonationSessionHandler(store, tenants, issuer, func() time.Time { return testNow })

	_, err := h.Handle(t.Context(), command.CreateImpersonationSessionCommand{
		OperatorID:            "op-1",
		OperatorSecurityStamp: "stamp",
		TargetTenantID:        tenant.ID("99999999-9999-9999-9999-999999999999"),
		Reason:                "operator-investigating-issue",
	})
	if !errors.Is(err, command.ErrImpersonationTargetMissing) {
		t.Fatalf("err = %v, want ErrImpersonationTargetMissing", err)
	}
}

// TestCreateImpersonationSession_HappyPath_MintsScopedToken exercises
// the full create flow: persist session record, mint scoped JWT, return
// access_token + expiry. Per ADR 0045 the scoped token is DOWNGRADED
// (is_platform=false, is_super_user=false) regardless of the operator's
// normal claims.
func TestCreateImpersonationSession_HappyPath_MintsScopedToken(t *testing.T) {
	t.Parallel()
	tenants := newFakeTenantRepo()
	tn := newTenant(t)
	_ = tenants.Add(t.Context(), tn)

	store := adapters.NewImpersonationInMemoryStore(func() time.Time { return testNow })
	issuer := newImpersonationIssuer(t)
	h := command.NewCreateImpersonationSessionHandler(store, tenants, issuer, func() time.Time { return testNow })

	res, err := h.Handle(t.Context(), command.CreateImpersonationSessionCommand{
		OperatorID:            "11111111-1111-1111-1111-111111111111",
		OperatorSecurityStamp: "stamp-current",
		TargetTenantID:        tn.ID(),
		Reason:                "investigate-stuck-onboarding-ticket",
		Duration:              30 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.SessionID == "" {
		t.Error("SessionID empty — store.Put should have persisted a session")
	}
	if res.AccessToken == "" {
		t.Error("AccessToken empty — issuer.Issue should have minted a token")
	}
	// Token is a compact JWS — three dot-separated base64url segments.
	if got := strings.Count(res.AccessToken, "."); got != 2 {
		t.Errorf("AccessToken dot count = %d, want 2 (header.payload.signature)", got)
	}
	if !res.AccessTokenExpiresAtUTC.Equal(res.ExpiresAtUTC) {
		t.Errorf("AccessTokenExpiresAtUTC = %v, want equal to ExpiresAtUTC %v", res.AccessTokenExpiresAtUTC, res.ExpiresAtUTC)
	}

	// Verify the token's claims to lock the DOWNGRADED-scope invariant.
	claims, err := issuer.Verify(res.AccessToken)
	if err != nil {
		t.Fatalf("issuer.Verify: %v", err)
	}
	if claims.IsPlatform {
		t.Error("scoped impersonation token has is_platform=true — must be downgraded per ADR 0045")
	}
	if claims.IsSuperUser {
		t.Error("scoped impersonation token has is_super_user=true — must be downgraded per ADR 0045")
	}
	if !claims.IsImpersonation() {
		t.Error("scoped impersonation token missing act claim — required per RFC 8693 §4.1")
	}
}
