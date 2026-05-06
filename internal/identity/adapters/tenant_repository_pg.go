package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/platform/pg"
)

// TenantRepository is the pgx/sqlc-backed implementation of
// [tenant.Repository]. Tenant is a global aggregate (each row IS a
// tenant — non-RLS), so writes run under platform-scope to satisfy the
// outbox table's RLS WITH CHECK policy. Reads bypass the transactor
// since the tenants table has no policies attached.
//
// The repository owns domain↔row mapping; sqlc-generated *Queries hold
// the SQL. Aggregates stay framework-free.
type TenantRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *Queries // direct (read path); writes go through tx.WithinTx + WithTx
}

// NewTenantRepository wires the repository against a connection pool +
// transactor. The transactor MUST be backed by the same pool — composing
// distinct pools across one repository would split the connection state
// the GUC binds to.
func NewTenantRepository(pool *pgxpool.Pool, tx *pg.Transactor) *TenantRepository {
	return &TenantRepository{pool: pool, tx: tx, q: New(pool)}
}

// Add satisfies [tenant.Repository] — persists a brand-new tenant from
// [tenant.New], drains its events into the outbox, all in one tx under
// platform scope (the new tenant has no current_tenant context yet).
func (r *TenantRepository) Add(ctx context.Context, t *tenant.Tenant) error {
	return r.tx.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		return r.AddInTx(ctx, tx, t)
	})
}

// AddInTx persists a brand-new tenant under an EXISTING transaction.
// Caller (typically a multi-aggregate orchestrator like
// TenantOnboardingService) owns the tx + chooses the scope; this
// method is scope-agnostic and assumes the caller already opened the
// tx with the appropriate GUC binding.
//
// Per TDL TransactionProvider escape hatch in messaging.md G.H.1 — the
// transactor remains in the repository for the simple Add path; the
// orchestrator's WithinTx + AddInTx composition is the documented
// alternative for multi-aggregate atomic writes.
func (r *TenantRepository) AddInTx(ctx context.Context, tx pgx.Tx, t *tenant.Tenant) error {
	q := r.q.WithTx(tx)
	if err := insertTenantRow(ctx, q, t); err != nil {
		return err
	}
	return drainTenantEvents(ctx, tx, t)
}

// UpdateByID satisfies [tenant.Repository] — TDL Sep 2024 UpdateFn pattern.
// Loads → calls updateFn → persists (if shouldPersist) → drains events.
// All in one tx under platform scope.
func (r *TenantRepository) UpdateByID(
	ctx context.Context,
	id tenant.ID,
	updateFn func(*tenant.Tenant) (bool, error),
) error {
	return r.tx.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		t, err := loadTenant(ctx, q, id)
		if err != nil {
			return err
		}
		shouldPersist, err := updateFn(t)
		if err != nil {
			return err
		}
		if !shouldPersist {
			return nil
		}
		if err := persistTenantStatus(ctx, q, t); err != nil {
			return err
		}
		return drainTenantEvents(ctx, tx, t)
	})
}

// GetByID satisfies [tenant.Repository]. Read-only — no tx needed since
// the tenants table is non-RLS.
func (r *TenantRepository) GetByID(ctx context.Context, id tenant.ID) (*tenant.Tenant, error) {
	return loadTenant(ctx, r.q, id)
}

// GetBySlug satisfies [tenant.Repository]. Read-only.
func (r *TenantRepository) GetBySlug(ctx context.Context, s slug.Slug) (*tenant.Tenant, error) {
	row, err := r.q.GetTenantBySlug(ctx, s.String())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, tenant.ErrNotFound
		}
		return nil, fmt.Errorf("tenant repo: get by slug: %w", err)
	}
	return rowToTenant(row)
}

// ----- Helpers ---------------------------------------------------------------

func loadTenant(ctx context.Context, q *Queries, id tenant.ID) (*tenant.Tenant, error) {
	uid, err := parseTenantID(id)
	if err != nil {
		return nil, err
	}
	row, err := q.GetTenantByID(ctx, pgUUID(uid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, tenant.ErrNotFound
		}
		return nil, fmt.Errorf("tenant repo: get by id: %w", err)
	}
	return rowToTenant(row)
}

func insertTenantRow(ctx context.Context, q *Queries, t *tenant.Tenant) error {
	uid, err := parseTenantID(t.ID())
	if err != nil {
		return err
	}
	err = q.InsertTenant(ctx, InsertTenantParams{
		ID:          pgUUID(uid),
		Slug:        t.Slug().String(),
		LegalName:   t.LegalName(),
		DisplayName: t.DisplayName(),
		AdminEmail:  t.AdminEmail().String(),
		Status:      t.Status().String(),
		CreatedAt:   pgRequiredTimestamp(t.CreatedAt()),
	})
	if err != nil {
		if isUniqueViolation(err) {
			return tenant.ErrSlugTaken
		}
		return fmt.Errorf("tenant repo: insert: %w", err)
	}
	return nil
}

func persistTenantStatus(ctx context.Context, q *Queries, t *tenant.Tenant) error {
	uid, err := parseTenantID(t.ID())
	if err != nil {
		return err
	}
	err = q.UpdateTenantStatus(ctx, UpdateTenantStatusParams{
		ID:          pgUUID(uid),
		Status:      t.Status().String(),
		ActivatedAt: pgTimestamp(t.ActivatedAt()),
		SuspendedAt: pgTimestamp(t.SuspendedAt()),
	})
	if err != nil {
		return fmt.Errorf("tenant repo: update status: %w", err)
	}
	return nil
}

// drainTenantEvents pulls events off the aggregate, maps each through
// integrationevents.FromDomainEvent, and writes the resulting V1
// records to the outbox. No-op when PullEvents returns nil
// (e.g. idempotent transitions).
func drainTenantEvents(ctx context.Context, tx pgx.Tx, t *tenant.Tenant) error {
	evs := t.PullEvents()
	if len(evs) == 0 {
		return nil
	}
	uid, err := parseTenantID(t.ID())
	if err != nil {
		return err
	}
	asAny := make([]any, len(evs))
	for i, e := range evs {
		asAny[i] = e
	}
	mapped, err := mapDomainEvents(asAny)
	if err != nil {
		return fmt.Errorf("tenant repo: map events: %w", err)
	}
	return writeOutboxEvents(ctx, tx, uid, mapped)
}

// rowToTenant converts a sqlc-generated IdentityTenant into the domain
// aggregate via UnmarshalFromDB. Validation invariants are NOT
// re-checked — the row was valid when stored (TDL canon).
func rowToTenant(row IdentityTenant) (*tenant.Tenant, error) {
	id := tenant.ID(uuidFromPg(row.ID).String())

	s, err := slug.New(row.Slug)
	if err != nil {
		return nil, fmt.Errorf("tenant repo: hydrate slug %q: %w", row.Slug, err)
	}
	addr, err := email.New(row.AdminEmail)
	if err != nil {
		return nil, fmt.Errorf("tenant repo: hydrate admin_email %q: %w", row.AdminEmail, err)
	}
	status, err := tenant.ParseStatus(row.Status)
	if err != nil {
		return nil, fmt.Errorf("tenant repo: hydrate status %q: %w", row.Status, err)
	}
	return tenant.UnmarshalFromDB(tenant.Snapshot{
		ID:          id,
		Slug:        s,
		LegalName:   row.LegalName,
		DisplayName: row.DisplayName,
		AdminEmail:  addr,
		Status:      status,
		CreatedAt:   timeFromPg(row.CreatedAt),
		ActivatedAt: timeFromPg(row.ActivatedAt),
		SuspendedAt: timeFromPg(row.SuspendedAt),
	}), nil
}

// parseTenantID converts the domain ID string into a uuid.UUID.
// tenant.ID is a UUIDv7 string per ids.NewV7; failure means data
// corruption upstream and is reported as a wrapped error.
func parseTenantID(id tenant.ID) (uuid.UUID, error) {
	parsed, err := uuid.Parse(id.String())
	if err != nil {
		return uuid.Nil, fmt.Errorf("tenant repo: parse id %q: %w", id, err)
	}
	return parsed, nil
}

// isUniqueViolation reports whether err wraps a Postgres unique-constraint
// violation (SQLSTATE [pg.SQLStateUniqueViolation]).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pg.SQLStateUniqueViolation
}
