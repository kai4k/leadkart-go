// Package commercepolicy is the one definition of a company's commerce policy:
// the versioned value object, its defaults, and the registry of every setting.
//
// The company_settings module owns the stored policy; Orders and Inventory each
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
type Expiry struct {
	StopMonthsBefore int   `json:"stop_months_before"`
	WarningDays      []int `json:"warning_days"`
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

// DrugSchedule is whether the product form records a drug schedule.
type DrugSchedule struct {
	Mode string `json:"mode"`
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
		Expiry: Expiry{StopMonthsBefore: 1, WarningDays: []int{180, 90, 30}},
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
		DrugSchedule: DrugSchedule{Mode: DrugScheduleOptional},
	}
}

// Initial is what a company with NO stored policy reads: the defaults at
// version 0. Every reader of a missing policy row (company_settings, the Orders
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

// Decode reads a stored or received policy. It is strict: an unknown key or an
// invalid value is an error, not something to paper over.
func Decode(raw []byte) (Policy, error) {
	var p Policy
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("commercepolicy: decode: %w", err)
	}
	if errs := Validate(p); len(errs) > 0 {
		return Policy{}, fmt.Errorf("commercepolicy: decode: invalid policy: %w", errs.Err())
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
	out.Tax.AnnualReturnFiled = make(map[string]string, len(p.Tax.AnnualReturnFiled))
	for k, v := range p.Tax.AnnualReturnFiled {
		out.Tax.AnnualReturnFiled[k] = v
	}
	return out
}
