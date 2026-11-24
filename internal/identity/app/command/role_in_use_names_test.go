package command_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// namedPersons gives every person a display name derived from the id, with ties (two people per name),
// so the order of the names a refusal carries is decided by the name and then the id, not by the
// order the repository happens to return holders in.
type namedPersons struct{ anyPersons }

func nameOf(id person.ID) string {
	s := string(id)
	n, _ := strconv.Atoi(strings.TrimLeft(s[len(s)-12:], "0"))
	return fmt.Sprintf("Name%04d", (10000-n)/2)
}

func (namedPersons) GetByIDs(_ context.Context, ids []person.ID) (map[person.ID]*person.Person, error) {
	out := make(map[person.ID]*person.Person, len(ids))
	for _, id := range ids {
		addr, err := email.New(strings.ReplaceAll(string(id), "-", "") + "@authz.test")
		if err != nil {
			return nil, err
		}
		hash, err := person.NewPasswordHash("$argon2id$v=19$m=65536,t=3,p=4$YWJjZGVmZ2g$ZS1zdHViLWhhc2gtdGVzdC1maXh0dXJlLW9ubHk")
		if err != nil {
			return nil, err
		}
		p, err := person.New(id, addr, nameOf(id), "Holder", hash, testNow)
		if err != nil {
			return nil, err
		}
		out[id] = p
	}
	return out, nil
}

// role_in_use.holder_names is the first five holders by display name, then person id: stable across
// calls and across repository order (G2k M4).
func TestDeactivateRole_HolderNamesAreInAStableOrder(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	l := command.AuthorityLocker(w.lock)
	del := command.NewDeactivateRoleHandler(fakeUoW{}, l, w.members, w.roles, namedPersons{}, awClock)
	custom := w.customRole(t, "Held by many", 60)
	var ids []person.ID
	for range 9 {
		ids = append(ids, w.member(t, custom).PersonID())
	}
	slices.SortFunc(ids, func(a, b person.ID) int {
		if c := strings.Compare(nameOf(a), nameOf(b)); c != 0 {
			return c
		}
		return strings.Compare(string(a), string(b))
	})
	want := make([]string, 0, command.MaxRoleInUseHolderNames)
	for _, id := range ids[:command.MaxRoleInUseHolderNames] {
		want = append(want, nameOf(id)+" Holder")
	}
	for range 3 {
		err := del.Handle(t.Context(), command.ChangeRoleStatusCommand{TenantID: awTenant, RoleID: custom.ID(), Actor: w.adminActor(t)})
		var inUse *command.RoleInUseError
		if !errors.As(err, &inUse) || inUse.Holders != 9 {
			t.Fatalf("deactivate: %v, want RoleInUseError with 9 holders", err)
		}
		if !slices.Equal(inUse.HolderNames, want) {
			t.Fatalf("holder_names = %v, want %v", inUse.HolderNames, want)
		}
	}
}
