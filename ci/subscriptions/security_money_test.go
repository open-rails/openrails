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
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/modules/checkout"
)

// Checkout money is the catalog's. Hosts forward browser-chosen price ids and
// saved-card ids; a price that does not grant the requested access, an
// archived price, a price with a negative amount, or confirmation fields
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
			// The engine refuses a price that does not grant an asserted
			// entitlement; no session asserts one, but the check stands.
			_, err := w.engineCheckout(c, checkout.CheckoutAttemptCreateRequest{
				PriceID: cheap.ID.String(), Entitlement: "content:vip", OfferKind: billing.OfferRecurring,
				Payment: checkout.CheckoutAttemptPaymentRequest{Rail: rail, PaymentMethodID: method},
			})
			require.Error(t, err, "a cheaper price for other access cannot buy content:vip")
			_, err = client.CreatePrice(ctx, billing.CreatePriceParams{ProductID: member.ProductID, Key: "negative-" + uuid.NewString()[:8], UnitAmount: -1, Currency: "USD"})
			require.Error(t, err, "negative prices are refused")
			negative := -24
			_, err = client.CreatePrice(ctx, billing.CreatePriceParams{ProductID: member.ProductID, Key: "neg-duration-" + uuid.NewString()[:8], UnitAmount: 1_000_000, Currency: "USD", AccessDurationHours: &negative})
			require.Error(t, err, "a negative access duration is refused")

			archived := w.membership("content:archived", 1_000_000)
			_, err = client.ArchiveProduct(ctx, billing.ArchiveProductParams{ProductID: archived.ProductID, Reason: "retired", IdempotencyKey: "archive-" + archived.ProductID.String()})
			require.ErrorIs(t, err, billing.ErrInvalid, "a product archive without a purchase action is a PATCH")
			_, err = client.UpdateProduct(ctx, archived.ProductID, billing.UpdateProductParams{Archived: catalog.Value(true)})
			require.NoError(t, err)
			_, err = c.sell(embedded, order{price: archived.ID})
			require.Error(t, err, "an archived price is not purchasable")

			// The caller cannot name a currency or a quantity.
			status, body := w.hostJSON(http.MethodPost, "/v1/admin/checkout-sessions", map[string]any{
				"customer": map[string]any{"id": c.id}, "price_id": member.ID, "currency": "JPY", "quantity": 0,
			})
			require.Equal(t, http.StatusBadRequest, status, "%v", body)
			code, _ := errorOf(body)
			require.Equal(t, billing.CodeUnknownField, code)

			c.mustCheckout(embedded, order{price: member.ID, rail: rail, method: method})
			ledger := w.railLedger(rail)
			require.Len(t, ledger, 1)
			require.EqualValues(t, 999, ledger[0].Amount, "the quoted catalog amount, in cents")
			require.True(t, c.entitled("content:vip"))
			require.False(t, c.entitled("content:basic"))
			require.False(t, c.entitled("content:archived"))
		})
	}
}

// One permanent product, one charge. A second checkout for the same product
// racing the first's in-flight charge, on another replica, is refused, and a
// completed purchase cannot be bought again.
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
			// Each purchase is its own session, minted and paid on one process.
			buy := func(mint *openrails.Client, server string) error {
				link, err := mint.CreateCheckoutSession(context.WithoutCancel(t.Context()), billing.CreateCheckoutSessionParams{Customer: c.identity(), PriceID: price.ID})
				if err != nil {
					return err
				}
				ctx := context.WithoutCancel(t.Context())
				session := hostedSession{w: w, id: link.ID}
				option, err := session.optionOf(ctx, server, rail)
				if err != nil {
					return err
				}
				return succeeded(session.payAt(ctx, server, option, c, order{method: method}))
			}
			g := w.chargeGate(rail)
			var first, racers sync.WaitGroup
			first.Add(1)
			go func() {
				defer first.Done()
				t.Logf("first purchase: %v", buy(client, w.server.URL))
			}()
			select {
			case <-g.arrived:
			case <-time.After(20 * time.Second):
				t.Fatal("the first purchase never reached the provider")
			}
			for _, other := range []struct {
				mint   *openrails.Client
				server string
			}{{replica.client, replica.server.URL}, {w.client[remote], w.server.URL}} {
				racers.Add(1)
				go func() {
					defer racers.Done()
					t.Logf("racing purchase: %v", buy(other.mint, other.server))
				}()
			}
			releaseAfterRacers(t, g, &racers)
			first.Wait()
			w.stripe.unhold()
			w.nmi.unhold()
			w.settle()
			require.Len(t, w.railLedger(rail), 1, "one charge for one permanent product")
			require.True(t, c.entitled("content:post"))
			require.Error(t, buy(client, w.server.URL), "an owned permanent product cannot be bought again")
			w.settle()
			require.Len(t, w.railLedger(rail), 1)
		})
	}
}
