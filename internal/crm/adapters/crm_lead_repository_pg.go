package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/crm/adapters/db"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// CrmLeadRepository is the pgx/sqlc-backed implementation of
// [crmlead.Repository]. Tenant-scoped — every read + write runs under
// [pg.TxScopeTenant] so the connection's `app.tenant_id` GUC binds
// before queries touch the table; Postgres RLS does the rest.
//
// Tenant scoping (ADR 0062 — TDL canon): the GUC is bound from an
// EXPLICIT tenantID Repository-method parameter via
// [pg.Transactor.WithinTxPgxTenant]. Add uses the aggregate's own
// TenantID (which the aggregate carries). ctx-tenancy.WithID is no
// longer required upstream.
//
// Domain↔row mapping lives here; sqlc-generated *db.Queries hold the
// SQL. Per ADR 0047, no app/ code imports this struct — handlers depend
// on [crmlead.Repository] (the interface).
type CrmLeadRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewCrmLeadRepository wires the repository against a pool + transactor.
func NewCrmLeadRepository(pool *pgxpool.Pool, tx *pg.Transactor) *CrmLeadRepository {
	return &CrmLeadRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// Add satisfies [crmlead.Repository] — persists a brand-new CrmLead +
// drains its events into the outbox, all in one tx under tenant scope.
// Joins a surrounding UnitOfWork tx when ctx carries one (per ADR 0047
// + identity-side precedent).
//
// The aggregate carries its own TenantID — the GUC is bound from
// l.TenantID() (TDL canon per ADR 0062: tenantID flows through
// explicit values, not ctx).
func (r *CrmLeadRepository) Add(ctx context.Context, l *crmlead.CrmLead) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, l)
	}
	return r.tx.WithinTxPgxTenant(ctx, l.TenantID().String(), func(ctx context.Context, tx pgx.Tx) error {
		return r.addOnTx(ctx, tx, l)
	})
}

func (r *CrmLeadRepository) addOnTx(ctx context.Context, tx pgx.Tx, l *crmlead.CrmLead) error {
	q := r.q.WithTx(tx)
	if err := insertLeadRow(ctx, q, l); err != nil {
		return err
	}
	return drainLeadEvents(ctx, tx, l)
}

// GetByID satisfies [crmlead.Repository]. Tenant-scoped read — GUC
// bound from the explicit tenantID parameter (TDL canon per ADR 0062).
func (r *CrmLeadRepository) GetByID(ctx context.Context, tenantID tenant.ID, id crmlead.ID) (*crmlead.CrmLead, error) {
	var out *crmlead.CrmLead
	err := r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		got, err := loadLead(ctx, q, id)
		if err != nil {
			return err
		}
		out = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetBySourcePurchaseID satisfies [crmlead.Repository] — subscriber
// idempotency lookup. Tenant-scoped — GUC bound from the explicit
// tenantID parameter.
func (r *CrmLeadRepository) GetBySourcePurchaseID(ctx context.Context, tenantID tenant.ID, purchaseID string) (*crmlead.CrmLead, error) {
	pid, err := uuid.Parse(purchaseID)
	if err != nil {
		return nil, fmt.Errorf("crm lead repo: parse purchase_id %q: %w", purchaseID, err)
	}
	var out *crmlead.CrmLead
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		row, err := q.GetCrmLeadByPurchaseID(ctx, pgUUID(pid))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return crmlead.ErrNotFound
			}
			return fmt.Errorf("crm lead repo: get by purchase id: %w", err)
		}
		hydrated, hErr := rowToLead(row)
		if hErr != nil {
			return hErr
		}
		out = hydrated
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateByID satisfies [crmlead.Repository] — TDL Sep 2024 UpdateFn
// pattern. Load → updateFn → persist (if shouldPersist) → drain events.
// All in one tenant-scoped transaction; joins a surrounding UoW when ctx
// carries one. GUC bound from the explicit tenantID parameter.
func (r *CrmLeadRepository) UpdateByID(
	ctx context.Context,
	tenantID tenant.ID,
	id crmlead.ID,
	updateFn func(*crmlead.CrmLead) (bool, error),
) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.updateOnTx(ctx, tx, id, updateFn)
	}
	return r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		return r.updateOnTx(ctx, tx, id, updateFn)
	})
}

func (r *CrmLeadRepository) updateOnTx(
	ctx context.Context,
	tx pgx.Tx,
	id crmlead.ID,
	updateFn func(*crmlead.CrmLead) (bool, error),
) error {
	q := r.q.WithTx(tx)
	l, err := loadLead(ctx, q, id)
	if err != nil {
		return err
	}
	persist, err := updateFn(l)
	if err != nil {
		return err
	}
	if !persist {
		// Drain any event the closure emitted but the caller chose not
		// to persist — keeps aggregate-event state predictable.
		_ = l.PullEvents()
		return nil
	}
	if err := persistLeadState(ctx, q, l); err != nil {
		return err
	}
	return drainLeadEvents(ctx, tx, l)
}

// ListPage satisfies [crmlead.Repository] — cursor-paginated list per
// ADR 0038. GUC bound from the explicit tenantID parameter (TDL canon
// per ADR 0062).
func (r *CrmLeadRepository) ListPage(
	ctx context.Context,
	tenantID tenant.ID,
	filter crmlead.ListFilter,
	cursor pagination.Cursor,
	pageSize int,
) (pagination.Page[*crmlead.CrmLead], error) {
	clamped := pagination.ClampPageSize(pageSize)
	tid, err := uuid.Parse(tenantID.String())
	if err != nil {
		return pagination.Page[*crmlead.CrmLead]{}, fmt.Errorf("crm lead repo: parse tenant id %q: %w", tenantID, err)
	}

	params := db.ListCrmLeadsPageParams{
		TenantID: pgUUID(tid),
		// peek-one-extra trick per ADR 0038
		PageSize:        int32(clamped) + 1, //nolint:gosec // clamped ≤ 200 by ClampPageSize
		ProductRanges:   nullableTextArray(filter.ProductRanges),
		DosageForms:     nullableTextArray(filter.DosageForms),
		Stage:           stringPtr(filter.Stage.String()),
		Temperature:     stringPtr(filter.Temperature.String()),
		City:            stringPtr(filter.City),
		Pincode:         stringPtr(filter.Pincode),
		BusinessType:    stringPtr(filter.BusinessType),
		MedicineSystem:  stringPtr(filter.MedicineSystem),
		NameQuery:       nameQueryPattern(filter.NameQuery),
		Assignee:        uuidParamOpt(filter.AssigneeMembershipID),
		SelfAssignee:    uuidParamOpt(filter.SelfFilter),
		CursorCreatedAt: pgTimestamp(cursor.SortValue),
		CursorID:        uuidParamOpt(cursor.ID),
	}

	var rows []db.CrmCrmLead
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		got, err := q.ListCrmLeadsPage(ctx, params)
		if err != nil {
			return fmt.Errorf("crm lead repo: list page: %w", err)
		}
		rows = got
		return nil
	})
	if err != nil {
		return pagination.Page[*crmlead.CrmLead]{}, err
	}

	hasMore := false
	if len(rows) > clamped {
		hasMore = true
		rows = rows[:clamped]
	}
	items := make([]*crmlead.CrmLead, 0, len(rows))
	for _, row := range rows {
		hydrated, hErr := rowToLead(row)
		if hErr != nil {
			return pagination.Page[*crmlead.CrmLead]{}, hErr
		}
		items = append(items, hydrated)
	}
	next := ""
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		next = pagination.Encode(pagination.Cursor{SortValue: last.CreatedAt(), ID: last.ID().String()})
	}
	return pagination.Page[*crmlead.CrmLead]{
		Items:      items,
		HasMore:    hasMore,
		NextCursor: next,
	}, nil
}

// ----- Helpers --------------------------------------------------------------

func loadLead(ctx context.Context, q *db.Queries, id crmlead.ID) (*crmlead.CrmLead, error) {
	lid, err := uuid.Parse(id.String())
	if err != nil {
		return nil, fmt.Errorf("crm lead repo: parse id %q: %w", id, err)
	}
	row, err := q.GetCrmLeadByID(ctx, pgUUID(lid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, crmlead.ErrNotFound
		}
		return nil, fmt.Errorf("crm lead repo: get by id: %w", err)
	}
	return rowToLead(row)
}

func insertLeadRow(ctx context.Context, q *db.Queries, l *crmlead.CrmLead) error {
	lid, err := uuid.Parse(l.ID().String())
	if err != nil {
		return fmt.Errorf("crm lead repo: parse id %q: %w", l.ID(), err)
	}
	tid, err := uuid.Parse(l.TenantID().String())
	if err != nil {
		return fmt.Errorf("crm lead repo: parse tenant id %q: %w", l.TenantID(), err)
	}
	extra, err := json.Marshal(l.Profile().Extra)
	if err != nil {
		return fmt.Errorf("crm lead repo: marshal extra_profile: %w", err)
	}
	p := l.Profile()
	return q.InsertCrmLead(ctx, db.InsertCrmLeadParams{
		ID:                      pgUUID(lid),
		TenantID:                pgUUID(tid),
		SourcePurchaseID:        uuidParamOpt(l.SourcePurchaseID()),
		SourcePlatformLeadID:    uuidParamOpt(l.SourcePlatformLeadID()),
		Stage:                   l.Stage().String(),
		Temperature:             l.Temperature().String(),
		ContactName:             p.ContactName,
		PhoneE164:               p.PhoneE164,
		City:                    p.City,
		District:                p.District,
		State:                   p.State,
		Pincode:                 p.Pincode,
		BusinessType:            p.BusinessType,
		MedicineSystem:          p.MedicineSystem,
		OrderValue:              p.OrderValue,
		BuyTimeline:             p.BuyTimeline,
		HasDrugLicence:          p.HasDrugLicence,
		HasGst:                  p.HasGst,
		GstVerified:             p.GstVerified,
		ProductRanges:           p.ProductRanges,
		DosageForms:             p.DosageForms,
		ExtraProfile:            extra,
		AssigneeMembershipID:    uuidParamOpt(l.AssigneeMembershipID()),
		AssignedAt:              pgTimestamp(l.AssignedAt()),
		ConvertedAt:             pgTimestamp(l.ConvertedAt()),
		ConvertedByMembershipID: uuidParamOpt(l.ConvertedByMembershipID()),
		LostAt:                  pgTimestamp(l.LostAt()),
		LostByMembershipID:      uuidParamOpt(l.LostByMembershipID()),
		LostReason:              l.LostReason(),
		CreatedAt:               pgRequiredTimestamp(l.CreatedAt()),
		CreatedByMembershipID:   uuidParamOpt(l.CreatedByMembershipID()),
	})
}

func persistLeadState(ctx context.Context, q *db.Queries, l *crmlead.CrmLead) error {
	lid, err := uuid.Parse(l.ID().String())
	if err != nil {
		return fmt.Errorf("crm lead repo: parse id %q: %w", l.ID(), err)
	}
	extra, err := json.Marshal(l.Profile().Extra)
	if err != nil {
		return fmt.Errorf("crm lead repo: marshal extra_profile: %w", err)
	}
	p := l.Profile()
	return q.UpdateCrmLead(ctx, db.UpdateCrmLeadParams{
		ID:                      pgUUID(lid),
		Stage:                   l.Stage().String(),
		Temperature:             l.Temperature().String(),
		AssigneeMembershipID:    uuidParamOpt(l.AssigneeMembershipID()),
		AssignedAt:              pgTimestamp(l.AssignedAt()),
		ConvertedAt:             pgTimestamp(l.ConvertedAt()),
		ConvertedByMembershipID: uuidParamOpt(l.ConvertedByMembershipID()),
		LostAt:                  pgTimestamp(l.LostAt()),
		LostByMembershipID:      uuidParamOpt(l.LostByMembershipID()),
		LostReason:              l.LostReason(),
		ExtraProfile:            extra,
		ContactName:             p.ContactName,
		PhoneE164:               p.PhoneE164,
		City:                    p.City,
		District:                p.District,
		State:                   p.State,
		Pincode:                 p.Pincode,
		BusinessType:            p.BusinessType,
		MedicineSystem:          p.MedicineSystem,
		OrderValue:              p.OrderValue,
		BuyTimeline:             p.BuyTimeline,
		HasDrugLicence:          p.HasDrugLicence,
		HasGst:                  p.HasGst,
		GstVerified:             p.GstVerified,
		ProductRanges:           p.ProductRanges,
		DosageForms:             p.DosageForms,
	})
}

func drainLeadEvents(ctx context.Context, tx pgx.Tx, l *crmlead.CrmLead) error {
	evs := l.PullEvents()
	if len(evs) == 0 {
		return nil
	}
	tid, err := uuid.Parse(l.TenantID().String())
	if err != nil {
		return fmt.Errorf("crm lead repo: parse tenant id %q: %w", l.TenantID(), err)
	}
	asAny := make([]any, len(evs))
	for i, e := range evs {
		asAny[i] = e
	}
	mapped, err := mapDomainEvents(asAny)
	if err != nil {
		return fmt.Errorf("crm lead repo: map events: %w", err)
	}
	return writeOutboxEvents(ctx, tx, tid, mapped)
}

// rowToLead hydrates the aggregate from the sqlc row.
func rowToLead(row db.CrmCrmLead) (*crmlead.CrmLead, error) {
	extra := crmlead.ExtraProfile{}
	if len(row.ExtraProfile) > 0 {
		if err := json.Unmarshal(row.ExtraProfile, &extra); err != nil {
			return nil, fmt.Errorf("crm lead repo: decode extra_profile: %w", err)
		}
	}
	stage, err := crmlead.ParseStage(row.Stage)
	if err != nil {
		return nil, fmt.Errorf("crm lead repo: stored stage %q invalid: %w", row.Stage, err)
	}
	temp, err := crmlead.ParseTemperature(row.Temperature)
	if err != nil {
		return nil, fmt.Errorf("crm lead repo: stored temperature %q invalid: %w", row.Temperature, err)
	}
	return crmlead.UnmarshalFromDB(crmlead.Snapshot{
		ID:                      crmlead.ID(uuidFromPg(row.ID).String()),
		TenantID:                tenant.ID(uuidFromPg(row.TenantID).String()),
		SourcePurchaseID:        uuidStringOrEmpty(row.SourcePurchaseID),
		SourcePlatformLeadID:    uuidStringOrEmpty(row.SourcePlatformLeadID),
		Stage:                   stage,
		Temperature:             temp,
		Profile: crmlead.Profile{
			ContactName:    row.ContactName,
			PhoneE164:      row.PhoneE164,
			City:           row.City,
			District:       row.District,
			State:          row.State,
			Pincode:        row.Pincode,
			BusinessType:   row.BusinessType,
			MedicineSystem: row.MedicineSystem,
			OrderValue:     row.OrderValue,
			BuyTimeline:    row.BuyTimeline,
			HasDrugLicence: row.HasDrugLicence,
			HasGst:         row.HasGst,
			GstVerified:    row.GstVerified,
			ProductRanges:  row.ProductRanges,
			DosageForms:    row.DosageForms,
			Extra:          extra,
		},
		AssigneeMembershipID:    uuidStringOrEmpty(row.AssigneeMembershipID),
		AssignedAt:              timeFromPg(row.AssignedAt),
		ConvertedAt:             timeFromPg(row.ConvertedAt),
		ConvertedByMembershipID: uuidStringOrEmpty(row.ConvertedByMembershipID),
		LostAt:                  timeFromPg(row.LostAt),
		LostByMembershipID:      uuidStringOrEmpty(row.LostByMembershipID),
		LostReason:              row.LostReason,
		CreatedAt:               timeFromPg(row.CreatedAt),
		CreatedByMembershipID:   uuidStringOrEmpty(row.CreatedByMembershipID),
	}), nil
}

// uuidParamOpt parses a string-form UUID into pgtype.UUID. Empty input
// maps to Valid=false (NULL). Returns the zero pgtype.UUID on parse
// failure too — the caller upstream is the domain factory that
// generates IDs via ids.NewV7, so a malformed string at this layer is
// adapter-side or a wire-decoded value that should already have failed
// in the HTTP DTO parse.
func uuidParamOpt(s string) pgtype.UUID {
	if s == "" {
		return pgtype.UUID{}
	}
	parsed, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{}
	}
	return pgUUID(parsed)
}

// uuidStringOrEmpty returns the string form of a pgtype.UUID, or "" if
// the column is NULL (Valid=false).
func uuidStringOrEmpty(p pgtype.UUID) string {
	if !p.Valid {
		return ""
	}
	return uuid.UUID(p.Bytes).String()
}

// nullableTextArray returns the slice or nil if empty — sqlc treats
// nil as the "no filter" branch for the @> predicate.
func nullableTextArray(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

// nameQueryPattern wraps the caller-supplied name in `%pat%` so the
// ILIKE filter lights up the GIN trgm index. Empty / whitespace input
// returns nil (no filter).
func nameQueryPattern(raw string) *string {
	if raw == "" {
		return nil
	}
	p := "%" + raw + "%"
	return &p
}
