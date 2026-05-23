package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/platform/adapters/db"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadform"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
)

// UnverifiedContactRepository is the pgx/sqlc-backed implementation of
// [unverifiedcontact.Repository]. Platform-only table — every write runs
// under TxScopePlatform.
type UnverifiedContactRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewUnverifiedContactRepository wires the repository.
func NewUnverifiedContactRepository(pool *pgxpool.Pool, tx *pg.Transactor) *UnverifiedContactRepository {
	return &UnverifiedContactRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// Add satisfies [unverifiedcontact.Repository]. Joins the surrounding
// UoW tx via pg.TxFromContext when present.
func (r *UnverifiedContactRepository) Add(ctx context.Context, c *unverifiedcontact.UnverifiedContact) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, c)
	}
	return r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		return r.addOnTx(ctx, tx, c)
	})
}

func (r *UnverifiedContactRepository) addOnTx(ctx context.Context, tx pgx.Tx, c *unverifiedcontact.UnverifiedContact) error {
	q := r.q.WithTx(tx)
	if err := insertUnverifiedContactRow(ctx, q, c); err != nil {
		return err
	}
	return drainContactEventsToOutbox(ctx, tx, c)
}

// UpdateByID satisfies [unverifiedcontact.Repository].
func (r *UnverifiedContactRepository) UpdateByID(
	ctx context.Context,
	id unverifiedcontact.ID,
	updateFn func(*unverifiedcontact.UnverifiedContact) (bool, error),
) error {
	run := func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		c, err := loadUnverifiedContact(ctx, q, id)
		if err != nil {
			return err
		}
		shouldPersist, err := updateFn(c)
		if err != nil {
			return err
		}
		if !shouldPersist {
			return nil
		}
		if err := updateUnverifiedContactRow(ctx, q, c); err != nil {
			return err
		}
		return drainContactEventsToOutbox(ctx, tx, c)
	}
	if tx, ok := pg.TxFromContext(ctx); ok {
		return run(ctx, tx)
	}
	return r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, run)
}

// GetByID satisfies [unverifiedcontact.Repository]. Honours any active
// tx in ctx so a sequence inside one UoW closure (mutate, then re-load
// for projection) sees its own writes.
func (r *UnverifiedContactRepository) GetByID(ctx context.Context, id unverifiedcontact.ID) (*unverifiedcontact.UnverifiedContact, error) {
	q := r.q
	if tx, ok := pg.TxFromContext(ctx); ok {
		q = r.q.WithTx(tx)
	}
	return loadUnverifiedContact(ctx, q, id)
}

// ----- Mappers --------------------------------------------------------------

func loadUnverifiedContact(ctx context.Context, q *db.Queries, id unverifiedcontact.ID) (*unverifiedcontact.UnverifiedContact, error) {
	uid, err := uuid.Parse(id.String())
	if err != nil {
		return nil, fmt.Errorf("unverified contact repo: parse id %q: %w", id, err)
	}
	row, err := q.GetUnverifiedContactByID(ctx, pgUUID(uid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, unverifiedcontact.ErrNotFound
		}
		return nil, fmt.Errorf("unverified contact repo: get: %w", err)
	}
	return rowToUnverifiedContact(row)
}

func insertUnverifiedContactRow(ctx context.Context, q *db.Queries, c *unverifiedcontact.UnverifiedContact) error {
	uid, err := uuid.Parse(c.ID().String())
	if err != nil {
		return fmt.Errorf("unverified contact repo: parse id: %w", err)
	}
	createdByID, err := uuid.Parse(c.CreatedByMembershipID().String())
	if err != nil {
		return fmt.Errorf("unverified contact repo: parse createdBy: %w", err)
	}
	f := c.Form()
	err = q.InsertUnverifiedContact(ctx, db.InsertUnverifiedContactParams{
		ID:                     pgUUID(uid),
		State:                  c.State().String(),
		RejectionReason:        c.RejectionReason(),
		BusyCallbackAt:         pgTimestamp(c.BusyCallbackAt()),
		BusyCallbackEndAt:      pgTimestamp(c.BusyCallbackEndAt()),
		PlatformLeadID:         pgUUIDOptStr(c.PlatformLeadID()),
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
		HasPan:                 f.HasPan(),
		PanNumber:              f.PanNumber(),
		BusinessType:           string(f.BusinessType()),
		MedicineSystem:         string(f.MedicineSystem()),
		ProductRanges:          f.ProductRanges(),
		DosageForms:            f.DosageForms(),
		OrderValue:             string(f.OrderValue()),
		BuyTimeline:            string(f.BuyTimeline()),
		CreatedAt:              pgRequiredTimestamp(c.CreatedAt()),
		CreatedByMembershipID:  pgUUID(createdByID),
		VerifiedAt:             pgTimestamp(c.VerifiedAt()),
		VerifiedByMembershipID: pgUUIDOptStr(c.VerifiedByMembershipID().String()),
		RejectedAt:             pgTimestamp(c.RejectedAt()),
		RejectedByMembershipID: pgUUIDOptStr(c.RejectedByMembershipID().String()),
	})
	if err != nil {
		return fmt.Errorf("unverified contact repo: insert: %w", err)
	}
	return nil
}

func updateUnverifiedContactRow(ctx context.Context, q *db.Queries, c *unverifiedcontact.UnverifiedContact) error {
	uid, err := uuid.Parse(c.ID().String())
	if err != nil {
		return fmt.Errorf("unverified contact repo: parse id: %w", err)
	}
	err = q.UpdateUnverifiedContact(ctx, db.UpdateUnverifiedContactParams{
		ID:                     pgUUID(uid),
		State:                  c.State().String(),
		RejectionReason:        c.RejectionReason(),
		BusyCallbackAt:         pgTimestamp(c.BusyCallbackAt()),
		BusyCallbackEndAt:      pgTimestamp(c.BusyCallbackEndAt()),
		PlatformLeadID:         pgUUIDOptStr(c.PlatformLeadID()),
		VerifiedAt:             pgTimestamp(c.VerifiedAt()),
		VerifiedByMembershipID: pgUUIDOptStr(c.VerifiedByMembershipID().String()),
		RejectedAt:             pgTimestamp(c.RejectedAt()),
		RejectedByMembershipID: pgUUIDOptStr(c.RejectedByMembershipID().String()),
	})
	if err != nil {
		return fmt.Errorf("unverified contact repo: update: %w", err)
	}
	return nil
}

func rowToUnverifiedContact(row db.PlatformUnverifiedContact) (*unverifiedcontact.UnverifiedContact, error) {
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
	state := unverifiedcontact.State(row.State)
	if !state.IsValid() {
		return nil, fmt.Errorf("unverified contact repo: invalid state %q", row.State)
	}
	return unverifiedcontact.UnmarshalFromDB(unverifiedcontact.Snapshot{
		ID:                     unverifiedcontact.ID(uuidFromPg(row.ID).String()),
		Form:                   form,
		State:                  state,
		RejectionReason:        row.RejectionReason,
		BusyCallbackAt:         timeFromPg(row.BusyCallbackAt),
		BusyCallbackEndAt:      timeFromPg(row.BusyCallbackEndAt),
		PlatformLeadID:         uuidStringIfValid(row.PlatformLeadID),
		CreatedAt:              timeFromPg(row.CreatedAt),
		CreatedByMembershipID:  unverifiedcontact.MembershipID(uuidFromPg(row.CreatedByMembershipID).String()),
		VerifiedAt:             timeFromPg(row.VerifiedAt),
		VerifiedByMembershipID: unverifiedcontact.MembershipID(uuidStringIfValid(row.VerifiedByMembershipID)),
		RejectedAt:             timeFromPg(row.RejectedAt),
		RejectedByMembershipID: unverifiedcontact.MembershipID(uuidStringIfValid(row.RejectedByMembershipID)),
	}), nil
}

// drainContactEventsToOutbox pulls events off the aggregate, maps each
// through the integration-event mapper, + writes to the outbox.
func drainContactEventsToOutbox(ctx context.Context, tx pgx.Tx, c *unverifiedcontact.UnverifiedContact) error {
	evs := c.PullEvents()
	if len(evs) == 0 {
		return nil
	}
	asAny := make([]any, len(evs))
	for i, e := range evs {
		asAny[i] = e
	}
	// UnverifiedContact is Platform-scoped → tenant_id = uuid.Nil on
	// outbox rows.
	return drainEventsToOutbox(ctx, tx, uuid.Nil, asAny)
}

// pgUUIDOptStr wraps a string-shaped optional UUID into pgtype.UUID.
// Empty string → Valid=false.
func pgUUIDOptStr(s string) pgtype.UUID {
	if s == "" {
		return pgtype.UUID{}
	}
	id, err := uuid.Parse(s)
	if err != nil {
		// Storage path — caller guarantees UUID shape from domain types.
		return pgtype.UUID{}
	}
	return pgtype.UUID{Bytes: id, Valid: true}
}

// uuidStringIfValid returns the string form of pg.UUID or "" when invalid.
func uuidStringIfValid(p pgtype.UUID) string {
	if !p.Valid {
		return ""
	}
	return uuid.UUID(p.Bytes).String()
}
