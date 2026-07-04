//go:build integration

package intents

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #730: manual dunning rebills arm NMI credentials the way production does —
// a rail_merchant_accounts row + scoped secret in the merchant-secrets store,
// resolved through the ONE #725 builder AT CHARGE TIME. Real Postgres store;
// only the external NMI gateway is a fake HTTP server.

func storeRebillConfig() *config.Config {
	// TestMode=sandbox → provider environment "test", matching the seeding below.
	return &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull}
}

func rebillMerchantsService(t *testing.T, dbi *db.DB) *merchants.Service {
	t.Helper()
	store, err := merchants.NewDBSecretStore(dbi.DataPool())
	require.NoError(t, err)
	svc, err := merchants.NewService(dbi.DataPool(), store, config.ExpectedProviderEnvironment(true))
	require.NoError(t, err)
	return svc
}

// seedNMIRailAccountForRebill declares an NMI account for the shared test
// merchant the way the merchant manifest does, arming the given scoped
// secrets. Returns the account row id — stamped on intents as the #704
// provenance scope so resolution stays deterministic against the shared DB.
func seedNMIRailAccountForRebill(t *testing.T, dbi *db.DB, svc *merchants.Service, accountID string, secrets map[string]string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	env := config.ExpectedProviderEnvironment(true)
	rail := string(models.RailNMI)
	for key, value := range secrets {
		name, err := merchants.RailMerchantAccountSecretName(rail, env, accountID, key)
		require.NoError(t, err)
		_, err = svc.Secrets().Put(ctx, dbtest.TestMerchantID, name, value)
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = svc.Secrets().Delete(context.Background(), dbtest.TestMerchantID, name)
		})
	}
	var rowID uuid.UUID
	mctx := merchant.WithID(ctx, dbtest.TestMerchantID)
	require.NoError(t, dbi.RunInMerchantConn(mctx, func(ctx context.Context) error {
		row, err := dbi.Gen(ctx).UpsertRailMerchantAccount(ctx, gen.UpsertRailMerchantAccountParams{
			MerchantID:  dbtest.TestMerchantID.UUID(),
			Rail:        rail,
			Environment: &env,
			AccountID:   accountID,
		})
		rowID = row.ID
		return err
	}))
	t.Cleanup(func() {
		_, _ = dbi.Pool().Exec(context.Background(),
			`DELETE FROM openrails.rail_merchant_accounts WHERE id = $1`, rowID)
	})
	return rowID
}

func storeRebillBuilder(dbi *db.DB, svc *merchants.Service, cfg *config.Config, gatewayURL string) *money.MerchantCollectionAdapterBuilder {
	return &money.MerchantCollectionAdapterBuilder{
		Config:      cfg,
		Rails:       nil, // boot plane empty inside the builder: store only
		DB:          dbi,
		MerchantsFn: func() *merchants.Service { return svc },
		Endpoints:   money.CollectionEndpoints{NMIDirectPostURL: gatewayURL, NMIQueryURL: gatewayURL},
	}
}

func storeArmedRebillRunner(fx rebillFixture, boot map[string]*nmi.NMIClient, resolver money.NMIClientResolver, cfg *config.Config) *Runner {
	h := NewManualRebillHandler(fx.db, cfg, boot, nil)
	h.SetNMIClientResolver(resolver)
	return &Runner{Store: fx.store, Registry: NewRegistry(h), Config: cfg}
}

// TestManualRebillStoreOnlyNMICredentials_ChargesThroughStore: the merchant's
// NMI key exists ONLY in the merchant-secrets store (boot Clients empty); the
// rebill resolves it at charge time, authenticates with the STORE key, and the
// handler's finalize renews the membership.
func TestManualRebillStoreOnlyNMICredentials_ChargesThroughStore(t *testing.T) {
	fx := seedPastDueSubscription(t)
	fake, bootClient := newFakeNMIRebillGateway(t)
	gatewayURL := bootClient.DirectPostURL

	msvc := rebillMerchantsService(t, fx.db)
	sfx := uuid.NewString()[:8]
	storeKey := "store-only-rebill-key-" + sfx
	accountRowID := seedNMIRailAccountForRebill(t, fx.db, msvc, "gw-rebill-"+sfx,
		map[string]string{"security_key": storeKey})

	cfg := storeRebillConfig()
	runner := storeArmedRebillRunner(fx, nil, storeRebillBuilder(fx.db, msvc, cfg, gatewayURL), cfg)

	params := fx.enqueueParams(1)
	params.RailMerchantAccountID = &accountRowID // #704 provenance stamp (what dunning enqueues)

	row, err := runner.EnqueueAndExecute(context.Background(), params)
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, row.Status)
	assert.EqualValues(t, 1, fake.saleCalls.Load())
	assert.Equal(t, storeKey, fake.saleAuthKey.Load(), "charge must authenticate with the STORE-resolved key")
	assert.Equal(t, "active", string(fx.subscription(t).Status), "finalize renewed the membership")
}

// TestManualRebillDeclaredAccountMissingSecret_FailsClosed: a declared account
// whose security_key is absent from the store parks the intent — it never
// falls back to the (working) boot client and never charges.
func TestManualRebillDeclaredAccountMissingSecret_FailsClosed(t *testing.T) {
	fx := seedPastDueSubscription(t)
	fake, bootClient := newFakeNMIRebillGateway(t)

	msvc := rebillMerchantsService(t, fx.db)
	sfx := uuid.NewString()[:8]
	accountRowID := seedNMIRailAccountForRebill(t, fx.db, msvc, "gw-secretless-"+sfx, nil)

	cfg := storeRebillConfig()
	boot := map[string]*nmi.NMIClient{"mobius": bootClient} // must NOT be reached
	runner := storeArmedRebillRunner(fx, boot, storeRebillBuilder(fx.db, msvc, cfg, bootClient.DirectPostURL), cfg)

	params := fx.enqueueParams(1)
	params.RailMerchantAccountID = &accountRowID

	row, err := runner.EnqueueAndExecute(context.Background(), params)
	require.NoError(t, err)
	assert.Equal(t, StatusPending, row.Status, "unarmable charge parks, never fires")
	require.NotNil(t, row.LastFailureReason)
	assert.Contains(t, *row.LastFailureReason, "missing")
	assert.Zero(t, fake.saleCalls.Load(), "fail-closed must never fall back to the boot client")
	assert.Equal(t, "past_due", string(fx.subscription(t).Status))
}

// TestManualRebillNoStoreRow_FallsBackToBootClients: a merchant with no
// declared NMI account charges through the boot-plane Clients map (embedded
// hosts' model).
func TestManualRebillNoStoreRow_FallsBackToBootClients(t *testing.T) {
	fx := seedPastDueSubscription(t)
	fake, bootClient := newFakeNMIRebillGateway(t)

	msvc := rebillMerchantsService(t, fx.db)
	// The builder resolves in the LIVE environment posture: integration tests
	// only ever declare test-environment accounts, so the pull scope is
	// deterministically empty here even when other packages concurrently seed
	// NMI rows for the shared test merchant.
	liveCfg := &config.Config{Env: "dev", TestMode: config.CredentialPostureLive, ProviderWriteMode: config.ProviderWriteModeFull}

	cfg := storeRebillConfig()
	boot := map[string]*nmi.NMIClient{"mobius": bootClient}
	runner := storeArmedRebillRunner(fx, boot, storeRebillBuilder(fx.db, msvc, liveCfg, bootClient.DirectPostURL), cfg)

	row, err := runner.EnqueueAndExecute(context.Background(), fx.enqueueParams(1))
	require.NoError(t, err)
	assert.Equal(t, StatusSucceeded, row.Status)
	assert.EqualValues(t, 1, fake.saleCalls.Load())
	assert.Equal(t, "test_security_key", fake.saleAuthKey.Load(), "boot client served the merchant with no store row")
	assert.Equal(t, "active", string(fx.subscription(t).Status))
}
