//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/testauth"
)

// The merchant refund boundary runs through real credentials, HTTP and the
// durable provider operation. Provider uncertainty details live in intents;
// this workflow retains distinct authority, amount and idempotency behavior.
func TestMerchantRefundAuthorityAndReplayWorkflow(t *testing.T) {
	ctx := t.Context()
	var calls atomic.Int64
	var amount atomic.Value
	wire := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/refund") {
			t.Errorf("unexpected refund fixture request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
			return
		}
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		amount.Store(string(body))
		fmt.Fprintf(w, `{"object":"transaction","id":"refund-%d","response":"1","response_text":"SUCCESS"}`, calls.Add(1))
	}))
	t.Cleanup(wire.Close)
	h := New(t, ctx)
	surface := h.StartStandalone("USD", WithConfig(func(c *config.Config) {
		c.MerchantConfigSource = config.MerchantConfigSourceAPI
		c.SecretBackend = config.SecretBackendDB
		c.ProviderWriteMode = config.ProviderWriteModeFull
		c.ProviderSandbox = &config.ProviderSandboxConfig{NMIGatewayURL: wire.URL}
	}))
	owned := surface.ProvisionOwnedMerchant("refund-" + uuid.NewString()[:8])
	client := surface.Client(openrails.WithMerchantID(owned.MerchantID), openrails.WithAPIKey(owned.APIKey))
	product, err := client.Products.Create(ctx, &openrails.ProductCreateParams{Key: "refundable", DisplayName: "Refundable access"})
	require.NoError(t, err)
	price, err := client.Prices.Create(ctx, &openrails.PriceCreateParams{ProductID: product.ID, UnitAmount: 10_000_000, Currency: "USD"})
	require.NoError(t, err)
	duration := 720
	monthly, err := client.Prices.Create(ctx, &openrails.PriceCreateParams{ProductID: product.ID, UnitAmount: 10_000_000, Currency: "USD", AccessDurationHours: &duration, AutoRenew: true})
	require.NoError(t, err)
	ccbillAccount := fmt.Sprintf("%06d-%04d", 100000+uuid.New().ID()%900000, uuid.New().ID()%10000)
	SeedPSPs(ctx, t, surface.App().Runtime, owned.MerchantID, config.PSPSet{
		"nmi":    {Rail: "nmi", AccountID: "refund-nmi-" + uuid.NewString(), NMI: &config.NMIRailConfig{SecurityKey: "synthetic-refund-key", WebhookSigningSecret: "synthetic-webhook"}},
		"ccbill": {Rail: "ccbill", AccountID: ccbillAccount, CCBill: &config.CCBillRailConfig{Salt: "synthetic", DataLinkUsername: "synthetic", DataLinkPassword: "synthetic"}},
	})
	pool := h.MerchantPool(owned.MerchantID.UUID())
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), `UPDATE billing.psps SET archived=true WHERE merchant_id=$1`, owned.MerchantID.UUID())
		require.NoError(t, err)
	})
	payment := func(rail string) openrails.PaymentID {
		t.Helper()
		customer := openrails.CustomerID(uuid.New())
		_, err := client.EnsureCustomer(ctx, customer)
		require.NoError(t, err)
		id := uuid.New()
		psp := dbtest.EnsureTestPSP(ctx, t, pool, owned.MerchantID.UUID(), rail)
		selectedPrice := sdkPriceID(t, price.ID).UUID()
		var subscription *uuid.UUID
		if rail == "ccbill" {
			sid := uuid.New()
			subscription = &sid
			selectedPrice = sdkPriceID(t, monthly.ID).UUID()
			_, err = pool.Exec(ctx, `INSERT INTO billing.subscriptions(id,merchant_id,customer_id,product_id,price_id,status,rail,rail_subscription_id,psp_id) VALUES($1,$2,$3,$4,$5,'active','ccbill',$6,$7)`, sid, owned.MerchantID.UUID(), customer.UUID(), sdkProductID(t, product.ID).UUID(), selectedPrice, "ccsub-"+sid.String(), psp)
			require.NoError(t, err)
		}
		_, err = pool.Exec(ctx, `INSERT INTO billing.payments(id,merchant_id,customer_id,price_id,rail,transaction_id,amount,list_amount,currency,status,money_movement,psp_id,subscription_id) VALUES($1,$2,$3,$4,$5,$6,10000000,10000000,'USD','completed','rail',$7,$8)`, id, owned.MerchantID.UUID(), customer.UUID(), selectedPrice, rail, "original-"+id.String(), psp, subscription)
		require.NoError(t, err)
		return openrails.PaymentID(id)
	}
	paid := payment("nmi")
	const validRefund = `{"amount":"4000000"}`
	refund := func(id openrails.PaymentID, key, token, body string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, surface.BaseURL+"/v1/merchant/payments/"+id.String()+"/refunds", bytes.NewBufferString(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", key)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		return res.StatusCode, raw
	}
	readOnly := surface.RegisterServiceJWTIssuer("refund-read-"+uuid.NewString()[:8], owned.MerchantSlug, []string{controlplane.PermMerchantPaymentsRead}).Token
	status, viewer, raw := mintKeyHTTP(t, surface.BaseURL, owned.APIKey, "refund-viewer", controlplane.MerchantRoleViewer)
	require.Equal(t, http.StatusCreated, status, string(raw))
	foreign := surface.ProvisionOwnedMerchant("refund-other-" + uuid.NewString()[:8])
	t.Run("manual access authority and deleted projection", func(t *testing.T) {
		customer := openrails.CustomerID(uuid.New())
		_, err := client.EnsureCustomer(ctx, customer)
		require.NoError(t, err)
		admin := surface.RegisterDelegatedIssuer("access-admin-"+uuid.NewString()[:8], owned.MerchantSlug).Mint(uuid.NewString(), "", "", []string{controlplane.PermMerchantCustomerSettingsUpdate})
		reader := surface.RegisterServiceJWTIssuer("access-read-"+uuid.NewString()[:8], owned.MerchantSlug, []string{controlplane.PermMerchantCustomerSettingsRead}).Token
		for _, row := range []struct{ path, body string }{{"product-access", fmt.Sprintf(`{"product_id":%q}`, product.ID)}, {"entitlements", `{"entitlement":"manual_access","hours":24}`}} {
			call := func(token string) (int, []byte) {
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, surface.BaseURL+"/v1/merchant/customers/"+customer.String()+"/"+row.path, strings.NewReader(row.body))
				require.NoError(t, err)
				require.NoError(t, testauth.Authorize(req, token))
				req.Header.Set("Content-Type", "application/json")
				res, err := http.DefaultClient.Do(req)
				require.NoError(t, err)
				defer res.Body.Close()
				raw, err := io.ReadAll(res.Body)
				require.NoError(t, err)
				return res.StatusCode, raw
			}
			for _, token := range []string{reader, viewer.Secret} {
				status, raw := call(token)
				require.Equal(t, http.StatusForbidden, status, string(raw))
			}
			status, raw := call(admin)
			require.Equal(t, http.StatusCreated, status, string(raw))
			if row.path == "entitlements" {
				var result struct {
					SourceType string     `json:"source_type"`
					SourceID   *uuid.UUID `json:"source_id"`
				}
				require.NoError(t, json.Unmarshal(raw, &result))
				require.Equal(t, "admin", result.SourceType)
				require.NotNil(t, result.SourceID)
				at := time.Now().UTC()
				active, err := client.HasEntitlement(ctx, customer, "manual_access", at)
				require.NoError(t, err)
				require.True(t, active)
				_, err = pool.Exec(ctx, `UPDATE billing.entitlements SET deleted_at=now() WHERE merchant_id=$1 AND customer_id=$2 AND entitlement='manual_access'`, owned.MerchantID.UUID(), customer.UUID())
				require.NoError(t, err)
				active, err = client.HasEntitlement(ctx, customer, "manual_access", at)
				require.NoError(t, err)
				require.False(t, active)
			}
		}
		require.Zero(t, calls.Load())
	})
	for _, row := range []struct {
		token string
		want  int
	}{{"", 401}, {readOnly, 403}, {viewer.Secret, 403}, {foreign.APIKey, 404}} {
		status, raw := refund(paid, uuid.NewString(), row.token, validRefund)
		require.Equal(t, row.want, status, string(raw))
		require.Zero(t, calls.Load())
	}
	for _, body := range []string{`{}`, `{"amount":"0"}`, `{"amount":"5000"}`} {
		status, raw := refund(paid, uuid.NewString(), owned.APIKey, body)
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		require.Zero(t, calls.Load(), "invalid native amounts never round into a provider refund")
	}
	key := uuid.NewString()
	for i, k := range []string{key, key, uuid.NewString()} {
		status, raw := refund(paid, k, owned.APIKey, validRefund)
		require.Equal(t, http.StatusCreated, status, string(raw))
		require.EqualValues(t, []int{1, 1, 2}[i], calls.Load())
	}
	require.Contains(t, amount.Load().(string), `"amount":4.00`, "exact native money reaches the provider once per authorized refund")
	got, err := client.GetPayment(ctx, paid)
	require.NoError(t, err)
	require.EqualValues(t, 8_000_000, got.AmountRefunded)
	require.Len(t, got.Refunds.Data, 2)
	var succeeded int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM billing.rail_intents WHERE payment_id=$1 AND intent_type='nmi_refund' AND status='succeeded'`, paid.UUID()).Scan(&succeeded))
	require.Equal(t, 2, succeeded)
	status, raw = refund(paid, uuid.NewString(), owned.APIKey, validRefund)
	require.Equal(t, 400, status, string(raw))
	require.EqualValues(t, 2, calls.Load(), "over-refund refuses before another provider call")

	// Unsupported CCBill refunds remain a manual portal operation regardless
	// of linked customer or whether DataLink credentials are configured.
	surface.App().Runtime.CCBillDataLinkEndpoint = wire.URL
	for _, configured := range []bool{true, false} {
		if !configured {
			for _, field := range []string{"datalink_username", "datalink_password"} {
				name, err := merchants.PSPSecretName("ccbill", "test", ccbillAccount, field)
				require.NoError(t, err)
				require.NoError(t, surface.App().Runtime.Merchants.Secrets().Delete(ctx, owned.MerchantID, name))
			}
		}
		id := payment("ccbill")
		status, raw := refund(id, uuid.NewString(), owned.APIKey, validRefund)
		require.Equal(t, 400, status, string(raw))
		require.Contains(t, string(raw), "automatic CCBill refunds are unavailable")
		var operations, refunds int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM billing.rail_intents WHERE payment_id=$1`, id.UUID()).Scan(&operations))
		require.Zero(t, operations)
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM billing.payments WHERE refunded_payment_id=$1`, id.UUID()).Scan(&refunds))
		require.Zero(t, refunds)
		var state string
		require.NoError(t, pool.QueryRow(ctx, `SELECT s.status FROM billing.subscriptions s JOIN billing.payments p ON p.subscription_id=s.id WHERE p.id=$1`, id.UUID()).Scan(&state))
		require.Equal(t, "active", state, "unsupported refund must not cancel a linked subscription")
	}
	require.EqualValues(t, 2, calls.Load(), "CCBill refusal never reaches any provider endpoint")
}
