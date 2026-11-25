package config

import (
	"fmt"
	"time"

	"github.com/open-rails/openrails/billing"
)

// NamingPolicy is a validated billing.NamingConfig.
type NamingPolicy struct {
	Enabled        bool
	RenameInterval time.Duration
	FormerNames    billing.FormerNamesMode
	// FormerNameRetention is the alias lifetime under FormerNamesFinite.
	FormerNameRetention time.Duration
}

// NormalizeNaming validates c, whose defaults server.Config.WithDefaults has
// filled. A
// finite retention of zero is immediate; the other modes take no duration.
func NormalizeNaming(c billing.NamingConfig) (NamingPolicy, error) {
	p := NamingPolicy{FormerNames: c.FormerNames.Mode}
	if c.Enabled != nil {
		p.Enabled = *c.Enabled
	}
	if c.RenameInterval != nil {
		p.RenameInterval = *c.RenameInterval
	}
	if p.RenameInterval < 0 {
		return NamingPolicy{}, fmt.Errorf("naming.rename_interval must be nonnegative")
	}
	switch p.FormerNames {
	case billing.FormerNamesFinite:
		if c.FormerNames.Duration != nil {
			p.FormerNameRetention = *c.FormerNames.Duration
		}
		if p.FormerNameRetention < 0 {
			return NamingPolicy{}, fmt.Errorf("naming.former_names.duration must be nonnegative")
		}
		if p.FormerNameRetention == 0 {
			p.FormerNames = billing.FormerNamesImmediate
		}
	case billing.FormerNamesForever, billing.FormerNamesImmediate:
		if c.FormerNames.Duration != nil {
			return NamingPolicy{}, fmt.Errorf("naming.former_names.duration requires finite retention")
		}
		p.FormerNameRetention = 0
	default:
		return NamingPolicy{}, fmt.Errorf("invalid naming.former_names.mode %q", p.FormerNames)
	}
	return p, nil
}
