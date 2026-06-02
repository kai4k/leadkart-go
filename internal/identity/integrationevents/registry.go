package integrationevents

// allEvents holds one zero-value of every V1/V2/… record. Each record file
// calls [register] in its package-level var block; arch tests iterate this
// slice to enforce marker + alias-regex compliance. Unexported: tests access
// via [all] (same package).
var allEvents []Event

// register appends e to the catalogue and returns it, enabling single-line
// var blocks alongside compile-time assertions:
//
//	var _ = register(TenantRegisteredV1{})
func register(e Event) Event {
	allEvents = append(allEvents, e)
	return e
}

// all returns a defensive copy of the registered catalogue for arch tests.
func all() []Event {
	out := make([]Event, len(allEvents))
	copy(out, allEvents)
	return out
}
