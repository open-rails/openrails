package merchantconfig

import (
	"strings"
	"testing"

	"github.com/open-rails/openrails/billing"
)

func TestAddressesAreOneEmailAddress(t *testing.T) {
	for _, bad := range []string{"ops@alpha.example, other@else.example", "Ops <ops@alpha.example>", "ops", "ops@alpha.example\r\nBcc: x@y.example", strings.Repeat("a", 310) + "@alpha.example"} {
		if _, err := Normalize("Alpha", billing.MerchantSettings{AlertEmail: &bad}); err == nil || err.Error() != "alert_email must be one email address" {
			t.Errorf("alert_email %q: %v", bad, err)
		}
		if _, err := Normalize("Alpha", billing.MerchantSettings{Profile: &billing.MerchantProfile{FromEmail: bad}}); err == nil || err.Error() != "profile.from_email must be one email address" {
			t.Errorf("from_email %q: %v", bad, err)
		}
	}
	good, blank := " ops@alpha.example ", ""
	got, err := Normalize("Alpha", billing.MerchantSettings{AlertEmail: &good, Profile: &billing.MerchantProfile{FromEmail: "billing@alpha.example"}})
	if err != nil || got.Config.AlertEmail != "ops@alpha.example" || got.Config.Profile.FromEmail != "billing@alpha.example" {
		t.Fatalf("%+v %v", got.Config, err)
	}
	if _, err := Normalize("Alpha", billing.MerchantSettings{AlertEmail: &blank}); err != nil {
		t.Fatalf("an empty alert_email is none: %v", err)
	}
}
