//go:build e2e && integration

package subscriptions_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
)

// An NMI sale whose answer was lost is not provably unexecuted from an empty
// order search: --not-executed is refused and the original order recovers its
// receipt once NMI shows it, so the buyer is charged once (tracker 1137). Only
// a definitive decline under the sale's order proves nothing was charged.
func TestNMISaleNonExecutionNeedsProviderEvidence(t *testing.T) {
	t.Parallel()
	for _, declined := range []bool{false, true} {
		name := "hidden_charge"
		if declined {
			name = "lost_decline"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			client := w.client[embedded]
			product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "post-" + uuid.NewString()[:8], DisplayName: "Paid post", Entitlements: []string{"content:post"}})
			require.NoError(t, err)
			price, err := client.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 4_990_000, Currency: "USD"})
			require.NoError(t, err)
			c := w.newCustomer()
			method := c.saveCard("nmi", visa)
			if declined {
				w.nmi.SetDecline(visa.Last4, "202")
			} else {
				w.nmi.HideSales(1)
			}
			w.nmi.DropSaleResponses(1)
			_, err = createCheckoutAttempt(t.Context(), client, billing.CreateCheckoutAttemptParams{
				OfferKind: billing.OfferPermanent, Customer: c.identity(), Entitlement: "content:post", PriceID: price.ID,
				IdempotencyKey: "buy-" + uuid.NewString(), PaymentOptions: billing.CheckoutPaymentOptions{PSP: "nmi", PaymentMethodID: pmid(method)},
				SuccessURL: "https://e2e.test/return", CancelURL: "https://e2e.test/return",
			})
			require.NoError(t, err)
			require.NoError(t, w.jobs.Stop(t.Context()))
			w.advance(2 * time.Hour)
			var id uuid.UUID
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT id FROM billing.provider_intents WHERE intent_type = 'nmi_sale'`)).Scan(&id))
			ctx := merchant.WithID(t.Context(), client.MerchantID())
			runner := engine.Graph(w.rt).Runtime.IntentRunner()
			row, err := runner.VerifyByID(ctx, id)
			require.NoError(t, err)
			require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status, "an empty search after the answer was lost settles nothing")

			_, err = runner.Resolve(ctx, id, intents.Resolution{NotExecuted: true, Actor: "test-operator", Reason: "not in the dashboard"})
			if declined {
				require.NoError(t, err, "NMI's decline under the order proves nothing was charged")
				require.NoError(t, w.jobs.Start(t.Context()))
				w.settle()
				require.Empty(t, w.nmi.Ledger(""))
				require.False(t, c.entitled("content:post"))
				return
			}
			require.ErrorContains(t, err, "does not prove")
			require.Len(t, w.nmi.Ledger(""), 1, "the hidden sale did charge")
			w.nmi.Reveal()
			require.NoError(t, w.jobs.Start(t.Context()))
			w.until(func() bool { return c.entitled("content:post") }, "the original order recovers its receipt")
			require.Len(t, completed(w.payments(embedded, c.id)), 1, "one charge, recorded once")
			require.Len(t, w.nmi.Ledger(""), 1)
		})
	}
}
