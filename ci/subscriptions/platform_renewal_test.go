//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/vaulttest"
	"github.com/stretchr/testify/require"
)

func platformSubscriptionWorld(t *testing.T) (*world, *btFake) {
	t.Helper()
	bt, endpoint := newBTFake(t)
	bt.tenant, bt.application = platformTenant, uuid.NewString()
	w := prepareWorld(t, 12)
	gateway := httptest.NewServer(w.nmi)
	t.Cleanup(gateway.Close)
	w.cfg = func(cfg *config.Config) {
		withPlatformVault(cfg)
		cfg.ProviderSandbox = &config.ProviderSandboxConfig{NMIGatewayURL: gateway.URL}
	}
	w.vault = vaulttest.New(t)
	w.booted = func(e *openrails.Engine) {
		rt := engine.Graph(e).Runtime
		rt.RailPaymentMethodService.BTBaseURLOverride = endpoint
		custody := rt.PurchaseService.CustodianSaleService
		custody.BTBaseURLOverride, custody.GatewayDirectPostURLOverride = endpoint, endpoint+"/nmi"
		resolver := rt.CollectionResolver.(*money.MerchantCollectionAdapterBuilder)
		resolver.Endpoints.BTBaseURL, resolver.Endpoints.NMIDirectPostURL = endpoint, endpoint+"/nmi"
	}
	w.start()
	client := w.client[remote]
	_, err := client.UpdatePSP(t.Context(), w.psp["stripe"], billing.UpdatePSPParams{Archived: true, AllowLast: true})
	require.NoError(t, err)
	_, err = client.UpdateMerchantConfiguration(t.Context(), setVault(billing.CardVaultPlatform))
	require.NoError(t, err)
	mid := client.MerchantID().UUID().String()
	w.vault.Put(t, "openrails/platform_vault/"+platformTenant, map[string]any{
		"api_key": "platform-private-test", "collection_applications": map[string]any{
			mid: map[string]any{"id": bt.application, "public_api_key": "platform-public-test"},
		},
	})
	keyed := w.nmi.AddVault(nmimock.Card{Brand: "visa", Last4: "1111"})
	bt.gateway = func(form url.Values, _ string) string {
		return w.nmi.AddSale(nmimock.Sale{Vault: keyed, OrderID: form.Get("orderid"), OrderDescription: form.Get("order_description"), Amount: form.Get("amount"), Currency: form.Get("currency"),
			InitiatedBy: form.Get("initiated_by"), Indicator: form.Get("stored_credential_indicator"), Initial: form.Get("initial_transaction_id")}).TransactionID
	}
	return w, bt
}

func TestPlatformSubscriptionLifecycle(t *testing.T) {
	for _, saved := range []bool{false, true} {
		name := "new card"
		if saved {
			name = "saved card"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w, bt := platformSubscriptionWorld(t)
			c := w.newCustomer()
			const number, replacement = "4111111111111111", "4242424242424242"
			price := w.membership("platform:member", 9_990_000)
			o := order{price: price.ID, rail: "nmi"}
			if saved {
				first := c.mustPurchase(remote, order{price: w.permanent("platform:once").ID, rail: "nmi", token: bt.collect(number)})
				o.method = first.Order.Payment.PaymentMethodID.String()
			} else {
				o.token = bt.collect(number)
			}
			paid := c.mustPurchase(remote, o)
			require.NotNil(t, paid.SubscriptionID)
			subID := *paid.SubscriptionID
			require.True(t, c.entitled("platform:member"))
			recurring := w.subscriptionMandate(remote, c.cid(), subID)
			sales := bt.salesOf(number)
			require.Equal(t, "stored", sales[len(sales)-1].Indicator)
			require.Equal(t, sales[len(sales)-1].TransactionID, *recurring.InitialTransactionID)
			end := *w.subscription(remote, subID).CurrentPeriodEndsAt
			w.advanceHealthyTo(end)
			w.runRenewals()
			w.until(func() bool { return w.subscription(remote, subID).CurrentPeriodEndsAt.After(end) }, "the purchased platform subscription renews")
			renewal := bt.salesOf(number)
			require.Len(t, renewal, len(sales)+1)
			require.Equal(t, *recurring.InitialTransactionID, renewal[len(renewal)-1].InitialTxnID)

			other := c.mustPurchase(remote, order{price: w.permanent("platform:other").ID, rail: "nmi", token: bt.collect(replacement)})
			c.must(http.MethodPut, "/subscriptions/"+subID.String()+"/payment-method", "", map[string]any{"payment_method_id": other.Order.Payment.PaymentMethodID.String()})
			moved := w.subscriptionMandate(remote, c.cid(), subID)
			require.NotEqual(t, *recurring.InitialTransactionID, *moved.InitialTransactionID)
			bt.mu.Lock()
			require.Len(t, bt.verifications, 1)
			require.Equal(t, "customer", bt.verifications[0].InitiatedBy)
			require.Equal(t, "stored", bt.verifications[0].Indicator)
			require.Empty(t, bt.verifications[0].CVV)
			bt.mu.Unlock()
			end = *w.subscription(remote, subID).CurrentPeriodEndsAt
			w.advanceHealthyTo(end)
			w.runRenewals()
			w.until(func() bool { return w.subscription(remote, subID).CurrentPeriodEndsAt.After(end) }, "the replacement card renews")
			require.Len(t, bt.salesOf(number), len(renewal), "the previous card is not charged again")
			changed := bt.salesOf(replacement)
			require.Equal(t, *moved.InitialTransactionID, changed[len(changed)-1].InitialTxnID)
			c.must(http.MethodPost, "/subscriptions/"+subID.String()+"/cancel", "", map[string]any{"reason": "done"})
			require.Equal(t, billing.SubscriptionCanceled, w.subscription(remote, subID).Status)
			c.must(http.MethodPost, "/subscriptions/"+subID.String()+"/resume", "", nil)
			require.Equal(t, billing.SubscriptionActive, w.subscription(remote, subID).Status)
			c.must(http.MethodPost, "/subscriptions/"+subID.String()+"/cancel", "", map[string]any{"reason": "done"})
			end = *w.subscription(remote, subID).CurrentPeriodEndsAt
			w.advanceHealthyTo(end)
			w.runRenewals()
			require.Len(t, bt.salesOf(replacement), len(changed), "cancellation stops collection")
		})
	}
}

func TestPlatformSubscriptionCardRecovery(t *testing.T) {
	for _, scenario := range []string{"recovered", "bounded grace", "expired intent", "canceled", "replaced", "replaced overdue"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			w, bt := platformSubscriptionWorld(t)
			c := w.newCustomer()
			if scenario == "bounded grace" || scenario == "replaced" {
				require.NoError(t, w.applySettings(t.Context(), billing.MerchantSettings{DunningPolicy: &billing.DunningPolicy{AccessWhileRenewalHeld: billing.DunningAccessSuspend}}))
			}
			// Have another saved card available before the original storage fails.
			other := c.mustPurchase(remote, order{price: w.permanent("platform:other").ID, rail: "nmi", token: bt.collect("4242424242424242")})
			bt.mu.Lock()
			bt.failConversion = true
			bt.mu.Unlock()
			const number = "4111111111111111"
			o := order{price: w.membership("platform:recovery", 9_990_000).ID, rail: "nmi", token: bt.collect(number)}
			sale, err := c.sell(remote, o)
			require.NoError(t, err)
			_, err = sale.buy(c, o)
			require.NoError(t, err)
			paid := sale.read(c)
			require.Equal(t, "succeeded", paid.Status, "card storage does not withhold a paid order")
			require.NotNil(t, paid.SubscriptionID)
			subID := *paid.SubscriptionID
			sub := w.subscription(remote, subID)
			require.Equal(t, billing.SubscriptionAwaitingMethod, sub.Status)
			require.True(t, c.entitled("platform:recovery"))
			require.Len(t, completed(w.payments(remote, c.id)), 2)
			var operation uuid.UUID
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT id FROM billing.provider_intents WHERE payload->>'order_id' = $1 AND intent_type = 'nmi_sale'`), sale.order.ID.UUID().String()).Scan(&operation))
			if scenario == "canceled" {
				c.must(http.MethodPost, "/subscriptions/"+subID.String()+"/cancel", "", map[string]any{"reason": "done"})
			}
			if scenario == "replaced overdue" {
				w.advanceHealthyTo(*sub.CurrentPeriodEndsAt)
			}
			if scenario == "replaced" || scenario == "replaced overdue" {
				c.must(http.MethodPut, "/subscriptions/"+subID.String()+"/payment-method", "", map[string]any{"payment_method_id": other.Order.Payment.PaymentMethodID.String()})
			}
			bt.mu.Lock()
			bt.failConversion = false
			if scenario == "expired intent" {
				delete(bt.intents, o.token)
			}
			bt.mu.Unlock()
			ctx := merchant.WithID(t.Context(), w.client[remote].MerchantID())
			runner := engine.Graph(w.rt).Runtime.IntentRunner()
			row, err := runner.VerifyByID(ctx, operation)
			require.NoError(t, err)
			require.Equal(t, intents.StatusSucceeded, row.Status)
			require.Len(t, bt.salesOf(number), 1, "storage recovery never charges again")
			require.Len(t, completed(w.payments(remote, c.id)), 2)
			sub = w.subscription(remote, subID)
			switch scenario {
			case "bounded grace":
				require.Equal(t, billing.SubscriptionActive, sub.Status)
				require.True(t, c.entitled("platform:recovery"), "storage recovery preserves paid access")
				// No due pass: the renewal stays held beyond its allowed grace.
				w.advance(sub.CurrentPeriodEndsAt.Add(graceFor(monthHours) + time.Second).Sub(w.clock.Now()))
				require.False(t, c.entitled("platform:recovery"), "recovery replaces waiting-for-card grace with the bounded renewal policy")
			case "recovered":
				require.Equal(t, billing.SubscriptionActive, sub.Status)
				mandate := w.subscriptionMandate(remote, c.cid(), subID)
				require.Equal(t, bt.salesOf(number)[0].TransactionID, *mandate.InitialTransactionID)
				w.advanceHealthyTo(*sub.CurrentPeriodEndsAt)
				w.runRenewals()
				w.until(func() bool { return w.subscription(remote, subID).CurrentPeriodEndsAt.After(*sub.CurrentPeriodEndsAt) }, "recovered original card renews")
				require.Len(t, bt.salesOf(number), 2)
			case "canceled":
				require.Equal(t, billing.SubscriptionCanceled, sub.Status)
			case "replaced":
				mandate := w.subscriptionMandate(remote, c.cid(), subID)
				require.Equal(t, other.Order.Payment.PaymentMethodID, mandate.PaymentMethodID)
				require.Equal(t, billing.SubscriptionActive, sub.Status, "a replacement card completes a paid enrollment")
				require.True(t, c.entitled("platform:recovery"))
				w.runRenewals()
				require.Len(t, completed(w.payments(remote, c.id)), 2, "the paid period is not charged again")
				w.advance(sub.CurrentPeriodEndsAt.Add(graceFor(monthHours) + time.Second).Sub(w.clock.Now()))
				require.False(t, c.entitled("platform:recovery"), "replacement restores the bounded renewal grace policy")
			case "replaced overdue":
				require.Equal(t, billing.SubscriptionPastDue, sub.Status)
				w.runRenewals()
				w.until(func() bool { return w.subscription(remote, subID).CurrentPeriodEndsAt.After(*sub.CurrentPeriodEndsAt) }, "the replacement card pays the due renewal")
				require.Len(t, completed(w.payments(remote, c.id)), 3)
				require.Len(t, bt.salesOf(number), 1, "the recovered original card stays unused")
			case "expired intent":
				require.Equal(t, billing.SubscriptionAwaitingMethod, sub.Status)
				w.advanceHealthyTo(*sub.CurrentPeriodEndsAt)
				w.runRenewals()
				require.Len(t, bt.salesOf(number), 1)
			}
		})
	}
}

func TestPlatformRenewal(t *testing.T) {
	for _, scenario := range []string{"paid", "response lost", "hidden response", "declined", "foreign container", "revoked agreement", "revoked before submission", "account archived", "custody switched"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			w, bt := platformSubscriptionWorld(t)
			c := w.newCustomer()
			const number = "4111111111111111"
			paid := c.mustPurchase(remote, order{price: w.membership("platform:renewal", 9_990_000).ID, rail: "nmi", token: bt.collect(number)})
			subID := *paid.SubscriptionID
			initial := w.subscriptionMandate(remote, c.cid(), subID).InitialTransactionID
			bt.mu.Lock()
			bt.decline = scenario == "declined"
			bt.loseSaleResponse = scenario == "response lost" || scenario == "hidden response"
			if scenario == "foreign container" {
				for token := range bt.tokens {
					bt.containers[token] = []string{"/merchants/another/"}
				}
			}
			revoke := func() {
				_, err := w.pool.Exec(t.Context(), w.q(`UPDATE billing.mandates SET status = 'revoked', ended_at = now(), end_reason = 'customer_revoked' WHERE subscription_id = $1`), subID.UUID())
				if err != nil {
					t.Error(err)
				}
			}
			if scenario == "revoked before submission" {
				bt.beforeTokenRead = revoke
			}
			bt.mu.Unlock()
			if scenario == "revoked agreement" {
				revoke()
			}
			if scenario == "hidden response" {
				w.nmi.HideSales(1)
			}
			if scenario == "account archived" {
				_, err := w.client[remote].UpdatePSP(t.Context(), w.psp["nmi"], billing.UpdatePSPParams{Archived: true, AllowLast: true})
				require.NoError(t, err)
			}
			if scenario == "custody switched" {
				_, err := w.client[remote].UpdateMerchantConfiguration(t.Context(), setVault("nmi"))
				require.NoError(t, err)
			}
			end := *w.subscription(remote, subID).CurrentPeriodEndsAt
			w.advanceHealthyTo(end)
			w.runRenewals()
			switch scenario {
			case "paid", "response lost", "custody switched":
				w.until(func() bool { return w.subscription(remote, subID).CurrentPeriodEndsAt.After(end) }, "platform renewal settles from the gateway receipt")
				w.runRenewals()
				sales := bt.salesOf(number)
				require.Len(t, sales, 2)
				require.Equal(t, "merchant", sales[1].InitiatedBy)
				require.Equal(t, "used", sales[1].Indicator)
				require.Equal(t, *initial, sales[1].InitialTxnID)
				require.Empty(t, sales[1].CVV)
				require.True(t, c.entitled("platform:renewal"))
				require.Len(t, completed(w.payments(remote, c.id)), 2)
			case "hidden response":
				w.runRenewals()
				require.True(t, end.Equal(*w.subscription(remote, subID).CurrentPeriodEndsAt))
				require.Len(t, bt.salesOf(number), 2, "an uncertain charge blocks another attempt")
				require.Len(t, completed(w.payments(remote, c.id)), 1)
			default:
				sub := w.subscription(remote, subID)
				require.True(t, end.Equal(*sub.CurrentPeriodEndsAt))
				if scenario == "declined" {
					require.Len(t, bt.salesOf(number), 2)
					require.Equal(t, billing.SubscriptionPastDue, sub.Status)
				} else {
					require.Len(t, bt.salesOf(number), 1)
					if scenario == "account archived" {
						require.Equal(t, billing.SubscriptionActive, sub.Status, "the operator must restore a charging account; the customer's card is valid")
					}
				}
			}
		})
	}
}

func TestPlatformRenewalChargeAccounts(t *testing.T) {
	t.Parallel()
	w, bt := platformSubscriptionWorld(t)
	c := w.newCustomer()
	const number = "4111111111111111"
	paid := c.mustPurchase(remote, order{price: w.membership("platform:routing", 9_990_000).ID, rail: "nmi", token: bt.collect(number)})
	subID := *paid.SubscriptionID
	original := w.subscriptionMandate(remote, c.cid(), subID)
	client := w.client[remote]
	second, err := client.CreatePSP(t.Context(), billing.CreatePSPParams{
		Key: "second", Rail: "nmi", Environment: "sandbox", PSPAccount: billing.PSPAccount{GatewayID: "second-account"},
		Credentials: map[string]string{"security_key": "second-account-key", "webhook_signing_secret": "second-webhook"},
	})
	require.NoError(t, err)
	_, err = client.UpdateMerchantConfiguration(t.Context(), setVault(billing.CardVaultPlatform, "second"))
	require.NoError(t, err)

	end := *w.subscription(remote, subID).CurrentPeriodEndsAt
	w.advanceHealthyTo(end)
	w.runRenewals()
	require.Len(t, bt.salesOf(number), 1, "neither the excluded account nor a different NMI account may collect")
	sub := w.subscription(remote, subID)
	require.Equal(t, billing.SubscriptionActive, sub.Status, "an account restriction is not a bad card")
	require.True(t, end.Equal(*sub.CurrentPeriodEndsAt))
	require.Equal(t, original.ID, w.subscriptionMandate(remote, c.cid(), subID).ID)

	_, err = client.UpdateMerchantConfiguration(t.Context(), setVault(billing.CardVaultPlatform, "second", "nmi"))
	require.NoError(t, err)
	w.runRenewals()
	w.until(func() bool { return w.subscription(remote, subID).CurrentPeriodEndsAt.After(end) }, "restoring the eligible account resumes collection")
	sales := bt.salesOf(number)
	require.Len(t, sales, 2)
	require.Equal(t, "e2e-nmi-key", sales[1].SecurityKey)
	require.Equal(t, *original.InitialTransactionID, sales[1].InitialTxnID)
	for _, payment := range completed(w.payments(remote, c.id)) {
		require.NotNil(t, payment.PSPID)
		require.NotEqual(t, second.ID, *payment.PSPID)
	}
}

func TestPlatformRenewalKeepsAcceptedAccount(t *testing.T) {
	for _, scenario := range []string{"configuration changes before submission", "configuration changes after lost response"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			w, bt := platformSubscriptionWorld(t)
			c := w.newCustomer()
			const number = "4111111111111111"
			paid := c.mustPurchase(remote, order{price: w.membership("platform:accepted-account", 9_990_000).ID, rail: "nmi", token: bt.collect(number)})
			subID := *paid.SubscriptionID
			client := w.client[remote]
			_, err := client.CreatePSP(t.Context(), billing.CreatePSPParams{
				Key: "second", Rail: "nmi", Environment: "sandbox", PSPAccount: billing.PSPAccount{GatewayID: "second-account"},
				Credentials: map[string]string{"security_key": "second-account-key", "webhook_signing_secret": "second-webhook"},
			})
			require.NoError(t, err)
			changed := make(chan error, 1)
			bt.mu.Lock()
			if scenario == "configuration changes before submission" {
				bt.beforeTokenRead = func() {
					_, err := client.UpdateMerchantConfiguration(t.Context(), setVault(billing.CardVaultPlatform, "second"))
					changed <- err
				}
			} else {
				bt.loseSaleResponse = true
				w.nmi.HideSales(1)
			}
			bt.mu.Unlock()
			end := *w.subscription(remote, subID).CurrentPeriodEndsAt
			orderRef := subscriptions.ObligationOrderReference(subID.UUID(), end)
			var reads, refunds atomic.Int64
			w.nmi.Intercept(func(r *http.Request) bool {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/refund") {
					refunds.Add(1)
					if r.Header.Get("Authorization") != "e2e-nmi-key" {
						t.Error("refund used a different gateway account")
					}
				}
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				} else if r.Form.Get("order_id") == orderRef {
					reads.Add(1)
					if r.Form.Get("security_key") != "e2e-nmi-key" {
						t.Error("renewal read used a different gateway account")
					}
				}
				return false
			}, nil)
			w.advanceHealthyTo(end)
			w.runRenewals()
			if scenario == "configuration changes before submission" {
				select {
				case err := <-changed:
					require.NoError(t, err)
				default:
					t.Fatal("configuration change did not run before submission")
				}
			} else {
				require.True(t, end.Equal(*w.subscription(remote, subID).CurrentPeriodEndsAt))
				_, err := client.UpdateMerchantConfiguration(t.Context(), setVault(billing.CardVaultPlatform, "second"))
				require.NoError(t, err)
				w.runRenewals()
				require.Len(t, bt.salesOf(number), 2, "the unresolved attempt prevents another charge")
				w.nmi.Reveal()
			}
			w.until(func() bool { return w.subscription(remote, subID).CurrentPeriodEndsAt.After(end) }, "accepted renewal settles on its original account")
			require.Positive(t, reads.Load(), "the renewal was checked against its gateway account")
			sales := bt.salesOf(number)
			require.Len(t, sales, 2)
			require.Equal(t, "e2e-nmi-key", sales[1].SecurityKey)
			payments := completed(w.payments(remote, c.id))
			require.Len(t, payments, 2)
			for _, payment := range payments {
				require.NotNil(t, payment.PSPID)
				require.Equal(t, w.psp["nmi"], *payment.PSPID)
			}
			renewal := payments[0]
			if payments[1].CreatedAt.After(renewal.CreatedAt) {
				renewal = payments[1]
			}
			_, err = client.RefundPayment(t.Context(), renewal.ID, billing.RefundPaymentParams{Full: true, Reason: "requested_by_customer", IdempotencyKey: "accepted-account-refund"})
			require.NoError(t, err)
			w.settle()
			refunded, err := client.GetPayment(t.Context(), renewal.ID)
			require.NoError(t, err)
			require.Equal(t, billing.PaymentRefunded, refunded.Status)
			require.Equal(t, renewal.PSPID, refunded.PSPID)
			require.EqualValues(t, 1, refunds.Load())
		})
	}
}
