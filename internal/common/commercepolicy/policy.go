// Package commercepolicy is the one definition of a company's commerce policy:
// the versioned value object, its defaults, and the registry of every setting.
//
// The companysettings module owns the stored policy; Orders and Inventory each
// hold an event-fed replica and read it inside their own transaction. The three
// share these pure types because modules may not import each other (the
// common/parcel precedent): stdlib plus common/humantext and common/textlimit
// only.
//
// A setting is a business rule a company may choose. Integrity and legal
// invariants (no sale past printed expiry, gapless invoice numbers, immutable
// issued documents) are NOT settings and never appear here.
package commercepolicy

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Policy is a company's full commerce policy. The zero value is not a valid
// policy; start from [Default].
type Policy struct {
	Expiry       Expiry       `json:"expiry"`
	Fulfilment   Fulfilment   `json:"fulfilment"`
	Pricing      Pricing      `json:"pricing"`
	Invoice      Invoice      `json:"invoice"`
	Tax          Tax          `json:"tax"`
	Doc          Doc          `json:"doc"`
	EInvoice     EInvoice     `json:"einvoice"`
	Eway         Eway         `json:"eway"`
	Payment      Payment      `json:"payment"`
	Credit       Credit       `json:"credit"`
	DrugSchedule DrugSchedule `json:"drug_schedule"`
}

// Expiry rules. StopMonthsBefore is how many months before the printed expiry
// month a batch stops being sellable: 0 is the legal limit (sellable through
// the printed month), 1 stops at the start of the printed month.
//
// BusinessDayUTCOffsetMinutes is the company's civil-time offset from UTC, in
// minutes: the business day (the date every expiry decision uses) is the date
// at that offset, so it flips at the company's midnight. It is company data;
// the starting value comes from the embedded defaults file.
type Expiry struct {
	StopMonthsBefore            int   `json:"stop_months_before"`
	WarningDays                 []int `json:"warning_days"`
	BusinessDayUTCOffsetMinutes int   `json:"business_day_utc_offset_minutes"`
}

// Fulfilment rules for a short order and for invoice timing.
type Fulfilment struct {
	ShortageAllowed    []string `json:"shortage_allowed"`
	ShortageDefault    string   `json:"shortage_default"`
	HoldKeepsAvailable bool     `json:"hold_keeps_available"`
	InvoiceTiming      string   `json:"invoice_timing"`
}

// Pricing rules for the quotation editor and the invoice line shape.
type Pricing struct {
	RateOverride    bool   `json:"rate_override"`
	DiscountEnabled bool   `json:"discount_enabled"`
	DiscountMaxBps  int    `json:"discount_max_bps"`
	SchemeEnabled   bool   `json:"scheme_enabled"`
	SchemeValuation string `json:"scheme_valuation"`
}

// Invoice rules.
type Invoice struct {
	RoundOff string `json:"round_off"`
}

// Tax rules. GSTRates is the company's own list: it starts empty and a product
// may only use a rate from it. It changes through its own routes, not through
// the settings patch.
type Tax struct {
	GSTRates          []GSTRate         `json:"gst_rates"`
	CreditNoteSplit   string            `json:"credit_note_split"`
	AnnualReturnFiled map[string]string `json:"annual_return_filed"`
}

// Doc is the document-number display prefix. The number itself is gapless and
// at most 16 characters whatever the prefix (legal invariant, not a setting).
type Doc struct {
	InvoicePrefix    string `json:"invoice_prefix"`
	CreditNotePrefix string `json:"credit_note_prefix"`
}

// EInvoice is the e-invoice dispatch gate.
type EInvoice struct {
	Mode string `json:"mode"`
}

// Eway is the e-way-bill dispatch gate over the statutory value.
type Eway struct {
	Enforcement string `json:"enforcement"`
}

// Payment rules. AdvanceValue is basis points for mode percent and paise for
// mode fixed; it is 0 for the other modes.
type Payment struct {
	AdvanceMode    string `json:"advance_mode"`
	AdvanceValue   int64  `json:"advance_value"`
	PartPayments   bool   `json:"part_payments"`
	Overpayment    string `json:"overpayment"`
	DispatchGate   string `json:"dispatch_gate"`
	CompletionRule string `json:"completion_rule"`
}

// Credit rules. Money is integer paise.
type Credit struct {
	Enabled           bool   `json:"enabled"`
	Enforcement       string `json:"enforcement"`
	DefaultLimitPaise int64  `json:"default_limit_paise"`
	DefaultTermsDays  int    `json:"default_terms_days"`
	BlockOnOverdue    bool   `json:"block_on_overdue"`
	OverdueGraceDays  int    `json:"overdue_grace_days"`
}

// DrugSchedule is whether the product form records a drug schedule (Mode) and
// the classes a product may carry (Classes). The classes are the company's own
// list: they start from the embedded defaults, and the company adds, renames and
// removes them through their own routes. A product stores a class id; nothing in
// the code is tied to any particular class.
type DrugSchedule struct {
	Mode    string          `json:"mode"`
	Classes []ScheduleClass `json:"classes"`
}

// Versioned is a policy and the version it was stored at. Version 0 is the
// defaults of a company with no stored policy; stored versions start at 1.
type Versioned struct {
	Version int64  `json:"version"`
	Policy  Policy `json:"policy"`
}

// Shortage choices, invoice timings and the other closed option sets.
const (
	ShortageHold           = "hold"
	ShortageRefuse         = "refuse"
	ShortagePartialBackord = "partial_backorder"
	ShortagePartialReduce  = "partial_reduce"

	InvoiceAtPack     = "at_pack"
	InvoiceAtDispatch = "at_dispatch"

	SchemeFreeQuantity       = "free_quantity"
	SchemeDiscountEquivalent = "discount_equivalent"

	RoundOffNone         = "none"
	RoundOffNearestRupee = "nearest_rupee"

	CreditSplitProRata      = "pro_rata"
	CreditSplitManualByRate = "manual_by_rate"

	EInvoiceOff       = "off"
	EInvoiceManualIRN = "manual_irn"

	EwayWarn  = "warn"
	EwayBlock = "block"

	AdvanceNone      = "none"
	AdvanceAnyAmount = "any_amount"
	AdvancePercent   = "percent"
	AdvanceFixed     = "fixed"

	OverpayRefuse         = "refuse"
	OverpayCustomerCredit = "customer_credit"
	OverpayRefundDue      = "refund_due"

	DispatchGateNone       = "none"
	DispatchGateAdvanceMet = "advance_met"
	DispatchGateFullPay    = "full_payment"

	CompletionFullPayment = "full_payment"
	CompletionOnDelivery  = "on_delivery"

	CreditWarn  = "warn"
	CreditBlock = "block"

	DrugScheduleOff      = "off"
	DrugScheduleOptional = "optional"
	DrugScheduleRequired = "required"
)

// Default is THE defaults of the policy. It is the only place a default value
// is written: a company with no stored policy reads it (through [Initial]),
// and a tenant's first stored policy starts from it. GST has no default rate:
// the company's list starts empty.
func Default() Policy {
	return Policy{
		Expiry: Expiry{StopMonthsBefore: 1, WarningDays: []int{180, 90, 30}, BusinessDayUTCOffsetMinutes: defaultData().BusinessDayUTCOffsetMinutes},
		Fulfilment: Fulfilment{
			ShortageAllowed: []string{ShortageHold, ShortageRefuse, ShortagePartialBackord, ShortagePartialReduce},
			ShortageDefault: ShortageHold,
			InvoiceTiming:   InvoiceAtPack,
		},
		Pricing: Pricing{
			RateOverride: true, DiscountEnabled: true, DiscountMaxBps: 10000,
			SchemeEnabled: true, SchemeValuation: SchemeFreeQuantity,
		},
		Invoice: Invoice{RoundOff: RoundOffNone},
		Tax: Tax{
			GSTRates:          []GSTRate{},
			CreditNoteSplit:   CreditSplitProRata,
			AnnualReturnFiled: map[string]string{},
		},
		Doc:      Doc{InvoicePrefix: "INV", CreditNotePrefix: "CDN"},
		EInvoice: EInvoice{Mode: EInvoiceOff},
		Eway:     Eway{Enforcement: EwayWarn},
		Payment: Payment{
			AdvanceMode: AdvanceAnyAmount, PartPayments: true, Overpayment: OverpayCustomerCredit,
			DispatchGate: DispatchGateAdvanceMet, CompletionRule: CompletionFullPayment,
		},
		Credit: Credit{
			Enabled: false, Enforcement: CreditBlock, DefaultLimitPaise: 0,
			DefaultTermsDays: 30, BlockOnOverdue: true, OverdueGraceDays: 0,
		},
		DrugSchedule: DrugSchedule{Mode: DrugScheduleOptional, Classes: defaultData().scheduleClasses()},
	}
}

// Initial is what a company with NO stored policy reads: the defaults at
// version 0. Every reader of a missing policy row (companysettings, the Orders
// replica, the Inventory replica) calls this one function, so "no row" can
// never mean two different things.
func Initial() Versioned { return Versioned{Version: 0, Policy: Default()} }

// Encode is the stored and wire form of a policy.
func Encode(p Policy) ([]byte, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("commercepolicy: encode: %w", err)
	}
	return b, nil
}

// Decode reads a stored or received policy onto the defaults: a key the stored
// document does not have (it predates the setting) takes its default, so adding
// a setting never needs every stored row rewritten. It is strict about unknown
// keys and validates the result: use it for the LIVE policy.
func Decode(raw []byte) (Policy, error) {
	p, err := DecodeStored(raw)
	if err != nil {
		return Policy{}, err
	}
	if errs := Validate(p); len(errs) > 0 {
		return Policy{}, fmt.Errorf("commercepolicy: decode: invalid policy: %w", errs.Err())
	}
	return p, nil
}

// DecodeStored reads a policy document as it was stored, onto the defaults and
// WITHOUT validating it: history rows are immutable and must stay readable after
// a later release tightens a bound.
func DecodeStored(raw []byte) (Policy, error) {
	p := Default()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("commercepolicy: decode: %w", err)
	}
	return p, nil
}

// Clone returns a deep copy, so a changed copy never shares a slice or map with
// the policy it came from.
func (p Policy) Clone() Policy {
	out := p
	out.Expiry.WarningDays = append([]int{}, p.Expiry.WarningDays...)
	out.Fulfilment.ShortageAllowed = append([]string{}, p.Fulfilment.ShortageAllowed...)
	out.Tax.GSTRates = append([]GSTRate{}, p.Tax.GSTRates...)
	out.DrugSchedule.Classes = append([]ScheduleClass{}, p.DrugSchedule.Classes...)
	out.Tax.AnnualReturnFiled = make(map[string]string, len(p.Tax.AnnualReturnFiled))
	for k, v := range p.Tax.AnnualReturnFiled {
		out.Tax.AnnualReturnFiled[k] = v
	}
	return out
}
