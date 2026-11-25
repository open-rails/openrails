package openrails

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
)

// Config is the engine's settings, billing.Config: plain data. Everything
// that reaches outside the process is in Deps. New fills a setting left zero
// from DefaultConfig.
type Config = billing.Config

// DefaultConfig is every engine setting's default, the value New gives a
// setting left zero.
func DefaultConfig() Config { return billing.DefaultConfig() }

// ParseMerchantDocument reads a merchant's document from YAML: a required
// slug and the configuration beside it, refusing unknown fields. A
// MerchantDeclaration's Issuer and Binding are set in code.
func ParseMerchantDocument(raw []byte) (billing.MerchantDocument, error) {
	return config.ParseMerchantDocument(raw)
}
