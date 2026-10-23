package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/retention"
)

func EventRetentionDuration(c *Config) (time.Duration, error) {
	if c == nil || strings.TrimSpace(c.EventRetention) == "" {
		return retention.EventLog, nil
	}
	value, err := time.ParseDuration(strings.TrimSpace(c.EventRetention))
	if err != nil || value < time.Second {
		return 0, fmt.Errorf("event_retention must be a duration of at least 1s")
	}
	return value, nil
}
