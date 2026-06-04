package integrationevents

// allEvents holds one zero-value of every V1/V2/… record defined in
// this package. Each record file calls [register] in its
// package-level var block; the arch test iterates this slice to
// enforce marker + alias-regex compliance across the catalogue.
var allEvents []Event //nolint:gochecknoglobals // arch-test registry mirror of crm catalogue

// register appends e to the package-level catalogue + returns it
// unchanged so the caller can write a single-line var block:
//
//	var _ = register(WorkItemCreatedV1{})
func register(e Event) Event {
	allEvents = append(allEvents, e)
	return e
}

// all returns a defensive copy of the registered catalogue. Used by
// arch tests in the same package.
func all() []Event {
	out := make([]Event, len(allEvents))
	copy(out, allEvents)
	return out
}
