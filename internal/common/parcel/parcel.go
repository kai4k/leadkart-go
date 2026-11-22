// Package parcel is the one rule set for a shipment's physical and carrier
// details: who carries it, how many boxes, how heavy, when it is due.
//
// Two modules enforce it. Orders checks it when an order is packed, BEFORE the
// order changes state; Dispatch checks it again when the consignment note is
// created from the packed event. The two must agree: a pack that Orders accepts
// but Dispatch refuses leaves an order packed with no consignment note, retried
// to the dead-letter queue. Keeping the rule here, in a dependency-free common
// package, is what makes them agree (modules may not import each other).
package parcel

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxCarrierNameChars is the dispatch.consignment_notes.carrier_name column
// limit (CHECK length BETWEEN 1 AND 200, counted in characters).
const MaxCarrierNameChars = 200

// Stable machine codes, one per rule. The HTTP layers answer 422 with the code
// of the first rule broken.
const (
	CodeCarrierRequired     = "carrier_name_required"
	CodeCarrierTooLong      = "carrier_name_too_long"
	CodeBoxCountInvalid     = "box_count_invalid"
	CodeWeightInvalid       = "weight_grams_invalid"
	CodeDeliveryNotInFuture = "expected_delivery_not_in_future"
)

// ErrInvalid is the sentinel every [Error] matches via errors.Is.
var ErrInvalid = errors.New("parcel: invalid")

// Error is one broken rule: a stable Code plus a human detail.
type Error struct {
	Code   string
	Detail string
}

func (e *Error) Error() string { return "parcel: invalid: " + e.Detail }

// Is makes errors.Is(err, ErrInvalid) true for every rule failure.
func (e *Error) Is(target error) bool { return target == ErrInvalid }

// Details are the shipment details a pack request carries.
type Details struct {
	CarrierName        string
	BoxCount           int32
	WeightGrams        int64
	ExpectedDeliveryAt *time.Time
}

// Validate returns nil, or an *[Error] for the first rule broken. now is the
// moment the rule is judged at; a delivery date before it is in the past.
func Validate(d Details, now time.Time) error {
	carrier := strings.TrimSpace(d.CarrierName)
	if carrier == "" {
		return &Error{Code: CodeCarrierRequired, Detail: "carrier_name required"}
	}
	if utf8.RuneCountInString(carrier) > MaxCarrierNameChars {
		return &Error{Code: CodeCarrierTooLong,
			Detail: fmt.Sprintf("carrier_name must be at most %d characters", MaxCarrierNameChars)}
	}
	if d.BoxCount <= 0 {
		return &Error{Code: CodeBoxCountInvalid,
			Detail: fmt.Sprintf("box_count must be positive (got %d)", d.BoxCount)}
	}
	if d.WeightGrams <= 0 {
		return &Error{Code: CodeWeightInvalid,
			Detail: fmt.Sprintf("weight_grams must be positive (got %d)", d.WeightGrams)}
	}
	if d.ExpectedDeliveryAt != nil && d.ExpectedDeliveryAt.Before(now) {
		return &Error{Code: CodeDeliveryNotInFuture, Detail: "expected_delivery_at must be in the future"}
	}
	return nil
}
