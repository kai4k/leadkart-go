// Package crmlead defines the CrmLead aggregate — the tenant-side
// lead-profile + lifecycle owner for the CRM module per ADR 0060.
//
// Per BRD §6.3 + the Identity model in `multi-tenancy.md`: CrmLead is
// tenant-scoped (every row carries a tenant_id; RLS+FORCE on the table).
// Construction is via [New] (factory enforcing invariants) for fresh
// leads, [NewFromPurchaseSnapshot] for the lead-purchased subscriber
// path, or [UnmarshalFromDB] for repository re-hydration.
//
// State machine: [Stage] forward + [Temperature] axis are INDEPENDENT.
// Aggregate methods emit one domain event per transition; the repository
// drains them via [PullEvents] when persisting.
package crmlead

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// validateUUIDString returns ErrInvalid wrapping a clear message when
// `val` is not a RFC 9562 UUID. Empty input is REJECTED (caller is
// responsible for "optional field" handling — pass the empty-string
// through validateOptionalUUID instead).
//
// Per ADR 0060 + reviewer finding H6: every domain ID stored on a
// CrmLead must parse as a UUID at AGGREGATE-CONSTRUCTION time, not
// later at the outbox boundary. This prevents the previous footgun
// where mustParseUUID(...) panicked from the request path on a
// malformed ID that snuck past command validation.
func validateUUIDString(name, val string) error {
	if val == "" {
		return fmt.Errorf("%w: %s required", ErrInvalid, name)
	}
	if _, err := uuid.Parse(val); err != nil {
		return fmt.Errorf("%w: %s not a valid uuid", ErrInvalid, name)
	}
	return nil
}

// validateOptionalUUID is the empty-allowed variant. Returns nil if
// val is empty; otherwise validates per validateUUIDString.
func validateOptionalUUID(name, val string) error {
	if val == "" {
		return nil
	}
	return validateUUIDString(name, val)
}

// ErrInvalid is the sentinel returned (wrapped via %w) by [New] +
// transition methods on invariant violation. Callers branch via
// [errors.Is] in error-mapping middleware.
var ErrInvalid = errs.New(errs.KindInvalidInput, "crm_lead", "invalid crm lead")

// ErrTerminal is returned by [ChangeStage] / [ChangeTemperature] /
// [Convert] / [Lose] / [Assign] / [Reassign] when the lead is in a
// terminal stage ([StageConverted] / [StageLost]). Caller maps to 409
// Conflict.
var ErrTerminal = errs.New(errs.KindConflict, "crm_lead", "lead is in a terminal stage")

// ID is the lead primary key — UUIDv7 string for B-tree locality.
type ID string

// IsZero reports whether ID is unset.
func (i ID) IsZero() bool { return i == "" }

// String returns the underlying UUID string.
func (i ID) String() string { return string(i) }

// Field length caps. Mirror the CHECK constraints in migration
// 20260602000001 so domain validation matches DB validation.
const (
	contactNameMin   = 1
	contactNameMax   = 200
	lostReasonMax    = 1000
	mobileE164Regex  = `^\+91[0-9]{10}$` // matches BRD §5 phone-format
	pincodeRegexLen  = 6
)

// Profile bundles the BRD §6.3 indexed columns + JSONB supplementary
// fields. Treated as a single VO at the aggregate boundary; the
// repository decomposes it into the per-column persistence shape.
//
// All filterable columns live as direct fields; non-filterable supplementary
// data (street / GST / PAN / email / notes) lives in [Extra]. The DB
// schema enforces the same split via dedicated columns + a single jsonb
// `extra_profile` column.
//
// Empty-string defaults match the migration's NOT NULL DEFAULT '' shape —
// the aggregate accepts partial profiles (manual import slice 2+ will use
// this).
type Profile struct {
	ContactName    string
	PhoneE164      string
	City           string
	District       string
	State          string
	Pincode        string
	BusinessType   string // "" | "PCD" | "ThirdParty"
	MedicineSystem string // "" | "Allopathic" | "Ayurvedic"
	OrderValue     string // "" | "Below5000" | "Upto25000" | "Upto50000" | "Above50000"
	BuyTimeline    string // "" | "WithinWeek" | "Within15Days" | "WithinMonth"
	HasDrugLicence bool
	HasGst         bool
	GstVerified    bool
	ProductRanges  []string
	DosageForms    []string

	Extra ExtraProfile // non-filterable supplementary fields (DB JSONB)
}

// ExtraProfile is the JSONB-backed supplementary-data carrier. Per BRD
// §6.3 + database.md "JSONB rule": never used in WHERE clauses.
type ExtraProfile struct {
	Street    string `json:"street,omitzero"`
	GstNumber string `json:"gst_number,omitzero"`
	PanNumber string `json:"pan_number,omitzero"`
	HasPan    bool   `json:"has_pan,omitzero"`
	Email     string `json:"email,omitzero"`
	Notes     string `json:"notes,omitzero"`
}

// CrmLead is the aggregate root.
//
// Invariants (enforced by [New] + state-transition methods):
//   - ID + TenantID non-zero; ContactName + PhoneE164 non-empty.
//   - Stage + Temperature are valid catalogue entries.
//   - Stage transitions follow the strict state machine + are blocked
//     once a terminal stage is reached.
//   - Convert / Lose are idempotent ONLY on self-transition (same actor +
//     same reason for Lose); otherwise rejected.
type CrmLead struct {
	id        ID
	tenantID  tenant.ID // owning tenant (typed alias prevents accidental ID swap)
	profile   Profile
	stage     Stage
	temperature Temperature

	// Source provenance — set for leads created via the lead-purchased
	// subscriber. Manual-import paths (slice 2+) leave these zero.
	sourcePurchaseID      string // platform.lead-purchased.v1 PurchaseID; UNIQUE on the table
	sourcePlatformLeadID  string // platform_leads.id

	// Current assignee (mirrored from the latest crm.assignment_history row).
	// Cleared (zero) when the lead has never been assigned.
	assigneeMembershipID string
	assignedAt           time.Time

	// Lifecycle metadata.
	convertedAt              time.Time
	convertedByMembershipID  string
	lostAt                   time.Time
	lostByMembershipID       string
	lostReason               string

	createdAt              time.Time
	createdByMembershipID  string // empty for subscriber-created leads

	events []Event
}

// New constructs a brand-new manually-entered CrmLead in [StageNew] +
// [TemperatureWarm]. Used by the (slice 2+) manual-import path; the
// subscriber path uses [NewFromPurchaseSnapshot] instead.
//
// On success, the lead has emitted a [CreatedEvent] which the repository
// drains via [PullEvents] when persisting.
//
// `now` is the injected wall-clock (Pure Domain canon — ADR 0047).
func New(id ID, tenantID tenant.ID, p Profile, createdByMembershipID string, now time.Time) (*CrmLead, error) {
	if id.IsZero() {
		return nil, fmt.Errorf("%w: id required", ErrInvalid)
	}
	if err := validateUUIDString("id", id.String()); err != nil {
		return nil, err
	}
	if err := validateUUIDString("tenant id", strings.TrimSpace(tenantID.String())); err != nil {
		return nil, err
	}
	if err := validateOptionalUUID("created by membership id", createdByMembershipID); err != nil {
		return nil, err
	}
	if err := validateProfile(p); err != nil {
		return nil, err
	}
	if now.IsZero() {
		return nil, fmt.Errorf("%w: now required", ErrInvalid)
	}
	l := &CrmLead{
		id:                    id,
		tenantID:              tenantID,
		profile:               p,
		stage:                 StageNew,
		temperature:           TemperatureWarm, // sensible default; product UX can override via ChangeTemperature
		createdAt:             now,
		createdByMembershipID: createdByMembershipID,
	}
	l.recordEvent(CreatedEvent{
		LeadID:                id,
		TenantID:              tenantID,
		SourcePurchaseID:      "",
		CreatedByMembershipID: createdByMembershipID,
		At:                    now,
	})
	return l, nil
}

// PurchaseSnapshot is the subscriber-side adapter for the Platform-emitted
// `platform.lead-purchased.v1` payload. Keeping it as a distinct type
// (vs reusing Profile) lets the subscriber pass through fields the
// aggregate ignores at slice 1 (e.g. AmountPaisa) without forcing
// the domain to know about pricing.
type PurchaseSnapshot struct {
	PurchaseID              string
	PlatformLeadID          string
	PurchasedByMembershipID string

	// All BRD §5 lead form fields — wire-stable strings matching the
	// platform.lead-purchased.v1 contract.
	ContactName    string
	MobileE164     string
	Email          string
	PinCode        string
	City           string
	District       string
	State          string
	Street         string
	HasDrugLicence bool
	HasGst         bool
	GstNumber      string
	HasPan         bool
	PanNumber      string
	BusinessType   string
	MedicineSystem string
	ProductRanges  []string
	DosageForms    []string
	OrderValue     string
	BuyTimeline    string
}

// NewFromPurchaseSnapshot is the subscriber-side factory. Used by the
// `platform.lead-purchased.v1` consumer when no existing CrmLead carries
// this PurchaseID (idempotency check via [Repository.GetBySourcePurchaseID]
// happens FIRST in the subscriber per ADR 0060).
//
// Initial state: [StageNew] + [TemperatureWarm]. Profile populated from
// the snapshot; provenance fields set; assignee left zero (auto-assign
// is a slice 2+ concern).
//
// `now` is the injected wall-clock (Pure Domain canon — ADR 0047).
func NewFromPurchaseSnapshot(id ID, tenantID tenant.ID, s PurchaseSnapshot, now time.Time) (*CrmLead, error) {
	if id.IsZero() {
		return nil, fmt.Errorf("%w: id required", ErrInvalid)
	}
	if err := validateUUIDString("id", id.String()); err != nil {
		return nil, err
	}
	if err := validateUUIDString("tenant id", strings.TrimSpace(tenantID.String())); err != nil {
		return nil, err
	}
	if err := validateUUIDString("snapshot purchase id", strings.TrimSpace(s.PurchaseID)); err != nil {
		return nil, err
	}
	if err := validateOptionalUUID("snapshot purchased by membership id", s.PurchasedByMembershipID); err != nil {
		return nil, err
	}
	p := Profile{
		ContactName:    s.ContactName,
		PhoneE164:      s.MobileE164,
		City:           s.City,
		District:       s.District,
		State:          s.State,
		Pincode:        s.PinCode,
		BusinessType:   s.BusinessType,
		MedicineSystem: s.MedicineSystem,
		OrderValue:     s.OrderValue,
		BuyTimeline:    s.BuyTimeline,
		HasDrugLicence: s.HasDrugLicence,
		HasGst:         s.HasGst,
		ProductRanges:  s.ProductRanges,
		DosageForms:    s.DosageForms,
		Extra: ExtraProfile{
			Street:    s.Street,
			GstNumber: s.GstNumber,
			PanNumber: s.PanNumber,
			HasPan:    s.HasPan,
			Email:     s.Email,
		},
	}
	if err := validateProfile(p); err != nil {
		return nil, err
	}
	if now.IsZero() {
		return nil, fmt.Errorf("%w: now required", ErrInvalid)
	}
	l := &CrmLead{
		id:                    id,
		tenantID:              tenantID,
		profile:               p,
		stage:                 StageNew,
		temperature:           TemperatureWarm,
		sourcePurchaseID:      s.PurchaseID,
		sourcePlatformLeadID:  s.PlatformLeadID,
		createdAt:             now,
		createdByMembershipID: s.PurchasedByMembershipID,
	}
	l.recordEvent(CreatedEvent{
		LeadID:                id,
		TenantID:              tenantID,
		SourcePurchaseID:      s.PurchaseID,
		CreatedByMembershipID: s.PurchasedByMembershipID,
		At:                    now,
	})
	return l, nil
}

// Snapshot is the persistence-layer DTO consumed by [UnmarshalFromDB].
// Adapter code scans DB rows into this struct, then re-hydrates via
// [UnmarshalFromDB] — keeps the adapter free of internal field knowledge.
type Snapshot struct {
	ID                       ID
	TenantID                 tenant.ID
	Profile                  Profile
	Stage                    Stage
	Temperature              Temperature
	SourcePurchaseID         string
	SourcePlatformLeadID     string
	AssigneeMembershipID     string
	AssignedAt               time.Time
	ConvertedAt              time.Time
	ConvertedByMembershipID  string
	LostAt                   time.Time
	LostByMembershipID       string
	LostReason               string
	CreatedAt                time.Time
	CreatedByMembershipID    string
}

// UnmarshalFromDB re-hydrates a CrmLead from persistence. Used ONLY by
// the repository on read paths — does NOT re-validate invariants per
// TDL canon (Wild Workouts Nov 2025).
func UnmarshalFromDB(s Snapshot) *CrmLead {
	return &CrmLead{
		id:                      s.ID,
		tenantID:                s.TenantID,
		profile:                 s.Profile,
		stage:                   s.Stage,
		temperature:             s.Temperature,
		sourcePurchaseID:        s.SourcePurchaseID,
		sourcePlatformLeadID:    s.SourcePlatformLeadID,
		assigneeMembershipID:    s.AssigneeMembershipID,
		assignedAt:              s.AssignedAt,
		convertedAt:             s.ConvertedAt,
		convertedByMembershipID: s.ConvertedByMembershipID,
		lostAt:                  s.LostAt,
		lostByMembershipID:      s.LostByMembershipID,
		lostReason:              s.LostReason,
		createdAt:               s.CreatedAt,
		createdByMembershipID:   s.CreatedByMembershipID,
	}
}

// ----- Getters --------------------------------------------------------------

// ID returns the lead primary key.
func (l *CrmLead) ID() ID { return l.id }

// TenantID returns the owning tenant ID.
func (l *CrmLead) TenantID() tenant.ID { return l.tenantID }

// Profile returns the lead's BRD §6.3 profile snapshot.
func (l *CrmLead) Profile() Profile { return l.profile }

// Stage returns the current stage in the lifecycle.
func (l *CrmLead) Stage() Stage { return l.stage }

// Temperature returns the current qualitative interest signal.
func (l *CrmLead) Temperature() Temperature { return l.temperature }

// SourcePurchaseID returns the Platform purchase-event ID that minted
// this lead, or empty for manual-import leads.
func (l *CrmLead) SourcePurchaseID() string { return l.sourcePurchaseID }

// SourcePlatformLeadID returns the originating platform_leads.id, or
// empty for manual-import leads.
func (l *CrmLead) SourcePlatformLeadID() string { return l.sourcePlatformLeadID }

// AssigneeMembershipID returns the current assignee, or empty when the
// lead has never been assigned.
func (l *CrmLead) AssigneeMembershipID() string { return l.assigneeMembershipID }

// AssignedAt returns the timestamp of the most recent assignment;
// zero when never assigned.
func (l *CrmLead) AssignedAt() time.Time { return l.assignedAt }

// ConvertedAt returns the terminal-Convert timestamp; zero when the
// lead is not in [StageConverted].
func (l *CrmLead) ConvertedAt() time.Time { return l.convertedAt }

// ConvertedByMembershipID returns the actor who closed the lead via
// Convert; empty when not in [StageConverted].
func (l *CrmLead) ConvertedByMembershipID() string { return l.convertedByMembershipID }

// LostAt returns the terminal-Lose timestamp; zero when the lead is
// not in [StageLost].
func (l *CrmLead) LostAt() time.Time { return l.lostAt }

// LostByMembershipID returns the actor who closed the lead via Lose.
func (l *CrmLead) LostByMembershipID() string { return l.lostByMembershipID }

// LostReason returns the audit reason supplied with Lose.
func (l *CrmLead) LostReason() string { return l.lostReason }

// CreatedAt returns the creation timestamp.
func (l *CrmLead) CreatedAt() time.Time { return l.createdAt }

// CreatedByMembershipID returns the actor who created the lead (manual
// import) or the purchaser membership (subscriber path). Empty for
// system-only paths.
func (l *CrmLead) CreatedByMembershipID() string { return l.createdByMembershipID }

// ----- State transitions ----------------------------------------------------

// Assign sets the current assignee. Used for both the FIRST assignment
// (when [AssigneeMembershipID] is zero) and explicit reassignment.
//
// Caller is the manager / admin issuing the change; the lead aggregate
// is the source of truth for the CURRENT assignee, while
// crm.assignment_history records the full audit trail (the App-layer
// command creates BOTH in one UoW — the aggregate emits the event, the
// command writes the history row).
//
// Idempotent: assigning the same member twice no-ops + returns nil.
// Refuses transitions on terminal stages.
//
// `reason` is optional — empty is allowed for the first assignment;
// reassignments SHOULD carry one for audit (the App layer can enforce).
//
// `now` is the injected wall-clock (Pure Domain canon — ADR 0047).
func (l *CrmLead) Assign(newAssignee, assignedBy, reason string, now time.Time) error {
	if err := validateUUIDString("assignee membership id", strings.TrimSpace(newAssignee)); err != nil {
		return err
	}
	if err := validateUUIDString("assigned-by membership id", strings.TrimSpace(assignedBy)); err != nil {
		return err
	}
	if l.stage.IsTerminal() {
		return fmt.Errorf("%w: stage=%s; assignment not allowed", ErrTerminal, l.stage)
	}
	if l.assigneeMembershipID == newAssignee {
		return nil // idempotent
	}
	if now.IsZero() {
		return fmt.Errorf("%w: now required", ErrInvalid)
	}
	previous := l.assigneeMembershipID
	l.assigneeMembershipID = newAssignee
	l.assignedAt = now
	l.recordEvent(AssignedEvent{
		LeadID:                 l.id,
		TenantID:               l.tenantID,
		PreviousAssignee:       previous,
		AssigneeMembershipID:   newAssignee,
		AssignedByMembershipID: assignedBy,
		Reason:                 reason,
		At:                     now,
	})
	return nil
}

// ChangeStage transitions to a new (non-terminal) stage per the state
// machine in [stage.go]. Convert / Lose use dedicated methods.
//
// Idempotent on self-transition. Rejects skips + backtracks + terminal
// transitions (caller must use Convert / Lose for those).
//
// `reason` is optional — sales executives may attach context for the
// audit log + downstream subscribers.
//
// `now` is the injected wall-clock (Pure Domain canon — ADR 0047).
func (l *CrmLead) ChangeStage(newStage Stage, changedBy, reason string, now time.Time) error {
	if !newStage.IsValid() {
		return fmt.Errorf("%w: target stage %q invalid", ErrInvalid, newStage)
	}
	if newStage == StageConverted || newStage == StageLost {
		return fmt.Errorf("%w: use Convert / Lose for terminal transitions", ErrInvalid)
	}
	if err := validateUUIDString("changed-by membership id", strings.TrimSpace(changedBy)); err != nil {
		return err
	}
	if l.stage.IsTerminal() {
		return fmt.Errorf("%w: stage=%s; stage change not allowed", ErrTerminal, l.stage)
	}
	if l.stage == newStage {
		return nil // idempotent no-op
	}
	if !canAdvance(l.stage, newStage) {
		return fmt.Errorf("%w: cannot transition %s → %s", ErrInvalid, l.stage, newStage)
	}
	if now.IsZero() {
		return fmt.Errorf("%w: now required", ErrInvalid)
	}
	old := l.stage
	l.stage = newStage
	l.recordEvent(StageChangedEvent{
		LeadID:                l.id,
		TenantID:              l.tenantID,
		OldStage:              old,
		NewStage:              newStage,
		ChangedByMembershipID: changedBy,
		Reason:                reason,
		At:                    now,
	})
	return nil
}

// ChangeTemperature transitions on the independent temperature axis.
// Allowed at any non-terminal stage. Idempotent on self-transition.
//
// `now` is the injected wall-clock (Pure Domain canon — ADR 0047).
func (l *CrmLead) ChangeTemperature(newTemp Temperature, changedBy string, now time.Time) error {
	if !newTemp.IsValid() {
		return fmt.Errorf("%w: target temperature %q invalid", ErrInvalid, newTemp)
	}
	if err := validateUUIDString("changed-by membership id", strings.TrimSpace(changedBy)); err != nil {
		return err
	}
	if l.stage.IsTerminal() {
		return fmt.Errorf("%w: stage=%s; temperature change not allowed", ErrTerminal, l.stage)
	}
	if l.temperature == newTemp {
		return nil // idempotent
	}
	if now.IsZero() {
		return fmt.Errorf("%w: now required", ErrInvalid)
	}
	old := l.temperature
	l.temperature = newTemp
	l.recordEvent(TemperatureChangedEvent{
		LeadID:                l.id,
		TenantID:              l.tenantID,
		OldTemperature:        old,
		NewTemperature:        newTemp,
		ChangedByMembershipID: changedBy,
		At:                    now,
	})
	return nil
}

// Convert is the terminal-success transition. Refuses if the lead is
// already in any terminal stage (no Lost → Converted resurrection).
//
// On success, the lead transitions to [StageConverted], records the
// timestamp + actor, and emits [ConvertedEvent] (which is the future
// Orders module's create-trigger per ADR 0060).
//
// `now` is the injected wall-clock (Pure Domain canon — ADR 0047).
func (l *CrmLead) Convert(convertedBy string, now time.Time) error {
	if err := validateUUIDString("converted-by membership id", strings.TrimSpace(convertedBy)); err != nil {
		return err
	}
	if l.stage.IsTerminal() {
		return fmt.Errorf("%w: stage=%s; conversion not allowed", ErrTerminal, l.stage)
	}
	if now.IsZero() {
		return fmt.Errorf("%w: now required", ErrInvalid)
	}
	l.stage = StageConverted
	l.convertedAt = now
	l.convertedByMembershipID = convertedBy
	l.recordEvent(ConvertedEvent{
		LeadID:                  l.id,
		TenantID:                l.tenantID,
		ConvertedByMembershipID: convertedBy,
		At:                      now,
	})
	return nil
}

// Lose is the terminal-failure transition. Refuses if the lead is
// already in any terminal stage.
//
// `reason` is REQUIRED (audit doctrine — `data-retention.md`); empty
// returns [ErrInvalid].
//
// `now` is the injected wall-clock (Pure Domain canon — ADR 0047).
func (l *CrmLead) Lose(lostBy, reason string, now time.Time) error {
	if err := validateUUIDString("lost-by membership id", strings.TrimSpace(lostBy)); err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: lose reason required for audit", ErrInvalid)
	}
	if len(reason) > lostReasonMax {
		return fmt.Errorf("%w: lose reason too long (max %d, got %d)", ErrInvalid, lostReasonMax, len(reason))
	}
	if l.stage.IsTerminal() {
		return fmt.Errorf("%w: stage=%s; lose not allowed", ErrTerminal, l.stage)
	}
	if now.IsZero() {
		return fmt.Errorf("%w: now required", ErrInvalid)
	}
	l.stage = StageLost
	l.lostAt = now
	l.lostByMembershipID = lostBy
	l.lostReason = reason
	l.recordEvent(LostEvent{
		LeadID:             l.id,
		TenantID:           l.tenantID,
		LostByMembershipID: lostBy,
		Reason:             reason,
		At:                 now,
	})
	return nil
}

// ----- Event handling -------------------------------------------------------

// PullEvents drains the recorded domain events and returns them.
// The repository calls this once per Save inside the same transaction
// that persists state, then forwards events to the outbox.
func (l *CrmLead) PullEvents() []Event {
	if len(l.events) == 0 {
		return nil
	}
	out := l.events
	l.events = nil
	return out
}

func (l *CrmLead) recordEvent(e Event) {
	l.events = append(l.events, e)
}

// ----- Validation helpers ---------------------------------------------------

func validateProfile(p Profile) error {
	if n := strings.TrimSpace(p.ContactName); len(n) < contactNameMin || len(n) > contactNameMax {
		return fmt.Errorf("%w: contact_name length %d not in [%d,%d]",
			ErrInvalid, len(n), contactNameMin, contactNameMax)
	}
	if !isValidIndianMobile(p.PhoneE164) {
		return fmt.Errorf("%w: phone_e164 must match %s, got %q", ErrInvalid, mobileE164Regex, p.PhoneE164)
	}
	if p.Pincode != "" && len(p.Pincode) != pincodeRegexLen {
		return fmt.Errorf("%w: pincode must be %d digits, got %q", ErrInvalid, pincodeRegexLen, p.Pincode)
	}
	if p.BusinessType != "" && p.BusinessType != "PCD" && p.BusinessType != "ThirdParty" {
		return fmt.Errorf("%w: business_type %q not in {PCD, ThirdParty}", ErrInvalid, p.BusinessType)
	}
	if p.MedicineSystem != "" && p.MedicineSystem != "Allopathic" && p.MedicineSystem != "Ayurvedic" {
		return fmt.Errorf("%w: medicine_system %q not in {Allopathic, Ayurvedic}", ErrInvalid, p.MedicineSystem)
	}
	return nil
}

// isValidIndianMobile checks the +91 + 10-digit shape without regex
// engine cost (hot path on the subscriber).
func isValidIndianMobile(s string) bool {
	if len(s) != 13 {
		return false
	}
	if s[0] != '+' || s[1] != '9' || s[2] != '1' {
		return false
	}
	for i := 3; i < 13; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
