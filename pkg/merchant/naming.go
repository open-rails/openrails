package merchant

import (
	"fmt"
	"time"
)

// NamingConfig is the site naming policy (config key auth.naming): whether a
// name may change, how often, and how long a former name keeps forwarding. It
// governs merchant names and AuthKit usernames. Unset fields take the defaults:
// renames on, every 72 hours, former names kept 90 days.
type NamingConfig struct {
	Enabled        *bool             `json:"enabled,omitempty" koanf:"enabled"`
	RenameInterval *time.Duration    `json:"rename_interval,omitempty" koanf:"rename_interval"`
	FormerNames    FormerNamesConfig `json:"former_names,omitempty" koanf:"former_names"`
}

// FormerNamesConfig says how long a former name keeps forwarding. Duration
// applies to FormerNamesFinite only.
type FormerNamesConfig struct {
	Mode     FormerNames    `json:"mode,omitempty" koanf:"mode"`
	Duration *time.Duration `json:"duration,omitempty" koanf:"duration"`
}

// FormerNames selects how long a former name keeps forwarding.
type FormerNames string

const (
	FormerNamesFinite    FormerNames = "finite"
	FormerNamesForever   FormerNames = "forever"
	FormerNamesImmediate FormerNames = "immediate"
)

// NamingPolicy is a validated NamingConfig.
type NamingPolicy struct {
	Enabled        bool
	RenameInterval time.Duration
	FormerNames    FormerNames
	// FormerNameRetention is the alias lifetime under FormerNamesFinite.
	FormerNameRetention time.Duration
}

// Normalize validates c and applies its defaults. A finite retention of zero is
// immediate; the other modes take no duration.
func (c NamingConfig) Normalize() (NamingPolicy, error) {
	p := NamingPolicy{Enabled: true, RenameInterval: 72 * time.Hour, FormerNames: FormerNamesFinite, FormerNameRetention: 90 * 24 * time.Hour}
	if c.Enabled != nil {
		p.Enabled = *c.Enabled
	}
	if c.RenameInterval != nil {
		p.RenameInterval = *c.RenameInterval
	}
	if p.RenameInterval < 0 {
		return NamingPolicy{}, fmt.Errorf("naming.rename_interval must be nonnegative")
	}
	if c.FormerNames.Mode != "" {
		p.FormerNames = c.FormerNames.Mode
	}
	switch p.FormerNames {
	case FormerNamesFinite:
		if c.FormerNames.Duration != nil {
			p.FormerNameRetention = *c.FormerNames.Duration
		}
		if p.FormerNameRetention < 0 {
			return NamingPolicy{}, fmt.Errorf("naming.former_names.duration must be nonnegative")
		}
		if p.FormerNameRetention == 0 {
			p.FormerNames = FormerNamesImmediate
		}
	case FormerNamesForever, FormerNamesImmediate:
		if c.FormerNames.Duration != nil {
			return NamingPolicy{}, fmt.Errorf("naming.former_names.duration requires finite retention")
		}
		p.FormerNameRetention = 0
	default:
		return NamingPolicy{}, fmt.Errorf("invalid naming.former_names.mode %q", p.FormerNames)
	}
	return p, nil
}
