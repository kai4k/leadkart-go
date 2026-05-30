package actclaim_test

import (
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/actclaim"
)

// TestClaim_IsZero pins the all-empty semantic: a partial Claim is
// treated as present, an all-empty Claim as absent.
func TestClaim_IsZero(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		c    actclaim.Claim
		want bool
	}{
		{"all empty", actclaim.Claim{}, true},
		{"operator only", actclaim.Claim{OperatorID: "op"}, false},
		{"session only", actclaim.Claim{SessionID: "sess"}, false},
		{"reason only", actclaim.Claim{Reason: "why"}, false},
		{"all set", actclaim.Claim{OperatorID: "op", SessionID: "sess", Reason: "why"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.c.IsZero(); got != tt.want {
				t.Errorf("IsZero() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestWithContext_RoundTrip pins the ctx propagation: a non-zero Claim
// survives a WithContext → FromContext round-trip; a zero Claim is dropped
// (ctx unchanged, FromContext reports absent).
func TestWithContext_RoundTrip(t *testing.T) {
	t.Parallel()
	base := t.Context()

	if got := actclaim.WithContext(base, actclaim.Claim{}); got != base {
		t.Error("WithContext(zero) should return the original ctx unchanged")
	}
	if _, ok := actclaim.FromContext(base); ok {
		t.Error("FromContext(no claim) should report absent")
	}

	want := actclaim.Claim{OperatorID: "op-1", SessionID: "sess-1", Reason: "debugging"}
	ctx := actclaim.WithContext(base, want)
	got, ok := actclaim.FromContext(ctx)
	if !ok {
		t.Fatal("FromContext should report present after WithContext")
	}
	if got != want {
		t.Errorf("round-trip Claim = %+v, want %+v", got, want)
	}
}
