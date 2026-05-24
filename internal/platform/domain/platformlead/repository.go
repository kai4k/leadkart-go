package platformlead

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
)

// ErrNotFound is returned when a lookup misses.
var ErrNotFound = errors.New("platformlead: not found")

// MarketplaceFilter carries the BRD §4.3 marketplace browse filters.
// All fields optional — zero/empty means "don't filter on this column".
// Nil / empty slices = "any value" for the GIN-indexed columns.
type MarketplaceFilter struct {
	State            string
	City             string
	District         string
	Pincode          string
	BusinessType     string
	MedicineSystem   string
	OrderValue       string
	BuyTimeline      string
	HasDrugLicence   *bool // nil = don't filter
	HasGst           *bool
	GstVerified      *bool
	ProductRanges    []string // ANY-overlap match via GIN `&&`
	DosageForms      []string // ANY-overlap match via GIN `&&`
}

// Repository persists PlatformLead aggregates + serves the marketplace
// browse + per-tenant purchased-lead reads.
type Repository interface {
	// Add persists a brand-new lead created via [NewFromUnverifiedContact].
	// Runs under TxScopePlatform (write policy is platform-only).
	Add(ctx context.Context, l *PlatformLead) error

	// UpdateByID loads, runs updateFn, persists + drains events — one tx.
	UpdateByID(ctx context.Context, id ID, updateFn func(*PlatformLead) (bool, error)) error

	// GetByID returns the lead or [ErrNotFound]. Read path — runs
	// under the caller's existing scope. The RLS SELECT policy
	// (unsold OR purchased-by-this-tenant OR platform) gates visibility.
	GetByID(ctx context.Context, id ID) (*PlatformLead, error)

	// MarketplaceBrowse returns a page of UNSOLD leads matching the
	// supplied filters, keyset-paginated on (verified_at, id) DESC.
	// pageSize is clamped to [1, 200] by the caller.
	MarketplaceBrowse(
		ctx context.Context,
		filter MarketplaceFilter,
		cursor pagination.Cursor,
		pageSize int,
	) ([]*PlatformLead, error)
}
