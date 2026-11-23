package commercepolicy_test

import (
	"encoding/json"
	"github.com/google/go-cmp/cmp"
	"slices"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
)

func changes(t *testing.T, kv map[string]any) commercepolicy.Changes {
	t.Helper()
	out := commercepolicy.Changes{}
	for k, v := range kv {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		out[k] = b
	}
	return out
}

func TestDefault_OwnerRulings(t *testing.T) {
	t.Parallel()
	d := commercepolicy.Default()
	if d.Expiry.StopMonthsBefore != 1 {
		t.Errorf("expiry.stop_months_before default = %d, want 1", d.Expiry.StopMonthsBefore)
	}
	if len(d.Tax.GSTRates) != 0 {
		t.Errorf("GST has NO default rate: list must start empty, got %v", d.Tax.GSTRates)
	}
	if d.DrugSchedule.Mode != commercepolicy.DrugScheduleOptional {
		t.Errorf("drug_schedule.mode default = %q, want optional", d.DrugSchedule.Mode)
	}
	if errs := commercepolicy.Validate(d); len(errs) != 0 {
		t.Fatalf("Default() must validate, got %v", errs)
	}
}

func TestInitial_IsDefaultsAtVersionZero(t *testing.T) {
	t.Parallel()
	got := commercepolicy.Initial()
	if got.Version != 0 || !cmp.Equal(got.Policy, commercepolicy.Default()) {
		t.Fatalf("Initial() = %+v, want defaults at version 0", got)
	}
}

func TestEncodeDecode_RoundTripAndStrict(t *testing.T) {
	t.Parallel()
	raw, err := commercepolicy.Encode(commercepolicy.Default())
	if err != nil {
		t.Fatal(err)
	}
	got, err := commercepolicy.Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !cmp.Equal(got, commercepolicy.Default()) {
		t.Fatalf("round trip changed the policy:\n%+v\n%+v", got, commercepolicy.Default())
	}
	if _, err := commercepolicy.Decode([]byte(`{"expiry":{"stop_months_before":1,"surprise":2}}`)); err == nil {
		t.Fatal("an unknown key must be refused")
	}
	// A document with no keys takes every default (review M3): a row stored before a
	// setting existed must keep reading.
	if empty, err := commercepolicy.Decode([]byte(`{}`)); err != nil || !cmp.Equal(empty, commercepolicy.Default()) {
		t.Fatalf("an empty document must decode onto the defaults: %v", err)
	}
}

func TestApply_ExpiryRange(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		v    any
		code string
	}{
		{"zero is the legal limit", 0, ""},
		{"one", 1, ""},
		{"twenty four", 24, ""},
		{"negative is refused", -1, commercepolicy.CodeOutOfRange},
		{"over range", 25, commercepolicy.CodeOutOfRange},
		{"fraction", 1.5, commercepolicy.CodeInvalidType},
		{"string", "2", commercepolicy.CodeInvalidType},
		{"null", nil, commercepolicy.CodeInvalidType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, errs := applyAt(commercepolicy.Default(), changes(t, map[string]any{"expiry.stop_months_before": tc.v}))
			got := ""
			if c := errs["expiry.stop_months_before"]; len(c) > 0 {
				got = c[0]
			}
			if got != tc.code {
				t.Fatalf("code = %q, want %q (errs %v)", got, tc.code, errs)
			}
		})
	}
}

func TestApply_ChangedKeysAndNoOp(t *testing.T) {
	t.Parallel()
	d := commercepolicy.Default()
	res, errs := applyAt(d, changes(t, map[string]any{
		"expiry.stop_months_before": 3,
		"pricing.discount_enabled":  true, // already true: not a change
		"eway.enforcement":          "block",
	}))
	if errs != nil {
		t.Fatalf("errs: %v", errs)
	}
	want := []string{"eway.enforcement", "expiry.stop_months_before"}
	if !slices.Equal(res.Changed, want) {
		t.Fatalf("changed = %v, want %v", res.Changed, want)
	}
	if res.Policy.Expiry.StopMonthsBefore != 3 || d.Expiry.StopMonthsBefore != 1 {
		t.Fatal("Apply must change the copy, never the input")
	}
	again, errs := applyAt(res.Policy, changes(t, map[string]any{"expiry.stop_months_before": 3}))
	if errs != nil || len(again.Changed) != 0 {
		t.Fatalf("re-applying the same value must be a no-op: %v %v", again.Changed, errs)
	}
}

func TestApply_NothingHalfApplied(t *testing.T) {
	t.Parallel()
	res, errs := applyAt(commercepolicy.Default(), changes(t, map[string]any{
		"expiry.stop_months_before": 2,
		"eway.enforcement":          "maybe",
	}))
	if errs["eway.enforcement"][0] != commercepolicy.CodeNotAnOption {
		t.Fatalf("errs = %v", errs)
	}
	if !cmp.Equal(res, commercepolicy.Result{}) {
		t.Fatal("a refused patch must return no policy")
	}
}

func TestApply_UnknownAndManagedElsewhere(t *testing.T) {
	t.Parallel()
	_, errs := applyAt(commercepolicy.Default(), changes(t, map[string]any{
		"expiry.grace_days": 3,
		"tax.gst_rates":     []any{},
	}))
	if errs["expiry.grace_days"][0] != commercepolicy.CodeUnknownSetting {
		t.Errorf("unknown key: %v", errs)
	}
	if errs["tax.gst_rates"][0] != commercepolicy.CodeManagedElsewhere {
		t.Errorf("gst_rates must be refused in a patch: %v", errs)
	}
}

func TestApply_WarningDays(t *testing.T) {
	t.Parallel()
	res, errs := applyAt(commercepolicy.Default(), changes(t, map[string]any{"expiry.warning_days": []int{30, 365, 60}}))
	if errs != nil {
		t.Fatal(errs)
	}
	if !slices.Equal(res.Policy.Expiry.WarningDays, []int{365, 60, 30}) {
		t.Fatalf("warning days stored descending, got %v", res.Policy.Expiry.WarningDays)
	}
	for name, v := range map[string][]int{
		"five values":  {1, 2, 3, 4, 5},
		"zero day":     {0},
		"over two yrs": {731},
		"duplicate":    {30, 30},
	} {
		_, errs := applyAt(commercepolicy.Default(), changes(t, map[string]any{"expiry.warning_days": v}))
		if len(errs["expiry.warning_days"]) == 0 {
			t.Errorf("%s must be refused", name)
		}
	}
	if _, errs := applyAt(commercepolicy.Default(), changes(t, map[string]any{"expiry.warning_days": []int{}})); errs != nil {
		t.Errorf("an empty list switches tiered warnings off: %v", errs)
	}
}

func TestApply_ShortageChoices(t *testing.T) {
	t.Parallel()
	d := commercepolicy.Default()
	if _, errs := applyAt(d, changes(t, map[string]any{"fulfilment.shortage_allowed": []string{}})); len(errs["fulfilment.shortage_allowed"]) == 0 {
		t.Error("empty allowed set must be refused")
	}
	// Removing the default from the allowed set is refused on the default key.
	_, errs := applyAt(d, changes(t, map[string]any{"fulfilment.shortage_allowed": []string{"refuse"}}))
	if errs["fulfilment.shortage_default"][0] != commercepolicy.CodeNotInAllowed {
		t.Errorf("default outside allowed: %v", errs)
	}
	// Both together is fine, and stored in canonical order.
	res, errs := applyAt(d, changes(t, map[string]any{
		"fulfilment.shortage_allowed": []string{"partial_reduce", "refuse"},
		"fulfilment.shortage_default": "refuse",
	}))
	if errs != nil {
		t.Fatal(errs)
	}
	if !slices.Equal(res.Policy.Fulfilment.ShortageAllowed, []string{"refuse", "partial_reduce"}) {
		t.Fatalf("allowed = %v", res.Policy.Fulfilment.ShortageAllowed)
	}
	if _, errs := applyAt(d, changes(t, map[string]any{"fulfilment.shortage_allowed": []string{"hold", "hold"}})); len(errs) == 0 {
		t.Error("duplicate option must be refused")
	}
}

func TestApply_AdvanceModeAndValue(t *testing.T) {
	t.Parallel()
	d := commercepolicy.Default()
	cases := []struct {
		name string
		kv   map[string]any
		key  string
		code string
	}{
		{"percent needs a value", map[string]any{"payment.advance_mode": "percent"}, "payment.advance_value", commercepolicy.CodeValueRequired},
		{"percent over 100", map[string]any{"payment.advance_mode": "percent", "payment.advance_value": 10001}, "payment.advance_value", commercepolicy.CodeOutOfRange},
		{"fixed needs a value", map[string]any{"payment.advance_mode": "fixed"}, "payment.advance_value", commercepolicy.CodeValueRequired},
		{"any amount takes no value", map[string]any{"payment.advance_value": 500}, "payment.advance_value", commercepolicy.CodeValueNotAllowed},
	}
	for _, tc := range cases {
		_, errs := applyAt(d, changes(t, tc.kv))
		if len(errs[tc.key]) == 0 || errs[tc.key][0] != tc.code {
			t.Errorf("%s: errs = %v, want %s on %s", tc.name, errs, tc.code, tc.key)
		}
	}
	res, errs := applyAt(d, changes(t, map[string]any{"payment.advance_mode": "percent", "payment.advance_value": 2500}))
	if errs != nil || res.Policy.Payment.AdvanceValue != 2500 {
		t.Fatalf("25%% advance: %v %v", res.Policy.Payment, errs)
	}
}

func TestApply_PrefixAndAnnualReturn(t *testing.T) {
	t.Parallel()
	d := commercepolicy.Default()
	for _, bad := range []string{"", "inv", "INVOI", "IN-V", "I N"} {
		if _, errs := applyAt(d, changes(t, map[string]any{"doc.invoice_prefix": bad})); len(errs["doc.invoice_prefix"]) == 0 {
			t.Errorf("prefix %q must be refused", bad)
		}
	}
	if _, errs := applyAt(d, changes(t, map[string]any{"doc.invoice_prefix": "A1B2"})); errs != nil {
		t.Errorf("A1B2 is a valid prefix: %v", errs)
	}
	ok := map[string]string{"2025-26": "2026-09-30"}
	if res, errs := applyAt(d, changes(t, map[string]any{"tax.annual_return_filed": ok})); errs != nil || res.Policy.Tax.AnnualReturnFiled["2025-26"] != "2026-09-30" {
		t.Errorf("valid filing: %v", errs)
	}
	for _, bad := range []map[string]string{{"2025-27": "2026-09-30"}, {"2025-26": "30/09/2026"}, {"FY26": "2026-09-30"}} {
		if _, errs := applyAt(d, changes(t, map[string]any{"tax.annual_return_filed": bad})); len(errs["tax.annual_return_filed"]) == 0 {
			t.Errorf("%v must be refused", bad)
		}
	}
}

func TestRequiresTaxManage(t *testing.T) {
	t.Parallel()
	if commercepolicy.RequiresTaxManage([]string{"expiry.stop_months_before", "credit.enabled"}) {
		t.Error("expiry and credit are manage-only")
	}
	for _, k := range []string{"fulfilment.invoice_timing", "pricing.scheme_valuation", "invoice.round_off", "tax.credit_note_split",
		"tax.annual_return_filed", "doc.invoice_prefix", "doc.credit_note_prefix", "einvoice.mode", "eway.enforcement", "drug_schedule.mode"} {
		if !commercepolicy.RequiresTaxManage([]string{"expiry.stop_months_before", k}) {
			t.Errorf("%s is a tax-manage setting", k)
		}
	}
}

func TestDescribe_CoversEveryRegistrySettingWithDefault(t *testing.T) {
	t.Parallel()
	info := commercepolicy.Describe()
	if len(info) != len(commercepolicy.Settings()) {
		t.Fatalf("describe = %d, registry = %d", len(info), len(commercepolicy.Settings()))
	}
	for _, i := range info {
		v, _ := commercepolicy.Get(commercepolicy.Default(), i.Key)
		want, _ := json.Marshal(v)
		if string(i.Default) != string(want) {
			t.Errorf("%s default metadata %v != %v", i.Key, i.Default, want)
		}
		if i.Permission != commercepolicy.PermissionManage && i.Permission != commercepolicy.PermissionTaxManage {
			t.Errorf("%s permission %q", i.Key, i.Permission)
		}
	}
}

func TestCheckGSTRate(t *testing.T) {
	t.Parallel()
	if errs := commercepolicy.CheckGSTRate(500, "GST 5%", "A. Mehta"); len(errs) != 0 {
		t.Fatalf("valid rate refused: %v", errs)
	}
	errs := commercepolicy.CheckGSTRate(-1, "", "x\x00y")
	for field, code := range map[string]string{
		"rate_bps": commercepolicy.CodeOutOfRange, "label": commercepolicy.CodeRequired, "confirmed_by": commercepolicy.CodeInvalidCharacters,
	} {
		if len(errs[field]) == 0 || errs[field][0] != code {
			t.Errorf("%s: %v want %s", field, errs[field], code)
		}
	}
	if len(commercepolicy.CheckGSTRate(10001, "x", "y")["rate_bps"]) == 0 {
		t.Error("over 100% must be refused")
	}
	if commercepolicy.CheckDate("2026-02-30") == "" {
		t.Error("an impossible date must be refused")
	}
}

func TestValidate_RejectsDuplicateGSTRates(t *testing.T) {
	t.Parallel()
	p := commercepolicy.Default()
	r := commercepolicy.GSTRate{ID: "a", RateBps: 500, Label: "GST 5%", ConfirmedBy: "A", ConfirmedOn: "2026-01-01", Active: true}
	r2 := r
	r2.ID = "b"
	r2.Label = "gst 5%"
	p.Tax.GSTRates = []commercepolicy.GSTRate{r, r2}
	if len(commercepolicy.Validate(p)) == 0 {
		t.Fatal("the same rate under the same label (any case) twice must be refused")
	}
}
