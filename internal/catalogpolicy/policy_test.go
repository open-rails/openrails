package catalogpolicy

import (
	"context"
	"testing"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
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

func TestDeclaredCatalogRefusesEveryoneButTheBootApplication(t *testing.T) {
	ctx := context.Background()
	require.NoError(t, CheckDeclared(ctx, nil))
	require.NoError(t, CheckDeclared(ctx, &config.Config{AllowCatalogUpdates: true}))
	declared := &config.Config{AllowCatalogUpdates: true, Catalog: &billing.CatalogApplyParams{SchemaVersion: 1}}
	require.ErrorIs(t, CheckDeclared(ctx, declared), ErrDeclared, "updates enabled do not reopen a declared catalog")
	require.ErrorIs(t, CheckDeclared(ctx, declared), billing.ErrCatalogDeclared, "hosts match the public sentinel")
	require.NoError(t, CheckDeclared(OperatorContext(ctx), declared))
}
