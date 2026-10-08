package catalogpolicy

import (
	"context"
	"testing"

	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/requestauth"
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

func TestStartupCatalogDoesNotMakeTheCatalogReadOnly(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{Catalog: &catalog.Application{SchemaVersion: 1}}
	host := requestauth.WithHostPrincipal(ctx, &requestauth.HostPrincipal{Subject: "host"})
	require.NoError(t, Check(host, cfg), "programmatic host writes remain available")
	require.NoError(t, Check(OperatorContext(ctx), cfg))
	require.ErrorIs(t, Check(ctx, cfg), ErrUpdatesDisabled, "startup YAML does not enable HTTP writes")
	cfg.AllowCatalogUpdates = true
	require.NoError(t, Check(ctx, cfg), "the HTTP flag allows updates alongside startup YAML")
}
