//go:build integration

package checkout

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
)

// or#880 phase 3 END TO END: a checkout CHARGES through a custodian that was
// declared once and referenced by two PSPs.
//
// The other custody tests either resolve credentials without charging
// (custodian_secrets_integration_test) or charge from a hand-built in-memory
// set (custodian_sale_integration_test, railresolve.FixedSet). This one joins
// them: real openrails.custodians + openrails.psps rows, real secret store,
// the production railresolve.MerchantsSource, and the same fake Basis Theory
// wire server the proxy-charge harness uses. If the phase-3 reference model
// failed to arm anything on the money path, the charge would not happen here.
func TestCustodianSale_ChargesThroughStoreArmedCustodian(t *testing.T) {
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()
	dbtest.EnsureTestMerchant(context.Background(), t, pool)
	ctx := merchant.WithID(context.Background(), dbtest.TestMerchantID)

	const tenantID = "tnt_store_880"
	const primaryGateway = "579145-880store"
	const backupGateway = "579146-880store"

	bt := newFakeBTServer(t)

	// ONE custodian, TWO PSPs referencing it — the phase-3 shape the manifest
	// converges to (internal/bootstrap proves the manifest lands these rows).
	var custodianID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO openrails.custodians (merchant_id, key, kind, environment, account_id, settings)
		VALUES ($1::uuid, 'bt', 'basis_theory', 'test', $2, '{"public_api_key":"key_pub_880"}'::jsonb)
		ON CONFLICT (kind, environment, account_id) DO UPDATE SET settings = EXCLUDED.settings
		RETURNING id
	`, dbtest.TestMerchantID.String(), tenantID).Scan(&custodianID))
	for _, gateway := range []string{primaryGateway, backupGateway} {
		_, err := pool.Exec(ctx, `
			INSERT INTO openrails.psps (merchant_id, rail, environment, account_id, archived, evidence, custodian_id)
			VALUES ($1::uuid, 'nmi', 'test', $2, false, '{"source":"test_880"}'::jsonb, $3::uuid)
			ON CONFLICT (rail, environment, account_id) DO UPDATE SET archived = false, custodian_id = EXCLUDED.custodian_id
		`, dbtest.TestMerchantID.String(), gateway, custodianID)
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.psps WHERE account_id = ANY($1)", []string{primaryGateway, backupGateway})
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.custodians WHERE id = $1", custodianID)
	})

	store := merchants.NewMemorySecretStore()
	put := func(name, value string) {
		_, err := store.Put(ctx, dbtest.TestMerchantID, name, value)
		require.NoError(t, err)
	}
	for gateway, key := range map[string]string{primaryGateway: "sk_primary_880", backupGateway: "sk_backup_880"} {
		name, err := merchants.PSPSecretName("nmi", "test", gateway, "security_key")
		require.NoError(t, err)
		put(name, key)
	}
	custodianSecret, err := merchants.CustodianSecretName(models.CustodianBasisTheory, "test", tenantID, custodians.SecretAPIKey)
	require.NoError(t, err)
	put(custodianSecret, "key_private_880")

	dbp := db.WrapPool(pool, "")
	msvc, err := merchants.NewService(dbp, store, config.ExpectedProviderEnvironment(true))
	require.NoError(t, err)
	source := railresolve.NewMerchantsSource(
		&config.Config{TestMode: config.CredentialPostureSandbox},
		func() *merchants.Service { return msvc },
	)

	now := time.Now().UTC().Truncate(time.Second)
	userID := uuid.New().String()
	customerID := dbtest.EnsureCustomerIDPgx(ctx, t, pool, userID)
	productID, priceID := uuid.New(), uuid.New()
	insertProductAndPrice(ctx, t, pool, &models.Product{
		ID: productID, Key: "custody-store-" + uuid.NewString()[:8], DisplayName: "Store-Armed Custody",
		Archived: false, CreatedAt: now, UpdatedAt: now,
	}, &models.Price{
		ID: priceID, ProductID: productID, Archived: false,
		Amount: 1_990_000, Currency: "USD", CreatedAt: now, UpdatedAt: now,
	})
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.rail_intents WHERE price_id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.entitlements WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payments WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.payment_methods WHERE customer_id = $1", customerID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.prices WHERE id = $1", priceID)
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.products WHERE id = $1", productID)
	})

	clock := clockwork.NewRealClock()
	svc := &CheckoutCustodianSaleService{
		DB: dbi,
		PurchaseService: NewCheckoutPurchaseService(
			catalog.NewPriceService(dbi),
			catalog.NewProductService(dbi),
			payments.NewPaymentService(dbi, clock),
			entitlements.NewEntitlementService(dbi, clock),
			nil,
			clock,
		),
		PaymentMethodService: paymentmethods.NewPaymentMethodService(dbi),
		Rails:                source,
		BTBaseURLOverride:    bt.srv.URL,
		DisableGatewayVerify: true, // the fake BT serves no query.php
	}
	runner := &intents.Runner{
		Store:    intents.NewStore(dbi),
		Registry: intents.NewRegistry(NewCustodianSaleIntentHandler(svc)),
		Config:   fullModeConfig(),
	}
	svc.Intents = runner

	// The custody arrangement resolves off the STORE, not a fixed set.
	cfg, err := svc.resolveConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, "bt", cfg.Custody.Key)
	require.Equal(t, tenantID, cfg.Custody.AccountID)
	require.Equal(t, "key_private_880", cfg.Custody.APIKey)

	intent, err := runner.EnqueueAndExecute(ctx, intents.EnqueueParams{
		MerchantID: dbtest.TestMerchantID.UUID(),
		Provider:   string(models.RailNMI),
		IntentType: TypeCustodianSale,
		PriceID:    &priceID,
		Payload: CustodianSalePayload{
			TokenIntentID: bt.intentID,
			AmountMicros:  1_990_000,
			Currency:      "USD",
			Description:   "Purchase: Store-Armed Custody",
			UserID:        userID,
			PriceID:       priceID,
		},
		IdempotencyKey: CustodianSaleIdempotencyKey("custody-store-" + uuid.NewString()[:8]),
		NextAttemptAt:  time.Now().UTC(),
		Origin:         intents.OriginUser,
		OriginReason:   "or#880 store-armed custody sale",
	})
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, intent.Status)

	// The charge rode the CUSTODIAN's proxy into a gateway whose security_key
	// came from the PSP — two declarations, one charge.
	form := bt.lastProxyForm(t)
	require.Equal(t, "1.99", form.Get("amount"))
	require.Contains(t, form.Get("ccnumber"), "token_intent: "+bt.intentID)
	require.Contains(t, []string{"sk_primary_880", "sk_backup_880"}, form.Get("security_key"))

	var tokenType string
	require.NoError(t, dbi.Pool().QueryRow(ctx,
		"SELECT COALESCE(token_type,'') FROM openrails.payments WHERE rail='nmi' AND transaction_id=$1 AND status='completed'",
		bt.txnID).Scan(&tokenType))
	require.Equal(t, charge.TokenTypePANViaProxy, tokenType)

	var custodian, methodRef string
	require.NoError(t, dbi.Pool().QueryRow(ctx,
		`SELECT custodian, rail_method_ref FROM openrails.payment_methods
		 WHERE rail='nmi' AND custodian='basis_theory' AND rail_method_ref=$1`,
		bt.tokenID).Scan(&custodian, &methodRef))
	require.Equal(t, models.CustodianBasisTheory, custodian)
	require.Equal(t, bt.tokenID, methodRef)
}
