package adapters

import (
	"context"
	"errors"
	"fmt"

	sq "github.com/Masterminds/squirrel"
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
// [platformlead.Repository]. Static reads via sqlc; the marketplace
// browse uses squirrel directly (dynamic WHERE clauses + GIN array
// filters don't map cleanly to sqlc parameter slots).
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

// MarketplaceBrowse satisfies [platformlead.Repository]. Uses squirrel
// for dynamic WHERE clauses; queries `platform.platform_leads` directly
// since sqlc can't express the optional `&&` GIN overlap predicates
// cleanly.
func (r *PlatformLeadRepository) MarketplaceBrowse(
	ctx context.Context,
	filter platformlead.MarketplaceFilter,
	cursor pagination.Cursor,
	pageSize int,
) ([]*platformlead.PlatformLead, error) {
	psql := sq.StatementBuilder.PlaceholderFormat(sq.Dollar)
	query := psql.
		Select(
			"id", "source_contact_id",
			"sold_to_tenant_id", "sold_at", "sold_to_membership_id", "amount_paisa",
			"contact_name", "mobile_e164", "email", "pincode", "city", "district", "state_geo", "street",
			"has_drug_licence", "has_gst", "gst_number", "gst_verified", "has_pan", "pan_number",
			"business_type", "medicine_system", "product_ranges", "dosage_forms",
			"order_value", "buy_timeline",
			"verified_at", "verified_by_membership_id", "created_at",
		).
		From("platform.platform_leads").
		Where(sq.Eq{"sold_to_tenant_id": nil})

	query = applyMarketplaceFilters(query, filter)
	query = applyMarketplaceCursor(query, cursor)

	limit := pageSize
	if limit <= 0 {
		limit = 1
	}
	//nolint:gosec // G115: app-layer ClampPageSize bounds to [1, 200]
	query = query.OrderBy("verified_at DESC", "id DESC").Limit(uint64(limit))

	sqlStr, args, err := query.ToSql()
	if err != nil {
		return nil, fmt.Errorf("platform lead repo: build browse SQL: %w", err)
	}

	rows, err := r.pool.Query(ctx, sqlStr, args...)
	if err != nil {
		return nil, fmt.Errorf("platform lead repo: browse query: %w", err)
	}
	defer rows.Close()

	var out []*platformlead.PlatformLead
	for rows.Next() {
		var row db.PlatformPlatformLead
		err := rows.Scan(
			&row.ID, &row.SourceContactID,
			&row.SoldToTenantID, &row.SoldAt, &row.SoldToMembershipID, &row.AmountPaisa,
			&row.ContactName, &row.MobileE164, &row.Email, &row.Pincode,
			&row.City, &row.District, &row.StateGeo, &row.Street,
			&row.HasDrugLicence, &row.HasGst, &row.GstNumber, &row.GstVerified,
			&row.HasPan, &row.PanNumber,
			&row.BusinessType, &row.MedicineSystem, &row.ProductRanges, &row.DosageForms,
			&row.OrderValue, &row.BuyTimeline,
			&row.VerifiedAt, &row.VerifiedByMembershipID, &row.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("platform lead repo: scan: %w", err)
		}
		out = append(out, rowToPlatformLead(row))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("platform lead repo: rows iter: %w", err)
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


// applyMarketplaceFilters chains the optional BRD §4.3 filter clauses
// onto an unsold-leads query. Extracted from MarketplaceBrowse to keep
// the orchestrator below the cyclomatic-complexity ceiling.
func applyMarketplaceFilters(q sq.SelectBuilder, f platformlead.MarketplaceFilter) sq.SelectBuilder {
	if f.State != "" {
		q = q.Where(sq.Eq{"state_geo": f.State})
	}
	if f.City != "" {
		q = q.Where(sq.Eq{"city": f.City})
	}
	if f.District != "" {
		q = q.Where(sq.Eq{"district": f.District})
	}
	if f.Pincode != "" {
		q = q.Where(sq.Eq{"pincode": f.Pincode})
	}
	if f.BusinessType != "" {
		q = q.Where(sq.Eq{"business_type": f.BusinessType})
	}
	if f.MedicineSystem != "" {
		q = q.Where(sq.Eq{"medicine_system": f.MedicineSystem})
	}
	if f.OrderValue != "" {
		q = q.Where(sq.Eq{"order_value": f.OrderValue})
	}
	if f.BuyTimeline != "" {
		q = q.Where(sq.Eq{"buy_timeline": f.BuyTimeline})
	}
	if f.HasDrugLicence != nil {
		q = q.Where(sq.Eq{"has_drug_licence": *f.HasDrugLicence})
	}
	if f.HasGst != nil {
		q = q.Where(sq.Eq{"has_gst": *f.HasGst})
	}
	if f.GstVerified != nil {
		q = q.Where(sq.Eq{"gst_verified": *f.GstVerified})
	}
	if len(f.ProductRanges) > 0 {
		q = q.Where(sq.Expr("product_ranges && ?", f.ProductRanges))
	}
	if len(f.DosageForms) > 0 {
		q = q.Where(sq.Expr("dosage_forms && ?", f.DosageForms))
	}
	return q
}

// applyMarketplaceCursor adds the keyset predicate for the (verified_at,
// id) DESC ordering. Malformed cursor.ID is dropped (no predicate added)
// rather than failing — keeps the browse endpoint forgiving of stale
// client-side cursor blobs.
func applyMarketplaceCursor(q sq.SelectBuilder, cursor pagination.Cursor) sq.SelectBuilder {
	if cursor.SortValue.IsZero() || cursor.ID == "" {
		return q
	}
	cursorID, err := uuid.Parse(cursor.ID)
	if err != nil {
		return q
	}
	return q.Where(sq.Expr(
		"(verified_at, id) < (?, ?)",
		cursor.SortValue, cursorID,
	))
}
