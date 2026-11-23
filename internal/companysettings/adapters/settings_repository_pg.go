package adapters

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/companysettings/adapters/db"
	"github.com/leadkart/leadkart-go/internal/companysettings/domain/commercesettings"
	"github.com/leadkart/leadkart-go/internal/companysettings/integrationevents"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// SettingsRepository is the pgx/sqlc-backed [commercesettings.Repository].
// Tenant-scoped: every read and write binds app.tenant_id through the
// transactor, so RLS gates the tables.
type SettingsRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewSettingsRepository wires the repository.
func NewSettingsRepository(pool *pgxpool.Pool, tx *pg.Transactor) *SettingsRepository {
	return &SettingsRepository{pool: pool, tx: tx, q: db.New(pool)}
}

var _ commercesettings.Repository = (*SettingsRepository)(nil)

// runInTx runs fn in the surrounding UoW tx when ctx carries one (a second
// pooled connection taken while the caller holds one deadlocks the pool), else
// in a fresh tenant-scoped tx (TxScopeTenant semantics: the GUC is bound from
// the explicit tenant id).
func (r *SettingsRepository) runInTx(ctx context.Context, tenantID tenant.ID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return fn(ctx, tx)
	}
	return r.tx.WithinTxPgxTenant(ctx, tenantID.String(), fn)
}

// GetByID satisfies [commercesettings.Repository]: the stored settings, or
// the defaults at version 0 for a tenant with no row.
func (r *SettingsRepository) GetByID(ctx context.Context, tenantID tenant.ID) (*commercesettings.Settings, error) {
	tid, err := uuid.Parse(tenantID.String())
	if err != nil {
		return nil, fmt.Errorf("settings repo: parse tenant id: %w", err)
	}
	var out *commercesettings.Settings
	err = r.runInTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		row, err := loadPolicy(ctx, q, tid)
		if errors.Is(err, commercesettings.ErrNotFound) {
			s, nerr := commercesettings.New(tenantID)
			out = s
			return nerr
		}
		if err != nil {
			return fmt.Errorf("settings repo: get policy: %w", err)
		}
		s, err := hydrate(ctx, q, row)
		out = s
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateByID satisfies [commercesettings.Repository] (TDL UpdateFn): lock
// the policy row (creating it at version 0 when absent), mutate, persist, write
// history, drain events to the outbox, all in one transaction.
func (r *SettingsRepository) UpdateByID(
	ctx context.Context,
	tenantID tenant.ID,
	mutator func(*commercesettings.Settings) (bool, error),
) error {
	tid, err := uuid.Parse(tenantID.String())
	if err != nil {
		return fmt.Errorf("settings repo: parse tenant id: %w", err)
	}
	return r.runInTx(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		row, err := lockPolicy(ctx, q, tid)
		if err != nil {
			return err
		}
		s, err := hydrate(ctx, q, row)
		if err != nil {
			return err
		}
		persist, err := mutator(s)
		if err != nil {
			return err
		}
		if !persist {
			return nil
		}
		if err := persistSettings(ctx, q, s); err != nil {
			return err
		}
		return drainSettingsEvents(ctx, q, tx, s)
	})
}

// loadPolicy reads the tenant's policy row, mapping a missing row to
// [commercesettings.ErrNotFound] (the caller reads the defaults instead).
func loadPolicy(ctx context.Context, q *db.Queries, tid uuid.UUID) (db.CompanysettingsCommercePolicy, error) {
	row, err := q.GetCommercePolicy(ctx, pgconv.PgUUID(tid))
	if errors.Is(err, pgx.ErrNoRows) {
		return row, commercesettings.ErrNotFound
	}
	return row, err
}

// getForUpdate row-locks the policy, mapping a missing row to
// [commercesettings.ErrNotFound].
func getForUpdate(ctx context.Context, q *db.Queries, pt pgtype.UUID) (db.CompanysettingsCommercePolicy, error) {
	row, err := q.GetCommercePolicyForUpdate(ctx, pt)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, commercesettings.ErrNotFound
	}
	return row, err
}

// lockPolicy row-locks the tenant's policy, creating the version-0 row first
// when none exists.
func lockPolicy(ctx context.Context, q *db.Queries, tid uuid.UUID) (db.CompanysettingsCommercePolicy, error) {
	pt := pgconv.PgUUID(tid)
	row, err := getForUpdate(ctx, q, pt)
	if !errors.Is(err, commercesettings.ErrNotFound) {
		return row, wrapRepoErr("lock policy", err)
	}
	raw, err := commercepolicy.Encode(commercepolicy.Initial().Policy)
	if err != nil {
		return db.CompanysettingsCommercePolicy{}, fmt.Errorf("settings repo: encode defaults: %w", err)
	}
	if err := q.InsertCommercePolicyIfAbsent(ctx, db.InsertCommercePolicyIfAbsentParams{TenantID: pt, Policy: raw}); err != nil {
		return db.CompanysettingsCommercePolicy{}, fmt.Errorf("settings repo: create version-0 row: %w", err)
	}
	row, err = getForUpdate(ctx, q, pt)
	return row, wrapRepoErr("lock created policy", err)
}

func wrapRepoErr(what string, err error) error {
	if err != nil {
		return fmt.Errorf("settings repo: %s: %w", what, err)
	}
	return nil
}

// hydrate rebuilds the aggregate: the stored settings plus the GST rate list.
func hydrate(ctx context.Context, q *db.Queries, row db.CompanysettingsCommercePolicy) (*commercesettings.Settings, error) {
	policy, err := commercepolicy.Decode(row.Policy)
	if err != nil {
		return nil, fmt.Errorf("settings repo: stored policy: %w", err)
	}
	rates, err := q.ListGSTRates(ctx, row.TenantID)
	if err != nil {
		return nil, fmt.Errorf("settings repo: list gst rates: %w", err)
	}
	policy.Tax.GSTRates = make([]commercepolicy.GSTRate, 0, len(rates))
	for _, rt := range rates {
		policy.Tax.GSTRates = append(policy.Tax.GSTRates, commercepolicy.GSTRate{
			ID: pgconv.UUIDFromPg(rt.ID).String(), RateBps: int(rt.RateBps), Label: rt.Label,
			ConfirmedBy: rt.ConfirmedBy, ConfirmedOn: pgconv.TimeFromPgDate(rt.ConfirmedOn).Format(commercepolicy.DateLayout),
			Active: rt.Active,
		})
	}
	commercepolicy.SortGSTRates(policy.Tax.GSTRates)
	var updatedBy membership.ID
	if row.UpdatedByMembershipID.Valid {
		updatedBy = membership.ID(pgconv.UUIDFromPg(row.UpdatedByMembershipID).String())
	}
	return commercesettings.UnmarshalFromDB(commercesettings.Snapshot{
		TenantID:  tenant.ID(pgconv.UUIDFromPg(row.TenantID).String()),
		Version:   row.Version,
		Policy:    policy,
		UpdatedAt: pgconv.TimeFromPg(row.UpdatedAt),
		UpdatedBy: updatedBy,
	}), nil
}

// storedForm is the policy as commerce_policy keeps it: the GST rate list
// lives in gst_rates, so it is stripped here.
func storedForm(p commercepolicy.Policy) ([]byte, error) {
	p = p.Clone()
	p.Tax.GSTRates = []commercepolicy.GSTRate{}
	return commercepolicy.Encode(p)
}

func persistSettings(ctx context.Context, q *db.Queries, s *commercesettings.Settings) error {
	tid, err := uuid.Parse(s.TenantID().String())
	if err != nil {
		return fmt.Errorf("settings repo: parse tenant id: %w", err)
	}
	policy := s.Policy()
	raw, err := storedForm(policy)
	if err != nil {
		return fmt.Errorf("settings repo: encode policy: %w", err)
	}
	updatedBy := pgtype.UUID{}
	if !s.UpdatedBy().IsZero() {
		by, perr := uuid.Parse(s.UpdatedBy().String())
		if perr != nil {
			return fmt.Errorf("settings repo: parse updated_by: %w", perr)
		}
		updatedBy = pgconv.PgUUID(by)
	}
	if err := q.UpdateCommercePolicy(ctx, db.UpdateCommercePolicyParams{
		TenantID: pgconv.PgUUID(tid), Version: s.Version(), Policy: raw,
		UpdatedAt: pgconv.PgRequiredTimestamp(s.UpdatedAt()), UpdatedByMembershipID: updatedBy,
	}); err != nil {
		return fmt.Errorf("settings repo: update policy: %w", err)
	}
	for _, rt := range policy.Tax.GSTRates {
		if err := upsertRate(ctx, q, tid, rt, s.UpdatedAt(), updatedBy); err != nil {
			return err
		}
	}
	return nil
}

func upsertRate(ctx context.Context, q *db.Queries, tid uuid.UUID, rt commercepolicy.GSTRate, now time.Time, createdBy pgtype.UUID) error {
	id, err := uuid.Parse(rt.ID)
	if err != nil {
		return fmt.Errorf("settings repo: parse gst rate id: %w", err)
	}
	on, err := time.Parse(commercepolicy.DateLayout, rt.ConfirmedOn)
	if err != nil {
		return fmt.Errorf("settings repo: parse confirmed_on: %w", err)
	}
	if err := q.UpsertGSTRate(ctx, db.UpsertGSTRateParams{
		ID: pgconv.PgUUID(id), TenantID: pgconv.PgUUID(tid), RateBps: int32(rt.RateBps), //nolint:gosec // 0..10000, checked by the policy
		Label: rt.Label, ConfirmedBy: rt.ConfirmedBy, ConfirmedOn: pgconv.PgDate(on), Active: rt.Active,
		CreatedAt: pgconv.PgRequiredTimestamp(now), CreatedByMembershipID: createdBy,
	}); err != nil {
		return fmt.Errorf("settings repo: upsert gst rate: %w", err)
	}
	return nil
}

// drainSettingsEvents writes the history row of every change and the outbox row
// of every event, in the aggregate's transaction.
func drainSettingsEvents(ctx context.Context, q *db.Queries, tx pgx.Tx, s *commercesettings.Settings) error {
	evs := s.PullEvents()
	if len(evs) == 0 {
		return nil
	}
	tid, err := uuid.Parse(s.TenantID().String())
	if err != nil {
		return fmt.Errorf("settings repo: parse tenant id for outbox: %w", err)
	}
	mapped := make([]integrationevents.Event, 0, len(evs))
	for _, e := range evs {
		if ch, ok := e.(commercesettings.PolicyChangedEvent); ok {
			if err := insertHistory(ctx, q, tid, ch); err != nil {
				return err
			}
		}
		ie, err := integrationevents.FromDomainEvent(e)
		if err != nil {
			return fmt.Errorf("settings repo: map event: %w", err)
		}
		mapped = append(mapped, ie)
	}
	return writeOutboxEvents(ctx, tx, tid, mapped)
}

func insertHistory(ctx context.Context, q *db.Queries, tid uuid.UUID, ev commercesettings.PolicyChangedEvent) error {
	raw, err := commercepolicy.Encode(ev.Policy)
	if err != nil {
		return fmt.Errorf("settings repo: encode history policy: %w", err)
	}
	actor := pgtype.UUID{}
	if !ev.Actor.MembershipID.IsZero() {
		a, perr := uuid.Parse(ev.Actor.MembershipID.String())
		if perr != nil {
			return fmt.Errorf("settings repo: parse actor: %w", perr)
		}
		actor = pgconv.PgUUID(a)
	}
	var imp *string
	if ev.Actor.ImpersonatorOperatorID != "" {
		imp = &ev.Actor.ImpersonatorOperatorID
	}
	keys := ev.ChangedKeys
	if keys == nil {
		keys = []string{}
	}
	if err := q.InsertPolicyHistory(ctx, db.InsertPolicyHistoryParams{
		TenantID: pgconv.PgUUID(tid), Version: ev.Version, Policy: raw, ChangedKeys: keys,
		ActorMembershipID: actor, ImpersonatorOperatorID: imp, Reason: ev.Reason,
		ChangedAt: pgconv.PgRequiredTimestamp(ev.At),
	}); err != nil {
		return fmt.Errorf("settings repo: insert history: %w", err)
	}
	return nil
}
