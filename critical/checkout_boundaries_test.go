//go:build integration

package critical_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/stretchr/testify/require"
)

func TestCheckoutBoundaries(t *testing.T) {
	t.Run("OR-J09-S02/customer-challenge-settles-only-after-confirmation", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		input := saleInput(t, a, p)
		p.core.SetDecline("4242", "auth")
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "challenged-purchase"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var waiting billing.Order
		require.NoError(t, json.Unmarshal(raw, &waiting))
		require.Equal(t, billing.OrderPaymentRequiresAction, waiting.Payment.Status)
		require.NotNil(t, waiting.Payment.NextAction)
		require.Empty(t, p.effects(t))
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID)), Entitlements: []string{"sale:owned"}})
		require.NoError(t, err)
		require.False(t, access.Entitlements["sale:owned"])
		calls := p.core.Submitted("/v1/payment_intents")
		require.Len(t, calls, 1)
		// Authenticate is the external bank's challenge completion; only the
		// following public confirmation can admit local access and settlement.
		intent, _, ok := strings.Cut(waiting.Payment.NextAction.Payload["client_secret"], "_secret_")
		require.True(t, ok, "the challenge must carry the provider's client capability")
		require.True(t, p.core.Authenticate(intent))
		var completed billing.Order
		for range 2 {
			status, raw = a.request(t, http.MethodPost, "/v1/me/orders/"+waiting.ID.String()+"/confirm", a.aliceToken, map[string]any{})
			require.Equal(t, http.StatusOK, status, string(raw))
			require.NoError(t, json.Unmarshal(raw, &completed))
			require.Equal(t, billing.OrderComplete, completed.Status)
			require.Equal(t, waiting.ID, completed.ID)
		}
		require.Len(t, p.effects(t), 1)
		require.EqualValues(t, 3000, p.effects(t)[0].Amount)
		payment, err := a.alpha.GetPayment(t.Context(), *completed.Payment.PaymentID)
		require.NoError(t, err)
		require.EqualValues(t, 30_000_000, payment.Amount)
		require.Equal(t, billing.PaymentSucceeded, payment.Status)
		access, err = a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: payment.CustomerID, Entitlements: []string{"sale:owned"}})
		require.NoError(t, err)
		require.True(t, access.Entitlements["sale:owned"])
	})
	t.Run("OR-J09-S03/definitive-decline-keeps-order-unpaid", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		input := saleInput(t, a, p)
		p.core.SetDecline("4242", "insufficient_funds")
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "declined-purchase"})
		require.Equal(t, http.StatusPaymentRequired, status, string(raw))
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		orders, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Len(t, orders.Items, 1)
		require.NotEqual(t, billing.OrderComplete, orders.Items[0].Status)
		require.NotEqual(t, billing.OrderPaymentProcessing, orders.Items[0].Payment.Status)
		payments, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: customer})
		require.NoError(t, err)
		for _, payment := range payments.Items {
			require.NotEqual(t, billing.PaymentSucceeded, payment.Status)
		}
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{"sale:owned"}})
		require.NoError(t, err)
		require.False(t, access.Entitlements["sale:owned"])
		require.Empty(t, p.effects(t))
		facts, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"payment.settled", "order.completed"}, Limit: 100})
		require.NoError(t, err)
		require.Empty(t, facts.Events)

		// On NMI with a ZIP-only checkout: a plain decline is reported as one,
		// another price of the product is the one paid, and the declines raise
		// no operator finding.
		nmi, journal := newNMIWorld(t)
		pass, err := nmi.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "nmi-pass", DisplayName: "NMI pass", Entitlements: []string{"pass:owned"}})
		require.NoError(t, err)
		cheap, err := nmi.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: pass.ID, Key: "nmi-pass-test", UnitAmount: 500_000, Currency: "USD"})
		require.NoError(t, err)
		dear, err := nmi.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: pass.ID, Key: "nmi-pass", UnitAmount: 23_000_000, Currency: "USD"})
		require.NoError(t, err)
		// As BuyButton buys: the customer's order of the lines, then its payment.
		buy := func(price billing.PriceID, total int64, card nmimock.Card, key string) (int, billing.Order) {
			t.Helper()
			status, raw := nmi.request(t, http.MethodPost, "/v1/me/orders", nmi.aliceToken, billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price}}})
			require.Contains(t, []int{http.StatusOK, http.StatusCreated}, status, string(raw))
			var order billing.Order
			require.NoError(t, json.Unmarshal(raw, &order))
			pay := billing.PayOrderParams{Payment: billing.OrderPaymentParams{Token: journal.core.Tokenize(card)}, ExpectedTotal: total, Reusable: ptr(false)}
			status, raw = nmi.request(t, http.MethodPost, "/v1/me/orders/"+order.ID.String()+"/pay", nmi.aliceToken, pay, map[string]string{"Idempotency-Key": key})
			require.Contains(t, []int{http.StatusOK, http.StatusPaymentRequired}, status, string(raw))
			current, err := nmi.alpha.GetOrder(t.Context(), order.ID)
			require.NoError(t, err)
			return status, *current
		}
		declined := nmimock.Card{Brand: "visa", Last4: "1111", CVV: "M", Decline: "200"}
		declined.AVS = "Z" // the ZIP matched; no street was sent
		_, first := buy(cheap.ID, 500_000, declined, "nmi-decline-zip-matched")
		require.Equal(t, string(billing.DeclineGeneric), first.Payment.LastPaymentError.Reason, "a matched ZIP is no address fault")
		tried, err := nmi.alpha.ListPaymentAttempts(t.Context(), billing.PaymentAttemptListParams{OrderID: first.ID})
		require.NoError(t, err)
		require.Len(t, tried.Items, 1)
		require.NotNil(t, tried.Items[0].Card, "the declined new card is named from NMI's report")
		require.Equal(t, "visa", *tried.Items[0].Card.Brand)
		require.Equal(t, "1111", *tried.Items[0].Card.Last4)
		declined.AVS = "N"
		_, again := buy(cheap.ID, 500_000, declined, "nmi-decline-zip-unmatched")
		require.Equal(t, first.ID, again.ID, "the same price pays the same order")
		require.Equal(t, string(billing.DeclineIncorrectZip), again.Payment.LastPaymentError.Reason, "an unmatched ZIP is named")
		// The preview agrees: the customer's unpaid order is no conflict for
		// another price, but still one for the merchant, who replaces nothing.
		status, raw = nmi.request(t, http.MethodPost, "/v1/me/orders/preview", nmi.aliceToken, billing.PreviewMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: dear.ID}}})
		require.Equal(t, http.StatusOK, status, string(raw))
		var preview billing.OrderPreview
		require.NoError(t, json.Unmarshal(raw, &preview))
		require.Nil(t, preview.Lines[0].Refusal, "the unpaid order of the other price is replaced")
		require.EqualValues(t, 23_000_000, preview.Total)
		require.NotEmpty(t, preview.PaymentOptions)
		aliceID := billing.CustomerID(uuid.MustParse(nmi.alice.ID))
		merchantPreview, err := nmi.alphaRemote.PreviewOrder(t.Context(), billing.PreviewOrderParams{CustomerID: aliceID, Lines: []billing.OrderLineParams{{PriceID: dear.ID}}})
		require.NoError(t, err)
		require.NotNil(t, merchantPreview.Lines[0].Refusal)
		require.Equal(t, "already_owned", merchantPreview.Lines[0].Refusal.Code)
		// An order the merchant made is not the customer's to replace.
		extra, err := nmi.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "nmi-extra", DisplayName: "NMI extra", Entitlements: []string{"extra:owned"}})
		require.NoError(t, err)
		extraA, err := nmi.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: extra.ID, Key: "nmi-extra-a", UnitAmount: 500_000, Currency: "USD"})
		require.NoError(t, err)
		extraB, err := nmi.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: extra.ID, Key: "nmi-extra-b", UnitAmount: 900_000, Currency: "USD"})
		require.NoError(t, err)
		_, err = nmi.alphaRemote.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: aliceID, Lines: []billing.OrderLineParams{{PriceID: extraA.ID}}, IdempotencyKey: "nmi-merchant-extra"})
		require.NoError(t, err)
		status, raw = nmi.request(t, http.MethodPost, "/v1/me/orders/preview", nmi.aliceToken, billing.PreviewMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: extraB.ID}}})
		require.Equal(t, http.StatusOK, status, string(raw))
		require.NoError(t, json.Unmarshal(raw, &preview))
		require.NotNil(t, preview.Lines[0].Refusal, "the merchant's order still holds it")
		require.Equal(t, "already_owned", preview.Lines[0].Refusal.Code)
		// Another price, by BuyButton's two calls, then by one: each pays the
		// price chosen and releases the unpaid order of the other.
		_, other := buy(dear.ID, 23_000_000, declined, "nmi-other-price")
		require.NotEqual(t, first.ID, other.ID)
		require.EqualValues(t, 23_000_000, other.Total)
		replaced, err := nmi.alpha.GetOrder(t.Context(), first.ID)
		require.NoError(t, err)
		require.Equal(t, billing.OrderCanceled, replaced.Status, "the unpaid order of the other price is released")
		in := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: cheap.ID}}, ExpectedTotal: ptr(int64(500_000)), Reusable: ptr(false),
			Payment: &billing.OrderPaymentParams{Token: journal.core.Tokenize(nmimock.Card{Brand: "mastercard", Last4: "5454", AVS: "Z", CVV: "M"})}}
		status, raw = nmi.request(t, http.MethodPost, "/v1/me/orders", nmi.aliceToken, in, map[string]string{"Idempotency-Key": "nmi-one-call"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var paid billing.Order
		require.NoError(t, json.Unmarshal(raw, &paid))
		require.Equal(t, billing.OrderComplete, paid.Status)
		require.EqualValues(t, 500_000, paid.Total, "the customer pays the price they chose")
		require.Equal(t, "0.50", journal.core.LastSale().Amount)
		replaced, err = nmi.alpha.GetOrder(t.Context(), other.ID)
		require.NoError(t, err)
		require.Equal(t, billing.OrderCanceled, replaced.Status)
		fleet := hostFleet(t, nmi.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, nmi.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		runRegistered(t, fleet, "openrails.merchant_health", 1)
		abandoned, err := nmi.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.provider_intent.abandoned"})
		require.NoError(t, err)
		require.Empty(t, abandoned.Items, "a customer leaving a declined card is no operator incident")

	})
	t.Run("OR-J09-S05/lost-sale-answer-settles-from-the-provider", func(t *testing.T) {
		// A sale whose answer is lost waits for NMI's own record: a decline
		// there reopens the order for another card, and a charge NMI can't yet
		// show stays unknown as an operator finding until its receipt appears.
		nmi, journal := newNMIWorld(t)
		fleet := hostFleet(t, nmi.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, nmi.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		lost, err := nmi.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "nmi-lost", DisplayName: "NMI lost", Entitlements: []string{"lost:owned"}})
		require.NoError(t, err)
		lostPrice, err := nmi.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: lost.ID, Key: "nmi-lost", UnitAmount: 700_000, Currency: "USD"})
		require.NoError(t, err)
		settle := func(order billing.OrderID, done func(billing.Order) bool) billing.Order {
			t.Helper()
			nmi.clock.Advance(6 * time.Minute)
			var current *billing.Order
			require.Eventually(t, func() bool {
				wakeJobs(t, fleet, "openrails.provider_operation")
				current, err = nmi.alpha.GetOrder(t.Context(), order)
				return err == nil && done(*current)
			}, 20*time.Second, 50*time.Millisecond)
			return *current
		}
		unresolved := func(order billing.Order) []billing.Finding {
			t.Helper()
			findings, err := nmi.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.sale.unresolved"})
			require.NoError(t, err)
			var out []billing.Finding
			for _, f := range findings.Items {
				if f.Evidence["order_id"] == order.ID.String() {
					out = append(out, f)
				}
			}
			return out
		}
		buy := func(price billing.PriceID, total int64, card nmimock.Card, key string) (int, billing.Order) {
			t.Helper()
			status, raw := nmi.request(t, http.MethodPost, "/v1/me/orders", nmi.aliceToken, billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price}}})
			require.Contains(t, []int{http.StatusOK, http.StatusCreated}, status, string(raw))
			var order billing.Order
			require.NoError(t, json.Unmarshal(raw, &order))
			pay := billing.PayOrderParams{Payment: billing.OrderPaymentParams{Token: journal.core.Tokenize(card)}, ExpectedTotal: total, Reusable: ptr(false)}
			status, raw = nmi.request(t, http.MethodPost, "/v1/me/orders/"+order.ID.String()+"/pay", nmi.aliceToken, pay, map[string]string{"Idempotency-Key": key})
			require.Equal(t, http.StatusOK, status, string(raw))
			current, err := nmi.alpha.GetOrder(t.Context(), order.ID)
			require.NoError(t, err)
			return status, *current
		}
		declined := nmimock.Card{Brand: "visa", Last4: "1111", AVS: "Z", CVV: "M", Decline: "200"}
		journal.core.DropSaleResponses(1)
		status, waiting := buy(lostPrice.ID, 700_000, declined, "nmi-lost-decline")
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, billing.OrderProcessing, waiting.Status)
		status, raw := nmi.request(t, http.MethodPost, "/v1/me/orders", nmi.aliceToken, billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: lostPrice.ID}}})
		require.Equal(t, http.StatusConflict, status, "the unknown sale still holds its order: %s", raw)
		reopened := settle(waiting.ID, func(o billing.Order) bool { return o.Status == billing.OrderOpen })
		require.NotNil(t, reopened.Payment.LastPaymentError)
		require.Equal(t, string(billing.DeclineGeneric), reopened.Payment.LastPaymentError.Reason)
		require.Empty(t, unresolved(waiting), "NMI's decline settles the sale; nobody has to")
		journal.core.DropSaleResponses(1)
		journal.core.HideSales(1)
		status, waiting = buy(lostPrice.ID, 700_000, nmimock.Card{Brand: "visa", Last4: "4242", AVS: "Z", CVV: "M"}, "nmi-lost-charge")
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, waiting.ID, reopened.ID, "the declined order is paid again")
		require.Equal(t, billing.OrderProcessing, waiting.Status)
		settle(waiting.ID, func(o billing.Order) bool { return len(unresolved(o)) == 1 })
		raised := unresolved(waiting)[0]
		require.Equal(t, "high", raised.Severity)
		require.Equal(t, billing.FindingStatus("requires_review"), raised.Status)
		require.Contains(t, *raised.RecommendedAction, "openrails intents resolve")
		current, err := nmi.alpha.GetOrder(t.Context(), waiting.ID)
		require.NoError(t, err)
		require.Equal(t, billing.OrderProcessing, current.Status, "a charge NMI can't show is never assumed absent")
		journal.core.Reveal()
		settle(waiting.ID, func(o billing.Order) bool { return o.Status == billing.OrderComplete })
		require.Empty(t, unresolved(waiting), "the receipt closes the finding")
		closed, err := nmi.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.sale.unresolved", Status: "fixed"})
		require.NoError(t, err)
		require.Len(t, closed.Items, 1)
		require.Equal(t, raised.ID, closed.Items[0].ID)
		// An answered sale NMI has yet to index waits out the delay quietly.
		late, err := nmi.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "nmi-late", DisplayName: "NMI late", Entitlements: []string{"late:owned"}})
		require.NoError(t, err)
		latePrice, err := nmi.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: late.ID, Key: "nmi-late", UnitAmount: 700_000, Currency: "USD"})
		require.NoError(t, err)
		journal.core.HideSales(1)
		status, waiting = buy(latePrice.ID, 700_000, nmimock.Card{Brand: "visa", Last4: "4242", AVS: "Z", CVV: "M"}, "nmi-late-receipt")
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, billing.OrderProcessing, waiting.Status)
		wakeJobs(t, fleet, "openrails.provider_operation")
		require.Never(t, func() bool { return len(unresolved(waiting)) > 0 }, time.Second, 100*time.Millisecond, "no finding inside the observation delay")
		journal.core.Reveal()
		settle(waiting.ID, func(o billing.Order) bool { return o.Status == billing.OrderComplete })
		require.Empty(t, unresolved(waiting))
		journal.money(t, 2, 140) // neither sale is ever resent
	})
	t.Run("OR-J10-S02/lost-client-response-replays-original-committed-result", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		input := saleInput(t, a, p)
		origin := a.server
		wire := http.DefaultTransport.(*http.Transport).Clone()
		defer wire.CloseIdleConnections()
		proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			forward, err := http.NewRequestWithContext(r.Context(), r.Method, origin.URL+r.URL.RequestURI(), r.Body)
			if err != nil {
				http.Error(w, "fixture forward", 500)
				return
			}
			forward.Header = r.Header.Clone()
			response, err := wire.RoundTrip(forward)
			if err != nil {
				http.Error(w, "fixture upstream", 502)
				return
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			connection, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = connection.Close()
			}
		}))
		defer proxy.Close()
		a.server = proxy
		_, _, err := a.do(t.Context(), http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "lost-client-answer"})
		a.server = origin
		require.Error(t, err, "the client must actually lose the committed answer")
		orders, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID))})
		require.NoError(t, err)
		require.Len(t, orders.Items, 1)
		require.Equal(t, billing.OrderComplete, orders.Items[0].Status)
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "lost-client-answer"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var replay billing.Order
		require.NoError(t, json.Unmarshal(raw, &replay))
		require.Equal(t, orders.Items[0].ID, replay.ID)
		require.Equal(t, orders.Items[0].Payment.PaymentID, replay.Payment.PaymentID)
		require.Len(t, p.effects(t), 1)
		require.EqualValues(t, 3000, p.effects(t)[0].Amount)
	})
}
