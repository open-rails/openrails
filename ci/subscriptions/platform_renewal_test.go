//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/vaulttest"
	"github.com/stretchr/testify/require"
)

func platformSubscriptionWorld(t *testing.T) (*world, *btFake) {
	t.Helper()
	bt, endpoint := newBTFake(t)
	bt.tenant, bt.application = platformTenant, uuid.NewString()
	w := prepareWorld(t, 12, withPlatformVault)
	w.vault = vaulttest.New(t)
	w.booted = func(e *openrails.Engine) {
		rt := engine.Graph(e).Runtime
		custody := rt.PurchaseService.CustodianSaleService
		custody.BTBaseURLOverride, custody.GatewayDirectPostURLOverride = endpoint, endpoint+"/nmi"
		resolver := rt.CollectionResolver.(*money.MerchantCollectionAdapterBuilder)
		resolver.Endpoints.BTBaseURL, resolver.Endpoints.NMIDirectPostURL = endpoint, endpoint+"/nmi"
	}
	w.start()
	client := w.client[remote]
	_, err := client.UpdatePSP(t.Context(), w.psp["stripe"], billing.UpdatePSPParams{Archived: true, AllowLast: true})
	require.NoError(t, err)
	_, err = client.UpdateMerchantConfiguration(t.Context(), setCustody(billing.CardCustodyPlatform))
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
	for _, scenario := range []string{"recovered", "bounded grace", "expired intent", "canceled", "replaced"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			w, bt := platformSubscriptionWorld(t)
			c := w.newCustomer()
			if scenario == "bounded grace" {
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
			if scenario == "replaced" {
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
				_, err := w.client[remote].UpdateMerchantConfiguration(t.Context(), setCustody(billing.CardCustodyMerchant))
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
