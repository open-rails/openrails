package merchantconfig

import (
	"strings"
	"testing"

	"github.com/open-rails/openrails/billing"
)

func TestAddressesAreOneMailbox(t *testing.T) {
	for _, bad := range []string{"ops@alpha.example, other@else.example", "Ops <ops@alpha.example>, x@y.example", "ops", "ops@alpha.example\r\nBcc: x@y.example", strings.Repeat("a", 310) + "@alpha.example"} {
		if _, err := Normalize("Alpha", billing.MerchantSettings{AlertEmail: &bad}); err == nil || err.Error() != "alert_email must be one email address" {
			t.Errorf("alert_email %q: %v", bad, err)
		}
		if _, err := Normalize("Alpha", billing.MerchantSettings{Profile: &billing.MerchantProfile{FromEmail: bad}}); err == nil || err.Error() != "profile.from_email must be one email address" {
			t.Errorf("from_email %q: %v", bad, err)
		}
	}
	for _, c := range []struct{ alert, from string }{{" ops@alpha.example ", "billing@alpha.example"}, {"Ops <ops@alpha.example>", `"Alpha Billing" <billing@alpha.example>`}} {
		got, err := Normalize("Alpha", billing.MerchantSettings{AlertEmail: &c.alert, Profile: &billing.MerchantProfile{FromEmail: c.from}})
		if err != nil || got.Config.AlertEmail != "ops@alpha.example" || got.Config.Profile.FromEmail != "billing@alpha.example" {
			t.Fatalf("a mailbox, named or bare, is its address: %+v %v", got.Config, err)
		}
	}
	blank := ""
	if _, err := Normalize("Alpha", billing.MerchantSettings{AlertEmail: &blank}); err != nil {
		t.Fatalf("an empty alert_email is none: %v", err)
	}
}
