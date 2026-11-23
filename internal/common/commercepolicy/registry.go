package commercepolicy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Kind is the value shape of a setting.
type Kind string

// The value shapes a setting takes.
const (
	KindInt     Kind = "int"
	KindBool    Kind = "bool"
	KindEnum    Kind = "enum"
	KindEnumSet Kind = "enum_set"
	KindIntList Kind = "int_list"
	KindCode    Kind = "code"
	KindDateMap Kind = "date_map"
)

// Stable refusal codes of a setting value, carried in errors{key:[code]}.
const (
	CodeUnknownSetting   = "unknown_setting"
	CodeManagedElsewhere = "managed_elsewhere"
	CodeInvalidType      = "invalid_type"
	CodeNotAnOption      = "not_an_option"
	CodeTooMany          = "too_many"
	CodeNotInAllowed     = "not_in_allowed"
	CodeValueNotAllowed  = "value_not_allowed"
	CodeValueRequired    = "value_required"
)

// Setting is one entry of the registry: its key, who may change it, its value
// shape and bounds. The registry is the single source for validation, the
// changed-key list, the permission split and the option metadata the web reads.
type Setting struct {
	// Key is "<group>.<name>", the wire name.
	Key   string
	Group string
	// TaxManage marks a setting only the tax-manage permission may change; a
	// change to one also needs a reason.
	TaxManage bool
	Kind      Kind
	// Options is the closed set of an enum or enum_set.
	Options []string
	// Min and Max bound an int, or each item of an int_list.
	Min, Max int64
	// MaxItems bounds an int_list.
	MaxItems int

	ptr func(*Policy) any
}

var codeRE = regexp.MustCompile(`^[A-Z0-9]{1,4}$`)
var fyRE = regexp.MustCompile(`^(\d{4})-(\d{2})$`)

// registry lists every patchable setting in display order. The GST rate list is
// deliberately absent: it has its own routes ([ManagedElsewhereKey]).
var registry = []Setting{
	{Key: "expiry.stop_months_before", Kind: KindInt, Min: 0, Max: 24, ptr: func(p *Policy) any { return &p.Expiry.StopMonthsBefore }},
	{Key: "expiry.warning_days", Kind: KindIntList, Min: 1, Max: 730, MaxItems: 4, ptr: func(p *Policy) any { return &p.Expiry.WarningDays }},

	{Key: "fulfilment.shortage_allowed", Kind: KindEnumSet, Options: []string{ShortageHold, ShortageRefuse, ShortagePartialBackord, ShortagePartialReduce}, ptr: func(p *Policy) any { return &p.Fulfilment.ShortageAllowed }},
	{Key: "fulfilment.shortage_default", Kind: KindEnum, Options: []string{ShortageHold, ShortageRefuse, ShortagePartialBackord, ShortagePartialReduce}, ptr: func(p *Policy) any { return &p.Fulfilment.ShortageDefault }},
	{Key: "fulfilment.hold_keeps_available", Kind: KindBool, ptr: func(p *Policy) any { return &p.Fulfilment.HoldKeepsAvailable }},
	{Key: "fulfilment.invoice_timing", TaxManage: true, Kind: KindEnum, Options: []string{InvoiceAtPack, InvoiceAtDispatch}, ptr: func(p *Policy) any { return &p.Fulfilment.InvoiceTiming }},

	{Key: "pricing.rate_override", Kind: KindBool, ptr: func(p *Policy) any { return &p.Pricing.RateOverride }},
	{Key: "pricing.discount_enabled", Kind: KindBool, ptr: func(p *Policy) any { return &p.Pricing.DiscountEnabled }},
	{Key: "pricing.discount_max_bps", Kind: KindInt, Min: 0, Max: 10000, ptr: func(p *Policy) any { return &p.Pricing.DiscountMaxBps }},
	{Key: "pricing.scheme_enabled", Kind: KindBool, ptr: func(p *Policy) any { return &p.Pricing.SchemeEnabled }},
	{Key: "pricing.scheme_valuation", TaxManage: true, Kind: KindEnum, Options: []string{SchemeFreeQuantity, SchemeDiscountEquivalent}, ptr: func(p *Policy) any { return &p.Pricing.SchemeValuation }},

	{Key: "invoice.round_off", TaxManage: true, Kind: KindEnum, Options: []string{RoundOffNone, RoundOffNearestRupee}, ptr: func(p *Policy) any { return &p.Invoice.RoundOff }},

	{Key: "tax.credit_note_split", TaxManage: true, Kind: KindEnum, Options: []string{CreditSplitProRata, CreditSplitManualByRate}, ptr: func(p *Policy) any { return &p.Tax.CreditNoteSplit }},
	{Key: "tax.annual_return_filed", TaxManage: true, Kind: KindDateMap, ptr: func(p *Policy) any { return &p.Tax.AnnualReturnFiled }},

	{Key: "doc.invoice_prefix", TaxManage: true, Kind: KindCode, ptr: func(p *Policy) any { return &p.Doc.InvoicePrefix }},
	{Key: "doc.credit_note_prefix", TaxManage: true, Kind: KindCode, ptr: func(p *Policy) any { return &p.Doc.CreditNotePrefix }},
	{Key: "einvoice.mode", TaxManage: true, Kind: KindEnum, Options: []string{EInvoiceOff, EInvoiceManualIRN}, ptr: func(p *Policy) any { return &p.EInvoice.Mode }},
	{Key: "eway.enforcement", TaxManage: true, Kind: KindEnum, Options: []string{EwayWarn, EwayBlock}, ptr: func(p *Policy) any { return &p.Eway.Enforcement }},

	{Key: "payment.advance_mode", Kind: KindEnum, Options: []string{AdvanceNone, AdvanceAnyAmount, AdvancePercent, AdvanceFixed}, ptr: func(p *Policy) any { return &p.Payment.AdvanceMode }},
	{Key: "payment.advance_value", Kind: KindInt, Min: 0, Max: 1 << 50, ptr: func(p *Policy) any { return &p.Payment.AdvanceValue }},
	{Key: "payment.part_payments", Kind: KindBool, ptr: func(p *Policy) any { return &p.Payment.PartPayments }},
	{Key: "payment.overpayment", Kind: KindEnum, Options: []string{OverpayRefuse, OverpayCustomerCredit, OverpayRefundDue}, ptr: func(p *Policy) any { return &p.Payment.Overpayment }},
	{Key: "payment.dispatch_gate", Kind: KindEnum, Options: []string{DispatchGateNone, DispatchGateAdvanceMet, DispatchGateFullPay}, ptr: func(p *Policy) any { return &p.Payment.DispatchGate }},
	{Key: "payment.completion_rule", Kind: KindEnum, Options: []string{CompletionFullPayment, CompletionOnDelivery}, ptr: func(p *Policy) any { return &p.Payment.CompletionRule }},

	{Key: "credit.enabled", Kind: KindBool, ptr: func(p *Policy) any { return &p.Credit.Enabled }},
	{Key: "credit.enforcement", Kind: KindEnum, Options: []string{CreditWarn, CreditBlock}, ptr: func(p *Policy) any { return &p.Credit.Enforcement }},
	{Key: "credit.default_limit_paise", Kind: KindInt, Min: 0, Max: 1 << 50, ptr: func(p *Policy) any { return &p.Credit.DefaultLimitPaise }},
	{Key: "credit.default_terms_days", Kind: KindInt, Min: 0, Max: 365, ptr: func(p *Policy) any { return &p.Credit.DefaultTermsDays }},
	{Key: "credit.block_on_overdue", Kind: KindBool, ptr: func(p *Policy) any { return &p.Credit.BlockOnOverdue }},
	{Key: "credit.overdue_grace_days", Kind: KindInt, Min: 0, Max: 365, ptr: func(p *Policy) any { return &p.Credit.OverdueGraceDays }},

	{Key: "drug_schedule.mode", TaxManage: true, Kind: KindEnum, Options: []string{DrugScheduleOff, DrugScheduleOptional, DrugScheduleRequired}, ptr: func(p *Policy) any { return &p.DrugSchedule.Mode }},
}

// ManagedElsewhereKey is the one policy key a patch may not set: the GST rate
// list changes through its own routes.
const ManagedElsewhereKey = "tax.gst_rates"

func init() {
	for i := range registry {
		registry[i].Group, _, _ = strings.Cut(registry[i].Key, ".")
	}
}

// Settings returns the registry (a copy), in display order.
func Settings() []Setting { return slices.Clone(registry) }

func find(key string) (Setting, bool) {
	for _, s := range registry {
		if s.Key == key {
			return s, true
		}
	}
	return Setting{}, false
}

// Get returns the current value of a setting key in p, or false for an unknown
// key. The value is a deep copy.
func Get(p Policy, key string) (any, bool) {
	s, ok := find(key)
	if !ok {
		return nil, false
	}
	c := p.Clone()
	return reflect.ValueOf(s.ptr(&c)).Elem().Interface(), true
}

// Changes is a decoded patch: setting key to its raw JSON value.
type Changes map[string]json.RawMessage

// Result is a successful apply: the new policy and the keys that actually
// changed, sorted. An empty Changed means the patch matched the current policy.
type Result struct {
	Policy  Policy
	Changed []string
}

// Apply sets every key in changes on a copy of p and validates the whole
// result. It returns the new policy and the changed keys, or field errors keyed
// by setting key. Nothing is half-applied.
func Apply(p Policy, changes Changes, now time.Time) (Result, FieldErrors) {
	errs := FieldErrors{}
	next := p.Clone()
	keys := make([]string, 0, len(changes))
	for k := range changes {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var changed []string
	for _, k := range keys {
		if k == ManagedElsewhereKey {
			errs.add(k, CodeManagedElsewhere)
			continue
		}
		s, ok := find(k)
		if !ok {
			errs.add(k, CodeUnknownSetting)
			continue
		}
		before := reflect.ValueOf(s.ptr(&next)).Elem().Interface()
		if code := s.assign(s.ptr(&next), changes[k]); code != "" {
			errs.add(k, code)
			continue
		}
		after := reflect.ValueOf(s.ptr(&next)).Elem().Interface()
		if !reflect.DeepEqual(before, after) {
			changed = append(changed, k)
		}
	}
	if len(errs) > 0 {
		return Result{}, errs
	}
	// The same integer is basis points under percent and paise under fixed, so a
	// change of mode must bring its own value: never re-read the stored one.
	if next.Payment.AdvanceMode != p.Payment.AdvanceMode &&
		(next.Payment.AdvanceMode == AdvancePercent || next.Payment.AdvanceMode == AdvanceFixed) {
		if _, sent := changes["payment.advance_value"]; !sent {
			errs.add("payment.advance_value", CodeValueRequired)
		}
	}
	for fy, date := range next.Tax.AnnualReturnFiled {
		if d, err := time.Parse(DateLayout, date); err == nil && p.Tax.AnnualReturnFiled[fy] != date &&
			d.After(now.UTC().AddDate(0, 0, 1)) {
			errs.add(AnnualReturnKey, CodeInFuture)
		}
	}
	if len(errs) > 0 {
		return Result{}, errs
	}
	for k, codes := range Validate(next) {
		for _, c := range codes {
			errs.add(k, c)
		}
	}
	if len(errs) > 0 {
		return Result{}, errs
	}
	return Result{Policy: next, Changed: changed}, nil
}

// RequiresTaxManage reports whether any of keys is a tax-manage setting.
func RequiresTaxManage(keys []string) bool {
	for _, k := range keys {
		if s, ok := find(k); ok && s.TaxManage {
			return true
		}
	}
	return false
}

// Validate checks a whole policy: every setting's own rule plus the rules that
// span settings. It returns field errors keyed by setting key (empty when valid).
func Validate(p Policy) FieldErrors {
	errs := FieldErrors{}
	c := p.Clone()
	for _, s := range registry {
		if code := s.check(reflect.ValueOf(s.ptr(&c)).Elem().Interface()); code != "" {
			errs.add(s.Key, code)
		}
	}
	if len(errs) > 0 {
		return errs
	}
	if !slices.Contains(c.Fulfilment.ShortageAllowed, c.Fulfilment.ShortageDefault) {
		errs.add("fulfilment.shortage_default", CodeNotInAllowed)
	}
	switch c.Payment.AdvanceMode {
	case AdvancePercent:
		if c.Payment.AdvanceValue < 1 || c.Payment.AdvanceValue > 10000 {
			errs.add("payment.advance_value", CodeOutOfRange)
		}
	case AdvanceFixed:
		if c.Payment.AdvanceValue < 1 {
			errs.add("payment.advance_value", CodeValueRequired)
		}
	default:
		if c.Payment.AdvanceValue != 0 {
			errs.add("payment.advance_value", CodeValueNotAllowed)
		}
	}
	if c.Doc.InvoicePrefix == c.Doc.CreditNotePrefix {
		// The two series count separately: one prefix would let one number be both an
		// invoice and a credit note in a financial year.
		errs.add("doc.invoice_prefix", CodeDuplicate)
		errs.add("doc.credit_note_prefix", CodeDuplicate)
	}
	for _, code := range validateGSTRates(c.Tax.GSTRates) {
		errs.add(ManagedElsewhereKey, code)
	}
	return errs
}

// assign decodes raw into the setting's field. It returns "" on success or the
// refusal code; on a refusal the field is left as it was.
func (s Setting) assign(target any, raw json.RawMessage) string {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return CodeInvalidType
	}
	switch t := target.(type) {
	case *int:
		v, code := s.decodeBoundedInt(raw)
		if code == "" {
			*t = int(v)
		}
		return code
	case *int64:
		v, code := s.decodeBoundedInt(raw)
		if code == "" {
			*t = v
		}
		return code
	case *bool:
		return decodeChecked(raw, t, s.check)
	case *string:
		return decodeChecked(raw, t, s.check)
	case *[]string:
		code := decodeChecked(raw, t, s.check)
		if code == "" {
			*t = canonicalSubset(s.Options, *t)
		}
		return code
	case *[]int:
		code := decodeChecked(raw, t, s.check)
		if code == "" {
			slices.SortFunc(*t, func(a, b int) int { return b - a })
			*t = append([]int{}, *t...)
		}
		return code
	case *map[string]string:
		code := decodeChecked(raw, t, s.check)
		if code == "" && *t == nil {
			*t = map[string]string{}
		}
		return code
	default:
		return CodeInvalidType
	}
}

func (s Setting) decodeBoundedInt(raw json.RawMessage) (int64, string) {
	v, code := decodeInt(raw)
	if code != "" {
		return 0, code
	}
	return v, rangeCode(v, s.Min, s.Max)
}

// decodeChecked decodes raw into *T and runs check on the value; *dst is set
// only when both pass.
func decodeChecked[T any](raw json.RawMessage, dst *T, check func(any) string) string {
	var v T
	if !strictJSON(raw, &v) {
		return CodeInvalidType
	}
	if code := check(v); code != "" {
		return code
	}
	*dst = v
	return ""
}

// check returns the code a current value breaks, or "".
func (s Setting) check(v any) string {
	switch x := v.(type) {
	case int:
		return rangeCode(int64(x), s.Min, s.Max)
	case int64:
		return rangeCode(x, s.Min, s.Max)
	case string:
		return s.checkString(x)
	case []string:
		return s.checkOptionSet(x)
	case []int:
		return s.checkIntList(x)
	case map[string]string:
		if len(x) > MaxAnnualReturnEntries {
			return CodeTooMany
		}
		for fy, date := range x {
			if !validFY(fy) || CheckDate(date) != "" {
				return CodeInvalidFormat
			}
			// A return cannot be filed before the year it closes has ended.
			if d, _ := time.Parse(DateLayout, date); d.Before(fyEnd(fy)) {
				return CodeOutOfRange
			}
		}
	}
	return ""
}

func (s Setting) checkString(x string) string {
	if s.Kind == KindCode {
		if !codeRE.MatchString(x) {
			return CodeInvalidFormat
		}
		return ""
	}
	if !slices.Contains(s.Options, x) {
		return CodeNotAnOption
	}
	return ""
}

func (s Setting) checkOptionSet(x []string) string {
	if len(x) == 0 {
		return CodeRequired
	}
	seen := map[string]bool{}
	for _, o := range x {
		if !slices.Contains(s.Options, o) {
			return CodeNotAnOption
		}
		if seen[o] {
			return CodeDuplicate
		}
		seen[o] = true
	}
	return ""
}

func (s Setting) checkIntList(x []int) string {
	if len(x) > s.MaxItems {
		return CodeTooMany
	}
	seen := map[int]bool{}
	for _, d := range x {
		if int64(d) < s.Min || int64(d) > s.Max {
			return CodeOutOfRange
		}
		if seen[d] {
			return CodeDuplicate
		}
		seen[d] = true
	}
	return ""
}

func rangeCode(v, lo, hi int64) string {
	if v < lo || v > hi {
		return CodeOutOfRange
	}
	return ""
}

// MaxAnnualReturnEntries caps the filing map: it is copied into every history row
// and event and decoded on every replica read.
const MaxAnnualReturnEntries = 20

// AnnualReturnKey is the registry key of the filing dates.
const AnnualReturnKey = "tax.annual_return_filed"

// fyEnd is the last day of a financial year "YYYY-YY": 31 March of the closing
// year (the GST financial year these filing dates belong to).
func fyEnd(fy string) time.Time {
	m := fyRE.FindStringSubmatch(fy)
	start, _ := strconv.Atoi(m[1])
	return time.Date(start+1, time.March, 31, 0, 0, 0, 0, time.UTC)
}

// validFY reports whether s is a financial year "YYYY-YY" whose second part is
// the next year (2025-26).
func validFY(s string) bool {
	m := fyRE.FindStringSubmatch(s)
	if m == nil {
		return false
	}
	start, _ := strconv.Atoi(m[1])
	end, _ := strconv.Atoi(m[2])
	return (start+1)%100 == end
}

// canonicalSubset returns the chosen options in the registry's option order.
func canonicalSubset(options, chosen []string) []string {
	out := make([]string, 0, len(chosen))
	for _, o := range options {
		if slices.Contains(chosen, o) {
			out = append(out, o)
		}
	}
	return out
}

func decodeInt(raw json.RawMessage) (int64, string) {
	// encoding/json reads a quoted number into json.Number; a setting wants a number.
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte(`"`)) {
		return 0, CodeInvalidType
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var n json.Number
	if err := dec.Decode(&n); err != nil {
		return 0, CodeInvalidType
	}
	v, err := strconv.ParseInt(n.String(), 10, 64)
	if err != nil {
		return 0, CodeInvalidType
	}
	return v, ""
}

// strictJSON decodes raw into v, refusing unknown fields and trailing data.
func strictJSON(raw json.RawMessage, v any) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return false
	}
	return !dec.More()
}

// SettingInfo is the option metadata of one setting for the web: how to render
// the control and which permission changes it.
type SettingInfo struct {
	Key        string          `json:"key"`
	Group      string          `json:"group"`
	Kind       Kind            `json:"kind"`
	Options    []string        `json:"options,omitzero"`
	Min        *int64          `json:"min,omitzero"`
	Max        *int64          `json:"max,omitzero"`
	MaxItems   int             `json:"max_items,omitzero"`
	Default    json.RawMessage `json:"default"`
	Permission string          `json:"permission"`
}

// Permission names a setting is changed under (the suffix of
const (
	PermissionManage    = "manage"
	PermissionTaxManage = "tax.manage"
)

// Describe returns the option metadata of every patchable setting, with the
// defaults from [Default].
func Describe() []SettingInfo {
	def := Default()
	out := make([]SettingInfo, 0, len(registry))
	for _, s := range registry {
		info := SettingInfo{
			Key: s.Key, Group: s.Group, Kind: s.Kind, Options: slices.Clone(s.Options),
			MaxItems: s.MaxItems, Permission: PermissionManage,
		}
		info.Default, _ = json.Marshal(reflect.ValueOf(s.ptr(&def)).Elem().Interface())
		if s.TaxManage {
			info.Permission = PermissionTaxManage
		}
		if s.Kind == KindInt || s.Kind == KindIntList {
			lo, hi := s.Min, s.Max
			info.Min, info.Max = &lo, &hi
		}
		out = append(out, info)
	}
	return out
}

// String names a setting for a log line.
func (s Setting) String() string { return fmt.Sprintf("setting(%s)", s.Key) }
