package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/druglicence"
	"github.com/leadkart/leadkart-go/internal/common/gst"
	"github.com/leadkart/leadkart-go/internal/common/pan"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/common/phone"
	"github.com/leadkart/leadkart-go/internal/common/postaladdress"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/adapters/db"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// TenantRepository is the pgx/sqlc-backed [tenant.Repository]. Tenant is
// a global aggregate (non-RLS); writes run under platform scope to satisfy
// the outbox RLS WITH CHECK policy.
type TenantRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries // direct (read path); writes go through tx.WithinTx + WithTx
}

// NewTenantRepository wires the repository against a connection pool +
// transactor.
func NewTenantRepository(pool *pgxpool.Pool, tx *pg.Transactor) *TenantRepository {
	return &TenantRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// Add satisfies [tenant.Repository]. Joins an ambient UoW tx when present
// (canonical multi-aggregate composition path for RegisterTenant).
func (r *TenantRepository) Add(ctx context.Context, t *tenant.Tenant) error {
	return r.runInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return r.addOnTx(ctx, tx, t)
	})
}

// addOnTx inserts the tenant row and drains events. Unexported.
func (r *TenantRepository) addOnTx(ctx context.Context, tx pgx.Tx, t *tenant.Tenant) error {
	q := r.q.WithTx(tx)
	if err := insertTenantRow(ctx, q, t); err != nil {
		return err
	}
	return drainTenantEvents(ctx, tx, t)
}

// runInTx joins an ambient UoW tx when present, else opens a platform-scoped
// tx (ADR 0067 Phase-4 UoW-join contract).
func (r *TenantRepository) runInTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return fn(ctx, tx)
	}
	return r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, fn)
}

// UpdateByID satisfies [tenant.Repository] — TDL Sep 2024 UpdateFn.
func (r *TenantRepository) UpdateByID(
	ctx context.Context,
	id tenant.ID,
	updateFn func(*tenant.Tenant) (bool, error),
) error {
	return r.runInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
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
		// Tenant.HardDelete() sets HardDeletedAt. Persist via UpdateTenant;
		// the physical DELETE is the post-saga HardDeleteRow path.
		if err := persistTenant(ctx, q, t); err != nil {
			return err
		}
		return drainTenantEvents(ctx, tx, t)
	})
}

// GetByID satisfies [tenant.Repository].
func (r *TenantRepository) GetByID(ctx context.Context, id tenant.ID) (*tenant.Tenant, error) {
	return loadTenant(ctx, r.q, id)
}

// GetBySlug satisfies [tenant.Repository].
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

// ListAll satisfies [tenant.Repository]. Cross-tenant — Platform-only.
func (r *TenantRepository) ListAll(ctx context.Context) ([]*tenant.Tenant, error) {
	rows, err := r.q.ListAllTenants(ctx)
	if err != nil {
		return nil, fmt.Errorf("tenant repo: list all: %w", err)
	}
	out := make([]*tenant.Tenant, 0, len(rows))
	for _, row := range rows {
		t, err := rowToTenant(row)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// HardDeleteRow issues the physical DELETE for the post-saga cleanup.
// Caller must have invoked Tenant.HardDelete() first. Platform-tier only.
func (r *TenantRepository) HardDeleteRow(ctx context.Context, id tenant.ID) error {
	uid, err := parseTenantID(id)
	if err != nil {
		return err
	}
	if err := r.q.HardDeleteTenant(ctx, pgconv.PgUUID(uid)); err != nil {
		return fmt.Errorf("tenant repo: hard delete: %w", err)
	}
	return nil
}

// ----- Helpers ---------------------------------------------------------------

func loadTenant(ctx context.Context, q *db.Queries, id tenant.ID) (*tenant.Tenant, error) {
	uid, err := parseTenantID(id)
	if err != nil {
		return nil, err
	}
	row, err := q.GetTenantByID(ctx, pgconv.PgUUID(uid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, tenant.ErrNotFound
		}
		return nil, fmt.Errorf("tenant repo: get by id: %w", err)
	}
	return rowToTenant(row)
}

func insertTenantRow(ctx context.Context, q *db.Queries, t *tenant.Tenant) error {
	uid, err := parseTenantID(t.ID())
	if err != nil {
		return err
	}
	stat := t.Statutory()
	contact := t.AdminContact()
	addr := contact.Address()
	policy := t.Settings().PasswordPolicy()
	prefs := t.DisplayPreferences()
	err = q.InsertTenant(ctx, db.InsertTenantParams{
		ID:                        pgconv.PgUUID(uid),
		Slug:                      t.Slug().String(),
		LegalName:                 t.LegalName(),
		DisplayName:               t.DisplayName(),
		Status:                    t.Status().String(),
		CreatedAt:                 pgconv.PgRequiredTimestamp(t.CreatedAt()),
		GstNumber:                 stat.GST().String(),
		PanNumber:                 stat.PAN().String(),
		DrugLicenceNumber:         stat.DrugLicence().String(),
		AdminPhone:                contact.Phone().String(),
		AdminAddressStreet:        addr.Street(),
		AdminAddressCity:          addr.City(),
		AdminAddressDistrict:      addr.District(),
		AdminAddressState:         addr.State(),
		AdminAddressStateCode:     addr.StateCode(),
		AdminAddressPincode:       addr.Pincode(),
		PasswordMinLength:         passwordPolicyInt32(policy.MinLength()),
		PasswordRequireUppercase:  policy.RequireUppercase(),
		PasswordRequireLowercase:  policy.RequireLowercase(),
		PasswordRequireDigit:      policy.RequireDigit(),
		PasswordRequireSymbol:     policy.RequireSymbol(),
		PasswordMaxFailedAttempts: passwordPolicyInt32(policy.MaxFailedAttempts()),
		PasswordLockoutMinutes:    passwordPolicyInt32(policy.LockoutMinutes()),
		Locale:                    prefs.Locale(),
		TimeZone:                  prefs.TimeZone(),
		DateFormat:                prefs.DateFormat(),
		Currency:                  prefs.Currency(),
		DeletionScheduledAt:       pgconv.PgTimestamp(t.DeletionScheduledAt()),
		DeletionReason:            t.DeletionReason(),
		HardDeletedAt:             pgconv.PgTimestamp(t.HardDeletedAt()),
	})
	if err != nil {
		if isUniqueViolation(err) {
			return tenant.ErrSlugTaken
		}
		return fmt.Errorf("tenant repo: insert: %w", err)
	}
	return nil
}

func persistTenant(ctx context.Context, q *db.Queries, t *tenant.Tenant) error {
	uid, err := parseTenantID(t.ID())
	if err != nil {
		return err
	}
	stat := t.Statutory()
	contact := t.AdminContact()
	addr := contact.Address()
	policy := t.Settings().PasswordPolicy()
	prefs := t.DisplayPreferences()
	err = q.UpdateTenant(ctx, db.UpdateTenantParams{
		ID:                        pgconv.PgUUID(uid),
		LegalName:                 t.LegalName(),
		DisplayName:               t.DisplayName(),
		Status:                    t.Status().String(),
		ActivatedAt:               pgconv.PgTimestamp(t.ActivatedAt()),
		SuspendedAt:               pgconv.PgTimestamp(t.SuspendedAt()),
		GstNumber:                 stat.GST().String(),
		PanNumber:                 stat.PAN().String(),
		DrugLicenceNumber:         stat.DrugLicence().String(),
		AdminPhone:                contact.Phone().String(),
		AdminAddressStreet:        addr.Street(),
		AdminAddressCity:          addr.City(),
		AdminAddressDistrict:      addr.District(),
		AdminAddressState:         addr.State(),
		AdminAddressStateCode:     addr.StateCode(),
		AdminAddressPincode:       addr.Pincode(),
		PasswordMinLength:         passwordPolicyInt32(policy.MinLength()),
		PasswordRequireUppercase:  policy.RequireUppercase(),
		PasswordRequireLowercase:  policy.RequireLowercase(),
		PasswordRequireDigit:      policy.RequireDigit(),
		PasswordRequireSymbol:     policy.RequireSymbol(),
		PasswordMaxFailedAttempts: passwordPolicyInt32(policy.MaxFailedAttempts()),
		PasswordLockoutMinutes:    passwordPolicyInt32(policy.LockoutMinutes()),
		Locale:                    prefs.Locale(),
		TimeZone:                  prefs.TimeZone(),
		DateFormat:                prefs.DateFormat(),
		Currency:                  prefs.Currency(),
		DeletionScheduledAt:       pgconv.PgTimestamp(t.DeletionScheduledAt()),
		DeletionReason:            t.DeletionReason(),
		HardDeletedAt:             pgconv.PgTimestamp(t.HardDeletedAt()),
	})
	if err != nil {
		return fmt.Errorf("tenant repo: update: %w", err)
	}
	return nil
}

// drainTenantEvents maps aggregate events to V1 integration events and
// writes them to the outbox.
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

// rowToTenant projects a sqlc [db.IdentityTenant] row into the aggregate.
// Single projector covers GetTenantByID, GetTenantBySlug, and ListAllTenants.
//
//nolint:gocyclo // Mechanical projection — splitting into helpers adds no clarity.
func rowToTenant(row db.IdentityTenant) (*tenant.Tenant, error) {
	tID := tenant.ID(pgconv.UUIDFromPg(row.ID).String())
	s, err := slug.New(row.Slug)
	if err != nil {
		return nil, fmt.Errorf("tenant repo: hydrate slug %q: %w", row.Slug, err)
	}
	status, err := tenant.ParseStatus(row.Status)
	if err != nil {
		return nil, fmt.Errorf("tenant repo: hydrate status %q: %w", row.Status, err)
	}
	stat, err := buildStatutory(row.GstNumber, row.PanNumber, row.DrugLicenceNumber)
	if err != nil {
		return nil, fmt.Errorf("tenant repo: hydrate statutory: %w", err)
	}
	contact, err := buildAdminContact(
		row.AdminPhone,
		row.AdminAddressStreet, row.AdminAddressCity, row.AdminAddressDistrict,
		row.AdminAddressState, row.AdminAddressStateCode, row.AdminAddressPincode,
	)
	if err != nil {
		return nil, fmt.Errorf("tenant repo: hydrate admin_contact: %w", err)
	}
	settings, err := buildSettings(
		int(row.PasswordMinLength),
		row.PasswordRequireUppercase, row.PasswordRequireLowercase,
		row.PasswordRequireDigit, row.PasswordRequireSymbol,
		int(row.PasswordMaxFailedAttempts), int(row.PasswordLockoutMinutes),
	)
	if err != nil {
		return nil, fmt.Errorf("tenant repo: hydrate settings: %w", err)
	}
	prefs, err := buildDisplayPreferences(row.Locale, row.TimeZone, row.DateFormat, row.Currency)
	if err != nil {
		return nil, fmt.Errorf("tenant repo: hydrate display_preferences: %w", err)
	}

	return tenant.UnmarshalFromDB(tenant.Snapshot{
		ID:                  tID,
		Slug:                s,
		LegalName:           row.LegalName,
		DisplayName:         row.DisplayName,
		Status:              status,
		Statutory:           stat,
		AdminContact:        contact,
		Settings:            settings,
		DisplayPreferences:  prefs,
		CreatedAt:           pgconv.TimeFromPg(row.CreatedAt),
		ActivatedAt:         pgconv.TimeFromPg(row.ActivatedAt),
		SuspendedAt:         pgconv.TimeFromPg(row.SuspendedAt),
		DeletionScheduledAt: pgconv.TimeFromPg(row.DeletionScheduledAt),
		DeletionReason:      row.DeletionReason,
		HardDeletedAt:       pgconv.TimeFromPg(row.HardDeletedAt),
	}), nil
}

// buildStatutory rehydrates [tenant.Statutory] from possibly-empty column
// strings. All-empty returns a zero VO; partial state may surface validation
// errors (e.g. PAN/GST mismatch from corrupted historical data).
func buildStatutory(gstStr, panStr, drugLicenceStr string) (tenant.Statutory, error) {
	if gstStr == "" && panStr == "" && drugLicenceStr == "" {
		return tenant.Statutory{}, nil
	}
	var (
		g   gst.Number
		p   pan.Number
		dl  druglicence.Number
		err error
	)
	if gstStr != "" {
		if g, err = gst.New(gstStr); err != nil {
			return tenant.Statutory{}, err
		}
	}
	if panStr != "" {
		if p, err = pan.New(panStr); err != nil {
			return tenant.Statutory{}, err
		}
	}
	if drugLicenceStr != "" {
		if dl, err = druglicence.New(drugLicenceStr); err != nil {
			return tenant.Statutory{}, err
		}
	}
	return tenant.NewStatutory(g, p, dl)
}

func buildAdminContact(phoneStr, street, city, district, state, stateCode, pincode string) (tenant.AdminContact, error) {
	allEmpty := phoneStr == "" && street == "" && city == "" && district == "" &&
		state == "" && stateCode == "" && pincode == ""
	if allEmpty {
		return tenant.AdminContact{}, nil
	}
	var (
		ph   phone.Number
		addr postaladdress.Address
		err  error
	)
	if phoneStr != "" {
		if ph, err = phone.New(phoneStr); err != nil {
			return tenant.AdminContact{}, err
		}
	}
	if pincode != "" || city != "" || street != "" {
		if addr, err = postaladdress.New(street, city, district, state, stateCode, pincode); err != nil {
			return tenant.AdminContact{}, err
		}
	}
	return tenant.NewAdminContact(ph, addr), nil
}

func buildSettings(minLen int, reqUpper, reqLower, reqDigit, reqSymbol bool, maxFailed, lockoutMin int) (tenant.Settings, error) {
	if minLen == 0 && !reqUpper && !reqLower && !reqDigit && !reqSymbol && maxFailed == 0 && lockoutMin == 0 {
		return tenant.Settings{}, nil
	}
	policy, err := tenant.NewPasswordPolicy(minLen, reqUpper, reqLower, reqDigit, reqSymbol, maxFailed, lockoutMin)
	if err != nil {
		return tenant.Settings{}, err
	}
	return tenant.NewSettings(policy), nil
}

func buildDisplayPreferences(locale, tz, dateFormat, currency string) (tenant.DisplayPreferences, error) {
	if locale == "" && tz == "" && dateFormat == "" && currency == "" {
		return tenant.DisplayPreferences{}, nil
	}
	return tenant.NewDisplayPreferences(locale, tz, dateFormat, currency)
}

// parseTenantID parses the domain ID into a uuid.UUID.
func parseTenantID(id tenant.ID) (uuid.UUID, error) {
	parsed, err := uuid.Parse(id.String())
	if err != nil {
		return uuid.Nil, fmt.Errorf("tenant repo: parse id %q: %w", id, err)
	}
	return parsed, nil
}

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE [pg.SQLStateUniqueViolation]).
func isUniqueViolation(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == pg.SQLStateUniqueViolation
}

// passwordPolicyInt32 narrows a domain-validated int to int32.
// The aggregate VO has already enforced the range ([0, 1440]); an
// out-of-range value here means in-process corruption — panic is correct.
func passwordPolicyInt32(v int) int32 {
	if v < 0 || v > 1<<30 {
		panic(fmt.Sprintf("tenant repo: password policy int %d out of bounds — domain VO invariant violated", v))
	}
	return int32(v)
}
