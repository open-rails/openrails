package billing

import (
	"reflect"
	"testing"
)

// A zero setting takes its default; a set one stays, and a bucket set to 0
// stays unlimited.
func TestWithDefaults(t *testing.T) {
	want := DefaultConfig()
	want.Database.RiverSchema = "billing_river"
	if got := (Config{}).WithDefaults(); !reflect.DeepEqual(got, want) {
		t.Fatalf("zero config: %+v, want %+v", got, want)
	}
	got := Config{
		Database:   DatabaseConfig{Schema: " Shop "},
		RateLimits: RateLimitsConfig{Payment: &RateLimit{}, Webhook: &RateLimit{RequestsPerMinute: 5}},
		Vault:      VaultConfig{KVMount: "secret"},
	}.WithDefaults()
	if got.Database.Schema != "shop" || got.Database.RiverSchema != "shop_river" || got.Database.LeaseConns != 4 {
		t.Errorf("database: %+v", got.Database)
	}
	if got.RateLimits.Payment.RequestsPerMinute != 0 || got.RateLimits.Webhook.RequestsPerMinute != 5 || got.RateLimits.Purchase.RequestsPerMinute != 10 {
		t.Errorf("rate limits: payment %+v, webhook %+v, purchase %+v", got.RateLimits.Payment, got.RateLimits.Webhook, got.RateLimits.Purchase)
	}
	if got.Vault != (VaultConfig{KVMount: "secret", ScopePrefix: "openrails", TransitMount: "transit"}) {
		t.Errorf("vault %+v", got.Vault)
	}
}
