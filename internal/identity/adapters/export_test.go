package adapters

import (
	"context"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// KeyForTest exposes the table key of an address so tests can look it up. The
// production code never needs the key outside the repository.
func (r *PhantomLockoutRepository) KeyForTest(e email.Address) string { return string(r.rowKey(e)) }

// ReportEvictFailureForTest reports a failed eviction at the given instant.
func (r *PersonRepository) ReportEvictFailureForTest(ctx context.Context, id person.ID, err error, now time.Time) {
	r.evict.report(ctx, id, err, now)
}
