// Package commercesettings owns the [Settings] aggregate: one company's
// versioned commerce policy (companysettings module, ADR 0070).
//
// Every accepted change moves the policy to a new version, records who and why,
// and emits ONE [PolicyChangedEvent] carrying the whole policy. Orders and
// Inventory keep event-fed replicas of it. A save names the version it was
// based on and a stale one is refused ([ErrVersionConflict]), so two screens
// can never silently overwrite each other.
//
// A tenant with no stored policy reads the defaults at version 0 through
// commercepolicy.Initial, the one function every reader uses.
package commercesettings

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/common/textlimit"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// MaxGSTRates caps a company's GST rate list. A company confirms a handful of
// rates; the cap keeps the list (part of the policy every event carries) small.
const MaxGSTRates = 50

// MaxReasonChars is the policy_history.reason column limit, in characters.
const MaxReasonChars = 500

// ReasonInitial is the history reason of the seeded first version.
const ReasonInitial = "initial policy"

// TaxKeyGSTRates is the changed-key name of an edit to the GST rate list.
const TaxKeyGSTRates = commercepolicy.ManagedElsewhereKey

// ErrInvalid is the sentinel for a constructor invariant violation.
var ErrInvalid = errors.New("commercesettings: invalid")

// ErrNotFound is the repository's internal "no stored row" answer; the
// repository turns it into the defaults at version 0 (commercepolicy.Initial).
var ErrNotFound = errors.New("commercesettings: not found")

// ErrVersionConflict is matched by a [VersionConflictError]: the save was based
// on a version that is no longer current. Map to HTTP 409
// settings_version_conflict.
var ErrVersionConflict = errors.New("commercesettings: version conflict")

// ErrAlreadySeeded means the tenant already has a stored policy; seeding twice is a
// no-op (a replayed tenant-registered event).
var ErrAlreadySeeded = errors.New("commercesettings: already seeded")

// ErrNothingToRepublish means no stored version exists to re-announce.
var ErrNothingToRepublish = errors.New("commercesettings: nothing to republish")

// ErrGSTRateNotFound means there is no rate with that id in the company's list. Map to 404.
var ErrGSTRateNotFound = errors.New("commercesettings: gst rate not found")

// VersionConflictError carries both versions so the client can reload.
type VersionConflictError struct {
	Expected, Current int64
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("commercesettings: version conflict (expected %d, current %d)", e.Expected, e.Current)
}

// Is makes errors.Is(err, ErrVersionConflict) match.
func (e *VersionConflictError) Is(t error) bool { return t == ErrVersionConflict }

// TaxManageRequiredError is returned when the patch changes tax-manage settings and the caller
// does not hold that permission. Map to HTTP 403.
type TaxManageRequiredError struct{ Keys []string }

func (e *TaxManageRequiredError) Error() string {
	return "commercesettings: tax-manage settings need the tax permission: " + strings.Join(e.Keys, ", ")
}

// ManageRequiredError is returned when the patch changes non-tax settings and
// the caller does not hold company_settings.manage. Map to HTTP 403.
type ManageRequiredError struct{ Keys []string }

func (e *ManageRequiredError) Error() string {
	return "commercesettings: these settings need the manage permission: " + strings.Join(e.Keys, ", ")
}

// Actor is who made a change: the acting membership and, during an
// impersonation session, the operator behind it. The zero Actor is the system
// (the seeded first version).
type Actor struct {
	MembershipID           membership.ID
	ImpersonatorOperatorID string
}

// Settings is the aggregate root: one row per tenant in
// companysettings.commerce_policy.
type Settings struct {
	tenantID  tenant.ID
	version   int64
	policy    commercepolicy.Policy
	updatedAt time.Time
	updatedBy membership.ID

	events []Event
}

// New returns the tenant's settings as a company with NO stored policy reads
// them: the defaults at version 0. It records no event.
func New(tenantID tenant.ID) (*Settings, error) {
	if _, err := uuid.Parse(tenantID.String()); err != nil {
		return nil, fmt.Errorf("%w: tenant_id not a valid uuid", ErrInvalid)
	}
	initial := commercepolicy.Initial()
	return &Settings{tenantID: tenantID, version: initial.Version, policy: initial.Policy}, nil
}

// Snapshot is the persistence DTO consumed by [UnmarshalFromDB].
type Snapshot struct {
	TenantID  tenant.ID
	Version   int64
	Policy    commercepolicy.Policy
	UpdatedAt time.Time
	UpdatedBy membership.ID
}

// UnmarshalFromDB rehydrates without validating or emitting events.
func UnmarshalFromDB(s Snapshot) *Settings {
	return &Settings{tenantID: s.TenantID, version: s.Version, policy: s.Policy, updatedAt: s.UpdatedAt, updatedBy: s.UpdatedBy}
}

// TenantID returns the owning tenant.
func (s *Settings) TenantID() tenant.ID { return s.tenantID }

// Version returns the stored version (0 when nothing is stored).
func (s *Settings) Version() int64 { return s.version }

// Policy returns a copy of the current policy.
func (s *Settings) Policy() commercepolicy.Policy { return s.policy.Clone() }

// UpdatedAt returns when the stored version was written (zero at version 0).
func (s *Settings) UpdatedAt() time.Time { return s.updatedAt }

// UpdatedBy returns the membership that wrote the stored version (empty for
// the system or at version 0).
func (s *Settings) UpdatedBy() membership.ID { return s.updatedBy }

// PullEvents returns and clears the recorded domain events.
func (s *Settings) PullEvents() []Event {
	out := s.events
	s.events = nil
	return out
}

// Seed stores the first version (the defaults) for a tenant that has none. The
// tenant-registered subscriber calls it; a replay finds the stored version and
// returns [ErrAlreadySeeded].
func (s *Settings) Seed(now time.Time) error {
	if s.version != 0 {
		return ErrAlreadySeeded
	}
	s.commit(Actor{}, ReasonInitial, nil, now)
	return nil
}

// ChangeInput is one PATCH.
type ChangeInput struct {
	// ExpectedVersion is the version the caller's form was loaded at.
	ExpectedVersion int64
	Changes         commercepolicy.Changes
	Reason          string
	Actor           Actor
	// MayManage and MayManageTax say whether the caller holds
	// company_settings.manage and company_settings.tax.manage; the route
	// decides them, the aggregate enforces them per changed key.
	MayManage    bool
	MayManageTax bool
	Now          time.Time
}

// ChangeResult says whether the patch moved the version.
type ChangeResult struct {
	// Changed is false when the patch matched the current policy.
	Changed bool
}

// Change applies a patch. Order of refusal: stale version, field errors (named
// per setting key), tax permission, reason. Nothing is applied on a refusal.
func (s *Settings) Change(in ChangeInput) (ChangeResult, error) {
	if in.ExpectedVersion != s.version {
		return ChangeResult{}, &VersionConflictError{Expected: in.ExpectedVersion, Current: s.version}
	}
	res, errs := commercepolicy.Apply(s.policy, in.Changes, in.Now)
	if errs != nil {
		return ChangeResult{}, errs.Err()
	}
	if len(res.Changed) == 0 {
		return ChangeResult{}, nil
	}
	taxKeys, otherKeys := splitTaxKeys(res.Changed)
	if len(taxKeys) > 0 && !in.MayManageTax {
		return ChangeResult{}, &TaxManageRequiredError{Keys: taxKeys}
	}
	if len(otherKeys) > 0 && !in.MayManage {
		return ChangeResult{}, &ManageRequiredError{Keys: otherKeys}
	}
	reason, err := checkReason(in.Reason, len(taxKeys) > 0)
	if err != nil {
		return ChangeResult{}, err
	}
	s.policy = res.Policy
	s.commit(in.Actor, reason, res.Changed, in.Now)
	return ChangeResult{Changed: true}, nil
}

// AddGSTRateInput is a new entry for the company's GST rate list.
type AddGSTRateInput struct {
	ID          string
	RateBps     int
	Label       string
	ConfirmedBy string
	ConfirmedOn string
	Reason      string
	Actor       Actor
	Now         time.Time
}

// AddGSTRate adds a confirmed, active rate. The same rate under the same label
// (case-insensitive) is refused as a duplicate; reactivate instead.
func (s *Settings) AddGSTRate(in AddGSTRateInput) error {
	if _, err := uuid.Parse(in.ID); err != nil {
		return fmt.Errorf("%w: gst rate id not a valid uuid", ErrInvalid)
	}
	errs := commercepolicy.CheckGSTRate(in.RateBps, in.Label, in.ConfirmedBy)
	if code := confirmedOnCode(in.ConfirmedOn, in.Now); code != "" {
		errs["confirmed_on"] = []string{code}
	}
	reason, rerr := checkReason(in.Reason, true)
	mergeReasonErr(errs, rerr)
	rate := commercepolicy.GSTRate{
		ID: in.ID, RateBps: in.RateBps, Label: commercepolicy.NormalizeLabel(in.Label),
		ConfirmedBy: strings.TrimSpace(in.ConfirmedBy), ConfirmedOn: in.ConfirmedOn, Active: true,
	}
	if slices.ContainsFunc(s.policy.Tax.GSTRates, func(r commercepolicy.GSTRate) bool { return commercepolicy.SameRate(r, rate) }) {
		errs["label"] = append(errs["label"], commercepolicy.CodeDuplicate)
	}
	if len(s.policy.Tax.GSTRates) >= MaxGSTRates {
		errs["rate_bps"] = append(errs["rate_bps"], commercepolicy.CodeTooMany)
	}
	if err := errs.Err(); err != nil {
		return err
	}
	next := s.policy.Clone()
	next.Tax.GSTRates = append(next.Tax.GSTRates, rate)
	commercepolicy.SortGSTRates(next.Tax.GSTRates)
	s.policy = next
	s.commit(in.Actor, reason, []string{TaxKeyGSTRates}, in.Now)
	return nil
}

// UpdateGSTRateInput edits a rate. A nil field is left alone. The rate itself
// never changes: add a new rate instead.
type UpdateGSTRateInput struct {
	ID          string
	Label       *string
	ConfirmedBy *string
	ConfirmedOn *string
	Active      *bool
	Reason      string
	Actor       Actor
	Now         time.Time
}

// UpdateGSTRate edits or deactivates a rate; a rate is never deleted. An edit
// that changes nothing is a no-op.
func (s *Settings) UpdateGSTRate(in UpdateGSTRateInput) error {
	idx := slices.IndexFunc(s.policy.Tax.GSTRates, func(r commercepolicy.GSTRate) bool { return r.ID == in.ID })
	if idx < 0 {
		return ErrGSTRateNotFound
	}
	rate := s.policy.Tax.GSTRates[idx]
	if in.Label != nil {
		rate.Label = commercepolicy.NormalizeLabel(*in.Label)
	}
	if in.ConfirmedBy != nil {
		rate.ConfirmedBy = strings.TrimSpace(*in.ConfirmedBy)
	}
	if in.ConfirmedOn != nil {
		rate.ConfirmedOn = *in.ConfirmedOn
	}
	if in.Active != nil {
		rate.Active = *in.Active
	}
	if rate == s.policy.Tax.GSTRates[idx] {
		return nil
	}
	errs := commercepolicy.CheckGSTRate(rate.RateBps, rate.Label, rate.ConfirmedBy)
	if code := confirmedOnCode(rate.ConfirmedOn, in.Now); code != "" {
		errs["confirmed_on"] = []string{code}
	}
	reason, rerr := checkReason(in.Reason, true)
	mergeReasonErr(errs, rerr)
	for i, o := range s.policy.Tax.GSTRates {
		if i != idx && commercepolicy.SameRate(o, rate) {
			errs["label"] = append(errs["label"], commercepolicy.CodeDuplicate)
		}
	}
	if err := errs.Err(); err != nil {
		return err
	}
	next := s.policy.Clone()
	next.Tax.GSTRates[idx] = rate
	commercepolicy.SortGSTRates(next.Tax.GSTRates)
	s.policy = next
	s.commit(in.Actor, reason, []string{TaxKeyGSTRates}, in.Now)
	return nil
}

// Republish re-announces the current stored version without changing it.
func (s *Settings) Republish(now time.Time) error {
	if s.version == 0 {
		return ErrNothingToRepublish
	}
	s.events = append(s.events, PolicyRepublishedEvent{TenantID: s.tenantID, Version: s.version, Policy: s.policy.Clone(), At: now})
	return nil
}

// commit moves to the next version and records the change event.
func (s *Settings) commit(actor Actor, reason string, changedKeys []string, now time.Time) {
	s.version++
	s.updatedAt = now
	s.updatedBy = actor.MembershipID
	s.events = append(s.events, PolicyChangedEvent{
		TenantID: s.tenantID, Version: s.version, Policy: s.policy.Clone(),
		ChangedKeys: slices.Clone(changedKeys), Reason: reason, Actor: actor, At: now,
	})
}

// splitTaxKeys separates the changed keys that need the tax permission from
// those that need the manage permission.
func splitTaxKeys(keys []string) (tax, other []string) {
	for _, k := range keys {
		if commercepolicy.RequiresTaxManage([]string{k}) {
			tax = append(tax, k)
		} else {
			other = append(other, k)
		}
	}
	return tax, other
}

// checkReason trims and bounds a reason; requiredForTax makes a blank one a
// refusal (the Tax group needs a reason on every change).
func checkReason(reason string, requiredForTax bool) (string, error) {
	reason = strings.TrimSpace(reason)
	errs := commercepolicy.FieldErrors{}
	switch err := textlimit.Chars("reason", reason, MaxReasonChars); {
	case errors.Is(err, textlimit.ErrInvalidCharacters):
		errs["reason"] = []string{commercepolicy.CodeInvalidCharacters}
	case errors.Is(err, textlimit.ErrTooLong):
		errs["reason"] = []string{commercepolicy.CodeTooLong}
	case reason == "" && requiredForTax:
		errs["reason"] = []string{commercepolicy.CodeRequired}
	}
	return reason, errs.Err()
}

// CodeInFuture is the refusal of a confirmation date later than today.
const CodeInFuture = commercepolicy.CodeInFuture

// confirmedOnCode returns the code a confirmation date breaks, or "". A date
// after tomorrow (UTC) is in the future whatever the company's time zone.
func confirmedOnCode(date string, now time.Time) string {
	if code := commercepolicy.CheckDate(date); code != "" {
		return code
	}
	d, _ := time.Parse(commercepolicy.DateLayout, date)
	if d.After(now.UTC().AddDate(0, 0, 1)) {
		return CodeInFuture
	}
	return ""
}

// mergeReasonErr folds a reason refusal into a field-error map.
func mergeReasonErr(errs commercepolicy.FieldErrors, err error) {
	for field, codes := range commercepolicy.Fields(err) {
		errs[field] = append(errs[field], codes...)
	}
}
