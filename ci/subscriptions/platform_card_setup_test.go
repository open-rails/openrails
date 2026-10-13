//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/writeposture"
	"github.com/stretchr/testify/require"
)

func TestPlatformCardSetup(t *testing.T) {
	for _, scenario := range []string{"saved", "foreign application", "foreign tenant", "expired", "archived PSP", "writes disabled", "raw card", "declined", "conversion unavailable", "conversion response lost", "verification response lost", "concurrent"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			w, bt := platformSubscriptionWorld(t)
			c := w.newCustomer()
			const number = "4111111111111111"
			token := bt.collect(number)
			bt.mu.Lock()
			if scenario == "foreign application" {
				bt.applications[token] = "another-application"
			}
			bt.decline = scenario == "declined"
			bt.failConversion = scenario == "conversion unavailable"
			bt.loseConversionResponse = scenario == "conversion response lost"
			bt.loseSaleResponse = scenario == "verification response lost"
			if scenario == "foreign tenant" {
				bt.tenant = "another-tenant"
			}
			if scenario == "expired" {
				delete(bt.intents, token)
			}
			bt.mu.Unlock()
			if scenario == "archived PSP" {
				_, err := w.client[remote].UpdatePSP(t.Context(), w.psp["nmi"], billing.UpdatePSPParams{Archived: true, AllowLast: true})
				require.NoError(t, err)
			}
			if scenario == "writes disabled" {
				database, err := db.NewWithPGXPool(w.pool, w.schema)
				require.NoError(t, err)
				mid := w.client[remote].MerchantID()
				require.NoError(t, database.RunInMerchantScope(t.Context(), mid, "setup hold", func(ctx context.Context) error {
					return writeposture.Set(ctx, database.Gen(ctx), mid.UUID(), writeposture.ReadOnly, writeposture.ReasonOperator, "test", w.clock.Now())
				}))
			}
			req := map[string]any{"psp_id": w.psp["nmi"].String(), "token": token}
			if scenario == "raw card" {
				req["card"] = map[string]any{"number": number, "exp_month": 12, "exp_year": 2035, "cvc": "123"}
			}
			if scenario == "concurrent" {
				var group sync.WaitGroup
				for range 2 {
					group.Go(func() { c.call(http.MethodPost, "/payment-methods", "", req) })
				}
				group.Wait()
			}
			status, out := c.call(http.MethodPost, "/payment-methods", "", req)
			if scenario == "foreign application" || scenario == "foreign tenant" || scenario == "expired" || scenario == "archived PSP" || scenario == "writes disabled" || scenario == "raw card" || scenario == "declined" {
				require.GreaterOrEqual(t, status, 400, "%v", out)
				var count int
				require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.payment_methods WHERE customer_id = $1`), c.id).Scan(&count))
				require.Zero(t, count)
				bt.mu.Lock()
				require.Empty(t, bt.verifications)
				require.Empty(t, bt.tokens)
				bt.mu.Unlock()
				return
			}
			if scenario == "conversion unavailable" || scenario == "conversion response lost" || scenario == "verification response lost" {
				require.GreaterOrEqual(t, status, 400, "%v", out)
				if scenario == "conversion response lost" {
					stranger := w.newCustomer()
					status, _ := stranger.call(http.MethodPost, "/payment-methods", "", req)
					require.Equal(t, http.StatusConflict, status, "remote recovery must also check the customer")
				}
				bt.mu.Lock()
				bt.failConversion = false
				if scenario == "conversion response lost" {
					delete(bt.intents, token) // Recovery cannot depend on a live collection.
					clear(bt.byKey)           // Nor on the provider's temporary replay cache.
				}
				bt.mu.Unlock()
				status, out = c.call(http.MethodPost, "/payment-methods", "", req)
			}
			require.Equal(t, http.StatusCreated, status, "%v", out)
			saved := unwrap(out)
			id := saved["id"].(string)
			require.Equal(t, "active", saved["status"])
			require.Empty(t, bt.salesOf(number), "setup must not take money")
			var methods, agreements, otherAgreements, orders, defaults int
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.payment_methods WHERE customer_id = $1`), c.id).Scan(&methods))
			require.Equal(t, 1, methods)
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FILTER (WHERE kind = 'card_on_file'), count(*) FILTER (WHERE kind <> 'card_on_file') FROM billing.mandates WHERE customer_id = $1`), c.id).Scan(&agreements, &otherAgreements))
			require.Equal(t, 1, agreements)
			require.Zero(t, otherAgreements)
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.orders WHERE customer_id = $1`), c.id).Scan(&orders))
			require.Zero(t, orders)
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.money_settings WHERE customer_id = $1 AND default_payment_method_id IS NOT NULL`), c.id).Scan(&defaults))
			require.Zero(t, defaults)
			bt.mu.Lock()
			verified := len(bt.verifications)
			require.NotZero(t, verified)
			require.NotEmpty(t, bt.verifications[0].CVV)
			bt.mu.Unlock()
			replay := unwrap(c.must(http.MethodPost, "/payment-methods", "", req))
			require.Equal(t, id, replay["id"])
			bt.mu.Lock()
			require.Len(t, bt.verifications, verified, "replay does not verify again")
			bt.mu.Unlock()
			stranger := w.newCustomer()
			status, _ = stranger.call(http.MethodPost, "/payment-methods", "", req)
			require.Equal(t, http.StatusConflict, status)
			if scenario == "saved" {
				c.must(http.MethodPatch, "/payment-methods/"+id, "", map[string]any{"reusable": false})
				c.must(http.MethodPost, "/payment-methods", "", req)
				var live int
				require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.mandates WHERE customer_id = $1 AND status = 'active'`), c.id).Scan(&live))
				require.Zero(t, live, "replay cannot restore revoked permission")
				// Exercise replay of a removed row; platform card deletion is a separate flow.
				_, err := w.pool.Exec(t.Context(), w.q(`UPDATE billing.payment_methods SET status = 'removed' WHERE customer_id = $1`), c.id)
				require.NoError(t, err)
				status, _ = c.call(http.MethodPost, "/payment-methods", "", req)
				require.GreaterOrEqual(t, status, 400)
			}
		})
	}
}

func TestPlatformSetupThenSubscriptionReplacement(t *testing.T) {
	t.Parallel()
	w, bt := platformSubscriptionWorld(t)
	c := w.newCustomer()
	const original, replacement = "4111111111111111", "4242424242424242"
	paid := c.mustPurchase(remote, order{price: w.membership("platform:setup", 9_990_000).ID, rail: "nmi", token: bt.collect(original)})
	subID := *paid.SubscriptionID
	before := w.subscriptionMandate(remote, c.cid(), subID)
	saved := unwrap(c.must(http.MethodPost, "/payment-methods", "", map[string]any{"psp_id": w.psp["nmi"].String(), "token": bt.collect(replacement)}))
	unchanged := w.subscriptionMandate(remote, c.cid(), subID)
	require.Equal(t, before.ID, unchanged.ID, "saving alone does not move the subscription")
	require.Empty(t, bt.salesOf(replacement))
	c.must(http.MethodPut, "/subscriptions/"+subID.String()+"/payment-method", "", map[string]any{"payment_method_id": saved["id"]})
	moved := w.subscriptionMandate(remote, c.cid(), subID)
	require.NotEqual(t, before.ID, moved.ID)
	end := *w.subscription(remote, subID).CurrentPeriodEndsAt
	w.advanceHealthyTo(end)
	w.runRenewals()
	w.until(func() bool { return w.subscription(remote, subID).CurrentPeriodEndsAt.After(end) }, "the newly added card renews the existing subscription")
	require.Len(t, bt.salesOf(original), 1)
	renewal := bt.salesOf(replacement)
	require.Len(t, renewal, 1)
	require.Equal(t, *moved.InitialTransactionID, renewal[0].InitialTxnID)
	require.Equal(t, billing.SubscriptionActive, w.subscription(remote, subID).Status)
}
