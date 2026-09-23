//go:build integration

package embed_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

// Payment rows name the product they bought, so hosts can label one-time
// charges and their refunds without another lookup.
func TestPaymentsCarryProductSummary(t *testing.T) {
	ctx := t.Context()
	_, pool, dsn := scopeWithoutRLSDatabase(t)
	buyer := uuid.New()
	authn := billingauth.AuthenticationFunc(func(_ context.Context, r *http.Request) (billingauth.Identity, error) {
		if r.Header.Get("Authorization") != "Bearer buyer" {
			return billingauth.Identity{}, billingauth.ErrUnauthenticated
		}
		return billingauth.Identity{Kind: billingauth.NativeUser, SubjectID: buyer.String(), CustomerID: buyer.String(), Issuer: "payment-product-fixture", CredentialClass: billingauth.CredentialClassUserSession}, nil
	})
	slug := "payment-product-" + uuid.NewString()
	runtime, mid, err := newDeclaredMerchant(ctx, embed.Options{
		Auth: &billingauth.Integration{Authentication: authn},
		HTTP: &embed.HTTPConfig{CustomerRoutes: []embed.CustomerRoutesConfig{{Merchant: slug, Scope: embed.CustomerBillingManagement}}},
		Config: &config.Config{TestMode: config.CredentialPostureSandbox,
			MerchantConfigHTTP: true, SecretBackend: config.SecretBackendDB, Encryption: &config.EncryptionConfig{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="},
			ProviderWriteMode: config.ProviderWriteModeReadOnly, DB: &config.DBConfig{URL: dsn}},
		PGXPool: pool, River: embed.RiverFromHost(),
	}, slug, embed.MerchantConfig{DisplayName: "Payment products"})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	routes, err := openrailshttp.Routes(runtime)
	require.NoError(t, err)
	mux := http.NewServeMux()
	require.NoError(t, routes.Mount(mux, "/billing"))
	exec := func(query string, args ...any) { _, err := pool.Exec(ctx, query, args...); require.NoError(t, err) }

	exec(`INSERT INTO billing.customers(id,merchant_id) VALUES($1,$2)`, buyer, mid.UUID())
	product, price := uuid.New(), uuid.New()
	key := "post:" + uuid.NewString()
	exec(`INSERT INTO billing.products(id,merchant_id,key,display_name,description) VALUES($1,$2,$3,'Sunset timelapse','Behind the scenes')`, product, mid.UUID(), key)
	exec(`INSERT INTO billing.prices(id,merchant_id,product_id,amount,currency) VALUES($1,$2,$3,1000000,'USD')`, price, mid.UUID(), product)
	psp := dbtest.EnsureTestPSP(ctx, t, pool, mid.UUID(), "nmi")
	charge, refund := uuid.New(), uuid.New()
	at := time.Now().UTC().Add(-time.Hour)
	exec(`INSERT INTO billing.payments(id,merchant_id,customer_id,price_id,rail,transaction_id,amount,list_amount,currency,status,money_movement,psp_id,created_at,purchased_at)
		VALUES($1,$2,$3,$4,'nmi',$5,1000000,1000000,'USD','completed','rail',$6,$7,$7)`, charge, mid.UUID(), buyer, price, charge.String(), psp, at)
	exec(`INSERT INTO billing.payments(id,merchant_id,customer_id,price_id,rail,transaction_id,amount,list_amount,currency,status,money_movement,psp_id,refunded_payment_id,created_at,purchased_at)
		VALUES($1,$2,$3,$4,'nmi',$5,-400000,1000000,'USD','completed','rail',$6,$7,$8,$8)`, refund, mid.UUID(), buyer, price, refund.String(), psp, charge, at.Add(time.Minute))
	want := &openrails.ProductSummary{ID: openrails.ProductID(product).String(), Key: key, DisplayName: "Sunset timelapse", Description: "Behind the scenes"}

	request := httptest.NewRequest(http.MethodGet, "/billing/v1/me/payments?limit=10", nil)
	request.Header.Set("Authorization", "Bearer buyer")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var page struct {
		Data []struct {
			ID      string                    `json:"id"`
			Object  string                    `json:"object"`
			Product *openrails.ProductSummary `json:"product"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
	require.Len(t, page.Data, 2)
	for _, row := range page.Data {
		require.Equal(t, want, row.Product, "customer %s rows name the product", row.Object)
	}

	client, err := runtime.Client()
	require.NoError(t, err)
	got, err := client.GetPayment(ctx, openrails.PaymentID(charge))
	require.NoError(t, err)
	require.Equal(t, want, got.Product)
	require.Len(t, got.Refunds.Data, 1)
	require.Equal(t, want, got.Refunds.Data[0].Product, "a refund names the product of the charge it reverses")
	list, err := client.ListPayments(ctx, openrails.PaymentFilter{CustomerID: buyer.String()})
	require.NoError(t, err)
	require.Len(t, list.Data, 2)
	for _, row := range list.Data {
		require.Equal(t, want, row.Product)
	}
}
