package config

import (
	"strings"
	"testing"

	"github.com/open-rails/openrails/billing"
)

// A merchant's return URLs name the billing origin, its issuer's or one of
// its embed origins, never another merchant's site; the billing portal
// returns to the browser's origin when it is one of them, else the issuer's,
// else the billing origin.
func TestReturnOriginsAreTheMerchantsOwn(t *testing.T) {
	cfg := &billing.Config{PublicURL: "https://billing.example.com/billing"}
	alpha := ReturnOrigins(cfg, "https://alpha.example", []string{"https://shop.alpha.example"})
	for raw, want := range map[string]bool{
		"https://billing.example.com/account":  true,
		"https://alpha.example/done?order=1":   true,
		"https://shop.alpha.example/thanks":    true,
		"https://beta.example/done":            false,
		"https://user@alpha.example/done":      false,
		"http://alpha.example/done":            false,
		"https://shop.alpha.example.evil/done": false,
	} {
		if got := ReturnURLAllowed(alpha, raw); got != want {
			t.Errorf("%s: allowed %v, want %v", raw, got, want)
		}
	}
	for name, c := range map[string]struct {
		issuer, browser, want string
	}{
		"the browser's own embed origin": {"https://alpha.example", "https://shop.alpha.example", "https://shop.alpha.example"},
		"another merchant's site":        {"https://alpha.example", "https://beta.example", "https://alpha.example"},
		"no issuer":                      {"", "https://beta.example", "https://billing.example.com"},
	} {
		if got := PortalReturnOrigin(cfg, c.issuer, []string{"https://shop.alpha.example"}, c.browser); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

// A schema whose derived River schema River cannot hold refuses boot, naming
// the key and the limit; one at the limit is accepted.
func TestTheDerivedRiverSchemaFitsRiver(t *testing.T) {
	fits := strings.Repeat("s", MaxRiverSchema-len("_river"))
	if err := ValidateDatabase(billing.DatabaseConfig{Schema: fits}); err != nil {
		t.Fatalf("%d characters: %v", len(fits), err)
	}
	err := ValidateDatabase(billing.DatabaseConfig{Schema: fits + "s"})
	if err == nil || !strings.Contains(err.Error(), "database.schema") || !strings.Contains(err.Error(), "at most 46") {
		t.Fatalf("one character more: %v", err)
	}
	if err := ValidateDatabase(billing.DatabaseConfig{Schema: fits + "s", RiverSchema: "river"}); err != nil {
		t.Fatalf("a named River schema is its own: %v", err)
	}
}
