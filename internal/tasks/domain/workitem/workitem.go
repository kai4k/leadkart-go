// Package workitem defines the WorkItem aggregate per BRD §6.8 —
// the Tasks module's only Phase C.2 aggregate.
//
// WorkItem is tenant-scoped (every row carries a tenant_id; RLS+FORCE
// on the table). Construction goes through [NewManual] (user-authored)
// or [NewAutoCreated] (subscriber-driven from CRM/Orders events) for
// fresh aggregates; [UnmarshalFromDB] for repository rehydration.
//
// State machine: [State] enforces the pending → in_progress →
// completed | cancelled flow, with the overdue scanner allowed to flip
// pending/in_progress → overdue when due_at slips. Aggregate methods
// emit one domain event per transition; the repository drains them via
// [PullEvents] on persist.
package workitem

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/errs"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ErrInvalid is the sentinel returned (wrapped via %w) by factories +
// transition methods on invariant violation. HTTP layer maps to 422.
var ErrInvalid = errs.New(errs.KindInvalidInput, "work_item", "invalid work item")

// ErrConflict is returned on illegal state-machine transitions
// (e.g. Start after Complete, Reassign after Cancel). HTTP maps to 409.
var ErrConflict = errs.New(errs.KindConflict, "work_item", "work item state transition not allowed")

// Field length bounds — mirror the CHECK constraints in migration
// 20260604000001 so domain validation matches DB validation.
const (
	titleMin              = 1
	titleMax              = 200
	descriptionMax        = 4000
	cancellationReasonMax = 1000
)

// ID is the work-item primary key — UUIDv7 string for B-tree locality.
type ID string

// IsZero reports whether ID is unset.
func (i ID) IsZero() bool { return i == "" }

// String returns the underlying UUID string.
func (i ID) String() string { return string(i) }

// Source captures the cross-module provenance of an auto-created task.
// Empty values indicate a manually-authored task.
type Source struct {
	Module     string // e.g. "crm", "orders", "inventory"
	EntityType string // e.g. "call_log", "crm_lead"
	EntityID   string // the foreign-key value as text (cross-schema reference)
}

// IsZero reports whether s carries no provenance (i.e. manual task).
func (s Source) IsZero() bool {
	return s.Module == "" && s.EntityType == "" && s.EntityID == ""
}

// validate enforces "all three set OR all three empty" per the
// chk_source_consistency CHECK constraint.
func (s Source) validate() error {
	allSet := s.Module != "" && s.EntityType != "" && s.EntityID != ""
	allEmpty := s.IsZero()
	if !allSet && !allEmpty {
		return fmt.Errorf("%w: source fields must be all set or all empty (got module=%q, entity_type=%q, entity_id=%q)",
			ErrInvalid, s.Module, s.EntityType, s.EntityID)
	}
	return nil
}

// WorkItem is the aggregate root.
//
// Invariants (enforced by factories + mutator methods):
//   - ID + TenantID + AssignedTo + AssignedBy + CreatedBy non-zero UUIDs.
//   - Type + Priority + State are valid catalogue entries.
//   - Title is 1..200 chars (trimmed).
//   - Description is at most 4000 chars.
//   - Source fields are all set or all empty.
//   - State transitions follow the documented machine — Start/Complete/
//     Cancel/MarkOverdue/Reassign emit a domain event on success or
//     return [ErrConflict] / [ErrInvalid] on rejection.
type WorkItem struct {
	id       ID
	tenantID tenant.ID

	itype       Type
	priority    Priority
	state       State
	title       string
	description string

	assignedToMembershipID string
	assignedByMembershipID string

	dueAt              time.Time
	completedAt        time.Time
	cancelledAt        time.Time
	cancellationReason string

	batchID string
	source  Source

	createdAt             time.Time
	createdByMembershipID string

	events []Event
}

// NewParams is the parameter struct for [NewManual]. Keeps the
// factory signature stable as new fields land.
type NewParams struct {
	ID                     ID
	TenantID               tenant.ID
	Type                   Type
	Priority               Priority
	Title                  string
	Description            string
	AssignedToMembershipID string
	AssignedByMembershipID string
	CreatedByMembershipID  string
	DueAt                  time.Time
	BatchID                string // optional bulk-assignment correlation
	Now                    time.Time
}

// NewManual constructs a brand-new user-authored WorkItem in
// [StatePending]. Used by the create-task HTTP handler. The aggregate
// emits a [CreatedEvent] which the repository drains via [PullEvents]
// on persist.
//
// `now` is the injected wall-clock (Pure Domain canon — ADR 0047).
func NewManual(p NewParams) (*WorkItem, error) {
	return newCommon(p, Source{})
}

// AutoCreateParams is the parameter struct for [NewAutoCreated]. Same
// shape as [NewParams] plus a populated [Source].
type AutoCreateParams struct {
	ID                     ID
	TenantID               tenant.ID
	Type                   Type
	Priority               Priority
	Title                  string
	Description            string
	AssignedToMembershipID string
	AssignedByMembershipID string // subscriber-side: the actor on the source event (e.g. logged_by_membership_id)
	CreatedByMembershipID  string // same as AssignedByMembershipID on the auto path; carved out for clarity
	DueAt                  time.Time
	BatchID                string
	Source                 Source
	Now                    time.Time
}

// NewAutoCreated constructs a subscriber-driven WorkItem in
// [StatePending] with a populated [Source]. Used by the CRM /
// Orders / Inventory subscribers when an upstream event triggers a
// task (e.g. CallLogged with callback_window → CallbackReminder).
//
// The (source.EntityType, source.EntityID) pair is the idempotency
// key — the repository's Add path translates the partial-unique-index
// 23505 SQLSTATE to [ErrAlreadyExistsForSource] so subscriber retries
// no-op.
func NewAutoCreated(p AutoCreateParams) (*WorkItem, error) {
	if err := p.Source.validate(); err != nil {
		return nil, err
	}
	if p.Source.IsZero() {
		return nil, fmt.Errorf("%w: NewAutoCreated requires populated Source", ErrInvalid)
	}
	return newCommon(NewParams{
		ID:                     p.ID,
		TenantID:               p.TenantID,
		Type:                   p.Type,
		Priority:               p.Priority,
		Title:                  p.Title,
		Description:            p.Description,
		AssignedToMembershipID: p.AssignedToMembershipID,
		AssignedByMembershipID: p.AssignedByMembershipID,
		CreatedByMembershipID:  p.CreatedByMembershipID,
		DueAt:                  p.DueAt,
		BatchID:                p.BatchID,
		Now:                    p.Now,
	}, p.Source)
}

func newCommon(p NewParams, src Source) (*WorkItem, error) {
	if err := validateUUIDString("id", p.ID.String()); err != nil {
		return nil, err
	}
	if err := validateUUIDString("tenant id", strings.TrimSpace(p.TenantID.String())); err != nil {
		return nil, err
	}
	if err := validateUUIDString("assigned_to membership id", p.AssignedToMembershipID); err != nil {
		return nil, err
	}
	if err := validateUUIDString("assigned_by membership id", p.AssignedByMembershipID); err != nil {
		return nil, err
	}
	if err := validateUUIDString("created_by membership id", p.CreatedByMembershipID); err != nil {
		return nil, err
	}
	if err := validateOptionalUUID("batch id", p.BatchID); err != nil {
		return nil, err
	}
	if !p.Type.IsValid() {
		return nil, fmt.Errorf("%w: type %q not in catalogue", ErrInvalid, p.Type)
	}
	priority := p.Priority
	if priority == "" {
		priority = PriorityMedium
	}
	if !priority.IsValid() {
		return nil, fmt.Errorf("%w: priority %q not in catalogue", ErrInvalid, p.Priority)
	}
	title := strings.TrimSpace(p.Title)
	if len(title) < titleMin || len(title) > titleMax {
		return nil, fmt.Errorf("%w: title length %d not in [%d,%d]",
			ErrInvalid, len(title), titleMin, titleMax)
	}
	if len(p.Description) > descriptionMax {
		return nil, fmt.Errorf("%w: description length %d > %d",
			ErrInvalid, len(p.Description), descriptionMax)
	}
	if p.DueAt.IsZero() {
		return nil, fmt.Errorf("%w: due_at required", ErrInvalid)
	}
	if p.Now.IsZero() {
		return nil, fmt.Errorf("%w: now required", ErrInvalid)
	}
	if err := src.validate(); err != nil {
		return nil, err
	}
	w := &WorkItem{
		id:                     p.ID,
		tenantID:               p.TenantID,
		itype:                  p.Type,
		priority:               priority,
		state:                  StatePending,
		title:                  title,
		description:            p.Description,
		assignedToMembershipID: p.AssignedToMembershipID,
		assignedByMembershipID: p.AssignedByMembershipID,
		dueAt:                  p.DueAt.UTC(),
		batchID:                p.BatchID,
		source:                 src,
		createdAt:              p.Now.UTC(),
		createdByMembershipID:  p.CreatedByMembershipID,
	}
	w.recordEvent(CreatedEvent{
		WorkItemID:             w.id,
		TenantID:               w.tenantID,
		Type:                   w.itype,
		Priority:               w.priority,
		Title:                  w.title,
		AssignedToMembershipID: w.assignedToMembershipID,
		AssignedByMembershipID: w.assignedByMembershipID,
		DueAt:                  w.dueAt,
		BatchID:                w.batchID,
		SourceModule:           src.Module,
		SourceEntityType:       src.EntityType,
		SourceEntityID:         src.EntityID,
		CreatedByMembershipID:  w.createdByMembershipID,
		At:                     w.createdAt,
	})
	return w, nil
}

// Snapshot is the persistence-layer DTO consumed by [UnmarshalFromDB].
// Adapter code scans DB rows into this struct, then re-hydrates via
// [UnmarshalFromDB] — keeps the adapter free of aggregate field
// knowledge.
type Snapshot struct {
	ID                     ID
	TenantID               tenant.ID
	Type                   Type
	Priority               Priority
	State                  State
	Title                  string
	Description            string
	AssignedToMembershipID string
	AssignedByMembershipID string
	DueAt                  time.Time
	CompletedAt            time.Time
	CancelledAt            time.Time
	CancellationReason     string
	BatchID                string
	Source                 Source
	CreatedAt              time.Time
	CreatedByMembershipID  string
}

// UnmarshalFromDB re-hydrates a WorkItem from persistence. Used ONLY
// by the repository on read paths — does NOT re-validate invariants
// per TDL canon (Wild Workouts Nov 2025).
func UnmarshalFromDB(s Snapshot) *WorkItem {
	return &WorkItem{
		id:                     s.ID,
		tenantID:               s.TenantID,
		itype:                  s.Type,
		priority:               s.Priority,
		state:                  s.State,
		title:                  s.Title,
		description:            s.Description,
		assignedToMembershipID: s.AssignedToMembershipID,
		assignedByMembershipID: s.AssignedByMembershipID,
		dueAt:                  s.DueAt,
		completedAt:            s.CompletedAt,
		cancelledAt:            s.CancelledAt,
		cancellationReason:     s.CancellationReason,
		batchID:                s.BatchID,
		source:                 s.Source,
		createdAt:              s.CreatedAt,
		createdByMembershipID:  s.CreatedByMembershipID,
	}
}

// ----- Getters --------------------------------------------------------------

// ID returns the work-item primary key.
func (w *WorkItem) ID() ID { return w.id }

// TenantID returns the owning tenant ID.
func (w *WorkItem) TenantID() tenant.ID { return w.tenantID }

// Type returns the workflow category.
func (w *WorkItem) Type() Type { return w.itype }

// Priority returns the current priority band.
func (w *WorkItem) Priority() Priority { return w.priority }

// State returns the current lifecycle state.
func (w *WorkItem) State() State { return w.state }

// Title returns the user-facing label.
func (w *WorkItem) Title() string { return w.title }

// Description returns the long-form context.
func (w *WorkItem) Description() string { return w.description }

// AssignedToMembershipID returns the current owner membership.
func (w *WorkItem) AssignedToMembershipID() string { return w.assignedToMembershipID }

// AssignedByMembershipID returns the membership that originally
// assigned the task (unchanged by Reassign — see [ReassignedEvent]
// for the change-actor).
func (w *WorkItem) AssignedByMembershipID() string { return w.assignedByMembershipID }

// DueAt returns the due timestamp (UTC).
func (w *WorkItem) DueAt() time.Time { return w.dueAt }

// CompletedAt returns the completion timestamp, zero if not completed.
func (w *WorkItem) CompletedAt() time.Time { return w.completedAt }

// CancelledAt returns the cancellation timestamp, zero if not cancelled.
func (w *WorkItem) CancelledAt() time.Time { return w.cancelledAt }

// CancellationReason returns the audit reason for cancellation, empty
// if not cancelled.
func (w *WorkItem) CancellationReason() string { return w.cancellationReason }

// BatchID returns the bulk-assignment correlation ID, empty if the
// task was created individually.
func (w *WorkItem) BatchID() string { return w.batchID }

// Source returns the cross-module provenance (zero-value for manual
// tasks).
func (w *WorkItem) Source() Source { return w.source }

// CreatedAt returns the creation timestamp.
func (w *WorkItem) CreatedAt() time.Time { return w.createdAt }

// CreatedByMembershipID returns the actor who created the task.
func (w *WorkItem) CreatedByMembershipID() string { return w.createdByMembershipID }

// ----- State transitions ----------------------------------------------------

// Start flips the work item from pending → in_progress. Idempotent
// on self-transition. Refuses transitions from terminal / overdue
// state (overdue → in_progress is forbidden; the user must Complete
// or Cancel an overdue task instead, per the BRD §6.8 doctrine that
// re-engaging overdue work creates a fresh task).
//
// `now` is the injected wall-clock (Pure Domain canon — ADR 0047).
func (w *WorkItem) Start(actorID string, now time.Time) error {
	if err := validateUUIDString("actor id", actorID); err != nil {
		return err
	}
	if now.IsZero() {
		return fmt.Errorf("%w: now required", ErrInvalid)
	}
	if w.state == StateInProgress {
		return nil // idempotent
	}
	if w.state.IsTerminal() {
		return fmt.Errorf("%w: cannot start a %s task", ErrConflict, w.state)
	}
	if w.state == StateOverdue {
		return fmt.Errorf("%w: cannot start an overdue task; complete or cancel it instead", ErrConflict)
	}
	w.state = StateInProgress
	w.recordEvent(StartedEvent{
		WorkItemID: w.id,
		TenantID:   w.tenantID,
		ActorID:    actorID,
		At:         now.UTC(),
	})
	return nil
}

// Complete terminally closes the work item as done. Idempotent on
// self-transition (already-completed → no-op). Refuses transitions
// from cancelled state.
func (w *WorkItem) Complete(actorID string, now time.Time) error {
	if err := validateUUIDString("actor id", actorID); err != nil {
		return err
	}
	if now.IsZero() {
		return fmt.Errorf("%w: now required", ErrInvalid)
	}
	if w.state == StateCompleted {
		return nil // idempotent
	}
	if w.state == StateCancelled {
		return fmt.Errorf("%w: cannot complete a cancelled task", ErrConflict)
	}
	w.state = StateCompleted
	w.completedAt = now.UTC()
	w.recordEvent(CompletedEvent{
		WorkItemID: w.id,
		TenantID:   w.tenantID,
		ActorID:    actorID,
		At:         now.UTC(),
	})
	return nil
}

// Cancel terminally drops the work item with an audit reason.
// Reason is mandatory (per BRD §6.8). Idempotent on self-transition
// when the supplied reason matches the previously-recorded reason;
// otherwise the second attempt is rejected.
func (w *WorkItem) Cancel(actorID, reason string, now time.Time) error {
	if err := validateUUIDString("actor id", actorID); err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return fmt.Errorf("%w: cancellation reason required", ErrInvalid)
	}
	if len(reason) > cancellationReasonMax {
		return fmt.Errorf("%w: cancellation reason length %d > %d",
			ErrInvalid, len(reason), cancellationReasonMax)
	}
	if now.IsZero() {
		return fmt.Errorf("%w: now required", ErrInvalid)
	}
	if w.state == StateCancelled {
		if w.cancellationReason == reason {
			return nil // strict idempotent
		}
		return fmt.Errorf("%w: cannot re-cancel with a different reason", ErrConflict)
	}
	if w.state == StateCompleted {
		return fmt.Errorf("%w: cannot cancel a completed task", ErrConflict)
	}
	w.state = StateCancelled
	w.cancelledAt = now.UTC()
	w.cancellationReason = reason
	w.recordEvent(CancelledEvent{
		WorkItemID: w.id,
		TenantID:   w.tenantID,
		ActorID:    actorID,
		Reason:     reason,
		At:         now.UTC(),
	})
	return nil
}

// MarkOverdue flips pending or in_progress → overdue. Used by the
// periodic overdue-scan job. Idempotent — already-overdue is a no-op
// without event emission. Terminal states (completed / cancelled) are
// silently ignored (returns nil) since the scanner shouldn't surface
// those rows anyway.
func (w *WorkItem) MarkOverdue(now time.Time) error {
	if now.IsZero() {
		return fmt.Errorf("%w: now required", ErrInvalid)
	}
	if w.state == StateOverdue {
		return nil
	}
	if w.state.IsTerminal() {
		return nil // scanner shouldn't see terminal rows, but tolerate replays
	}
	if w.state != StatePending && w.state != StateInProgress {
		return fmt.Errorf("%w: cannot mark %s task overdue", ErrConflict, w.state)
	}
	w.state = StateOverdue
	w.recordEvent(OverdueEvent{
		WorkItemID:             w.id,
		TenantID:               w.tenantID,
		AssignedToMembershipID: w.assignedToMembershipID,
		DueAt:                  w.dueAt,
		At:                     now.UTC(),
	})
	return nil
}

// Reassign moves the work item to a new owner. The HIERARCHY GATE per
// BRD §6.7 visibility rule lives in the App-layer command handler
// (the aggregate is a stateless invariant store; it does not import
// the identity hierarchy reader). Idempotent on self-transition.
// Refuses reassignment of terminal tasks.
func (w *WorkItem) Reassign(newAssigneeID, reassignedByID, reason string, now time.Time) error {
	if err := validateUUIDString("new assignee id", newAssigneeID); err != nil {
		return err
	}
	if err := validateUUIDString("reassigned-by id", reassignedByID); err != nil {
		return err
	}
	if now.IsZero() {
		return fmt.Errorf("%w: now required", ErrInvalid)
	}
	if w.state.IsTerminal() {
		return fmt.Errorf("%w: cannot reassign a %s task", ErrConflict, w.state)
	}
	if w.assignedToMembershipID == newAssigneeID {
		return nil // idempotent
	}
	previous := w.assignedToMembershipID
	w.assignedToMembershipID = newAssigneeID
	w.recordEvent(ReassignedEvent{
		WorkItemID:               w.id,
		TenantID:                 w.tenantID,
		PreviousAssignee:         previous,
		NewAssigneeMembershipID:  newAssigneeID,
		ReassignedByMembershipID: reassignedByID,
		Reason:                   reason,
		At:                       now.UTC(),
	})
	return nil
}

// ----- Event handling -------------------------------------------------------

// PullEvents drains the recorded domain events and returns them. The
// repository calls this once per Add / committed UpdateByID inside
// the same transaction that persists state, then forwards events to
// the outbox.
func (w *WorkItem) PullEvents() []Event {
	if len(w.events) == 0 {
		return nil
	}
	out := w.events
	w.events = nil
	return out
}

func (w *WorkItem) recordEvent(e Event) {
	w.events = append(w.events, e)
}

// ----- Validation helpers ---------------------------------------------------

// validateUUIDString returns ErrInvalid when val is not a RFC 9562
// UUID. Empty input is REJECTED — pass through validateOptionalUUID
// for nullable fields.
func validateUUIDString(name, val string) error {
	if val == "" {
		return fmt.Errorf("%w: %s required", ErrInvalid, name)
	}
	if _, err := uuid.Parse(val); err != nil {
		return fmt.Errorf("%w: %s not a valid uuid", ErrInvalid, name)
	}
	return nil
}

// validateOptionalUUID is the empty-allowed variant.
func validateOptionalUUID(name, val string) error {
	if val == "" {
		return nil
	}
	return validateUUIDString(name, val)
}
