//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// SEC: checkout money is the catalog's. Hosts forward browser-chosen price
// ids and saved-card ids; a price that does not grant the requested access,
// an archived price, a price with a negative amount, or confirmation fields
// naming another amount or currency never change what is charged.
func TestSecurityCheckoutTermsAreServerSide(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			client := w.client[embedded]
			ctx := t.Context()
			member := w.membership("content:vip", 9_990_000)
			cheap := w.membership("content:basic", 1_000_000)
			c := w.newCustomer()
			method := c.saveCard(rail, visa)
			request := func(priceID billing.PriceID, entitlement string) billing.CreateCheckoutAttemptParams {
				return billing.CreateCheckoutAttemptParams{
					OfferKind: billing.OfferRecurring, Customer: billing.CheckoutCustomerIdentity{ID: cid(c.id)}, Entitlement: entitlement, PriceID: priceID,
					IdempotencyKey: "terms-" + uuid.NewString(), PaymentOptions: billing.CheckoutPaymentOptions{PSP: rail, PaymentMethodID: pmid(method)},
					SuccessURL: "https://e2e.test/return", CancelURL: "https://e2e.test/return",
				}
			}

			for _, tp := range []topology{embedded, remote} {
				_, err := w.client[tp].CreateCheckoutAttempt(ctx, request(cheap.ID, "content:vip"))
				require.Error(t, err, "a cheaper price for other access cannot buy content:vip")
			}
			_, err := client.CreatePrice(ctx, billing.CreatePriceParams{ProductID: member.ProductID, Key: "negative-" + uuid.NewString()[:8], UnitAmount: -1, Currency: "USD"})
			require.Error(t, err, "negative prices are refused")
			negative := -24
			_, err = client.CreatePrice(ctx, billing.CreatePriceParams{ProductID: member.ProductID, Key: "neg-duration-" + uuid.NewString()[:8], UnitAmount: 1_000_000, Currency: "USD", AccessDurationHours: &negative})
			require.Error(t, err, "a negative access duration is refused")

			archived := w.membership("content:archived", 1_000_000)
			_, err = client.ArchiveProduct(ctx, billing.ArchiveProductParams{ProductID: archived.ProductID, PurchaseAction: billing.PurchaseActionNone, Reason: "retired", IdempotencyKey: "archive-" + archived.ProductID.String()})
			require.NoError(t, err)
			_, err = client.CreateCheckoutAttempt(ctx, request(archived.ID, "content:archived"))
			require.Error(t, err, "an archived price is not purchasable")

			// The caller cannot name an amount, a currency or a quantity.
			status, body := w.staffJSON(http.MethodPost, "/v1/merchant/checkout-attempts", map[string]any{
				"customer": map[string]any{"id": c.id}, "price_id": member.ID, "payment": map[string]any{"psp": rail}, "amount": "1", "currency": "JPY", "quantity": 0,
			})
			require.Equal(t, http.StatusBadRequest, status, "%v", body)
			code, _ := errorOf(body)
			require.Equal(t, billing.CodeUnknownField, code)

			_, err = client.CreateCheckoutAttempt(ctx, request(member.ID, "content:vip"))
			require.NoError(t, err)
			w.settle()
			ledger := w.railLedger(rail)
			require.Len(t, ledger, 1)
			require.EqualValues(t, 999, ledger[0].Amount, "the quoted catalog amount, in cents")
			require.True(t, c.entitled("content:vip"))
			require.False(t, c.entitled("content:basic"))
			require.False(t, c.entitled("content:archived"))
		})
	}
}

// SEC: one permanent product, one charge. A second checkout for the same
// product racing the first's in-flight charge, on another replica, is
// refused, and a completed purchase cannot be bought again.
func TestSecurityConcurrentPermanentPurchaseChargesOnce(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			replica := w.sibling()
			client := w.client[embedded]
			product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "post-" + uuid.NewString()[:8], DisplayName: "Post", Entitlements: []string{"content:post"}})
			require.NoError(t, err)
			price, err := client.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 4_990_000, Currency: "USD"})
			require.NoError(t, err)
			c := w.newCustomer()
			method := c.saveCard(rail, visa)
			buy := func(client *openrails.Client) error {
				_, err := client.CreateCheckoutAttempt(context.WithoutCancel(t.Context()), billing.CreateCheckoutAttemptParams{
					OfferKind: billing.OfferPermanent, Customer: billing.CheckoutCustomerIdentity{ID: cid(c.id)}, Entitlement: "content:post", PriceID: price.ID,
					IdempotencyKey: "post-" + uuid.NewString(), PaymentOptions: billing.CheckoutPaymentOptions{PSP: rail, PaymentMethodID: pmid(method)},
					SuccessURL: "https://e2e.test/return", CancelURL: "https://e2e.test/return",
				})
				return err
			}
			g := w.chargeGate(rail)
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				t.Logf("first purchase: %v", buy(client))
			}()
			select {
			case <-g.arrived:
			case <-time.After(20 * time.Second):
				t.Fatal("the first purchase never reached the provider")
			}
			for _, other := range []*openrails.Client{replica.client, w.client[remote]} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					t.Logf("racing purchase: %v", buy(other))
				}()
			}
			time.Sleep(500 * time.Millisecond)
			close(g.release)
			wg.Wait()
			w.stripe.unhold()
			w.nmi.unhold()
			w.settle()
			require.Len(t, w.railLedger(rail), 1, "one charge for one permanent product")
			require.True(t, c.entitled("content:post"))
			require.Error(t, buy(client), "an owned permanent product cannot be bought again")
			w.settle()
			require.Len(t, w.railLedger(rail), 1)
		})
	}
}
