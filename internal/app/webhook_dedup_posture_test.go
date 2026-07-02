package app

import (
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
)

// #678: dedup truth moved to Postgres (webhook_events), so a Redis-less boot
// is safe in every topology — the old standalone-production boot refusal is
// now a loud warning. The pure message chooser is asserted so the standalone
// warning stays the scarier one.
func TestWebhookDedupPosture(t *testing.T) {
	prod := &config.Config{Env: "production"}
	dev := &config.Config{Env: "development"}

	t.Run("no redis never refuses boot", func(t *testing.T) {
		require.NoError(t, enforceWebhookDedupPosture(prod, false, false))
		require.NoError(t, enforceWebhookDedupPosture(prod, false, true))
		require.NoError(t, enforceWebhookDedupPosture(dev, false, false))
		require.NoError(t, enforceWebhookDedupPosture(nil, false, false))
	})

	t.Run("with redis passes silently", func(t *testing.T) {
		require.NoError(t, enforceWebhookDedupPosture(prod, true, false))
	})

	t.Run("standalone non-dev gets the multi-replica warning", func(t *testing.T) {
		msg := webhookDedupPostureWarning(prod, false)
		require.Contains(t, msg, "standalone")
		require.Contains(t, msg, "lease coordination")
		require.Contains(t, msg, "Postgres (safe)")
	})

	t.Run("dev and embedded get the per-process warning", func(t *testing.T) {
		for _, msg := range []string{
			webhookDedupPostureWarning(dev, false),
			webhookDedupPostureWarning(&config.Config{}, false), // empty env = dev
			webhookDedupPostureWarning(prod, true),
			webhookDedupPostureWarning(nil, false),
		} {
			require.Contains(t, msg, "per-process")
			require.NotContains(t, msg, "standalone")
		}
	})
}
