// Package hostconfig loads standalone host billing and identity configuration.
package hostconfig

import (
	"fmt"

	billing "github.com/open-rails/openrails/internal/config"
)

// Config composes host identity with neutral billing configuration.
type Config struct {
	*billing.Config `koanf:",squash"`
	Auth            *billing.AuthConfig `koanf:"auth,omitempty"`
}

// Validate checks the composed standalone host configuration.
func Validate(cfg *Config) error {
	if cfg == nil || cfg.Config == nil {
		return fmt.Errorf("standalone config is required")
	}
	if err := billing.Validate(cfg.Config); err != nil {
		return err
	}
	return cfg.Auth.ValidateTransport()
}
