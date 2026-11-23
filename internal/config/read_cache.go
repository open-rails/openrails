package config

// Read names a family of Client reads served from memory: the route catalog
// marks each cached GET with one. openrails.Read is its public spelling and
// carries the policies.
type Read string

// The cacheable reads.
const (
	ReadCatalog               Read = "catalog"
	ReadMeters                Read = "meters"
	ReadPublicConfig          Read = "public_config"
	ReadMerchantConfiguration Read = "merchant_configuration"
)
