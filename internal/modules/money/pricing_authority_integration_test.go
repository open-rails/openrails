//go:build integration

package money_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

// TestPricingAuthoritySeparatesFinalCapturesFromCatalogInputs proves #1004:
// captured usage (including amount zero) is final host pricing and is excluded
// from catalog rating, while zero-cost RecordUsage remains a catalog input.
func TestPricingAuthoritySeparatesFinalCapturesFromCatalogInputs(t *testing.T) {
	svc, d, pool, payer, currency, ctx := moneyInEnvWithDB(t)
	merchantID, productID := dbtest.TestMerchantID.UUID(), uuid.New()
	meterKey := "authority-meter-" + uuid.NewString()
	require.NoError(t, pool.QueryRow(ctx, `SELECT 1`).Scan(new(int)))
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.usage_events WHERE customer_id=$1`, payer.UUID())
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.invoice_items WHERE customer_id=$1`, payer.UUID())
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.invoices WHERE customer_id=$1`, payer.UUID())
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.catalog_rate_cards WHERE merchant_id=$1 AND product_id=$2`, merchantID, productID)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.catalog_meters WHERE merchant_id=$1 AND key=$2`, merchantID, meterKey)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.products WHERE merchant_id=$1 AND id=$2`, merchantID, productID)
	})
	require.NoError(t, pool.QueryRow(ctx, `SELECT 1`).Scan(new(int)))
	_, err := svc.UpsertAccountSettings(ctx, payer, currency, money.AccountSettingsInput{BillingMode: strptr(money.BillingModeArrears)})
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO openrails.products (id,key,display_name,merchant_id) VALUES ($1,$2,'Pricing authority',$3)`, productID, "authority-product-"+uuid.NewString(), merchantID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO openrails.catalog_meters (merchant_id,key,event_type,value_property,aggregation) VALUES ($1,$2,$2,$2,'sum')`, merchantID, meterKey)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO openrails.catalog_rate_cards (merchant_id,product_id,ordinal,meter_key,payment_term,price) VALUES ($1,$2,1,$3,'in_arrears','{"model":"per_unit","currency":"USD","per_unit":{"unit_amount":"100","divide_by":1}}'::jsonb)`, merchantID, productID, meterKey)
	require.NoError(t, err)

	_, err = svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: "owner", Currency: currency, Amount: 1000, Source: "seed"})
	require.NoError(t, err)

	gate := spendgate.New(d)
	admit := func(requestID string, estimate int64) {
		require.NoError(t, svc.WithLockedAdmissionCapacity(ctx, payer, currency, func(ctx context.Context, tx *db.DB, capacity money.AdmissionCapacity) error {
			deadline := time.Now().Add(time.Hour)
			if estimate == 0 {
				deadline = time.Time{}
			}
			result, err := gate.Admit(ctx, tx.Gen(ctx), spendgate.AdmitInput{Customer: payer.UUID(), Currency: currency, RequestID: requestID, Cost: estimate,
				ExpiresAt: deadline, AccountBalance: capacity.Balance - capacity.Held, Terms: spendgate.Terms{Invoker: "user", InvokerType: "payer"}})
			require.NoError(t, err)
			require.True(t, result.Allowed)
			return nil
		}))
	}
	admit("host-positive", 40)
	_, err = svc.CaptureAdmission(ctx, "host-positive", 40, &openrails.CaptureUsage{EventType: meterKey, SourceID: "host-positive", Dimensions: map[string]int64{meterKey: 2}})
	// openrails is imported only for the shared capture usage terms.
	require.NoError(t, err)
	admit("host-zero", 0)
	_, err = svc.CaptureAdmission(ctx, "host-zero", 0, &openrails.CaptureUsage{EventType: meterKey, SourceID: "host-zero", Dimensions: map[string]int64{meterKey: 2}})
	require.NoError(t, err)
	_, err = svc.RecordUsage(ctx, money.RecordUsageParams{Payer: &payer, Invoker: "user", Currency: currency, EventType: meterKey,
		Dimensions: map[string]int64{meterKey: 2}, Amount: 0, Key: money.MustIdempotencyKey(money.UsageOperation(meterKey), "catalog", "catalog-input")})
	require.NoError(t, err)
	var authorities []string
	rows, err := pool.Query(ctx, `SELECT pricing_authority FROM openrails.usage_events WHERE customer_id=$1 AND event_type=$2 ORDER BY created_at`, payer.UUID(), meterKey)
	require.NoError(t, err)
	for rows.Next() {
		var authority string
		require.NoError(t, rows.Scan(&authority))
		authorities = append(authorities, authority)
	}
	rows.Close()
	require.Equal(t, []string{"host", "host", "catalog"}, authorities)
	before, err := svc.GetAdmissionCapacity(ctx, payer, currency)
	require.NoError(t, err)
	from, to := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	invoice, err := svc.FinalizeInvoice(ctx, payer, currency, from, to)
	require.NoError(t, err)
	after, err := svc.GetAdmissionCapacity(ctx, payer, currency)
	require.NoError(t, err)
	require.EqualValues(t, 0, before.OutstandingOwed)
	require.EqualValues(t, 200, after.OutstandingOwed)
	require.EqualValues(t, 200, invoice.AmountDue)
}
