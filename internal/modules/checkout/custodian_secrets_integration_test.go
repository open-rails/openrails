//go:build integration

package checkout

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #795 spec C12 / or#879 / or#880: a custodian-held card arms end-to-end from
// real openrails.custodians + openrails.psps rows and the merchant secret
// store, in BOTH secret modes (MODE 2 DB-backed store; MODE 1 in-memory
// manifest plane).
//
// The phase-3 shape is the point: ONE custodian row, TWO PSPs referencing it.
// Both gateways arm with their own security_key and the SAME custodial tenant
// id and application key — one declaration, not a copy per PSP that can drift.
func TestCustodianHeldCardResolvesFromStoreBothModes(t *testing.T) {
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(context.Background(), t, pool)
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)

	const tenantID = "tnt_c12_test"
	const primaryGateway = "579145-c12"
	const backupGateway = "579146-c12"

	// Layer B rows (identical in both modes — mode 1 converges the same rows).
	var custodianID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO openrails.custodians (merchant_id, key, kind, environment, account_id, settings)
		VALUES ($1::uuid, 'bt', 'basis_theory', 'test', $2, '{"public_api_key":"key_pub_c12","network_tokens":true}'::jsonb)
		ON CONFLICT (kind, environment, account_id) DO UPDATE SET settings = EXCLUDED.settings
		RETURNING id
	`, dbtest.TestMerchantID.String(), tenantID).Scan(&custodianID))
	for _, gateway := range []string{primaryGateway, backupGateway} {
		_, err := pool.Exec(ctx, `
			INSERT INTO openrails.psps (merchant_id, rail, environment, account_id, archived, evidence, custodian_id)
			VALUES ($1::uuid, 'nmi', 'test', $2, false, '{"source":"test_795"}'::jsonb, $3::uuid)
			ON CONFLICT (rail, environment, account_id) DO UPDATE SET archived = false, custodian_id = EXCLUDED.custodian_id
		`, dbtest.TestMerchantID.String(), gateway, custodianID)
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.psps WHERE account_id = ANY($1)", []string{primaryGateway, backupGateway})
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.custodians WHERE id = $1", custodianID)
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
			putPSP := func(accountID, key, value string) {
				secretName, err := merchants.PSPSecretName("nmi", "test", accountID, key)
				require.NoError(t, err)
				_, err = store.Put(ctx, dbtest.TestMerchantID, secretName, value)
				require.NoError(t, err)
			}
			putPSP(primaryGateway, "security_key", "sk_gateway_c12_"+name)
			putPSP(backupGateway, "security_key", "sk_backup_c12_"+name)

			// The custodial secret is scoped to the CUSTODIAN's identity, so it
			// is written ONCE and serves both gateways.
			custodianSecret, err := merchants.CustodianSecretName(models.CustodianBasisTheory, "test", tenantID, custodians.SecretAPIKey)
			require.NoError(t, err)
			require.Equal(t, "custodians/basis_theory/test/"+tenantID+"/api_key", custodianSecret)
			_, err = store.Put(ctx, dbtest.TestMerchantID, custodianSecret, "key_private_c12_"+name)
			require.NoError(t, err)

			msvc, err := merchants.NewService(dbp, store, config.ExpectedProviderEnvironment(true))
			require.NoError(t, err)
			source := railresolve.NewMerchantsSource(
				&config.Config{TestMode: config.CredentialPostureSandbox},
				func() *merchants.Service { return msvc },
			)

			armed, err := source.Armed(ctx, string(models.RailNMI))
			require.NoError(t, err)
			require.True(t, armed)

			for gateway, gatewayKey := range map[string]string{
				primaryGateway: "sk_gateway_c12_" + name,
				backupGateway:  "sk_backup_c12_" + name,
			} {
				rc, err := source.RailConfig(ctx, string(models.RailNMI), gateway)
				require.NoError(t, err)
				// The gateway half: each PSP's own, and still what charges.
				require.Equal(t, gateway, rc.AccountID)
				require.NotNil(t, rc.NMI)
				require.Equal(t, gatewayKey, rc.NMI.SecurityKey)
				// The custody half: ONE declaration, shared.
				require.NotNil(t, rc.Custody)
				require.Equal(t, "bt", rc.Custody.Key)
				require.Equal(t, models.CustodianBasisTheory, rc.Custody.Custodian)
				require.Equal(t, tenantID, rc.Custody.AccountID)
				require.Equal(t, "key_private_c12_"+name, rc.Custody.APIKey)
				require.True(t, rc.Custody.NetworkTokens)
				require.Equal(t, "key_pub_c12", rc.Custody.PublicAPIKey)
			}

			// A custodian webhook resolves the SAME credentials by vendor
			// identity alone — no PSP involved, because one custodian backs two.
			cc, err := source.CustodianConfig(ctx, models.CustodianBasisTheory, tenantID)
			require.NoError(t, err)
			require.Equal(t, "key_private_c12_"+name, cc.APIKey)
			require.Equal(t, tenantID, cc.AccountID)
		})
	}

	// Fail-closed: a PSP that REFERENCES a custodian with no api_key secret
	// must refuse to arm — never a silent downgrade to "no custody", which
	// would route the charge as if the gateway held the card.
	t.Run("missing_custodian_api_key_fails_closed", func(t *testing.T) {
		emptyStore := merchants.NewMemorySecretStore()
		msvc, err := merchants.NewService(dbp, emptyStore, config.ExpectedProviderEnvironment(true))
		require.NoError(t, err)
		source := railresolve.NewMerchantsSource(
			&config.Config{TestMode: config.CredentialPostureSandbox},
			func() *merchants.Service { return msvc },
		)
		_, err = source.RailConfig(ctx, string(models.RailNMI), primaryGateway)
		require.Error(t, err)
		require.ErrorIs(t, err, railresolve.ErrRailNotArmed)
	})

	// The custodian row is the merchant's own: a PSP may not reference another
	// merchant's, and the composite foreign key — not a validator — is what
	// makes that impossible.
	t.Run("cross_merchant_reference_is_refused_by_the_database", func(t *testing.T) {
		other := uuid.New()
		_, err := pool.Exec(ctx, `
			INSERT INTO openrails.psps (merchant_id, rail, environment, account_id, evidence, custodian_id)
			VALUES ($1::uuid, 'nmi', 'test', 'foreign-gateway-c12', '{}'::jsonb, $2::uuid)
		`, other.String(), custodianID)
		require.Error(t, err, "a PSP owned by another merchant must not reference this custodian")
	})
}
