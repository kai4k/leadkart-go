package person_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/humantext"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// The columns check length() <= 100, which counts characters; so does the domain.
func TestPersonNames_AreCountedInCharacters(t *testing.T) {
	t.Parallel()
	hash := mustHash(t)
	ok, over := strings.Repeat("अ", 100), strings.Repeat("अ", 101)
	p, err := person.New(newID(t), mustEmail(t, "hindi@example.test"), ok, ok, hash, testNow)
	if err != nil {
		t.Fatalf("100 characters (300 bytes) refused on create: %v", err)
	}
	if _, err := person.New(newID(t), mustEmail(t, "h2@example.test"), over, "K", hash, testNow); !errors.Is(err, person.ErrInvalid) {
		t.Errorf("create with a 101-character first name: err = %v", err)
	}
	if _, err := person.New(newID(t), mustEmail(t, "h3@example.test"), "K", over, hash, testNow); !errors.Is(err, person.ErrInvalid) {
		t.Errorf("create with a 101-character last name: err = %v", err)
	}
	if err := p.UpdateProfile(ok, ok, testNow); err != nil {
		t.Fatalf("100 characters refused on update: %v", err)
	}
	if err := p.UpdateProfile(over, ok, testNow); !errors.Is(err, person.ErrInvalid) {
		t.Errorf("update with a 101-character first name: err = %v", err)
	}
	if err := p.UpdateProfile(ok, over, testNow); !errors.Is(err, person.ErrInvalid) {
		t.Errorf("update with a 101-character last name: err = %v", err)
	}
}

// An unchanged stored name is not measured again; a changed one still over the cap is refused.
func TestUpdateProfile_OnlyAChangedOverlongNameIsRefused(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("n", 150)
	fresh := func() *person.Person {
		return person.UnmarshalFromDB(person.Snapshot{ID: newID(t), Email: mustEmail(t, "long@example.test"), FirstName: long, LastName: long, PasswordHash: mustHash(t), IsActive: true, CreatedAt: testNow})
	}
	p := fresh()
	if err := p.UpdateProfile(long, long, testNow); err != nil {
		t.Fatalf("re-sending the stored over-long names: %v", err)
	}
	p = fresh()
	if err := p.UpdateProfile(long, "Kumar", testNow); err != nil {
		t.Fatalf("changing only the last name beside an over-long first name: %v", err)
	}
	p = fresh()
	if err := p.UpdateProfile("Ravi", long, testNow); err != nil {
		t.Fatalf("changing only the first name beside an over-long last name: %v", err)
	}
	for name, args := range map[string][2]string{"first": {long + "x", "Kumar"}, "last": {"Ravi", long + "x"}} {
		err := fresh().UpdateProfile(args[0], args[1], testNow)
		var he *humantext.Error
		if !errors.Is(err, person.ErrInvalid) || !errors.As(err, &he) || he.WireCode() != "too_long" {
			t.Errorf("a changed over-long %s name: err = %v, want too_long", name, err)
		}
	}
}
