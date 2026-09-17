//go:build integration

package embed_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
)

// Payment reads run identically through the in-process and HTTP transports and
// carry every int64 amount exactly (the platform-billing settlement path).
func TestPaymentReadsThroughSharedClient(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	remote := h.StartStandalone("USD")
	owned := remote.ProvisionOwnedMerchant("payments-" + uuid.NewString()[:8])
	mid := owned.MerchantID
	runtime, err := embed.New(ctx, embed.Options{
		Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB, DB: &config.DBConfig{URL: h.DSN}},
		Redis:  h.Redis, River: embed.RiverManagedByOpenRails(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	local, err := runtime.Client(openrails.WithMerchantID(mid))
	require.NoError(t, err)
	standalone := remote.Client(openrails.WithTokenProvider(func(context.Context) (string, error) { return owned.APIKey, nil }))

	pool := h.MerchantPool(mid.UUID())
	payer, other, product, price, payment, refund := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) { _, err := pool.Exec(ctx, sql, args...); require.NoError(t, err) }
	exec(`INSERT INTO openrails.customers (id,merchant_id) VALUES ($1,$2),($3,$2)`, payer, mid.UUID(), other)
	exec(`INSERT INTO openrails.products (id,merchant_id,key,display_name) VALUES ($1,$2,$3,'Payment reads')`, product, mid.UUID(), uuid.NewString())
	exec(`INSERT INTO openrails.prices (id,merchant_id,product_id,amount,currency) VALUES ($1,$2,$3,$4,'USD')`, price, mid.UUID(), product, int64(math.MaxInt64))
	psp := dbtest.EnsureTestPSP(ctx, t, pool, mid.UUID(), "nmi")
	purchased := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	exec(`INSERT INTO openrails.payments (id,merchant_id,customer_id,price_id,rail,transaction_id,amount,list_amount,currency,status,money_movement,psp_id,purchased_at)
		VALUES ($1,$2,$3,$4,'nmi',$5,$6,$6,'USD','completed','rail',$7,$8)`, payment, mid.UUID(), payer, price, uuid.NewString(), int64(math.MaxInt64), psp, purchased)
	exec(`INSERT INTO openrails.payments (id,merchant_id,customer_id,price_id,rail,transaction_id,amount,list_amount,currency,status,money_movement,psp_id,refunded_payment_id,purchased_at)
		VALUES ($1,$2,$3,$4,'nmi',$5,-1,-1,'USD','completed','rail',$6,$7,$8)`, refund, mid.UUID(), payer, price, uuid.NewString(), psp, payment, purchased.Add(time.Minute))
	exec(`INSERT INTO openrails.payments (id,merchant_id,customer_id,price_id,rail,transaction_id,amount,list_amount,currency,status,money_movement,psp_id,purchased_at)
		VALUES ($1,$2,$3,$4,'nmi',$5,500,500,'USD','completed','rail',$6,$7)`, uuid.New(), mid.UUID(), other, price, uuid.NewString(), psp, purchased)

	var reference *openrails.Payment
	for name, client := range map[string]*openrails.Client{"embedded": local, "standalone": standalone} {
		t.Run(name, func(t *testing.T) {
			got, err := client.GetPayment(ctx, openrails.PaymentID(payment))
			require.NoError(t, err)
			require.Equal(t, openrails.PaymentID(payment), got.ID)
			require.EqualValues(t, math.MaxInt64, got.Amount, "the full int64 range survives the wire")
			require.EqualValues(t, 1, got.AmountRefunded)
			require.Equal(t, "USD", got.Currency)
			require.Equal(t, openrails.CustomerID(payer), got.CustomerID)
			require.NotNil(t, got.Price)
			require.Equal(t, openrails.PriceID(price), got.Price.ID)
			require.EqualValues(t, math.MaxInt64, got.Price.UnitAmount)
			require.NotNil(t, got.Refunds)
			require.Len(t, got.Refunds.Data, 1)
			require.EqualValues(t, -1, got.Refunds.Data[0].Amount)
			if reference == nil {
				reference = got
			} else {
				require.Equal(t, reference, got, "both transports return the same payment")
			}

			page, err := client.ListPayments(ctx, openrails.PaymentFilter{CustomerID: openrails.CustomerID(payer), PriceID: openrails.PriceID(price)})
			require.NoError(t, err)
			require.EqualValues(t, 2, page.Total, "the payer's payment and its refund")
			for _, item := range page.Data {
				require.Equal(t, openrails.CustomerID(payer), item.CustomerID)
			}
			all, err := client.ListPayments(ctx, openrails.PaymentFilter{PageOptions: openrails.PageOptions{Limit: 1}})
			require.NoError(t, err)
			require.Len(t, all.Data, 1)
			require.EqualValues(t, 3, all.Total)
			require.True(t, all.HasMore)

			_, err = client.GetPayment(ctx, openrails.PaymentID(uuid.New()))
			require.ErrorIs(t, err, openrails.ErrNotFound)
			_, err = client.GetPayment(ctx, openrails.PaymentID{})
			require.ErrorIs(t, err, openrails.ErrInvalid)
		})
	}

	// Another merchant's key never sees these payments.
	stranger := remote.Client()
	_, err = stranger.GetPayment(ctx, openrails.PaymentID(payment))
	require.ErrorIs(t, err, openrails.ErrNotFound)
}
