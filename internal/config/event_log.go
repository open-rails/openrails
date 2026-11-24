package config

import (
	"fmt"
	"strings"
	"time"
)

func EventRetentionDuration(c *Config) (time.Duration, error) {
	if c == nil || strings.TrimSpace(c.EventRetention) == "" {
		return DefaultEventRetention, nil
	}
	value, err := time.ParseDuration(strings.TrimSpace(c.EventRetention))
	if err != nil || value < time.Second {
		return 0, fmt.Errorf("event_retention must be a duration of at least 1s")
	}
	return value, nil
}
