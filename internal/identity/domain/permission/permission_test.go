package permission_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
)

func TestErrors_AreSentinels(t *testing.T) {
	t.Parallel()
	if !errors.Is(permission.ErrUnknown, permission.ErrUnknown) {
		t.Fatal("ErrUnknown not a sentinel")
	}
	if !errors.Is(permission.ErrEmpty, permission.ErrEmpty) {
		t.Fatal("ErrEmpty not a sentinel")
	}
	if !errors.Is(permission.ErrFormat, permission.ErrFormat) {
		t.Fatal("ErrFormat not a sentinel")
	}
}

func TestPermission_NilSafe(t *testing.T) {
	t.Parallel()
	var p *permission.Permission
	if p.Name() != "" {
		t.Fatal("nil.Name() should return empty")
	}
	if p.String() != "" {
		t.Fatal("nil.String() should return empty")
	}
}

func TestPermission_Equal(t *testing.T) {
	t.Parallel()
	var nilP *permission.Permission
	if !nilP.Equal(nil) {
		t.Fatal("nil.Equal(nil) should be true")
	}
}
