package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/platform/adapters/db"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadform"
	"github.com/leadkart/leadkart-go/internal/platform/domain/platformlead"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
)

// PlatformLeadRepository is the pgx/sqlc implementation of
// [platformlead.Repository]. MarketplaceBrowse uses null-guarded params +
// GIN && overlap predicates expressed natively in the generated query.
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
		// Row-lock the lead + hydrate its buyer set so RecordPurchase's
		// sale-limit count is race-free under concurrent purchases (ADR 0065).
		l, err := loadPlatformLeadForUpdate(ctx, q, id)
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
		if err := insertPendingPurchases(ctx, q, l); err != nil {
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

// MarketplaceBrowse satisfies [platformlead.Repository]. Null-guarded
// optional filters (nil = don't filter) + GIN && overlap + (verified_at, id)
// keyset, all in SQL.
//
// H12: the SELECT OMITS PII columns (email, gst_number, pan_number,
// mobile_e164, street). They land at insertion but must never reach
// cross-tenant browsers (BRD §4.3, ADR 0059 SELECT policy). The strict
// subset makes sqlc emit db.MarketplaceBrowseRow with no PII fields to leak;
// [marketplaceRowToPlatformLead] hydrates from it. Post-purchase, the buyer
// reads the full row (incl PII) via GetByID under their own tenant.
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
		State:          pgconv.ZeroToNil(filter.State),
		City:           pgconv.ZeroToNil(filter.City),
		District:       pgconv.ZeroToNil(filter.District),
		Pincode:        pgconv.ZeroToNil(filter.Pincode),
		BusinessType:   pgconv.ZeroToNil(filter.BusinessType),
		MedicineSystem: pgconv.ZeroToNil(filter.MedicineSystem),
		OrderValue:     pgconv.ZeroToNil(filter.OrderValue),
		BuyTimeline:    pgconv.ZeroToNil(filter.BuyTimeline),
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
	row, err := q.GetPlatformLeadByID(ctx, pgconv.PgUUID(uid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, platformlead.ErrNotFound
		}
		return nil, fmt.Errorf("platform lead repo: get: %w", err)
	}
	// Plain reads don't hydrate the buyer set (availability is a query
	// concern; the purchase path uses loadPlatformLeadForUpdate).
	return rowToPlatformLead(row, nil), nil
}

// loadPlatformLeadForUpdate row-locks the lead and hydrates its buyer set so
// RecordPurchase enforces the sale limit + no-double-buy race-free.
func loadPlatformLeadForUpdate(ctx context.Context, q *db.Queries, id platformlead.ID) (*platformlead.PlatformLead, error) {
	uid, err := uuid.Parse(id.String())
	if err != nil {
		return nil, fmt.Errorf("platform lead repo: parse id %q: %w", id, err)
	}
	row, err := q.GetPlatformLeadByIDForUpdate(ctx, pgconv.PgUUID(uid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, platformlead.ErrNotFound
		}
		return nil, fmt.Errorf("platform lead repo: get for update: %w", err)
	}
	buyerUUIDs, err := q.ListLeadPurchaseTenantIDs(ctx, pgconv.PgUUID(uid))
	if err != nil {
		return nil, fmt.Errorf("platform lead repo: list buyers: %w", err)
	}
	buyers := make([]platformlead.TenantID, 0, len(buyerUUIDs))
	for _, b := range buyerUUIDs {
		buyers = append(buyers, platformlead.TenantID(pgconv.UUIDFromPg(b).String()))
	}
	return rowToPlatformLead(forUpdateRowAsByIDRow(row), buyers), nil
}

// forUpdateRowAsByIDRow adapts the FOR UPDATE row to the shared mapper input
// (identical column set, distinct sqlc type).
func forUpdateRowAsByIDRow(r db.GetPlatformLeadByIDForUpdateRow) db.GetPlatformLeadByIDRow {
	return db.GetPlatformLeadByIDRow(r)
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
		ID:                     pgconv.PgUUID(id),
		SourceContactID:        pgconv.PgUUID(srcID),
		Tier:                   l.Tier().String(),
		SaleLimit:              intPtrToInt32Ptr(l.SaleLimit()),
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
		VerifiedAt:             pgconv.PgRequiredTimestamp(l.VerifiedAt()),
		VerifiedByMembershipID: pgconv.PgUUID(verifiedBy),
		CreatedAt:              pgconv.PgRequiredTimestamp(l.CreatedAt()),
	})
	if err != nil {
		return fmt.Errorf("platform lead repo: insert: %w", err)
	}
	return nil
}

// updatePlatformLeadRow persists the only mutable column on the lead row,
// gst_verified. A purchase is an INSERT into lead_purchases (see
// insertPendingPurchases), not an update here.
func updatePlatformLeadRow(ctx context.Context, q *db.Queries, l *platformlead.PlatformLead) error {
	id, err := uuid.Parse(l.ID().String())
	if err != nil {
		return fmt.Errorf("platform lead repo: parse id: %w", err)
	}
	err = q.UpdatePlatformLeadGstVerified(ctx, db.UpdatePlatformLeadGstVerifiedParams{
		ID:          pgconv.PgUUID(id),
		GstVerified: l.GstVerified(),
	})
	if err != nil {
		return fmt.Errorf("platform lead repo: update gst_verified: %w", err)
	}
	return nil
}

// insertPendingPurchases drains the lead's recorded purchases and INSERTs each
// lead_purchases row. A UNIQUE(lead_id, tenant_id) 23505 (a concurrent buyer
// won the race) maps to [platformlead.ErrAlreadyPurchased].
func insertPendingPurchases(ctx context.Context, q *db.Queries, l *platformlead.PlatformLead) error {
	for _, p := range l.PullPendingPurchases() {
		pid, err := uuid.Parse(p.ID())
		if err != nil {
			return fmt.Errorf("platform lead repo: parse purchase id: %w", err)
		}
		leadID, err := uuid.Parse(p.LeadID().String())
		if err != nil {
			return fmt.Errorf("platform lead repo: parse purchase lead id: %w", err)
		}
		tenantID, err := uuid.Parse(p.TenantID().String())
		if err != nil {
			return fmt.Errorf("platform lead repo: parse purchase tenant id: %w", err)
		}
		membershipID, err := uuid.Parse(p.MembershipID().String())
		if err != nil {
			return fmt.Errorf("platform lead repo: parse purchase membership id: %w", err)
		}
		err = q.InsertLeadPurchase(ctx, db.InsertLeadPurchaseParams{
			ID:                    pgconv.PgUUID(pid),
			LeadID:                pgconv.PgUUID(leadID),
			TenantID:              pgconv.PgUUID(tenantID),
			CreatedByMembershipID: pgconv.PgUUID(membershipID),
			AmountPaisa:           p.AmountPaisa(),
			PurchasedAt:           pgconv.PgRequiredTimestamp(p.PurchasedAt()),
		})
		if err != nil {
			if isUniqueViolation(err) {
				return platformlead.ErrAlreadyPurchased
			}
			return fmt.Errorf("platform lead repo: insert purchase: %w", err)
		}
	}
	return nil
}

// isUniqueViolation reports whether err is a Postgres 23505 unique-violation.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == pg.SQLStateUniqueViolation
}

func rowToPlatformLead(row db.GetPlatformLeadByIDRow, buyers []platformlead.TenantID) *platformlead.PlatformLead {
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
		ID:                     platformlead.ID(pgconv.UUIDFromPg(row.ID).String()),
		SourceContactID:        unverifiedcontact.ID(pgconv.UUIDFromPg(row.SourceContactID).String()),
		Form:                   form,
		GstVerified:            row.GstVerified,
		Tier:                   platformlead.Tier(row.Tier),
		SaleLimit:              int32PtrToIntPtr(row.SaleLimit),
		BuyerTenantIDs:         buyers,
		VerifiedAt:             pgconv.TimeFromPg(row.VerifiedAt),
		VerifiedByMembershipID: unverifiedcontact.MembershipID(pgconv.UUIDFromPg(row.VerifiedByMembershipID).String()),
		CreatedAt:              pgconv.TimeFromPg(row.CreatedAt),
	})
}

// int32PtrToIntPtr / intPtrToInt32Ptr bridge the sqlc nullable column type
// (*int32) and the domain's platform-agnostic *int for the sale_limit override.
func int32PtrToIntPtr(v *int32) *int {
	if v == nil {
		return nil
	}
	n := int(*v)
	return &n
}

func intPtrToInt32Ptr(v *int) *int32 {
	if v == nil {
		return nil
	}
	//nolint:gosec // sale_limit is a small positive bound, never overflows int32.
	n := int32(*v)
	return &n
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
	// Aggregate-side events are suppressed by the mapper (the handler emits
	// LeadVerifiedV1 + LeadPurchasedV1 directly with the snapshot); drain
	// no-ops on them.
	return drainEventsToOutbox(ctx, tx, uuid.Nil, asAny)
}

// marketplaceRowToPlatformLead hydrates an aggregate from the PII-omitting
// projection. The omitted columns are absent from db.MarketplaceBrowseRow, so
// they hydrate to zero values: nothing to leak (H12).
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
		ID:                     platformlead.ID(pgconv.UUIDFromPg(row.ID).String()),
		SourceContactID:        unverifiedcontact.ID(pgconv.UUIDFromPg(row.SourceContactID).String()),
		Form:                   form,
		GstVerified:            row.GstVerified,
		Tier:                   platformlead.Tier(row.Tier),
		SaleLimit:              int32PtrToIntPtr(row.SaleLimit),
		VerifiedAt:             pgconv.TimeFromPg(row.VerifiedAt),
		VerifiedByMembershipID: unverifiedcontact.MembershipID(pgconv.UUIDFromPg(row.VerifiedByMembershipID).String()),
		CreatedAt:              pgconv.TimeFromPg(row.CreatedAt),
	})
}

// applyMarketplaceCursorParams sets the (verified_at, id) DESC keyset
// predicate. A zero/malformed cursor is dropped (params left nil, SQL skips
// the predicate) so the endpoint tolerates stale client cursor blobs.
func applyMarketplaceCursorParams(params *db.MarketplaceBrowseParams, cursor pagination.Cursor) {
	if cursor.SortValue.IsZero() || cursor.ID == "" {
		return
	}
	cursorID, err := uuid.Parse(cursor.ID)
	if err != nil {
		return
	}
	params.CursorVerifiedAt = pgconv.PgTimestamp(cursor.SortValue)
	params.CursorID = pgconv.PgUUID(cursorID)
}

// nilIfEmptySlice returns nil for an empty slice so the GIN-overlap param
// maps to SQL NULL (don't filter).
func nilIfEmptySlice(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}
