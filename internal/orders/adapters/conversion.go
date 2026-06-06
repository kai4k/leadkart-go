package adapters

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
)

// ----- JSONB value-object serialisation -------------------------------------
//
// Nested aggregate VOs (line items, revisions, tax lines) persist as JSONB.
// Adapter-local DTOs with snake_case tags decouple the stored shape from the
// domain struct's Go field names (state-based persistence, ADR 0063).

type jsonLineItem struct {
	ProductID     string `json:"product_id"`
	SKU           string `json:"sku"`
	Description   string `json:"description"`
	Quantity      int32  `json:"quantity"`
	UnitMrpPaise  int64  `json:"unit_mrp_paise"`
	UnitSalePaise int64  `json:"unit_sale_paise"`
	GstRateBps    int32  `json:"gst_rate_bps"`
}

type jsonRevision struct {
	Number              int64          `json:"number"`
	Items               []jsonLineItem `json:"items"`
	Note                string         `json:"note"`
	RevisedAt           time.Time      `json:"revised_at"`
	RevisedByMembership string         `json:"revised_by_membership"`
}

type jsonTaxLine struct {
	HSNCode           string `json:"hsn_code"`
	GSTRateBps        int32  `json:"gst_rate_bps"`
	TaxableValuePaise int64  `json:"taxable_value_paise"`
	TaxAmountPaise    int64  `json:"tax_amount_paise"`
}

func lineItemToJSON(li quotation.LineItem) jsonLineItem {
	return jsonLineItem{
		ProductID: li.ProductID, SKU: li.SKU, Description: li.Description,
		Quantity: li.Quantity, UnitMrpPaise: li.UnitMrpPaise,
		UnitSalePaise: li.UnitSalePaise, GstRateBps: li.GstRateBps,
	}
}

func lineItemFromJSON(j jsonLineItem) quotation.LineItem {
	return quotation.LineItem{
		ProductID: j.ProductID, SKU: j.SKU, Description: j.Description,
		Quantity: j.Quantity, UnitMrpPaise: j.UnitMrpPaise,
		UnitSalePaise: j.UnitSalePaise, GstRateBps: j.GstRateBps,
	}
}

func marshalLineItems(items []quotation.LineItem) ([]byte, error) {
	out := make([]jsonLineItem, len(items))
	for i, li := range items {
		out[i] = lineItemToJSON(li)
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("orders repo: marshal line items: %w", err)
	}
	return b, nil
}

func unmarshalLineItems(raw []byte) ([]quotation.LineItem, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var in []jsonLineItem
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("orders repo: unmarshal line items: %w", err)
	}
	out := make([]quotation.LineItem, len(in))
	for i, j := range in {
		out[i] = lineItemFromJSON(j)
	}
	return out, nil
}

func marshalRevisions(revs []quotation.Revision) ([]byte, error) {
	out := make([]jsonRevision, len(revs))
	for i, r := range revs {
		items := make([]jsonLineItem, len(r.Items))
		for j, li := range r.Items {
			items[j] = lineItemToJSON(li)
		}
		out[i] = jsonRevision{
			Number: r.Number, Items: items, Note: r.Note,
			RevisedAt: r.RevisedAt.UTC(), RevisedByMembership: r.RevisedByMembership.String(),
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("orders repo: marshal revisions: %w", err)
	}
	return b, nil
}

func unmarshalRevisions(raw []byte) ([]quotation.Revision, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var in []jsonRevision
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("orders repo: unmarshal revisions: %w", err)
	}
	out := make([]quotation.Revision, len(in))
	for i, jr := range in {
		items := make([]quotation.LineItem, len(jr.Items))
		for j, ji := range jr.Items {
			items[j] = lineItemFromJSON(ji)
		}
		out[i] = quotation.Revision{
			Number: jr.Number, Items: items, Note: jr.Note,
			RevisedAt: jr.RevisedAt.UTC(), RevisedByMembership: membership.ID(jr.RevisedByMembership),
		}
	}
	return out, nil
}

func marshalTaxLines(lines []invoice.TaxLine) ([]byte, error) {
	out := make([]jsonTaxLine, len(lines))
	for i, tl := range lines {
		out[i] = jsonTaxLine{
			HSNCode: tl.HSNCode, GSTRateBps: tl.GSTRateBps,
			TaxableValuePaise: tl.TaxableValuePaise, TaxAmountPaise: tl.TaxAmountPaise,
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("orders repo: marshal tax lines: %w", err)
	}
	return b, nil
}

func unmarshalTaxLines(raw []byte) ([]invoice.TaxLine, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var in []jsonTaxLine
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("orders repo: unmarshal tax lines: %w", err)
	}
	out := make([]invoice.TaxLine, len(in))
	for i, j := range in {
		out[i] = invoice.TaxLine{
			HSNCode: j.HSNCode, GSTRateBps: j.GSTRateBps,
			TaxableValuePaise: j.TaxableValuePaise, TaxAmountPaise: j.TaxAmountPaise,
		}
	}
	return out, nil
}

// ----- nullable-membership conversion ---------------------------------------

// pgUUIDFromMembershipPtr maps an optional membership pointer to a nullable
// pgtype.UUID. nil → NULL; a malformed id → NULL (impossible for validated
// aggregates).
func pgUUIDFromMembershipPtr(p *membership.ID) pgtype.UUID {
	if p == nil {
		return pgconv.PgUUIDOrNull(uuid.Nil)
	}
	parsed, err := uuid.Parse(p.String())
	if err != nil {
		return pgconv.PgUUIDOrNull(uuid.Nil)
	}
	return pgconv.PgUUIDOrNull(parsed)
}

// membershipPtrFromPg maps a nullable pgtype.UUID back to an optional
// membership pointer. NULL → nil.
func membershipPtrFromPg(p pgtype.UUID) *membership.ID {
	if !p.Valid {
		return nil
	}
	m := membership.ID(uuid.UUID(p.Bytes).String())
	return &m
}

// uuidStringPtrOrNil maps an optional cross-module UUID string column to a
// nullable pgtype.UUID. "" → NULL.
func pgUUIDFromStringOrNull(s string) pgtype.UUID {
	parsed, err := uuid.Parse(s)
	if err != nil {
		return pgconv.PgUUIDOrNull(uuid.Nil)
	}
	return pgconv.PgUUIDOrNull(parsed)
}

// stringFromPgUUID returns the string form of a nullable uuid, "" when NULL.
func stringFromPgUUID(p pgtype.UUID) string {
	if !p.Valid {
		return ""
	}
	return uuid.UUID(p.Bytes).String()
}
