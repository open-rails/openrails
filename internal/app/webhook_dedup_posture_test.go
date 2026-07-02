package app

import (
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
)

// #678: standalone non-development boots must refuse to run webhook dedup on
// the per-process memory store; dev and embedded hosts keep the fallback.
func TestWebhookDedupPosture(t *testing.T) {
	prod := &config.Config{Env: "production"}
	dev := &config.Config{Env: "development"}

	t.Run("standalone production without redis fails", func(t *testing.T) {
		err := enforceWebhookDedupPosture(prod, false, false)
		require.Error(t, err)
		require.Contains(t, err.Error(), "redis is required")
	})

	t.Run("standalone production with redis passes", func(t *testing.T) {
		require.NoError(t, enforceWebhookDedupPosture(prod, true, false))
	})

	t.Run("embedded production without redis warns but passes", func(t *testing.T) {
		require.NoError(t, enforceWebhookDedupPosture(prod, false, true))
	})

	t.Run("standalone development without redis warns but passes", func(t *testing.T) {
		require.NoError(t, enforceWebhookDedupPosture(dev, false, false))
		require.NoError(t, enforceWebhookDedupPosture(&config.Config{}, false, false)) // empty env = dev
	})

	t.Run("nil config passes", func(t *testing.T) {
		require.NoError(t, enforceWebhookDedupPosture(nil, false, false))
	})
}
