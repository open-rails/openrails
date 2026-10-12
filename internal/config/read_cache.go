package config

import "time"

// Read names a family of Client reads served from memory.
type Read string

// The cacheable reads. Customer and money reads (orders, payments,
// subscriptions, entitlements, balances, customers) are never cached.
const (
	// ReadCatalog is GetProduct, ListProducts, GetPrice (without Verify) and
	// ListPrices.
	ReadCatalog Read = "catalog"
	// ReadMeters is GetMeter and ListMeters.
	ReadMeters Read = "meters"
	// ReadPublicConfig is GetPublicConfig.
	ReadPublicConfig Read = "public_config"
	// ReadMerchantConfiguration is GetMerchantConfiguration.
	ReadMerchantConfiguration Read = "merchant_configuration"
)

// ReadPolicy is how a read is served from memory: an answer younger than
// RefreshAfter is served as is; an older one is served while it reloads in
// the background, and while reloads fail until it is MaxStale old. The zero
// policy reads live.
type ReadPolicy struct {
	RefreshAfter time.Duration `yaml:"refresh_after" json:"refresh_after"`
	MaxStale     time.Duration `yaml:"max_stale" json:"max_stale"`
}

// ReadCache replaces the default policy of each Read it names.
type ReadCache map[Read]ReadPolicy
