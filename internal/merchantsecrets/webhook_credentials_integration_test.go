//go:build integration

package merchantsecrets

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/stretchr/testify/require"
)

func TestWebhookCredentialsRequireEncryption(t *testing.T) {
	ctx := context.Background()
	pool := db.WrapPool(dbtest.SharedSuperuserPGXPool(t), config.DefaultSchema)
	for _, manifestMode := range []bool{false, true} {
		cfg := &config.Config{Env: "dev", MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB}
		var backend *Store
		var err error
		if manifestMode {
			cfg.Env = "production"
			cfg.MerchantSource = config.MerchantSourceManifest
			backend, err = BuildManifest(ctx, cfg, merchants.NewManifestSecretStore(), pool)
		} else {
			backend, err = Build(ctx, cfg, pool)
		}
		require.NoError(t, err)
		name := merchants.AlertWebhookURLSecretName(uuid.New())
		_, err = backend.Secrets.Put(ctx, dbtest.TestMerchantID, name, "https://hooks.example/never-persist-this")
		require.ErrorContains(t, err, "ENCRYPTION_MASTER_KEY")
		var count int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM openrails.merchant_secrets WHERE merchant_id=$1 AND name=$2`, dbtest.TestMerchantID.UUID(), name).Scan(&count))
		require.Zero(t, count)
	}
}
