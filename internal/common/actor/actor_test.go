package actor_test

import (
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/actor"
)

func TestPerson_RoundTripAndAbsent(t *testing.T) {
	t.Parallel()
	if _, ok := actor.PersonFromContext(t.Context()); ok {
		t.Fatal("empty ctx reports an actor")
	}
	if ctx := actor.WithPerson(t.Context(), ""); ctx != t.Context() {
		if _, ok := actor.PersonFromContext(ctx); ok {
			t.Fatal("empty id stored")
		}
	}
	id, ok := actor.PersonFromContext(actor.WithPerson(t.Context(), "p-1"))
	if !ok || id != "p-1" {
		t.Fatalf("got %q %v", id, ok)
	}
}
