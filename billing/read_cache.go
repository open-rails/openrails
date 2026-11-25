package billing

import "time"

// Read names a family of Client reads served from memory.
type Read string

// The cacheable reads. Customer and money reads (invoices, payments,
// subscriptions, entitlements, balances, customers) are never cached.
const (
	// ReadCatalog is GetProduct, ListProducts, GetPrice (without Verify) and
	// ListPrices.
	ReadCatalog Read = "catalog"
	// ReadMeters is GetMeter, ListMeters, GetLimit and ListLimits.
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
	RefreshAfter time.Duration
	MaxStale     time.Duration
}

// ReadCache is Config.ReadCache and openrails.RemoteOptions.ReadCache: the
// policy of each Read it names, replacing its default. Defaults in process:
// ReadCatalog and ReadMeters refresh after 1 minute, stale up to 10; the
// configuration reads are live. Over HTTP: ReadCatalog, ReadMeters and
// ReadPublicConfig refresh after 10 minutes, stale up to an hour;
// ReadMerchantConfiguration after 30 seconds, stale up to 10 minutes. A
// handle's own catalog, configuration or archive write, and any change the
// embedded engine commits, drops the merchant's cached answers at once;
// another replica's change shows within RefreshAfter.
type ReadCache map[Read]ReadPolicy
