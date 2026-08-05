//go:build integration

package checkout

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #681: a test_mode deployment declares environment=test provider accounts. The
// checkout money paths must resolve NMI and CCBill scoped credentials from those
// test rows end-to-end: real Postgres rail_merchant_accounts + merchant_secrets
// rows, resolved through the REAL merchants.Service (no fake resolvers).
func TestSandboxPostureCheckoutResolvesNMIAndCCBillFromTestRows(t *testing.T) {
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(context.Background(), t, pool)
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)

	dbp := db.WrapPool(pool, "")
	store, err := merchants.NewDBSecretStore(dbp)
	require.NoError(t, err)
	// Deployment posture: test_mode ⇒ the service resolves environment=test rows.
	msvc, err := merchants.NewService(dbp, store, config.ExpectedProviderEnvironment(true))
	require.NoError(t, err)

	// The row carries its manifest key so the assertion below can name THIS
	// account instead of relying on it being the newest one in a database other
	// tests also seed accounts into.
	seed := func(rail, key, accountID string) {
		_, err := pool.Exec(ctx, `
			INSERT INTO openrails.psps (merchant_id, rail, environment, account_id, key, archived, evidence)
			VALUES ($1::uuid, $2, 'test', $3, $4, false, '{"source":"test_681"}'::jsonb)
			ON CONFLICT (rail, environment, account_id) DO UPDATE SET archived = false, key = EXCLUDED.key
		`, dbtest.TestMerchantID.String(), rail, accountID, key)
		require.NoError(t, err)
	}
	putSecret := func(rail, accountID, key, value string) {
		name, err := merchants.PSPSecretName(rail, "test", accountID, key)
		require.NoError(t, err)
		_, err = store.Put(ctx, dbtest.TestMerchantID, name, value)
		require.NoError(t, err)
	}

	const nmiAccount = "sandbox-gw-681"
	const nmiKey = "sandbox681"
	seed("nmi", nmiKey, nmiAccount)
	putSecret("nmi", nmiAccount, "security_key", "nmi-sandbox-key-681")

	const ccbillAccount = "945284-0001"
	seed("ccbill", "ccbill", ccbillAccount)
	putSecret("ccbill", ccbillAccount, "salt", "ccbill-sandbox-salt-681")

	svc := &CheckoutService{Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeFull, TestMode: config.CredentialPostureSandbox}}
	svc.SetMerchantSecretStore(store)
	svc.SetRailMerchantAccountSecretResolver(msvc)

	client, err := svc.resolveNMIClient(ctx, nmiKey)
	require.NoError(t, err)
	require.Equal(t, "nmi-sandbox-key-681", client.SecurityKey)

	cfg, err := svc.resolveCCBillConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, "945284", cfg.ClientAccNum)
	require.Equal(t, "0001", cfg.ClientSubAcc)
	require.Equal(t, "ccbill-sandbox-salt-681", cfg.Salt)
}
