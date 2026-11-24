package config

import (
	"errors"
	"time"

	"github.com/open-rails/openrails/billing"
)

// EventRetentionDuration is how long the event log keeps facts: the
// configured lifetime, the default when unset, never under 1s.
func EventRetentionDuration(c *billing.Config) (time.Duration, error) {
	if c == nil || c.EventRetention == 0 {
		return defaults.EventRetention, nil
	}
	if c.EventRetention < time.Second {
		return 0, errors.New("event_retention must be a duration of at least 1s")
	}
	return c.EventRetention, nil
}
