package integrationevents

// allEvents holds one zero-value of every V{N} record in this package.
// Each record file calls [register] in its package-level var block; the
// arch test iterates this slice to enforce marker + alias-regex compliance.
//
// Mirror of internal/platform/integrationevents/registry.go.
var allEvents []Event //nolint:gochecknoglobals // arch-test registry mirror of platform catalogue

// register appends e to the catalogue and returns it unchanged, enabling
// a single-line var declaration:
//
//	var _ = register(OrderPackedV1{})
func register(e Event) Event {
	allEvents = append(allEvents, e)
	return e
}

// all returns a defensive copy of the registered catalogue. Used by arch tests.
func all() []Event {
	out := make([]Event, len(allEvents))
	copy(out, allEvents)
	return out
}
