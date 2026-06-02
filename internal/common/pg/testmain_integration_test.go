//go:build integration

package pg_test

import (
	"testing"

	"go.uber.org/goleak"

	"github.com/leadkart/leadkart-go/internal/common/pgtest"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m, pgtest.GoleakOptions()...)
}
