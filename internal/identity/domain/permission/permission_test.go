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
