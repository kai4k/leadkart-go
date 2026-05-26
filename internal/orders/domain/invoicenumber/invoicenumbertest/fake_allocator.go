// Package invoicenumbertest provides the in-memory FakeAllocator
// implementing [invoicenumber.Allocator]. TDL canon — per-primitive
// fakes live in <primitive>test/ sibling packages.
//
// Allocation is monotonic per (tenant, fy, kind) — the fake increments
// an in-memory counter on every Allocate call. Tests assert the
// returned Number.Seq is the expected next value.
package invoicenumbertest

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoicenumber"
)

// FakeAllocator is an in-memory [invoicenumber.Allocator]. Zero-value-
// NOT-usable — construct via [NewFakeAllocator].
type FakeAllocator struct {
	// Sequences holds the last-allocated value per (tenant, fy, kind).
	// The key tuple is rendered as `tenant|fy|kind` for map storage.
	Sequences map[string]int64

	// ForceError, when non-nil, makes the NEXT Allocate return this
	// error + then clears itself.
	ForceError error
}

// ErrAllocate is returned by the fake when [FakeAllocator.ForceError]
// is set without a specific sentinel — drives "DB hiccup" tests.
var ErrAllocate = errors.New("invoicenumbertest: allocate failed")

// NewFakeAllocator returns an empty allocator.
func NewFakeAllocator() *FakeAllocator {
	return &FakeAllocator{Sequences: make(map[string]int64)}
}

// Compile-time interface conformance.
var _ invoicenumber.Allocator = (*FakeAllocator)(nil)

// Allocate satisfies [invoicenumber.Allocator]. Increments the
// per-tuple counter + returns a fresh Number.
func (f *FakeAllocator) Allocate(
	_ context.Context, tenantID tenant.ID, fy invoicenumber.FinancialYear, kind invoicenumber.Kind,
) (invoicenumber.Number, error) {
	if f.ForceError != nil {
		err := f.ForceError
		f.ForceError = nil
		return invoicenumber.Number{}, err
	}
	key := string(tenantID) + "|" + string(fy) + "|" + string(kind)
	f.Sequences[key]++
	return invoicenumber.New(kind, fy, f.Sequences[key])
}
