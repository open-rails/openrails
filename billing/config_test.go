package billing

import (
	"reflect"
	"testing"
	"time"
)

// A zero setting takes its default; a set one, and an empty RateLimits, stay.
func TestWithDefaults(t *testing.T) {
	want := DefaultConfig()
	want.Database.RiverSchema = "billing_river"
	if got := (Config{}).WithDefaults(); !reflect.DeepEqual(got, want) {
		t.Fatalf("zero config: %+v, want %+v", got, want)
	}
	got := Config{
		Database:             DatabaseConfig{Schema: " Shop "},
		RateLimits:           RateLimitsConfig{},
		Vault:                VaultConfig{KVMount: "secret"},
		WebhookSecretOverlap: time.Hour,
	}.WithDefaults()
	if got.Database.Schema != "shop" || got.Database.RiverSchema != "shop_river" || got.Database.LeaseConns != 4 {
		t.Errorf("database: %+v", got.Database)
	}
	if got.RateLimits == nil || len(got.RateLimits) != 0 {
		t.Errorf("an empty RateLimits limits nothing: %v", got.RateLimits)
	}
	if got.Vault != (VaultConfig{KVMount: "secret", ScopePrefix: "openrails", TransitMount: "transit"}) || got.WebhookSecretOverlap != time.Hour {
		t.Errorf("vault %+v, overlap %s", got.Vault, got.WebhookSecretOverlap)
	}
}
