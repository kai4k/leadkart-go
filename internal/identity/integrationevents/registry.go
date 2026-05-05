package integrationevents

// allEvents holds one zero-value of every V1/V2/… record defined in
// this package. Each record file calls [register] in its package-level
// var block; the arch test iterates this slice to enforce marker +
// alias-regex compliance across the catalogue.
//
// The slice is unexported — production code never enumerates it. Tests
// access it via [All] in arch_test.go (same package, no exported API
// surface needed by callers outside the test boundary).
var allEvents []Event

// register appends e to the package-level catalogue + returns it
// unchanged so the caller can write a single-line var block:
//
//	var _ = register(TenantRegisteredV1{})
//
// The return-passthrough lets compile-time assertions sit alongside
// without an extra statement.
func register(e Event) Event {
	allEvents = append(allEvents, e)
	return e
}

// All returns a defensive copy of the registered catalogue. Used by
// arch tests in the same package (no external consumers).
func all() []Event {
	out := make([]Event, len(allEvents))
	copy(out, allEvents)
	return out
}
