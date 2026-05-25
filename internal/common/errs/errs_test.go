package errs_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/errs"
)

func TestKind_StringRoundtrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		kind errs.Kind
		want string
	}{
		{errs.KindUnknown, "unknown"},
		{errs.KindNotFound, "not_found"},
		{errs.KindAlreadyExists, "already_exists"},
		{errs.KindInvalidInput, "invalid_input"},
		{errs.KindUnauthenticated, "unauthenticated"},
		{errs.KindPermissionDenied, "permission_denied"},
		{errs.KindConflict, "conflict"},
		{errs.KindUnavailable, "unavailable"},
		{errs.KindInternal, "internal"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := tc.kind.String(); got != tc.want {
				t.Fatalf("Kind(%d).String() = %q, want %q", tc.kind, got, tc.want)
			}
		})
	}
}

func TestNew_CreatesErrorWithKindAndMessage(t *testing.T) {
	t.Parallel()
	err := errs.New(errs.KindNotFound, "tenant", "tenant abc not found")

	if err == nil {
		t.Fatal("New returned nil")
	}
	if got := err.Error(); got != "tenant: tenant abc not found" {
		t.Fatalf("Error() = %q, want %q", got, "tenant: tenant abc not found")
	}
	if got := errs.KindOf(err); got != errs.KindNotFound {
		t.Fatalf("KindOf = %v, want %v", got, errs.KindNotFound)
	}
}

func TestKindOf_UnwrapsThroughFmtErrorf(t *testing.T) {
	t.Parallel()
	root := errs.New(errs.KindNotFound, "tenant", "id 42 missing")
	wrapped := fmt.Errorf("loading: %w", root)
	doubleWrapped := fmt.Errorf("repository.GetByID: %w", wrapped)

	if got := errs.KindOf(doubleWrapped); got != errs.KindNotFound {
		t.Fatalf("KindOf through wrap = %v, want %v", got, errs.KindNotFound)
	}
}

func TestKindOf_PlainErrorReturnsUnknown(t *testing.T) {
	t.Parallel()
	plain := errors.New("not one of ours")
	if got := errs.KindOf(plain); got != errs.KindUnknown {
		t.Fatalf("KindOf plain = %v, want KindUnknown", got)
	}
	if got := errs.KindOf(nil); got != errs.KindUnknown {
		t.Fatalf("KindOf nil = %v, want KindUnknown", got)
	}
}

func TestIs_MatchesByKind(t *testing.T) {
	t.Parallel()
	root := errs.New(errs.KindAlreadyExists, "tenant", "slug taken")
	wrapped := fmt.Errorf("registering: %w", root)

	cases := []struct {
		name string
		kind errs.Kind
		want bool
	}{
		{"same kind matches", errs.KindAlreadyExists, true},
		{"different kind no match", errs.KindNotFound, false},
		{"unknown no match", errs.KindUnknown, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := errs.Is(wrapped, tc.kind); got != tc.want {
				t.Fatalf("Is(_, %v) = %v, want %v", tc.kind, got, tc.want)
			}
		})
	}
}

func TestIs_NilAlwaysFalse(t *testing.T) {
	t.Parallel()
	if errs.Is(nil, errs.KindNotFound) {
		t.Fatal("Is(nil, _) = true, want false")
	}
}

func TestNew_UnknownKindRejected(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("New(KindUnknown, ...) did not panic — KindUnknown is not a valid construction kind")
		}
	}()
	_ = errs.New(errs.KindUnknown, "bad", "should panic") // arch-test:ignore-err — asserts panic; return value unreachable
}

// errs.Is should NOT match its own returned error unwrapped via errors.Is
// (which checks identity); errs.Is checks BY KIND. Conflating them is the
// canonical footgun.
func TestIs_DiffersFromErrorsIs(t *testing.T) {
	t.Parallel()
	root := errs.New(errs.KindNotFound, "x", "y")
	other := errs.New(errs.KindNotFound, "x", "y") // different instance, same kind

	if errors.Is(root, other) {
		t.Fatal("errors.Is matched two different *Error instances — broken identity")
	}
	if !errs.Is(root, errs.KindNotFound) {
		t.Fatal("errs.Is should match by kind")
	}
}
