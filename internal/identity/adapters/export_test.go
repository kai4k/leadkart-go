package adapters

import "github.com/leadkart/leadkart-go/internal/common/email"

// KeyForTest exposes the table key of an address so tests can look it up. The
// production code never needs the key outside the repository.
func (r *PhantomLockoutRepository) KeyForTest(e email.Address) string { return string(r.rowKey(e)) }
