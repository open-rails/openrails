package config

import (
	"fmt"
	"time"
)

// NamingConfig is the site naming policy for merchant names and usernames:
// whether a name may change, how often, and how long a former name keeps
// forwarding. Unset fields take the defaults: renames on, every 72 hours,
// former names kept 90 days.
type NamingConfig struct {
	Enabled        *bool
	RenameInterval *time.Duration
	FormerNames    FormerNamesConfig
}

// FormerNamesConfig says how long a former name keeps forwarding. Duration
// applies to FormerNamesFinite only.
type FormerNamesConfig struct {
	Mode     FormerNamesMode
	Duration *time.Duration
}

// FormerNamesMode selects how long a former name keeps forwarding.
type FormerNamesMode string

const (
	FormerNamesFinite    FormerNamesMode = "finite"
	FormerNamesForever   FormerNamesMode = "forever"
	FormerNamesImmediate FormerNamesMode = "immediate"
)

// NamingPolicy is a validated NamingConfig.
type NamingPolicy struct {
	Enabled        bool
	RenameInterval time.Duration
	FormerNames    FormerNamesMode
	// FormerNameRetention is the alias lifetime under FormerNamesFinite.
	FormerNameRetention time.Duration
}

// NormalizeNaming validates c and applies its defaults. A finite retention of
// zero is immediate; the other modes take no duration.
func NormalizeNaming(c NamingConfig) (NamingPolicy, error) {
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
