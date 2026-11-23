package commercepolicy_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
)

var reviewNow = time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

func codesOf(t *testing.T, errs commercepolicy.FieldErrors, key string) []string {
	t.Helper()
	return errs[key]
}

// H3: the same integer is basis points under percent and paise under fixed, so a
// change of mode alone silently re-reads the stored value in another unit.
func TestApply_ChangingTheAdvanceModeRequiresTheValueInTheSamePatch(t *testing.T) {
	t.Parallel()
	p := commercepolicy.Default()
	p.Payment.AdvanceMode, p.Payment.AdvanceValue = commercepolicy.AdvancePercent, 2500

	_, errs := applyAt(p, changes(t, map[string]any{"payment.advance_mode": "fixed"}))
	if got := codesOf(t, errs, "payment.advance_value"); !slices.Contains(got, commercepolicy.CodeValueRequired) {
		t.Fatalf("mode alone percent->fixed: errs = %v, want advance_value: value_required", errs)
	}
	res, errs := applyAt(p, changes(t, map[string]any{"payment.advance_mode": "fixed", "payment.advance_value": 5000000}))
	if len(errs) != 0 || res.Policy.Payment.AdvanceValue != 5000000 {
		t.Fatalf("mode and value together: %v %+v", errs, res)
	}
	// Leaving a valued mode for none still zeroes the value through the existing rule.
	if _, errs := applyAt(p, changes(t, map[string]any{"payment.advance_mode": "none"})); len(errs) == 0 {
		t.Fatal("none with a stored value must be refused")
	}
	res, errs = applyAt(p, changes(t, map[string]any{"payment.advance_mode": "none", "payment.advance_value": 0}))
	if len(errs) != 0 || res.Policy.Payment.AdvanceValue != 0 {
		t.Fatalf("none with value 0: %v", errs)
	}
}

// M1: two series that count separately must not share a prefix.
func TestApply_TheTwoDocumentPrefixesMustDiffer(t *testing.T) {
	t.Parallel()
	p := commercepolicy.Default()
	_, errs := applyAt(p, changes(t, map[string]any{"doc.invoice_prefix": p.Doc.CreditNotePrefix}))
	if got := codesOf(t, errs, "doc.invoice_prefix"); !slices.Contains(got, commercepolicy.CodeDuplicate) {
		t.Fatalf("equal prefixes: %v", errs)
	}
	if errs := commercepolicy.Validate(func() commercepolicy.Policy { q := p; q.Doc.CreditNotePrefix = q.Doc.InvoicePrefix; return q }()); len(errs["doc.credit_note_prefix"]) == 0 && len(errs["doc.invoice_prefix"]) == 0 {
		t.Fatal("Validate must refuse equal prefixes too")
	}
}

// M3: a stored row without a newer key takes its default; history is read as
// stored, never re-judged against today's bounds.
func TestDecode_AMissingKeyTakesItsDefaultAndHistoryIsNotRevalidated(t *testing.T) {
	t.Parallel()
	raw, err := commercepolicy.Encode(commercepolicy.Default())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "drug_schedule")
	var pay map[string]json.RawMessage
	if err := json.Unmarshal(m["payment"], &pay); err != nil {
		t.Fatal(err)
	}
	delete(pay, "overpayment")
	var err2 error
	if m["payment"], err2 = json.Marshal(pay); err2 != nil {
		t.Fatal(err2)
	}
	old, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	got, err := commercepolicy.Decode(old)
	if err != nil {
		t.Fatalf("a row from before a key existed must decode onto the defaults: %v", err)
	}
	def := commercepolicy.Default()
	if got.Payment.Overpayment != def.Payment.Overpayment || got.DrugSchedule.Mode != def.DrugSchedule.Mode {
		t.Fatalf("missing keys must take their defaults: %+v", got)
	}

	// History: a stored value outside today's bounds still reads.
	tight := commercepolicy.Default()
	tight.Expiry.StopMonthsBefore = 999
	rawTight, _ := commercepolicy.Encode(tight)
	if _, err := commercepolicy.Decode(rawTight); err == nil {
		t.Fatal("Decode of a live policy must still validate")
	}
	hist, err := commercepolicy.DecodeStored(rawTight)
	if err != nil || hist.Expiry.StopMonthsBefore != 999 {
		t.Fatalf("DecodeStored must read history as stored: %v %+v", err, hist)
	}
}

// M6: the filing map is capped, and a filing date must come after the year it
// closes and not lie in the future.
func TestApply_AnnualReturnFiledIsCappedAndDatesMakeSense(t *testing.T) {
	t.Parallel()
	p := commercepolicy.Default()
	good := changes(t, map[string]any{"tax.annual_return_filed": map[string]string{"2025-26": "2026-10-15"}})
	if _, errs := applyAt(p, good); len(errs) != 0 {
		t.Fatalf("a sensible filing: %v", errs)
	}
	before := changes(t, map[string]any{"tax.annual_return_filed": map[string]string{"2025-26": "2001-01-01"}})
	if _, errs := applyAt(p, before); len(errs["tax.annual_return_filed"]) == 0 {
		t.Fatal("a filing date before the year ends must be refused")
	}
	future := changes(t, map[string]any{"tax.annual_return_filed": map[string]string{"2025-26": "2027-12-01"}})
	if _, errs := applyAt(p, future); !slices.Contains(errs["tax.annual_return_filed"], commercepolicy.CodeInFuture) {
		t.Fatalf("a filing date in the future: %v", errs)
	}
	big := map[string]string{}
	for i := range 21 {
		big[fmt.Sprintf("%d-%02d", 2000+i, (2001+i)%100)] = "2026-01-01"
	}
	if _, errs := applyAt(p, changes(t, map[string]any{"tax.annual_return_filed": big})); !slices.Contains(errs["tax.annual_return_filed"], commercepolicy.CodeTooMany) {
		t.Fatalf("21 entries: %v", errs)
	}
}

// applyAt is Apply at the fixed test clock.
func applyAt(p commercepolicy.Policy, c commercepolicy.Changes) (commercepolicy.Result, commercepolicy.FieldErrors) {
	return commercepolicy.Apply(p, c, reviewNow)
}
