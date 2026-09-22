//go:build integration

package embed_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestCustomerPaymentRefundTotalsAcrossPages(t *testing.T) {
	ctx := t.Context()
	_, pool, dsn := scopeWithoutRLSDatabase(t)
	runtime, mid, err := newDeclaredMerchant(ctx, embed.Options{
		Config: &config.Config{Env: "development", TestMode: config.CredentialPostureSandbox,
			MerchantConfigSource: config.MerchantConfigSourceAPI, SecretBackend: config.SecretBackendDB,
			ProviderWriteMode: config.ProviderWriteModeReadOnly, NewSubscriptionCollectionPolicy: "engine", DB: &config.DBConfig{URL: dsn}},
		PGXPool: pool, River: embed.RiverFromHost(),
	}, "refund-history-"+uuid.NewString(), embed.MerchantConfig{DisplayName: "Refund history"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	alice, bob := uuid.New(), uuid.New()
	authn := billingauth.DelegatedAuthenticatorFunc(func(_ context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
		var customer uuid.UUID
		switch r.Header.Get("Authorization") {
		case "Bearer alice":
			customer = alice
		case "Bearer bob":
			customer = bob
		default:
			return nil, billingauth.ErrUnauthenticated
		}
		return &billingauth.DelegatedPrincipal{MerchantID: mid.String(), SubjectID: customer.String(), CredentialClass: billingauth.CredentialClassUserSession}, nil
	})
	require.NoError(t, runtime.ConfigureHTTP(embed.HTTPConfig{CustomerExposures: []embed.CustomerHTTPConfig{{Prefix: "/v1/me", Scope: embed.CustomerBillingManagement, DelegatedAuthenticator: authn}}}))
	routes, err := openrailshttp.Routes(runtime)
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, routes.Mount(mux, "/billing"))
	exec := func(query string, args ...any) { _, err := pool.Exec(ctx, query, args...); require.NoError(t, err) }
	exec(`INSERT INTO billing.customers(id,merchant_id) VALUES($1,$2),($3,$2)`, alice, mid.UUID(), bob)
	product, price := uuid.New(), uuid.New()
	exec(`INSERT INTO billing.products(id,merchant_id,key,display_name) VALUES($1,$2,$3,'Refunded product')`, product, mid.UUID(), product.String())
	exec(`INSERT INTO billing.prices(id,merchant_id,product_id,amount,currency) VALUES($1,$2,$3,1000000,'USD')`, price, mid.UUID(), product)
	psp := dbtest.EnsureTestPSP(ctx, t, pool, mid.UUID(), "nmi")
	ordinal := 0
	insert := func(customer uuid.UUID, amount int64, status string, original *uuid.UUID) uuid.UUID {
		ordinal++
		id := uuid.New()
		created := time.Date(2026, 1, 1, 0, 0, ordinal, 0, time.UTC)
		exec(`INSERT INTO billing.payments(id,merchant_id,customer_id,price_id,rail,transaction_id,amount,list_amount,currency,status,money_movement,psp_id,refunded_payment_id,created_at,purchased_at)
   VALUES($1,$2,$3,$4,'nmi',$5,$6,1000000,'USD',$7,'rail',$8,$9,$10,$10)`, id, mid.UUID(), customer, price, id.String(), amount, status, psp, original, created)
		return id
	}
	none := insert(alice, 1000000, "completed", nil)
	partial := insert(alice, 1000000, "completed", nil)
	full := insert(alice, 1000000, "completed", nil)
	bobCharge := insert(bob, 1000000, "completed", nil)
	firstPartial := insert(alice, -200000, "completed", &partial)
	insert(alice, -100000, "completed", &partial)
	fullRefund := insert(alice, -1000000, "completed", &full)
	insert(alice, -400000, "pending", &partial)
	insert(alice, -500000, "failed", &partial)
	deleted := insert(alice, -600000, "completed", &partial)
	exec(`UPDATE billing.payments SET deleted_at=now() WHERE id=$1`, deleted)
	insert(bob, -700000, "completed", &bobCharge)
	type row struct {
		ID             string `json:"id"`
		Object         string `json:"object"`
		Status         string `json:"status"`
		Amount         int64  `json:"amount,string"`
		AmountRefunded int64  `json:"amount_refunded,string"`
		Refunded       bool   `json:"refunded"`
	}
	pages := func(token string) map[string]row {
		t.Helper()
		found := map[string]row{}
		for offset := 0; ; offset++ {
			request := httptest.NewRequest(http.MethodGet, "/billing/v1/me/payments?limit=1&offset="+strconv.Itoa(offset)+"&customer_id="+alice.String(), nil)
			request.Header.Set("Authorization", "Bearer "+token)
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var page struct {
				Data  []row `json:"data"`
				Total int   `json:"total"`
			}
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
			if offset >= page.Total {
				require.Empty(t, page.Data)
				break
			}
			require.Len(t, page.Data, 1, "each original and refund must occupy different pages")
			require.NotContains(t, found, page.Data[0].ID)
			found[page.Data[0].ID] = page.Data[0]
		}
		return found
	}
	foreignMerchant, foreignProduct, foreignPrice, foreignCharge := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO billing.merchants(id,slug) VALUES($1,$2)`, foreignMerchant, "foreign-"+foreignMerchant.String())
	exec(`INSERT INTO billing.customers(id,merchant_id) VALUES($1,$2)`, alice, foreignMerchant)
	exec(`INSERT INTO billing.products(id,merchant_id,key,display_name) VALUES($1,$2,$3,'Foreign product')`, foreignProduct, foreignMerchant, foreignProduct.String())
	exec(`INSERT INTO billing.prices(id,merchant_id,product_id,amount,currency) VALUES($1,$2,$3,1000000,'USD')`, foreignPrice, foreignMerchant, foreignProduct)
	foreignPSP := dbtest.EnsureTestPSP(ctx, t, pool, foreignMerchant, "nmi")
	for _, entry := range []struct {
		id       uuid.UUID
		amount   int64
		original *uuid.UUID
	}{{foreignCharge, 1000000, nil}, {uuid.New(), -900000, &foreignCharge}} {
		exec(`INSERT INTO billing.payments(id,merchant_id,customer_id,price_id,rail,transaction_id,amount,list_amount,currency,status,money_movement,psp_id,refunded_payment_id)
		 VALUES($1,$2,$3,$4,'nmi',$5,$6,1000000,'USD','completed','rail',$7,$8)`, entry.id, foreignMerchant, alice, foreignPrice, entry.id.String(), entry.amount, foreignPSP, entry.original)
	}
	// Current storage rejects cross-customer/merchant links. Do not manufacture
	// historical exceptions by disabling the ownership constraint in this test.
	for _, bad := range []struct{ merchant, customer, price, psp uuid.UUID }{{mid.UUID(), bob, price, psp}, {foreignMerchant, alice, foreignPrice, foreignPSP}} {
		_, err := pool.Exec(ctx, `INSERT INTO billing.payments(id,merchant_id,customer_id,price_id,rail,transaction_id,amount,list_amount,currency,status,money_movement,psp_id,refunded_payment_id)
		 VALUES($1,$2,$3,$4,'nmi',$5,-100000,1000000,'USD','completed','rail',$6,$7)`, uuid.New(), bad.merchant, bad.customer, bad.price, uuid.NewString(), bad.psp, partial)
		require.ErrorContains(t, err, "payments_refunded_payment_id_fkey")
	}
	aliceRows := pages("alice")
	require.Len(t, aliceRows, 8)
	require.NotContains(t, aliceRows, openrails.PaymentID(bobCharge).String())
	require.NotContains(t, aliceRows, openrails.PaymentID(deleted).String())
	for _, tt := range []struct {
		id       uuid.UUID
		amount   int64
		status   string
		refunded bool
	}{
		{none, 0, "succeeded", false}, {partial, 300000, "partially_refunded", false}, {full, 1000000, "refunded", true},
	} {
		got := aliceRows[openrails.PaymentID(tt.id).String()]
		require.Equal(t, "charge", got.Object)
		require.Equal(t, tt.amount, got.AmountRefunded)
		require.Equal(t, tt.status, got.Status)
		require.Equal(t, tt.refunded, got.Refunded)
	}
	for _, id := range []uuid.UUID{firstPartial, fullRefund} {
		got := aliceRows[openrails.PaymentID(id).String()]
		require.Equal(t, "refund", got.Object)
		require.Negative(t, got.Amount)
		require.Zero(t, got.AmountRefunded)
		require.False(t, got.Refunded)
	}
	bobRows := pages("bob")
	require.Len(t, bobRows, 2)
	require.NotContains(t, bobRows, openrails.PaymentID(partial).String())
	require.Equal(t, int64(700000), bobRows[openrails.PaymentID(bobCharge).String()].AmountRefunded)

	// Even a caller supplying a foreign original ID cannot bypass either scope.
	database, err := db.NewWithPGXPool(pool, "billing")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	service := payments.NewPaymentService(database)
	totals, err := service.GetCustomerPaymentRefundTotals(merchant.WithID(ctx, mid), bob.String(), []uuid.UUID{partial, bobCharge})
	require.NoError(t, err)
	require.Equal(t, map[uuid.UUID]int64{bobCharge: 700000}, totals)
	totals, err = service.GetCustomerPaymentRefundTotals(merchant.WithID(ctx, merchant.ID(foreignMerchant)), alice.String(), []uuid.UUID{partial, full, foreignCharge})
	require.NoError(t, err)
	require.Equal(t, map[uuid.UUID]int64{foreignCharge: 900000}, totals)
}
