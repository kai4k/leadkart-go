package integrationevents

// allEvents holds one zero-value of every V{N} record in this package. Each
// record file calls [register] in its var block; the arch test iterates this
// to enforce marker + alias-regex compliance. Mirrors identity's registry.go.
var allEvents []Event

// register appends e to the catalogue and returns it unchanged, so callers can
// write a one-line var block:
//
//	var _ = register(LeadPurchasedV1{})
func register(e Event) Event {
	allEvents = append(allEvents, e)
	return e
}

// all returns a defensive copy of the catalogue. Used by in-package arch tests.
func all() []Event {
	out := make([]Event, len(allEvents))
	copy(out, allEvents)
	return out
}
