package billing

import "time"

// NamingConfig is the standalone server's rename policy for merchant names:
// whether a name may change, how often, and how long a former name keeps
// forwarding. Unset fields take the defaults: renames on, every 72 hours,
// former names kept 90 days.
type NamingConfig struct {
	Enabled        *bool             `yaml:"enabled"`
	RenameInterval *time.Duration    `yaml:"rename_interval"`
	FormerNames    FormerNamesConfig `yaml:"former_names"`
}

// FormerNamesConfig is how long a former name keeps forwarding. Duration
// applies to FormerNamesFinite only.
type FormerNamesConfig struct {
	Mode     FormerNamesMode `yaml:"mode"`
	Duration *time.Duration  `yaml:"duration"`
}

// FormerNamesMode is FormerNamesConfig.Mode.
type FormerNamesMode string

const (
	// FormerNamesFinite forwards a former name for FormerNamesConfig.Duration.
	FormerNamesFinite FormerNamesMode = "finite"
	// FormerNamesForever forwards a former name indefinitely.
	FormerNamesForever FormerNamesMode = "forever"
	// FormerNamesImmediate releases a former name at once.
	FormerNamesImmediate FormerNamesMode = "immediate"
)
