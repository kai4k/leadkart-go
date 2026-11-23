package crmlead_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/textlimit"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
)

// M2: contact_name is limited to 200 CHARACTERS (CHECK on length()); the domain
// used to count bytes and refuse a Hindi name of 67 letters.
func TestNew_ContactNameLimitCountsCharactersNotBytes(t *testing.T) {
	t.Parallel()
	p := validProfile()
	p.ContactName = strings.Repeat("क", 200) // 600 bytes
	if _, err := crmlead.New(crmlead.ID(tidLeadState), tenantID1, p, tidMemActor, fixedNow); err != nil {
		t.Fatalf("200 Devanagari characters must be accepted: %v", err)
	}
	p.ContactName = strings.Repeat("क", 201)
	_, err := crmlead.New(crmlead.ID(tidLeadState), tenantID1, p, tidMemActor, fixedNow)
	if te, ok := textlimit.Of(err); !errors.Is(err, crmlead.ErrInvalid) || !ok || te.Code() != "contact_name_too_long" || te.Unit != textlimit.UnitChars {
		t.Fatalf("201 characters must be refused as contact_name_too_long (characters), got %v", err)
	}
	p.ContactName = "   "
	if _, err := crmlead.New(crmlead.ID(tidLeadState), tenantID1, p, tidMemActor, fixedNow); !errors.Is(err, crmlead.ErrInvalid) {
		t.Fatalf("a blank contact name must stay refused, got %v", err)
	}
}
