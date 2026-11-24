// Package lifecycle is the one vocabulary of "Inactive, never deleted" (ADR 0073): the ?status=
// filter every list of a record that can be deactivated takes, and the per-status counts those
// lists return for their status chips. Members, products, roles and drug schedule classes share it.
package lifecycle

// StatusFilter selects which records a listing returns.
type StatusFilter string

// The three listings of a ?status= parameter.
const (
	FilterActive   StatusFilter = "active"
	FilterInactive StatusFilter = "inactive"
	FilterAll      StatusFilter = "all"
)

// ParseStatusFilter reads the ?status= query value; an empty value is [FilterActive] (the
// listing's default). The bool is false for any other value.
func ParseStatusFilter(s string) (StatusFilter, bool) {
	switch StatusFilter(s) {
	case "", FilterActive:
		return FilterActive, true
	case FilterInactive:
		return FilterInactive, true
	case FilterAll:
		return FilterAll, true
	}
	return "", false
}

// IncludesActive reports whether the listing keeps active records.
// The zero value counts as [FilterActive], the default.
func (f StatusFilter) IncludesActive() bool { return f != FilterInactive }

// IncludesInactive reports whether the listing keeps inactive records.
func (f StatusFilter) IncludesInactive() bool { return f == FilterInactive || f == FilterAll }

// Counts is how many records are in each status, whatever the listing's filter is. It is also
// the wire shape of the lists' `status_counts`.
type Counts struct {
	Active   int `json:"active"`
	Inactive int `json:"inactive"`
}
