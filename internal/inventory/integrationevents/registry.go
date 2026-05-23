package integrationevents

// allEvents holds one zero-value of every V1/V2/… record defined in
// this package. Each record file calls [register] in its package-level
// var block; the arch test iterates this slice to enforce marker +
// alias-regex compliance across the catalogue.
var allEvents []Event

// register appends e to the package-level catalogue + returns it
// unchanged so the caller can write a single-line var block:
//
//	var _ = register(ProductCreatedV1{})
func register(e Event) Event {
	allEvents = append(allEvents, e)
	return e
}

// all returns a defensive copy of the registered catalogue. Used by
// arch tests in the same package (no external consumers).
func all() []Event {
	out := make([]Event, len(allEvents))
	copy(out, allEvents)
	return out
}
