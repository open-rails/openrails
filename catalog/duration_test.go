package catalog

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestApplicationDurationAliases(t *testing.T) {
	var first [32]byte
	for i, access := range []string{"access_duration: 72 hours", "access_duration: 3 days", "access_duration_hours: 72"} {
		raw := fmt.Sprintf("schema_version: 1\nproducts:\n  - key: p\n    prices:\n      - key: x\n        %s\n        billing_interval: 4 weeks\n        trial_duration: 1 day\n", access)
		app, err := ParseApplicationYAML([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		price := app.Products[0].Prices[0]
		if price.AccessDurationHours != Value(72) || price.BillingIntervalHours != Value(672) || price.TrialDurationHours != Value(24) {
			t.Fatalf("incorrect normalized durations: %+v", price)
		}
		if i == 0 {
			first = digest(t, *app)
		} else if got := digest(t, *app); got != first {
			t.Fatalf("equivalent duration %q changed application identity", access)
		}
		encoded, err := json.Marshal(app)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), `"access_duration":`) || !strings.Contains(string(encoded), `"access_duration_hours":72`) {
			t.Fatalf("wire duration was not normalized: %s", encoded)
		}
	}
}

func TestApplicationDurationNullAndOmitted(t *testing.T) {
	app, err := ParseApplicationJSON([]byte(`{"schema_version":1,"products":[{"key":"p","prices":[{"key":"x","access_duration":null,"billing_interval":null},{"key":"y"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	prices := app.Products[0].Prices
	if prices[0].AccessDurationHours != Null[int]() || prices[0].BillingIntervalHours != Null[int]() || prices[0].TrialDurationHours.Set {
		t.Fatalf("null and absent fields collapsed: %+v", prices[0])
	}
	if prices[1].AccessDurationHours.Set || prices[1].BillingIntervalHours.Set || prices[1].TrialDurationHours.Set {
		t.Fatalf("absent duration fields acquired a value: %+v", prices[1])
	}
}

func TestApplicationRejectsInvalidDurations(t *testing.T) {
	for _, fields := range []string{
		`"access_duration":"0 hours"`,
		`"access_duration":"-1 day"`,
		`"access_duration":"1.5 days"`,
		`"access_duration":"1 month"`,
		`"access_duration":"18446744073709551615 days"`,
		`"billing_interval":""`,
		`"trial_duration":72`,
		`"billing_interval_hours":0`,
		`"billing_interval_hours":-1`,
		`"access_duration":"3 days","access_duration_hours":72`,
		`"billing_interval":null,"billing_interval_hours":null`,
		`"trial_duration":"1 day","trial_duration_hours":48`,
		`"Access_Duration":"3 days"`,
		`"auto_renew":true`,
		`"auto_renew":false`,
	} {
		raw := `{"schema_version":1,"products":[{"key":"p","prices":[{"key":"x",` + fields + `}]}]}`
		if _, err := ParseApplicationJSON([]byte(raw)); err == nil {
			t.Errorf("accepted invalid fields %s", fields)
		}
	}
}
