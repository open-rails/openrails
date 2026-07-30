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

// #795 spec C12 / or#879: a custodian-held card arms end-to-end from real psps
// rows + the merchant secret store, in BOTH secret modes (MODE 2 DB-backed
// store; MODE 1 in-memory manifest plane). ONE PSP now carries both halves —
// the NMI gateway's security_key and the custodian's private application key —
// resolved through the real merchants.Service + railresolve seam.
func TestCustodianHeldCardResolvesFromStoreBothModes(t *testing.T) {
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(context.Background(), t, pool)
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)

	const tenantID = "tnt_c12_test"
	const gatewayID = "579145-c12"

	// Layer B row (identical in both modes — mode 1 converges the same row).
	// ONE PSP: rail nmi, custody declared in its settings.
	_, err := pool.Exec(ctx, `
		INSERT INTO openrails.psps (merchant_id, rail, environment, account_id, archived, evidence)
		VALUES ($1::uuid, 'nmi', 'test', $2, false, '{"source":"test_795","settings":{"custodian":"basis_theory","custodian_account_id":"tnt_c12_test","custodian_network_tokens":true,"custodian_public_api_key":"key_pub_c12"}}'::jsonb)
		ON CONFLICT (rail, environment, account_id) DO UPDATE SET archived = false, evidence = EXCLUDED.evidence
	`, dbtest.TestMerchantID.String(), gatewayID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.psps WHERE account_id = $1", gatewayID)
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
			put("nmi", gatewayID, "security_key", "sk_gateway_c12_"+name)
			put("nmi", gatewayID, config.CustodianSecretAPIKey, "key_private_c12_"+name)

			msvc, err := merchants.NewService(dbp, store, config.ExpectedProviderEnvironment(true))
			require.NoError(t, err)
			source := railresolve.NewMerchantsSource(
				&config.Config{TestMode: config.CredentialPostureSandbox},
				func() *merchants.Service { return msvc },
			)

			armed, err := source.Armed(ctx, string(models.RailNMI))
			require.NoError(t, err)
			require.True(t, armed)

			rc, err := source.RailConfig(ctx, string(models.RailNMI), "")
			require.NoError(t, err)
			// The gateway half: unchanged, and still the thing that charges.
			require.Equal(t, gatewayID, rc.AccountID)
			require.NotNil(t, rc.NMI)
			require.Equal(t, "sk_gateway_c12_"+name, rc.NMI.SecurityKey)
			// The custody half, on the same PSP.
			require.NotNil(t, rc.Custody)
			require.Equal(t, models.CustodianBasisTheory, rc.Custody.Custodian)
			require.Equal(t, tenantID, rc.Custody.AccountID)
			require.Equal(t, "key_private_c12_"+name, rc.Custody.APIKey)
			require.True(t, rc.Custody.NetworkTokens)
			require.Equal(t, "key_pub_c12", rc.Custody.PublicAPIKey)
		})
	}

	// Fail-closed: a PSP that DECLARES a custodian but has no custodian_api_key
	// secret must refuse to arm — never a silent downgrade to "no custody",
	// which would route the charge as if the gateway held the card.
	t.Run("missing_custodian_api_key_fails_closed", func(t *testing.T) {
		emptyStore := merchants.NewMemorySecretStore()
		msvc, err := merchants.NewService(dbp, emptyStore, config.ExpectedProviderEnvironment(true))
		require.NoError(t, err)
		source := railresolve.NewMerchantsSource(
			&config.Config{TestMode: config.CredentialPostureSandbox},
			func() *merchants.Service { return msvc },
		)
		_, err = source.RailConfig(ctx, string(models.RailNMI), "")
		require.Error(t, err)
		require.ErrorIs(t, err, railresolve.ErrRailNotArmed)
	})
}
