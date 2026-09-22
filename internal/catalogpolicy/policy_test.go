package catalogpolicy

import (
	"context"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
)

func TestCatalogWritePolicy(t *testing.T) {
	ctx := context.Background()
	require.ErrorIs(t, Check(ctx, nil), ErrUpdatesDisabled)
	for _, source := range []string{config.MerchantConfigSourceManifest, config.MerchantConfigSourceAPI} {
		cfg := &config.Config{MerchantConfigSource: source}
		require.ErrorIs(t, Check(ctx, cfg), ErrUpdatesDisabled)
		require.NoError(t, Check(OperatorContext(ctx), cfg))
		cfg.AllowCatalogUpdates = true
		require.NoError(t, Check(ctx, cfg))
	}
	child, cancel := context.WithCancel(OperatorContext(ctx))
	defer cancel()
	require.NoError(t, Check(child, nil), "nested service calls retain operator authority")
	require.ErrorIs(t, Check(context.WithValue(ctx, "operator", true), nil), ErrUpdatesDisabled)
}
