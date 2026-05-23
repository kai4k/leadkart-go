package actclaim_test

import (
	"context"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/actclaim"
)

func TestClaim_IsZero(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		c    actclaim.Claim
		want bool
	}{
		{"empty", actclaim.Claim{}, true},
		{"only operator", actclaim.Claim{OperatorID: "op"}, false},
		{"only session", actclaim.Claim{SessionID: "ses"}, false},
		{"only reason", actclaim.Claim{Reason: "why"}, false},
		{"full", actclaim.Claim{OperatorID: "op", SessionID: "ses", Reason: "why"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.c.IsZero(); got != tc.want {
				t.Errorf("IsZero() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWithContext_RoundTrip(t *testing.T) {
	t.Parallel()
	c := actclaim.Claim{OperatorID: "op", SessionID: "ses", Reason: "why"}
	ctx := actclaim.WithContext(context.Background(), c)

	got, ok := actclaim.FromContext(ctx)
	if !ok {
		t.Fatal("FromContext = false, want true")
	}
	if got != c {
		t.Errorf("round-trip = %+v, want %+v", got, c)
	}
}

func TestWithContext_Zero_IsNoOp(t *testing.T) {
	t.Parallel()
	parent := context.Background()
	ctx := actclaim.WithContext(parent, actclaim.Claim{})
	// Same parent — zero claim is a no-op.
	if ctx != parent {
		t.Error("expected same ctx (zero claim is a no-op)")
	}
	if _, ok := actclaim.FromContext(ctx); ok {
		t.Error("FromContext should return false for un-tagged ctx")
	}
}

func TestFromContext_Absent(t *testing.T) {
	t.Parallel()
	got, ok := actclaim.FromContext(context.Background())
	if ok {
		t.Errorf("ok = true on bare ctx; got %+v", got)
	}
	if !got.IsZero() {
		t.Errorf("got = %+v, want zero", got)
	}
}
