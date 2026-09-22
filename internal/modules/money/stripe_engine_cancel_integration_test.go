//go:build integration

package money_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

func TestStripeEngineCancellationUsesExecuteGates(t *testing.T) {
	for _, mode := range []string{"readonly", "limited", "full", "lost cancel"} {
		t.Run(mode, func(t *testing.T) {
			e := newNMIReceiptEnv(t)
			now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
			clock := clockwork.NewFakeClockAt(now)
			mid := dbtest.TestMerchantID.UUID()
			account := "stripe-engine-" + uuid.NewString()
			seedPSPSecrets(t, e.db, e.merchants, "stripe", account, map[string]string{"secret_key": "sk_test_synthetic"})
			var psp uuid.UUID
			require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT id FROM billing.psps WHERE merchant_id=$1 AND account_id=$2 AND rail='stripe'`, mid, account).Scan(&psp))
			_, err := e.pool.Exec(e.ctx, `UPDATE billing.payment_methods SET rail='stripe',psp_id=$2,rail_customer_ref='cus_engine',rail_method_ref='pm_engine',stored_credential_recurring_ref='pi_initial' WHERE id=$1`, e.method, psp)
			require.NoError(t, err)
			product, price, sub := uuid.New(), uuid.New(), uuid.New()
			_, err = e.pool.Exec(e.ctx, `INSERT INTO billing.products(id,merchant_id,key,display_name,entitlements_spec) VALUES($1,$2,$1::uuid::text,'Engine Stripe','{"engine_access":null}')`, product, mid)
			require.NoError(t, err)
			_, err = e.pool.Exec(e.ctx, `INSERT INTO billing.prices(id,merchant_id,product_id,amount,currency,auto_renew,access_duration_hours) VALUES($1,$2,$3,9990000,'USD',true,720)`, price, mid, product)
			require.NoError(t, err)
			_, err = e.pool.Exec(e.ctx, `INSERT INTO billing.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,payment_method_id,rail,collection_policy,status,current_period_starts_at,current_period_ends_at,entitlements_spec_snapshot) VALUES($1,$2,$3,$4,$5,$6,$7,'stripe','engine','active',$8,$9,'{"engine_access":null}')`, sub, mid, e.payer.UUID(), product, price, psp, e.method, now.Add(-30*24*time.Hour), now)
			require.NoError(t, err)
			var mu sync.Mutex
			var pi map[string]any
			posts, cancels := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == "POST" && r.URL.Path == "/v1/payment_intents":
					posts++
					require.NoError(t, r.ParseForm())
					require.Equal(t, "true", r.Form.Get("off_session"))
					require.Empty(t, r.Form.Get("setup_future_usage"))
					require.Equal(t, "999", r.Form.Get("amount"))
					meta := map[string]string{}
					for k, v := range r.Form {
						if strings.HasPrefix(k, "metadata[") {
							meta[strings.TrimSuffix(strings.TrimPrefix(k, "metadata["), "]")] = v[0]
						}
					}
					pi = map[string]any{"id": "pi_renewal", "status": "processing", "customer": "cus_engine", "payment_method": "pm_engine", "amount": 999, "amount_received": 999, "currency": "usd", "capture_method": "automatic", "confirmation_method": "automatic", "latest_charge": "ch_renewal", "metadata": meta, "livemode": false}
					if mode == "authentication" {
						pi["status"] = "requires_action"
					}
					if mode == "declined" {
						pi["status"] = "requires_payment_method"
						pi["last_payment_error"] = map[string]any{"code": "card_declined", "decline_code": "insufficient_funds"}
					}
					if mode == "lost_reply" {
						c, _, err := w.(http.Hijacker).Hijack()
						require.NoError(t, err)
						_ = c.Close()
						return
					}
					_ = json.NewEncoder(w).Encode(pi)
				case r.Method == "POST" && r.URL.Path == "/v1/payment_intents/pi_renewal/cancel":
					cancels++
					pi["status"] = "canceled"
					if mode == "lost cancel" {
						c, _, err := w.(http.Hijacker).Hijack()
						require.NoError(t, err)
						_ = c.Close()
						return
					}
					_ = json.NewEncoder(w).Encode(pi)
				case r.Method == "GET" && r.URL.Path == "/v1/payment_intents":
					_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{pi}, "has_more": false})
				case r.Method == "GET" && r.URL.Path == "/v1/payment_intents/pi_renewal":
					_ = json.NewEncoder(w).Encode(pi)
				case r.Method == "GET" && r.URL.Path == "/v1/charges/ch_renewal":
					charge := map[string]any{"id": "ch_renewal", "amount": 999, "amount_captured": 999, "currency": "usd", "customer": "cus_engine", "payment_method": "pm_engine", "payment_intent": "pi_renewal", "paid": true, "captured": true, "status": "succeeded"}
					if mode == "refunded" {
						charge["refunded"] = true
						charge["amount_refunded"] = 999
					}
					if mode == "partial_refund" {
						charge["amount_refunded"] = 100
					}
					if mode == "disputed" {
						charge["disputed"] = true
					}
					_ = json.NewEncoder(w).Encode(charge)
				default:
					t.Errorf("unexpected Stripe provider write/read %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected", 400)
				}
			}))
			defer server.Close()
			e.plane.Endpoints.StripeBaseURL = server.URL
			op, err := e.svc.AdmitDueSubscriptionCollection(e.ctx, sub, now)
			require.NoError(t, err)

			e.plane.Config.ProviderWriteMode = config.ProviderWriteModeFull
			h := money.NewSubscriptionCollectionHandler(e.db, e.plane, e.plane.Config, clock)
			runner := &intents.Runner{Store: intents.NewStore(e.db), Registry: intents.NewRegistry(h), Config: e.plane.Config, Clock: clock}
			op, err = runner.ExecuteByID(e.ctx, op.ID)
			require.NoError(t, err)
			require.Equal(t, intents.StatusUnknownNeedsVerify, op.Status)
			require.Equal(t, string(intents.OriginSystem), op.Origin)
			mu.Lock()
			require.Equal(t, 1, posts)
			require.Zero(t, cancels)
			pi["status"] = "requires_payment_method"
			pi["last_payment_error"] = map[string]any{"code": "card_declined", "decline_code": "insufficient_funds"}
			mu.Unlock()
			switch mode {
			case "readonly":
				e.plane.Config.ProviderWriteMode = config.ProviderWriteModeReadOnly
			case "limited":
				e.plane.Config.ProviderWriteMode = config.ProviderWriteModeLimited
			}
			blocked, reason := intents.GateExecution(e.plane.Config, intents.OriginSystem)
			if mode == "readonly" || mode == "limited" {
				require.True(t, blocked)
			}
			op, err = runner.VerifyByID(e.ctx, op.ID)
			require.NoError(t, err)
			mu.Lock()
			t.Logf("VERIFY mode=%s systemExecuteBlocked=%v reason=%q createPOST=%d cancelPOST=%d status=%s", mode, blocked, reason, posts, cancels, op.Status)
			require.Equal(t, 1, posts, "no fresh financial create")
			require.Zero(t, cancels, "Verify must never cancel")
			require.Equal(t, intents.StatusFailedRetryable, op.Status)
			mu.Unlock()
			op, err = runner.ExecuteByID(e.ctx, op.ID)
			require.NoError(t, err)
			mu.Lock()
			if mode == "full" {
				require.Equal(t, 1, cancels)
				require.Equal(t, intents.StatusFailedTerminal, op.Status)
			} else if mode == "lost cancel" {
				require.Equal(t, 1, cancels)
				require.Equal(t, intents.StatusUnknownNeedsVerify, op.Status)
			} else {
				require.Zero(t, cancels, "Execute obeys blocked mode/origin")
			}
			mu.Unlock()
			e.plane.Config.ProviderWriteMode = config.ProviderWriteModeFull
			op, err = runner.VerifyByID(e.ctx, op.ID)
			require.NoError(t, err)
			op, err = runner.ExecuteByID(e.ctx, op.ID)
			require.NoError(t, err)
			require.Equal(t, intents.StatusFailedTerminal, op.Status)
			_, err = runner.ExecuteByID(e.ctx, op.ID)
			require.NoError(t, err)
			_, err = runner.VerifyByID(e.ctx, op.ID)
			require.NoError(t, err)
			mu.Lock()
			require.Equal(t, 1, posts)
			require.Equal(t, 1, cancels, "terminal replay cannot cancel again")
			mu.Unlock()
		})
	}
}
