package messaging

import "testing"

func TestModuleTopicFromAlias(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"identity.person_created.v1":           "identity.events",
		"platform.lead_purchased.v1":           "platform.events",
		"orders.order_packed.v1":               "orders.events",
		"crm.lead_assigned.v1":                 "crm.events",
		"dispatch.consignment_note_created.v1": "dispatch.events",
		"inventory.product_created.v1":         "inventory.events",
	}
	for alias, want := range cases {
		got, err := moduleTopicFromAlias(alias)
		if err != nil {
			t.Errorf("%s: unexpected err %v", alias, err)
			continue
		}
		if got != want {
			t.Errorf("%s: got %q want %q", alias, got, want)
		}
	}
}

func TestModuleTopicFromAlias_Malformed(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", "nodot"} {
		if _, err := moduleTopicFromAlias(bad); err == nil {
			t.Errorf("%q: want error, got nil", bad)
		}
	}
}
