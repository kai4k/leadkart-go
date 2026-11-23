package person_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/humantext"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

var rlo = string(rune(0x202e))

var rloName = "Amit" + rlo + "Rao"

func requireInvalidCharacters(t *testing.T, err error, field string) {
	t.Helper()
	var he *humantext.Error
	if !errors.Is(err, person.ErrInvalid) || !errors.As(err, &he) || he.Field != field {
		t.Fatalf("err = %v, want person.ErrInvalid + humantext.Error for %q", err, field)
	}
}

func TestNew_RefusesNamesWithDirectionOverrideOrControlCharacters(t *testing.T) {
	t.Parallel()
	_, err := person.New(newID(t), mustEmail(t, "a@b.io"), rloName, "B", mustHash(t), testNow)
	requireInvalidCharacters(t, err, "first_name")
	_, err = person.New(newID(t), mustEmail(t, "a@b.io"), "A", "B\x00", mustHash(t), testNow)
	requireInvalidCharacters(t, err, "last_name")
}

func TestUpdateProfile_RefusesAChangedNameButAcceptsAnUnchangedLegacyOne(t *testing.T) {
	t.Parallel()
	p := person.UnmarshalFromDB(person.Snapshot{ID: newID(t), Email: mustEmail(t, "a@b.io"), FirstName: rloName, LastName: "Rao",
		PasswordHash: mustHash(t), IsActive: true, CreatedAt: testNow})

	if err := p.UpdateProfile(rloName, "Rao", testNow); err != nil {
		t.Fatalf("unchanged legacy first name must be accepted: %v", err)
	}
	requireInvalidCharacters(t, p.UpdateProfile(rloName+"x", "Rao", testNow), "first_name")
	requireInvalidCharacters(t, p.UpdateProfile(rloName, "Rao"+string(rune(0x200b)), testNow), "last_name")
	if p.FirstName() != rloName || p.LastName() != "Rao" {
		t.Fatal("a refused update must change nothing")
	}
}

func TestSuspend_RefuseReasonsWithControlCharacters(t *testing.T) {
	t.Parallel()
	requireInvalidCharacters(t, newPerson(t).GloballySuspend("fraud"+rlo+"claim", testNow), "reason")
}
