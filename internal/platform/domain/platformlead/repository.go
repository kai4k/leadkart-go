package platformlead

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
)

// ErrNotFound is returned when a lookup misses.
var ErrNotFound = errors.New("platformlead: not found")

// MarketplaceFilter carries the BRD §4.3 browse filters. Zero/empty fields
// (incl. nil/empty slices) skip that column's predicate.
type MarketplaceFilter struct {
	State          string
	City           string
	District       string
	Pincode        string
	BusinessType   string
	MedicineSystem string
	OrderValue     string
	BuyTimeline    string
	HasDrugLicence *bool // nil = don't filter
	HasGst         *bool
	GstVerified    *bool
	ProductRanges  []string // ANY-overlap match via GIN `&&`
	DosageForms    []string // ANY-overlap match via GIN `&&`
}

// Repository persists PlatformLead aggregates and serves marketplace browse
// plus per-tenant purchased-lead reads.
type Repository interface {
	// Add persists a new lead. Write policy is platform-only (TxScopePlatform).
	Add(ctx context.Context, l *PlatformLead) error

	// UpdateByID loads, runs updateFn, persists, and drains events in one tx.
	UpdateByID(ctx context.Context, id ID, updateFn func(*PlatformLead) (bool, error)) error

	// GetByID returns the lead or [ErrNotFound]. The RLS SELECT policy
	// (unsold OR purchased-by-this-tenant OR platform) gates visibility.
	GetByID(ctx context.Context, id ID) (*PlatformLead, error)

	// MarketplaceBrowse returns a page of unsold leads matching filter,
	// keyset-paginated on (verified_at, id) DESC. Caller clamps pageSize.
	MarketplaceBrowse(
		ctx context.Context,
		filter MarketplaceFilter,
		cursor pagination.Cursor,
		pageSize int,
	) ([]*PlatformLead, error)
}
