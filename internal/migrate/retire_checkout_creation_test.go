package migrate

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/stretchr/testify/require"
)

func TestRetireCheckoutCreationPreservesAcceptedEvidence(t *testing.T) {
	dsn := os.Getenv("SQLC_DATABASE_URL")
	if dsn == "" {
		t.Skip("SQLC_DATABASE_URL is required for the populated migration test")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := "checkout_retire_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
	})
	chain, err := loadMigrations(schema)
	require.NoError(t, err)
	before := -1
	for i, m := range chain {
		if strings.Contains(m.Name, "retire_checkout_creation") {
			before = i
		}
	}
	require.Positive(t, before)
	runner, err := migratekit.NewPostgresFromPGXPool(pool, "openrails")
	require.NoError(t, err)
	defer runner.Close()
	runner.WithSchema(schema).WithStrictIntegrity().WithRender(loadMigrations)
	require.NoError(t, runner.ApplyMigrations(t.Context(), chain[:before]))
	q := func(sql string) string { return strings.ReplaceAll(sql, "billing.", schema+".") }
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(t.Context(), q(sql), args...)
		require.NoError(t, err)
	}
	mid, customer, product, price, custodian := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO billing.merchants(id,slug,status) VALUES($1,'retire-checkout','active')`, mid)
	exec(`INSERT INTO billing.customers(merchant_id,id) VALUES($1,$2)`, mid, customer)
	exec(`INSERT INTO billing.products(merchant_id,id,key,display_name,tier_rank,archived) VALUES($1,$2,'retained','Accepted product',0,false)`, mid, product)
	exec(`INSERT INTO billing.prices(merchant_id,id,product_id,key,amount,currency,archived) VALUES($1,$2,$3,'accepted',10000000,'USD',false)`, mid, price, product)
	exec(`INSERT INTO billing.custodians(merchant_id,id,key,kind,environment,account_id) VALUES($1,$2,'hs','hyperswitch','test',$3)`, mid, custodian, "hs_"+mid.String())
	psps := map[string]uuid.UUID{}
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	for _, rail := range []string{"nmi", "stripe", "solana", "ccbill"} {
		psp := uuid.New()
		psps[rail] = psp
		exec(`INSERT INTO billing.psps(merchant_id,id,key,rail,environment,account_id) VALUES($1,$2,$3,$3,'test',$4)`, mid, psp, rail, rail+mid.String())
		id := uuid.New()
		accepted := map[string]any{"accepted_purchase": map[string]any{"price_id": price, "product_id": product, "payment_id": uuid.New(), "product_name": "Accepted product", "amount": "10000000", "currency": "USD", "accepted_at": now, "entitlement_start": now}}
		raw, err := json.Marshal(accepted)
		require.NoError(t, err)
		exec(`INSERT INTO billing.checkout_attempts(merchant_id,id,customer_id,psp_id,price_id,mode,rail,status,amount,currency,rail_state,expires_at,reference)
            VALUES($1,$2,$3,$4,$5,'one_off',$6,'requires_action',10000000,'USD',$7,$8,$9)`, mid, id, customer, psp, price, rail, raw, now.Add(time.Hour), "accepted-"+rail)
	}
	setupID := uuid.New()
	capture := models.CheckoutCapture{MerchantID: mid, CustomerID: customer, PSPID: psps["nmi"], CustodianID: custodian, Environment: "test", AccountID: "hs_" + mid.String(), ProfileID: "profile", PublicAPIKey: "pk", APIBaseURL: "https://hs.example", SDKURL: "https://hs.example/sdk.js", VendorCustomerID: "vendor-customer", VendorSessionID: "vendor-session", ExpiresAt: now.Add(time.Minute), SecretCiphertext: "same-encrypted-secret"}
	raw, err := json.Marshal(map[string]any{"capture": capture})
	require.NoError(t, err)
	exec(`INSERT INTO billing.checkout_attempts(merchant_id,id,customer_id,psp_id,mode,rail,status,rail_state,expires_at)
        VALUES($1,$2,$3,$4,'payment_method','nmi','requires_action',$5,$6)`, mid, setupID, customer, psps["nmi"], raw, capture.ExpiresAt)
	require.NoError(t, runner.ApplyMigrations(t.Context(), chain))
	database, err := db.NewWithPGXPool(pool, schema)
	require.NoError(t, err)
	referenced, err := database.Gen(merchant.WithID(t.Context(), billing.MerchantID(mid))).PricesHoldReferences(t.Context(), gen.PricesHoldReferencesParams{MerchantID: mid, PriceIds: []uuid.UUID{price}})
	require.NoError(t, err)
	require.True(t, referenced, "catalog deletion must retain the price needed by accepted recovery")
	var removed int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.columns WHERE table_schema=$1 AND table_name='checkout_attempts' AND column_name IN ('price_id','mode')`, schema).Scan(&removed))
	require.Zero(t, removed)
	for rail, psp := range psps {
		var row gen.BillingCheckoutAttempt
		require.NoError(t, pool.QueryRow(t.Context(), q(`SELECT id,customer_id,psp_id,rail,status,amount,currency,rail_state,reference FROM billing.checkout_attempts WHERE merchant_id=$1 AND reference=$2`), mid, "accepted-"+rail).Scan(&row.ID, &row.CustomerID, &row.PspID, &row.Rail, &row.Status, &row.Amount, &row.Currency, &row.RailState, &row.Reference))
		restored, err := models.CheckoutAttemptFromGen(row)
		require.NoError(t, err)
		require.Equal(t, &price, restored.AcceptedPriceID)
		require.False(t, restored.AcceptedSubscription)
		require.Equal(t, psp, restored.PspID)
		require.Equal(t, "requires_action", string(restored.Status))
		require.Equal(t, int64(10000000), *restored.Amount)
		original := restored.RailState["accepted_purchase"].(map[string]any)
		require.Equal(t, "10000000", original["amount"])
		require.Equal(t, price.String(), original["price_id"])
	}
	var persisted []byte
	var status, setup string
	require.NoError(t, pool.QueryRow(t.Context(), q(`SELECT capture,status,setup_ref FROM billing.payment_methods WHERE merchant_id=$1 AND id=$2`), mid, setupID).Scan(&persisted, &status, &setup))
	expected, err := json.Marshal(capture)
	require.NoError(t, err)
	require.JSONEq(t, string(expected), string(persisted), "same id and accepted binding keep the encrypted AAD valid")
	require.Equal(t, "requires_action", status)
	require.Equal(t, "vendor-session", setup)
	require.NoError(t, pool.QueryRow(t.Context(), q(`SELECT rail_state,status FROM billing.checkout_attempts WHERE merchant_id=$1 AND id=$2`), mid, setupID).Scan(&persisted, &status))
	require.Equal(t, "canceled", status)
	require.NotContains(t, string(persisted), "secret_ciphertext", "the pending payment method is the sole secret owner")
	var paymentPrice int
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.columns WHERE table_schema=$1 AND table_name='payments' AND column_name='price_id'`, schema).Scan(&paymentPrice))
	require.Equal(t, 1, paymentPrice, "provider-billed payment redesign is outside this migration")
}
