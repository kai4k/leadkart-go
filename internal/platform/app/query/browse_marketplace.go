package query

import (
	"context"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/platform/domain/platformlead"
)

// BrowseMarketplaceQuery carries the BRD §4.3 filter set + paging.
// All filter fields optional; nil/empty = "any value".
type BrowseMarketplaceQuery struct {
	Filter   platformlead.MarketplaceFilter
	Cursor   pagination.Cursor
	PageSize int
}

// MarketplaceLeadView is the wire-shaped projection — UNSOLD leads browsable by
// any authenticated tenant. Email/GST/PAN are excluded: they reach the
// purchaser's CRM only after purchase (privacy-by-default for unsold prospects).
type MarketplaceLeadView struct {
	ID             string
	ContactName    string
	City           string
	District       string
	State          string
	PinCode        string
	HasDrugLicence bool
	HasGst         bool
	GstVerified    bool
	HasPan         bool
	BusinessType   string
	MedicineSystem string
	ProductRanges  []string
	DosageForms    []string
	OrderValue     string
	BuyTimeline    string
	VerifiedAt     time.Time
}

// BrowseMarketplaceHandler returns a page of unsold marketplace leads matching
// the filter. Reads via platformlead.Repository.MarketplaceBrowse to keep the
// read path consistent with the write-side RLS posture.
type BrowseMarketplaceHandler struct {
	leads platformlead.Repository
}

// NewBrowseMarketplaceHandler wires the handler.
func NewBrowseMarketplaceHandler(leads platformlead.Repository) BrowseMarketplaceHandler {
	return BrowseMarketplaceHandler{leads: leads}
}

// Handle fetches LIMIT+1 from the repo and builds the wire page.
func (h BrowseMarketplaceHandler) Handle(
	ctx context.Context,
	q BrowseMarketplaceQuery,
) (pagination.Page[MarketplaceLeadView], error) {
	size := pagination.ClampPageSize(q.PageSize)
	leads, err := h.leads.MarketplaceBrowse(ctx, q.Filter, q.Cursor, size+1)
	if err != nil {
		return pagination.Page[MarketplaceLeadView]{}, fmt.Errorf("browse marketplace: %w", err)
	}
	views := make([]MarketplaceLeadView, 0, len(leads))
	for _, l := range leads {
		form := l.Form()
		views = append(views, MarketplaceLeadView{
			ID:             l.ID().String(),
			ContactName:    form.ContactName(),
			City:           form.City(),
			District:       form.District(),
			State:          form.State(),
			PinCode:        form.Pincode(),
			HasDrugLicence: form.HasDrugLicence(),
			HasGst:         form.HasGst(),
			GstVerified:    l.GstVerified(),
			HasPan:         form.HasPan(),
			BusinessType:   string(form.BusinessType()),
			MedicineSystem: string(form.MedicineSystem()),
			ProductRanges:  form.ProductRanges(),
			DosageForms:    form.DosageForms(),
			OrderValue:     string(form.OrderValue()),
			BuyTimeline:    string(form.BuyTimeline()),
			VerifiedAt:     l.VerifiedAt(),
		})
	}
	return pagination.BuildPage(views, size, func(v MarketplaceLeadView) pagination.Cursor {
		return pagination.Cursor{SortValue: v.VerifiedAt, ID: v.ID}
	}), nil
}
