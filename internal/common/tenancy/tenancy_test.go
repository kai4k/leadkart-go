package tenancy_test

import (
	"context"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
)

func TestID_IsZero(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		id   tenancy.ID
		want bool
	}{
		{"empty is zero", tenancy.ID(""), true},
		{"non-empty is not zero", tenancy.ID("019df708-f642-7f66-b73b-c7919f2447cb"), false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.id.IsZero(); got != tc.want {
				t.Fatalf("IsZero() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestID_String(t *testing.T) {
	t.Parallel()
	id := tenancy.ID("019df708-f642-7f66-b73b-c7919f2447cb")
	if got := id.String(); got != "019df708-f642-7f66-b73b-c7919f2447cb" {
		t.Fatalf("String() = %q, want %q", got, "019df708-f642-7f66-b73b-c7919f2447cb")
	}
}

func TestWithID_FromContext_Roundtrip(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	want := tenancy.ID("019df708-f642-7f66-b73b-c7919f2447cb")
	ctx = tenancy.WithID(ctx, want)

	got, ok := tenancy.FromContext(ctx)
	if !ok {
		t.Fatal("FromContext: ok = false, want true")
	}
	if got != want {
		t.Fatalf("FromContext: got = %q, want %q", got, want)
	}
}

func TestFromContext_AbsentReturnsFalse(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	got, ok := tenancy.FromContext(ctx)
	if ok {
		t.Fatalf("FromContext: ok = true, want false (got id=%q)", got)
	}
	if !got.IsZero() {
		t.Fatalf("FromContext: got = %q, want zero", got)
	}
}

func TestMustFromContext_PanicsWhenAbsent(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("MustFromContext did not panic on absent tenant")
		}
	}()
	tenancy.MustFromContext(t.Context())
}

func TestMustFromContext_PanicsWhenZero(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("MustFromContext did not panic on zero tenant")
		}
	}()
	ctx := tenancy.WithID(t.Context(), tenancy.ID(""))
	tenancy.MustFromContext(ctx)
}

func TestMustFromContext_ReturnsValueWhenPresent(t *testing.T) {
	t.Parallel()
	want := tenancy.ID("019df708-f642-7f66-b73b-c7919f2447cb")
	ctx := tenancy.WithID(t.Context(), want)
	if got := tenancy.MustFromContext(ctx); got != want {
		t.Fatalf("got = %q, want %q", got, want)
	}
}

// Anti-collision test: a different package using its own context key with
// the same string name MUST NOT collide with tenancy's key. This locks in
// the unexported-key-type discipline (Go canon for ctx.Value keys).
func TestKey_DoesNotCollideWithStringKey(t *testing.T) {
	t.Parallel()
	type rogueKey string
	ctx := context.WithValue(t.Context(), rogueKey("tenant"), tenancy.ID("rogue"))

	got, ok := tenancy.FromContext(ctx)
	if ok {
		t.Fatalf("FromContext: tenancy read a rogue string key (got=%q) — key isolation broken", got)
	}
}
