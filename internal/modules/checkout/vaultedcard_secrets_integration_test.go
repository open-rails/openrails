//go:build integration

package checkout

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #795 spec C12: the vaulted_card rail arms end-to-end from real
// rail_merchant_accounts rows + the merchant secret store, in BOTH secret
// modes (MODE 2 DB-backed store; MODE 1 in-memory manifest plane): the private
// api_key + the LINKED NMI gateway account's security key resolve into one
// VaultedCardRailConfig through the real merchants.Service + railresolve seam.
func TestVaultedCardRailResolvesFromStoreBothModes(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(context.Background(), t, pool)
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)

	const tenantID = "tnt_c12_test"
	const gatewayID = "579145-c12"

	// Layer B rows (identical in both modes — mode 1 converges the same rows).
	_, err := pool.Exec(ctx, `
		INSERT INTO openrails.rail_merchant_accounts (merchant_id, rail, environment, account_id, archived, evidence)
		VALUES ($1::uuid, 'vaulted_card', 'test', $2, false, '{"source":"test_795","settings":{"gateway_account":"579145-c12","network_tokens":true,"public_api_key":"key_pub_c12"}}'::jsonb)
		ON CONFLICT (rail, environment, account_id) DO UPDATE SET archived = false
	`, dbtest.TestMerchantID.String(), tenantID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.rail_merchant_accounts (merchant_id, rail, environment, account_id, archived, evidence)
		VALUES ($1::uuid, 'nmi', 'test', $2, false, '{"source":"test_795"}'::jsonb)
		ON CONFLICT (rail, environment, account_id) DO UPDATE SET archived = false
	`, dbtest.TestMerchantID.String(), gatewayID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.rail_merchant_accounts WHERE account_id IN ($1, $2)", tenantID, gatewayID)
	})

	dbp := db.WrapPool(pool, "")
	dbStore, err := merchants.NewDBSecretStore(dbp)
	require.NoError(t, err)
	memStore := merchants.NewMemorySecretStore()

	for name, store := range map[string]merchants.MerchantSecretStore{
		"mode2_db_store":       dbStore,
		"mode1_manifest_plane": memStore,
	} {
		t.Run(name, func(t *testing.T) {
			put := func(rail, accountID, key, value string) {
				secretName, err := merchants.PSPSecretName(rail, "test", accountID, key)
				require.NoError(t, err)
				_, err = store.Put(ctx, dbtest.TestMerchantID, secretName, value)
				require.NoError(t, err)
			}
			put("vaulted_card", tenantID, "api_key", "key_private_c12_"+name)
			put("nmi", gatewayID, "security_key", "sk_gateway_c12_"+name)

			msvc, err := merchants.NewService(dbp, store, config.ExpectedProviderEnvironment(true))
			require.NoError(t, err)
			source := railresolve.NewMerchantsSource(
				&config.Config{TestMode: config.CredentialPostureSandbox},
				func() *merchants.Service { return msvc },
			)

			armed, err := source.Armed(ctx, string(models.RailVaultedCard))
			require.NoError(t, err)
			require.True(t, armed)

			rc, err := source.RailConfig(ctx, string(models.RailVaultedCard), "")
			require.NoError(t, err)
			require.NotNil(t, rc.VaultedCard)
			require.Equal(t, tenantID, rc.AccountID)
			require.Equal(t, "key_private_c12_"+name, rc.VaultedCard.APIKey)
			require.Equal(t, gatewayID, rc.VaultedCard.GatewayAccountID)
			require.Equal(t, "sk_gateway_c12_"+name, rc.VaultedCard.GatewaySecurityKey)
			require.True(t, rc.VaultedCard.NetworkTokens)
			require.Equal(t, "key_pub_c12", rc.VaultedCard.PublicAPIKey)
		})
	}

	// Fail-closed: a vaulted_card account whose api_key secret is missing must
	// refuse to arm (never a silent fallback).
	t.Run("missing_api_key_fails_closed", func(t *testing.T) {
		emptyStore := merchants.NewMemorySecretStore()
		msvc, err := merchants.NewService(dbp, emptyStore, config.ExpectedProviderEnvironment(true))
		require.NoError(t, err)
		source := railresolve.NewMerchantsSource(
			&config.Config{TestMode: config.CredentialPostureSandbox},
			func() *merchants.Service { return msvc },
		)
		_, err = source.RailConfig(ctx, string(models.RailVaultedCard), "")
		require.Error(t, err)
		require.ErrorIs(t, err, railresolve.ErrRailNotArmed)
	})
}
