package integrationevents

// allEvents holds one zero-value of every V1/V2/… record in this package; the
// arch test iterates it to enforce marker and alias compliance.
//
//nolint:gochecknoglobals // arch-test registry mirror of the other modules' catalogues
var allEvents []Event

// register appends e to the catalogue and returns it unchanged.
func register(e Event) Event {
	allEvents = append(allEvents, e)
	return e
}

// all returns a defensive copy of the registered catalogue.
func all() []Event {
	out := make([]Event, len(allEvents))
	copy(out, allEvents)
	return out
}
