package ports

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/leadkart/leadkart-go/internal/common/textlimit"
	"github.com/leadkart/leadkart-go/internal/inventory/app/query"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// Rule codes carried in errors{field:[code]}. Wire-stable; the names follow the
// identity module's (`required`, `too_long`, `invalid_characters`, `invalid_type`).
const (
	ruleTooLong           = "too_long"
	ruleInvalidCharacters = "invalid_characters"
	ruleTooSmall          = "too_small"
	ruleTooLarge          = "too_large"
	ruleInvalidType       = "invalid_type"
)

// msgFieldsInvalid is the fixed message of a refusal that names its fields.
const msgFieldsInvalid = "one or more fields are invalid; see errors"

// fieldErrors collects errors{field:[code]} for one request.
type fieldErrors map[string][]string

func (f fieldErrors) add(field, rule string) { f[field] = append(f[field], rule) }

// intRange records too_small / too_large for a sent value outside [min, max].
func (f fieldErrors) intRange(field string, v *int, lo, hi int) {
	switch {
	case v == nil:
	case *v < lo:
		f.add(field, ruleTooSmall)
	case *v > hi:
		f.add(field, ruleTooLarge)
	}
}

// text records invalid_characters / too_long for a sent value (trimmed, as the
// aggregate trims it) over max characters or holding the NUL character.
func (f fieldErrors) text(field string, v *string, maxChars int) {
	if v == nil {
		return
	}
	err := textlimit.Chars(field, strings.TrimSpace(*v), maxChars)
	switch {
	case errors.Is(err, textlimit.ErrInvalidCharacters):
		f.add(field, ruleInvalidCharacters)
	case errors.Is(err, textlimit.ErrTooLong):
		f.add(field, ruleTooLong)
	}
}

// productFieldErrors checks the product form's catalogue and alert fields
// against the aggregate's own bounds, so a refusal can name the field. A nil
// pointer is a field the request did not send. Returns nil when all pass.
func productFieldErrors(category *string, reorderLevel, thresholdDays *int) map[string][]string {
	errs := fieldErrors{}
	errs.text("product_category", category, product.ProductCategoryMaxLength)
	errs.intRange("reorder_level", reorderLevel, product.ReorderLevelMin, product.ReorderLevelMax)
	errs.intRange("expiry_alert_threshold_days", thresholdDays,
		product.ExpiryAlertThresholdDaysMin, product.ExpiryAlertThresholdDaysMax)
	if len(errs) == 0 {
		return nil
	}
	return errs
}

// productStockFilters reads the product list's stock filters (`category`,
// `low_stock`, `expiring_within_days`) into filter. A value the server cannot
// read is refused by name, never ignored: a list that silently dropped
// `low_stock=yes` would show every product as if it were low on stock.
// Returns nil when all pass.
func productStockFilters(q url.Values, filter *query.ProductListFilter) map[string][]string {
	errs := fieldErrors{}

	category := strings.TrimSpace(q.Get("category"))
	errs.text("category", &category, product.ProductCategoryMaxLength)
	filter.Category = category

	switch q.Get("low_stock") {
	case "", "false":
	case "true":
		filter.LowStock = true
	default:
		errs.add("low_stock", ruleInvalidType)
	}

	if raw := q.Get("expiring_within_days"); raw != "" {
		days, err := strconv.Atoi(raw)
		if err != nil {
			errs.add("expiring_within_days", ruleInvalidType)
		} else {
			errs.intRange("expiring_within_days", &days, 0, product.ExpiryAlertThresholdDaysMax)
			filter.ExpiringWithinDays = &days
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return errs
}

// writeFieldErrors emits a refusal that names the fields at fault.
func writeFieldErrors(w http.ResponseWriter, status int, code string, errs map[string][]string) {
	writeJSON(w, status, ErrorResponse{
		Type:    problemType(code),
		Title:   http.StatusText(status),
		Status:  status,
		Detail:  msgFieldsInvalid,
		Error:   code,
		Message: msgFieldsInvalid,
		Errors:  errs,
	})
}
