package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/platform/adapters/db"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadform"
	"github.com/leadkart/leadkart-go/internal/platform/domain/platformlead"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
)

// PlatformLeadRepository is the pgx/sqlc-backed implementation of
// [platformlead.Repository]. All reads go through sqlc — the
// marketplace browse uses null-guarded params + GIN `&&` overlap
// predicates expressed natively in the generated MarketplaceBrowse query.
type PlatformLeadRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewPlatformLeadRepository wires the repository.
func NewPlatformLeadRepository(pool *pgxpool.Pool, tx *pg.Transactor) *PlatformLeadRepository {
	return &PlatformLeadRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// Add satisfies [platformlead.Repository].
func (r *PlatformLeadRepository) Add(ctx context.Context, l *platformlead.PlatformLead) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, l)
	}
	return r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		return r.addOnTx(ctx, tx, l)
	})
}

func (r *PlatformLeadRepository) addOnTx(ctx context.Context, tx pgx.Tx, l *platformlead.PlatformLead) error {
	q := r.q.WithTx(tx)
	if err := insertPlatformLeadRow(ctx, q, l); err != nil {
		return err
	}
	return drainPlatformLeadEventsToOutbox(ctx, tx, l)
}

// UpdateByID satisfies [platformlead.Repository].
func (r *PlatformLeadRepository) UpdateByID(
	ctx context.Context,
	id platformlead.ID,
	updateFn func(*platformlead.PlatformLead) (bool, error),
) error {
	run := func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		l, err := loadPlatformLead(ctx, q, id)
		if err != nil {
			return err
		}
		shouldPersist, err := updateFn(l)
		if err != nil {
			return err
		}
		if !shouldPersist {
			return nil
		}
		if err := updatePlatformLeadRow(ctx, q, l); err != nil {
			return err
		}
		return drainPlatformLeadEventsToOutbox(ctx, tx, l)
	}
	if tx, ok := pg.TxFromContext(ctx); ok {
		return run(ctx, tx)
	}
	return r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, run)
}

// GetByID satisfies [platformlead.Repository].
func (r *PlatformLeadRepository) GetByID(ctx context.Context, id platformlead.ID) (*platformlead.PlatformLead, error) {
	q := r.q
	if tx, ok := pg.TxFromContext(ctx); ok {
		q = r.q.WithTx(tx)
	}
	return loadPlatformLead(ctx, q, id)
}

// MarketplaceBrowse satisfies [platformlead.Repository]. Backed by the
// sqlc MarketplaceBrowse query — null-guarded optional filters
// (a nil arg = "don't filter") + GIN `&&` overlap predicates +
// (verified_at, id) keyset cursor, all expressed natively in SQL.
//
// H12 hardening (review-pass): the SELECT list explicitly OMITS PII
// columns (email, gst_number, pan_number, mobile_e164, street). These
// fields land on the lead row at insertion (the verification form is
// captured wholesale) but the marketplace surface MUST NOT expose
// them to cross-tenant browsers per BRD §4.3 + ADR 0059 marketplace
// SELECT policy. Because the projection is a strict subset, sqlc emits
// a custom db.MarketplaceBrowseRow type (not the full
// PlatformPlatformLead model) that simply has no PII fields to leak —
// [marketplaceRowToPlatformLead] re-builds the aggregate from it and
// never touches the omitted columns.
//
// Once the purchaser owns the lead, the full row (incl PII) is
// available via GetByID under the buyer's tenant context — that's
// the only legitimate read of the omitted columns post-purchase.
func (r *PlatformLeadRepository) MarketplaceBrowse(
	ctx context.Context,
	filter platformlead.MarketplaceFilter,
	cursor pagination.Cursor,
	pageSize int,
) ([]*platformlead.PlatformLead, error) {
	limit := pageSize
	if limit <= 0 {
		limit = 1
	}
	params := db.MarketplaceBrowseParams{
		State:          nilIfEmpty(filter.State),
		City:           nilIfEmpty(filter.City),
		District:       nilIfEmpty(filter.District),
		Pincode:        nilIfEmpty(filter.Pincode),
		BusinessType:   nilIfEmpty(filter.BusinessType),
		MedicineSystem: nilIfEmpty(filter.MedicineSystem),
		OrderValue:     nilIfEmpty(filter.OrderValue),
		BuyTimeline:    nilIfEmpty(filter.BuyTimeline),
		HasDrugLicence: filter.HasDrugLicence,
		HasGst:         filter.HasGst,
		GstVerified:    filter.GstVerified,
		ProductRanges:  nilIfEmptySlice(filter.ProductRanges),
		DosageForms:    nilIfEmptySlice(filter.DosageForms),
		//nolint:gosec // G115: app-layer ClampPageSize bounds to [1, 200]
		Lim: int32(limit),
	}
	applyMarketplaceCursorParams(&params, cursor)

	rows, err := r.q.MarketplaceBrowse(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("platform lead repo: browse query: %w", err)
	}
	out := make([]*platformlead.PlatformLead, 0, len(rows))
	for _, row := range rows {
		out = append(out, marketplaceRowToPlatformLead(row))
	}
	return out, nil
}

// ----- Mappers --------------------------------------------------------------

func loadPlatformLead(ctx context.Context, q *db.Queries, id platformlead.ID) (*platformlead.PlatformLead, error) {
	uid, err := uuid.Parse(id.String())
	if err != nil {
		return nil, fmt.Errorf("platform lead repo: parse id %q: %w", id, err)
	}
	row, err := q.GetPlatformLeadByID(ctx, pgUUID(uid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, platformlead.ErrNotFound
		}
		return nil, fmt.Errorf("platform lead repo: get: %w", err)
	}
	return rowToPlatformLead(row), nil
}

func insertPlatformLeadRow(ctx context.Context, q *db.Queries, l *platformlead.PlatformLead) error {
	id, err := uuid.Parse(l.ID().String())
	if err != nil {
		return fmt.Errorf("platform lead repo: parse id: %w", err)
	}
	srcID, err := uuid.Parse(l.SourceContactID().String())
	if err != nil {
		return fmt.Errorf("platform lead repo: parse source id: %w", err)
	}
	verifiedBy, err := uuid.Parse(l.VerifiedByMembershipID().String())
	if err != nil {
		return fmt.Errorf("platform lead repo: parse verified_by: %w", err)
	}
	f := l.Form()
	err = q.InsertPlatformLead(ctx, db.InsertPlatformLeadParams{
		ID:                     pgUUID(id),
		SourceContactID:        pgUUID(srcID),
		SoldToTenantID:         pgUUIDOptStr(l.SoldToTenantID().String()),
		SoldAt:                 pgTimestamp(l.SoldAt()),
		SoldToMembershipID:     pgUUIDOptStr(l.SoldToMembershipID().String()),
		AmountPaisa:            l.AmountPaisa(),
		ContactName:            f.ContactName(),
		MobileE164:             f.MobileE164(),
		Email:                  f.Email(),
		Pincode:                f.Pincode(),
		City:                   f.City(),
		District:               f.District(),
		StateGeo:               f.State(),
		Street:                 f.Street(),
		HasDrugLicence:         f.HasDrugLicence(),
		HasGst:                 f.HasGst(),
		GstNumber:              f.GstNumber(),
		GstVerified:            l.GstVerified(),
		HasPan:                 f.HasPan(),
		PanNumber:              f.PanNumber(),
		BusinessType:           string(f.BusinessType()),
		MedicineSystem:         string(f.MedicineSystem()),
		ProductRanges:          f.ProductRanges(),
		DosageForms:            f.DosageForms(),
		OrderValue:             string(f.OrderValue()),
		BuyTimeline:            string(f.BuyTimeline()),
		VerifiedAt:             pgRequiredTimestamp(l.VerifiedAt()),
		VerifiedByMembershipID: pgUUID(verifiedBy),
		CreatedAt:              pgRequiredTimestamp(l.CreatedAt()),
	})
	if err != nil {
		return fmt.Errorf("platform lead repo: insert: %w", err)
	}
	return nil
}

func updatePlatformLeadRow(ctx context.Context, q *db.Queries, l *platformlead.PlatformLead) error {
	id, err := uuid.Parse(l.ID().String())
	if err != nil {
		return fmt.Errorf("platform lead repo: parse id: %w", err)
	}
	err = q.UpdatePlatformLead(ctx, db.UpdatePlatformLeadParams{
		ID:                 pgUUID(id),
		SoldToTenantID:     pgUUIDOptStr(l.SoldToTenantID().String()),
		SoldAt:             pgTimestamp(l.SoldAt()),
		SoldToMembershipID: pgUUIDOptStr(l.SoldToMembershipID().String()),
		AmountPaisa:        l.AmountPaisa(),
		GstVerified:        l.GstVerified(),
	})
	if err != nil {
		return fmt.Errorf("platform lead repo: update: %w", err)
	}
	return nil
}

func rowToPlatformLead(row db.PlatformPlatformLead) *platformlead.PlatformLead {
	form := leadform.UnmarshalFromDB(leadform.Input{
		ContactName:    row.ContactName,
		MobileE164:     row.MobileE164,
		Email:          row.Email,
		Pincode:        row.Pincode,
		City:           row.City,
		District:       row.District,
		State:          row.StateGeo,
		Street:         row.Street,
		HasDrugLicence: row.HasDrugLicence,
		HasGst:         row.HasGst,
		GstNumber:      row.GstNumber,
		HasPan:         row.HasPan,
		PanNumber:      row.PanNumber,
		BusinessType:   leadform.BusinessType(row.BusinessType),
		MedicineSystem: leadform.MedicineSystem(row.MedicineSystem),
		ProductRanges:  row.ProductRanges,
		DosageForms:    row.DosageForms,
		OrderValue:     leadform.OrderValue(row.OrderValue),
		BuyTimeline:    leadform.BuyTimeline(row.BuyTimeline),
	})
	return platformlead.UnmarshalFromDB(platformlead.Snapshot{
		ID:                     platformlead.ID(uuidFromPg(row.ID).String()),
		SourceContactID:        unverifiedcontact.ID(uuidFromPg(row.SourceContactID).String()),
		Form:                   form,
		GstVerified:            row.GstVerified,
		SoldToTenantID:         platformlead.TenantID(uuidStringIfValid(row.SoldToTenantID)),
		SoldAt:                 timeFromPg(row.SoldAt),
		SoldToMembershipID:     unverifiedcontact.MembershipID(uuidStringIfValid(row.SoldToMembershipID)),
		AmountPaisa:            row.AmountPaisa,
		VerifiedAt:             timeFromPg(row.VerifiedAt),
		VerifiedByMembershipID: unverifiedcontact.MembershipID(uuidFromPg(row.VerifiedByMembershipID).String()),
		CreatedAt:              timeFromPg(row.CreatedAt),
	})
}

func drainPlatformLeadEventsToOutbox(ctx context.Context, tx pgx.Tx, l *platformlead.PlatformLead) error {
	evs := l.PullEvents()
	if len(evs) == 0 {
		return nil
	}
	asAny := make([]any, len(evs))
	for i, e := range evs {
		asAny[i] = e
	}
	// PlatformLead aggregate-side events are SUPPRESSED by the
	// mechanical mapper (handler emits LeadVerifiedV1 + LeadPurchasedV1
	// directly with the LeadSnapshot). Calling drainEventsToOutbox is
	// still correct — it returns nil events + no-ops.
	return drainEventsToOutbox(ctx, tx, uuid.Nil, asAny)
}
// marketplaceRowToPlatformLead re-builds a PlatformLead aggregate from
// the PII-omitting marketplace projection. The omitted columns
// (email / mobile_e164 / gst_number / pan_number / street) are absent
// from db.MarketplaceBrowseRow entirely, so they hydrate to their zero
// values — there is nothing to leak through the DTO mapper (H12).
func marketplaceRowToPlatformLead(row db.MarketplaceBrowseRow) *platformlead.PlatformLead {
	form := leadform.UnmarshalFromDB(leadform.Input{
		ContactName:    row.ContactName,
		Pincode:        row.Pincode,
		City:           row.City,
		District:       row.District,
		State:          row.StateGeo,
		HasDrugLicence: row.HasDrugLicence,
		HasGst:         row.HasGst,
		HasPan:         row.HasPan,
		BusinessType:   leadform.BusinessType(row.BusinessType),
		MedicineSystem: leadform.MedicineSystem(row.MedicineSystem),
		ProductRanges:  row.ProductRanges,
		DosageForms:    row.DosageForms,
		OrderValue:     leadform.OrderValue(row.OrderValue),
		BuyTimeline:    leadform.BuyTimeline(row.BuyTimeline),
	})
	return platformlead.UnmarshalFromDB(platformlead.Snapshot{
		ID:                     platformlead.ID(uuidFromPg(row.ID).String()),
		SourceContactID:        unverifiedcontact.ID(uuidFromPg(row.SourceContactID).String()),
		Form:                   form,
		GstVerified:            row.GstVerified,
		SoldToTenantID:         platformlead.TenantID(uuidStringIfValid(row.SoldToTenantID)),
		SoldAt:                 timeFromPg(row.SoldAt),
		SoldToMembershipID:     unverifiedcontact.MembershipID(uuidStringIfValid(row.SoldToMembershipID)),
		AmountPaisa:            row.AmountPaisa,
		VerifiedAt:             timeFromPg(row.VerifiedAt),
		VerifiedByMembershipID: unverifiedcontact.MembershipID(uuidFromPg(row.VerifiedByMembershipID).String()),
		CreatedAt:              timeFromPg(row.CreatedAt),
	})
}

// applyMarketplaceCursorParams sets the keyset predicate params for the
// (verified_at, id) DESC ordering. A zero/empty or malformed cursor is
// dropped (params left nil → SQL skips the predicate) rather than
// failing — keeps the browse endpoint forgiving of stale client-side
// cursor blobs.
func applyMarketplaceCursorParams(params *db.MarketplaceBrowseParams, cursor pagination.Cursor) {
	if cursor.SortValue.IsZero() || cursor.ID == "" {
		return
	}
	cursorID, err := uuid.Parse(cursor.ID)
	if err != nil {
		return
	}
	params.CursorVerifiedAt = pgTimestamp(cursor.SortValue)
	params.CursorID = pgUUID(cursorID)
}

// nilIfEmpty returns nil for the empty string so a sqlc narg param maps
// to SQL NULL ("don't filter on this column").
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// nilIfEmptySlice returns nil for an empty/nil slice so the GIN-overlap
// narg param maps to SQL NULL ("don't filter").
func nilIfEmptySlice(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}
