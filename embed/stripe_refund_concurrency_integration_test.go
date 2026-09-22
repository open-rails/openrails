//go:build integration

package embed_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/httptesthost"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/productaccess"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Stripe CLI delivers refund.created and refund.updated independently.
// Different signed event IDs must both commit, even when they revoke one grant.
func TestStripeRefundDeliveriesConcurrentlyRevokeOnePurchase(t *testing.T) {
	ctx := t.Context()
	dsn := dbtest.SharedSuperuserDSN(t)
	slug := "refund-race-" + uuid.NewString()
	secret := "whsec_refund_concurrency"
	accountID := "acct_" + uuid.NewString()

	rt, mid, err := newDeclaredMerchant(ctx, embed.Options{Config: &config.Config{
		Env: "development", TestMode: config.CredentialPostureSandbox,
		MerchantConfigSource: config.MerchantConfigSourceManifest,
		AllowCatalogUpdates:  true,
		ProviderWriteMode:    config.ProviderWriteModeReadOnly,
		DB:                   &config.DBConfig{URL: dsn},
	}}, slug, embed.MerchantConfig{PSPs: map[string]embed.PSPConfig{
		"stripe": {"stripe": {AccountID: accountID, Secrets: map[string]string{"secret_key": "sk_test_fixture", "webhook_signing_secret": secret}}},
	}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rt.Close(context.Background())) })
	require.NoError(t, err)
	ctx = merchant.WithID(ctx, mid)
	database := app.HostGraph(rt).Runtime.DB
	pool := database.Pool()
	user := uuid.NewString()
	customer := dbtest.EnsureCustomerIDPgxFor(ctx, t, pool, mid.UUID(), user)
	product, price, payment := uuid.New(), uuid.New(), uuid.New()
	var psp uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM billing.psps WHERE merchant_id=$1 AND rail='stripe'`, mid.UUID()).Scan(&psp))
	_, err = pool.Exec(ctx, `INSERT INTO billing.products(id,merchant_id,key,display_name) VALUES($1,$2,$3,'Refund race')`, product, mid.UUID(), uuid.NewString())
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO billing.prices(id,merchant_id,product_id,amount,currency,auto_renew) VALUES($1,$2,$3,1000000,'USD',false)`, price, mid.UUID(), product)
	require.NoError(t, err)
	charge := "ch_" + uuid.NewString()
	_, err = pool.Exec(ctx, `INSERT INTO billing.payments(id,merchant_id,customer_id,price_id,rail,psp_id,transaction_id,amount,list_amount,currency,status,money_movement,purchased_at) VALUES($1,$2,$3,$4,'stripe',$5,$6,1000000,1000000,'USD','completed','rail',now())`, payment, mid.UUID(), customer, price, psp, charge)
	require.NoError(t, err)
	access := productaccess.NewService(database)
	grant, _, err := access.GrantProductAccess(ctx, productaccess.GrantParams{UserID: user, ProductID: product, PaymentID: &payment, SourceType: models.ProductAccessSourcePurchase, SourceID: payment.String()})
	require.NoError(t, err)
	handler, err := httptesthost.Handler(rt, httptesthost.Options{HTTP: embed.HTTPConfig{}})
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	refund := "re_" + uuid.NewString()
	type response struct {
		status int
		body   string
		err    error
	}
	start := make(chan struct{})
	results := make(chan response, 16)
	for i := range 16 {
		eventType := "refund.created"
		if i%2 == 1 {
			eventType = "refund.updated"
		}
		payload, err := json.Marshal(map[string]any{"id": "evt_" + uuid.NewString(), "type": eventType, "created": time.Now().Unix(), "data": map[string]any{"object": map[string]any{"id": refund, "object": "refund", "charge": charge, "amount": 100, "currency": "usd", "status": "succeeded"}}})
		require.NoError(t, err)
		go func() {
			<-start
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/merchants/"+slug+"/webhooks/stripe/"+accountID, bytes.NewReader(payload))
			if err != nil {
				results <- response{err: err}
				return
			}
			timestamp := strconv.FormatInt(time.Now().Unix(), 10)
			mac := hmac.New(sha256.New, []byte(secret))
			fmt.Fprintf(mac, "%s.", timestamp)
			mac.Write(payload)
			req.Header.Set("Stripe-Signature", "t="+timestamp+",v1="+hex.EncodeToString(mac.Sum(nil)))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				results <- response{err: err}
				return
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			results <- response{status: resp.StatusCode, body: string(body), err: err}
		}()
	}
	close(start)
	for range 16 {
		got := <-results
		require.NoError(t, got.err)
		assert.Equal(t, http.StatusOK, got.status, got.body)
	}
	var terminations, refunds int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM billing.grants WHERE supersedes_id=$1`, grant.ID).Scan(&terminations))
	require.Equal(t, 1, terminations)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM billing.payments WHERE merchant_id=$1 AND transaction_id=$2`, mid.UUID(), refund).Scan(&refunds))
	require.Equal(t, 1, refunds)
	ledger := payments.NewPaymentService(database)
	replay, err := ledger.Refund(ctx, payment, refund, 1000000, payments.ReversalRefund)
	require.NoError(t, err, "exact replay stays valid after the full amount has been refunded")
	require.Equal(t, int64(-1000000), replay.Amount)
	_, err = ledger.Refund(ctx, payment, refund, 500000, payments.ReversalRefund)
	require.ErrorContains(t, err, "different payment facts")
	_, err = ledger.Refund(ctx, payment, "re_different", 1, payments.ReversalRefund)
	require.ErrorContains(t, err, "exceed original payment")
	has, err := access.HasProductAccess(ctx, user, product)
	require.NoError(t, err)
	require.False(t, has)
}
