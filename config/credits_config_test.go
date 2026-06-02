package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestDefaultCreditExpiryDays(t *testing.T) {
	t.Run("nil config defaults to 365", func(t *testing.T) {
		var cfg *Config
		assert.Equal(t, 365, cfg.DefaultCreditExpiryDays())
	})
	t.Run("nil credits section defaults to 365", func(t *testing.T) {
		cfg := &Config{}
		assert.Equal(t, 365, cfg.DefaultCreditExpiryDays())
	})
	t.Run("unset value defaults to 365", func(t *testing.T) {
		cfg := &Config{Credits: &CreditsConfig{}}
		assert.Equal(t, 365, cfg.DefaultCreditExpiryDays())
	})
	t.Run("explicit value honored", func(t *testing.T) {
		cfg := &Config{Credits: &CreditsConfig{DefaultExpiryDays: 30}}
		assert.Equal(t, 30, cfg.DefaultCreditExpiryDays())
	})
	t.Run("negative value passed through as disabled", func(t *testing.T) {
		cfg := &Config{Credits: &CreditsConfig{DefaultExpiryDays: -1}}
		assert.Equal(t, -1, cfg.DefaultCreditExpiryDays())
	})
}

func TestLowBalanceAlertCooldown(t *testing.T) {
	t.Run("nil config defaults to 24h", func(t *testing.T) {
		var cfg *Config
		assert.Equal(t, 24*time.Hour, cfg.LowBalanceAlertCooldown())
	})
	t.Run("non-positive falls back to 24h", func(t *testing.T) {
		cfg := &Config{Credits: &CreditsConfig{LowBalanceAlertCooldownHours: 0}}
		assert.Equal(t, 24*time.Hour, cfg.LowBalanceAlertCooldown())
	})
	t.Run("explicit hours honored", func(t *testing.T) {
		cfg := &Config{Credits: &CreditsConfig{LowBalanceAlertCooldownHours: 6}}
		assert.Equal(t, 6*time.Hour, cfg.LowBalanceAlertCooldown())
	})
}
