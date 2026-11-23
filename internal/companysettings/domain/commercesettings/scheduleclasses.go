package commercesettings

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
)

// KeyScheduleClasses is the changed-key name of an edit to the drug schedule
// class list.
const KeyScheduleClasses = commercepolicy.ScheduleClassesKey

// ErrScheduleClassNotFound means there is no class with that id in the company's
// list. Map to 404.
var ErrScheduleClassNotFound = errors.New("commercesettings: drug schedule class not found")

// ErrScheduleClassInUse means a product still carries the class, so it cannot be
// removed. Map to 409 drug_schedule_class_in_use.
var ErrScheduleClassInUse = errors.New("commercesettings: drug schedule class is in use")

// CodeScheduleInUse is the refusal of turning drug_schedule.mode off while a
// product still carries a schedule class.
const CodeScheduleInUse = "schedule_in_use"

// AddScheduleClassInput is a new entry for the company's class list.
type AddScheduleClassInput struct {
	ID     string
	Label  string
	Reason string
	Actor  Actor
	Now    time.Time
}

// AddScheduleClass adds a class. The same label (any case) is refused as a
// duplicate.
func (s *Settings) AddScheduleClass(in AddScheduleClassInput) error {
	if _, err := uuid.Parse(in.ID); err != nil {
		return fmt.Errorf("%w: drug schedule class id not a valid uuid", ErrInvalid)
	}
	errs := commercepolicy.CheckScheduleClassLabel(in.Label)
	reason, rerr := checkReason(in.Reason, true)
	mergeReasonErr(errs, rerr)
	label := commercepolicy.NormalizeScheduleLabel(in.Label)
	if slices.ContainsFunc(s.policy.DrugSchedule.Classes, func(c commercepolicy.ScheduleClass) bool {
		return commercepolicy.SameScheduleLabel(c.Label, label)
	}) {
		errs["label"] = append(errs["label"], commercepolicy.CodeDuplicate)
	}
	if len(s.policy.DrugSchedule.Classes) >= commercepolicy.MaxScheduleClasses {
		errs["label"] = append(errs["label"], commercepolicy.CodeTooMany)
	}
	if err := errs.Err(); err != nil {
		return err
	}
	next := s.policy.Clone()
	next.DrugSchedule.Classes = append(next.DrugSchedule.Classes, commercepolicy.ScheduleClass{ID: in.ID, Label: label})
	commercepolicy.SortScheduleClasses(next.DrugSchedule.Classes)
	s.policy = next
	s.commit(in.Actor, reason, []string{KeyScheduleClasses}, in.Now)
	return nil
}

// RenameScheduleClassInput renames a class; products keep it (they hold its id).
type RenameScheduleClassInput struct {
	ID     string
	Label  string
	Reason string
	Actor  Actor
	Now    time.Time
}

// RenameScheduleClass renames a class. A rename to the same label is a no-op.
func (s *Settings) RenameScheduleClass(in RenameScheduleClassInput) error {
	idx := slices.IndexFunc(s.policy.DrugSchedule.Classes, func(c commercepolicy.ScheduleClass) bool { return c.ID == in.ID })
	if idx < 0 {
		return ErrScheduleClassNotFound
	}
	label := commercepolicy.NormalizeScheduleLabel(in.Label)
	if label == s.policy.DrugSchedule.Classes[idx].Label {
		return nil
	}
	errs := commercepolicy.CheckScheduleClassLabel(in.Label)
	reason, rerr := checkReason(in.Reason, true)
	mergeReasonErr(errs, rerr)
	for i, o := range s.policy.DrugSchedule.Classes {
		if i != idx && commercepolicy.SameScheduleLabel(o.Label, label) {
			errs["label"] = append(errs["label"], commercepolicy.CodeDuplicate)
		}
	}
	if err := errs.Err(); err != nil {
		return err
	}
	next := s.policy.Clone()
	next.DrugSchedule.Classes[idx].Label = label
	commercepolicy.SortScheduleClasses(next.DrugSchedule.Classes)
	s.policy = next
	s.commit(in.Actor, reason, []string{KeyScheduleClasses}, in.Now)
	return nil
}

// RemoveScheduleClassInput removes a class. InUse is whether any product still
// carries it, read by the caller from Inventory (this module cannot).
type RemoveScheduleClassInput struct {
	ID     string
	InUse  bool
	Reason string
	Actor  Actor
	Now    time.Time
}

// RemoveScheduleClass removes a class from the list. A class a product carries
// cannot be removed ([ErrScheduleClassInUse]).
func (s *Settings) RemoveScheduleClass(in RemoveScheduleClassInput) error {
	idx := slices.IndexFunc(s.policy.DrugSchedule.Classes, func(c commercepolicy.ScheduleClass) bool { return c.ID == in.ID })
	if idx < 0 {
		return ErrScheduleClassNotFound
	}
	if in.InUse {
		return ErrScheduleClassInUse
	}
	reason, err := checkReason(in.Reason, true)
	if err != nil {
		return err
	}
	next := s.policy.Clone()
	next.DrugSchedule.Classes = slices.Delete(next.DrugSchedule.Classes, idx, idx+1)
	s.policy = next
	s.commit(in.Actor, reason, []string{KeyScheduleClasses}, in.Now)
	return nil
}
