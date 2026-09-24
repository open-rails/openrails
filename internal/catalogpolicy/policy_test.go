package catalogpolicy

import (
	"context"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
)

func TestCatalogWritesDeniedUnlessEnabledOrOperator(t *testing.T) {
	ctx := context.Background()
	require.ErrorIs(t, Check(ctx, nil), ErrUpdatesDisabled, "missing config never enables writes")
	for _, backend := range []string{config.SecretBackendSnapshot, config.SecretBackendDB} {
		cfg := &config.Config{SecretBackend: backend}
		require.ErrorIs(t, Check(ctx, cfg), ErrUpdatesDisabled, backend)
		require.NoError(t, Check(OperatorContext(ctx), cfg), backend)
		cfg.AllowCatalogUpdates = true
		require.NoError(t, Check(ctx, cfg), backend)
	}
	child, cancel := context.WithCancel(OperatorContext(ctx))
	defer cancel()
	require.NoError(t, Check(child, nil), "derived contexts keep operator authority")
	type lookalike string
	require.ErrorIs(t, Check(context.WithValue(ctx, lookalike("operator"), true), nil), ErrUpdatesDisabled, "only the private key grants authority")
}
