//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/solanafake"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// Each scenario acts just past an expiry with no expiry pass run, and gets
// the outcome the pass would have left.

// passlessFleet runs workers with every periodic pass removed.
func passlessFleet(t *testing.T, w *world) *river.Client[pgx.Tx] {
	t.Helper()
	fleet := hostFleet(t, w, 2)
	fleet.PeriodicJobs().Clear()
	require.NoError(t, w.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
	require.NoError(t, fleet.Start(t.Context()))
	return fleet
}

func myOrder(t *testing.T, a *authorityWorld, id billing.OrderID) billing.Order {
	t.Helper()
	status, raw := a.request(t, http.MethodGet, "/v1/me/orders/"+id.String(), a.aliceToken, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var order billing.Order
	require.NoError(t, json.Unmarshal(raw, &order))
	return order
}

func confirmMyOrder(t *testing.T, a *authorityWorld, id billing.OrderID) billing.Order {
	t.Helper()
	status, raw := a.request(t, http.MethodPost, "/v1/me/orders/"+id.String()+"/confirm", a.aliceToken, billing.ConfirmOrderParams{})
	require.Equal(t, http.StatusOK, status, string(raw))
	var order billing.Order
	require.NoError(t, json.Unmarshal(raw, &order))
	return order
}

// expiredAsThePassLeavesIt: closed at its expiry, nothing left to pay with.
func expiredAsThePassLeavesIt(t *testing.T, order billing.Order) {
	t.Helper()
	require.Equal(t, billing.OrderExpired, order.Status)
	require.Equal(t, billing.OrderPaymentRequiresPaymentMethod, order.Payment.Status)
	require.Nil(t, order.Payment.NextAction)
	require.Empty(t, order.PaymentOptions)
	require.NotNil(t, order.ExpiredAt)
	require.True(t, order.ExpiredAt.Equal(*order.ExpiresAt), "expired at its expiry, not when a pass wrote it")
}

func uniqueSale(t *testing.T, a *authorityWorld, key string) billing.PriceID {
	t.Helper()
	product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: "Unique " + key, Ownership: catalog.OwnershipUnique, Entitlements: []string{key + ":owned"}})
	require.NoError(t, err)
	price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: key, UnitAmount: 5_000_000, Currency: "USD"})
	require.NoError(t, err)
	return price.ID
}

func TestOrderExpiryClock(t *testing.T) {
	t.Run("OR-J37-S70/transfer-after-order-expiry-goes-to-review-before-the-pass", func(t *testing.T) {
		s := newSolanaWorld(t, false, solanaFixtureOptions{manualFleet: true})
		passlessFleet(t, s.world)
		product, err := s.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "chain-expiring", DisplayName: "Chain purchase", Ownership: catalog.OwnershipConsumable, Entitlements: []string{"chain:paid"}})
		require.NoError(t, err)
		price, err := s.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, UnitAmount: 10_000_000, Currency: "USD", SellVia: []string{billing.SellViaSolana}})
		require.NoError(t, err)
		customer := billing.CustomerID(uuid.MustParse(s.alice.ID))
		expires := s.clock.Now().Add(time.Hour)
		created, err := s.alpha.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: customer, Lines: []billing.OrderLineParams{{PriceID: price.ID}}, ExpiresAt: &expires, IdempotencyKey: "solana-expiring"})
		require.NoError(t, err)
		// Quoted ten minutes before the order expires: the quote and its late
		// window outlive the order.
		s.clock.Advance(50 * time.Minute)
		pay := billing.PayOrderParams{ExpectedTotal: 10_000_000, Payment: billing.OrderPaymentParams{TokenSymbol: "USDC"}}
		status, raw := s.request(t, http.MethodPost, "/v1/me/orders/"+created.ID.String()+"/pay", s.aliceToken, pay, map[string]string{"Idempotency-Key": "solana-expiring-pay"})
		require.Equal(t, http.StatusOK, status, string(raw))
		var waiting billing.Order
		require.NoError(t, json.Unmarshal(raw, &waiting))
		require.NotNil(t, waiting.Payment.NextAction)
		require.Equal(t, "solana_pay", waiting.Payment.NextAction.Type)
		prepared := s.walletTransaction(t, waiting)
		s.clock.Advance(11 * time.Minute)
		sig, err := s.node.Pay(solanafake.Transfer{Payer: s.payer.PublicKey(), Recipient: s.treasury.PublicKey().String(), Mint: solanafake.DevnetUSDCMint, Amount: 10_000_000,
			Reference: waiting.Payment.NextAction.Payload["reference"], BlockTime: s.clock.Now(), Memo: solanaMemo(t, prepared)})
		require.NoError(t, err)
		var review *billing.Finding
		require.Eventually(t, func() bool {
			s.clock.Advance(time.Second)
			findings, err := s.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "consistency.ledger.unbooked"})
			require.NoError(t, err)
			for i, f := range findings.Items {
				if f.Evidence["transaction_id"] == sig {
					review = &findings.Items[i]
				}
			}
			return review != nil
		}, 20*time.Second, 100*time.Millisecond, "the poller must settle the landed transfer")
		require.Equal(t, "solana_pay_session_closed", review.Evidence["operation"])
		expired, err := s.alpha.GetOrder(t.Context(), created.ID)
		require.NoError(t, err)
		require.Equal(t, billing.OrderExpired, expired.Status)
		s.noAccess(t, waiting)
		payments, err := s.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Empty(t, payments.Items, "money that landed after the order expired is held for review, never credited")
	})

	t.Run("OR-J09-S70/challenge-and-hosted-confirm-after-expiry-close-the-order-before-the-pass", func(t *testing.T) {
		// A payment is never recorded in the database's future.
		a, p := newPaymentWorld(t, func(w *world) { w.clock.Advance(-30 * 24 * time.Hour) })
		passlessFleet(t, a.world)
		p.core.SendWebhooksTo(a.server.URL+"/v1/webhooks/stripe/acct_e2e", "whsec_critical_fixture")
		input := saleInput(t, a, p)
		p.core.SetDecline("4242", "auth")
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "challenged-expiring"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var challenged billing.Order
		require.NoError(t, json.Unmarshal(raw, &challenged))
		require.NotNil(t, challenged.Payment.NextAction)
		hostedInput := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: input.Lines[0].PriceID, Quantity: ptr(2)}}, ExpectedTotal: ptr(int64(60_000_000)),
			Payment: &billing.OrderPaymentParams{ReturnURL: "https://critical.invalid/paid"}}
		status, raw = a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, hostedInput, map[string]string{"Idempotency-Key": "hosted-expiring"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var hosted billing.Order
		require.NoError(t, json.Unmarshal(raw, &hosted))
		require.NotNil(t, hosted.Payment.NextAction)
		require.Equal(t, "redirect_to_url", hosted.Payment.NextAction.Type)
		hostedInput.Lines[0].Quantity = ptr(3)
		hostedInput.ExpectedTotal = ptr(int64(90_000_000))
		status, raw = a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, hostedInput, map[string]string{"Idempotency-Key": "hosted-paid-late"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var paidLate billing.Order
		require.NoError(t, json.Unmarshal(raw, &paidLate))
		require.Len(t, p.core.CheckoutSessions(), 2)
		session := func(order billing.Order) map[string]any {
			id := strings.TrimPrefix(*order.Payment.NextAction.URL, stripemock.CheckoutURL)
			for _, s := range p.core.CheckoutSessions() {
				if s["id"] == id {
					return s
				}
			}
			t.Fatalf("no Checkout Session %s", id)
			return nil
		}
		a.clock.Advance(orderTTL)

		// The unfinished challenge: the order closes and its challenge is
		// abandoned; nothing is charged.
		expiredAsThePassLeavesIt(t, confirmMyOrder(t, a, challenged.ID))
		// The hosted return: the order closes and Stripe's page is expired.
		expiredAsThePassLeavesIt(t, confirmMyOrder(t, a, hosted.ID))
		require.Equal(t, "expired", session(hosted)["status"])
		require.Empty(t, p.effects(t))

		// Money that moved after the expiry pays the order it closed: it is
		// expired at its expiry, then completed by the payment.
		_, err := p.core.CompleteCheckoutSession(t.Context(), session(paidLate)["id"].(string))
		require.NoError(t, err)
		var settled *billing.Order
		require.Eventually(t, func() bool {
			settled, err = a.alpha.GetOrder(t.Context(), paidLate.ID)
			return err == nil && settled.Status == billing.OrderComplete
		}, 20*time.Second, 50*time.Millisecond)
		require.NotNil(t, settled.ExpiredAt)
		require.True(t, settled.ExpiredAt.Equal(*settled.ExpiresAt))
		require.NotNil(t, settled.Payment.PaymentID)
		payment, err := a.alpha.GetPayment(t.Context(), *settled.Payment.PaymentID)
		require.NoError(t, err)
		require.Equal(t, billing.PaymentSucceeded, payment.Status)
		require.EqualValues(t, 90_000_000, payment.Amount)
		events, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"order.expired", "order.completed"}, Limit: 100})
		require.NoError(t, err)
		var trail []string
		for _, e := range events.Events {
			if strings.Contains(string(e.Data), paidLate.ID.String()) {
				trail = append(trail, e.Type)
			}
		}
		require.Equal(t, []string{"order.expired", "order.completed"}, trail)
	})

	t.Run("OR-J08-S70/expired-open-order-claims-nothing-before-the-pass", func(t *testing.T) {
		a, _ := newPaymentWorld(t)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		forMerchant, forCart := uniqueSale(t, a, "lapsed-merchant"), uniqueSale(t, a, "lapsed-cart")
		var lapsed []billing.OrderID
		for _, price := range []billing.PriceID{forMerchant, forCart} {
			status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price}}})
			require.Equal(t, http.StatusCreated, status, string(raw))
			var order billing.Order
			require.NoError(t, json.Unmarshal(raw, &order))
			lapsed = append(lapsed, order.ID)
		}
		_, err := a.alphaRemote.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: customer, Lines: []billing.OrderLineParams{{PriceID: forMerchant}}, IdempotencyKey: "while-held"})
		var refused *billing.StatusError
		require.ErrorAs(t, err, &refused)
		require.Equal(t, "already_owned", refused.Code, "before its expiry the open order holds the line")
		a.clock.Advance(orderTTL)

		preview, err := a.alpha.PreviewOrder(t.Context(), billing.PreviewOrderParams{CustomerID: customer, Lines: []billing.OrderLineParams{{PriceID: forMerchant}, {PriceID: forCart}}})
		require.NoError(t, err)
		for _, line := range preview.Lines {
			require.Nil(t, line.Refusal)
		}
		merchantOrder, err := a.alpha.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: customer, Lines: []billing.OrderLineParams{{PriceID: forMerchant}}, IdempotencyKey: "after-expiry"})
		require.NoError(t, err)
		require.Equal(t, billing.OrderOpen, merchantOrder.Status)
		status, raw := a.request(t, http.MethodPut, "/v1/me/cart/lines/"+forCart.String(), a.aliceToken, billing.SetCartLineParams{})
		require.Equal(t, http.StatusOK, status, string(raw))
		status, raw = a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, billing.CreateMyOrderParams{Cart: true})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var cartOrder billing.Order
		require.NoError(t, json.Unmarshal(raw, &cartOrder))
		require.Equal(t, billing.OrderOpen, cartOrder.Status)
		for _, id := range lapsed {
			order, err := a.alpha.GetOrder(t.Context(), id)
			require.NoError(t, err)
			require.Equal(t, billing.OrderExpired, order.Status)
		}
		open, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer, Status: billing.OrderOpen})
		require.NoError(t, err)
		require.Len(t, open.Items, 2)
		require.ElementsMatch(t, []billing.OrderID{merchantOrder.ID, cartOrder.ID}, []billing.OrderID{open.Items[0].ID, open.Items[1].ID})
	})

	t.Run("OR-J08-S71/expired-open-order-reads-expired-everywhere-before-the-pass", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		input := saleInput(t, a, p)
		p.core.SetDecline("4242", "auth")
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "challenged-read"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var created billing.Order
		require.NoError(t, json.Unmarshal(raw, &created))
		live := myOrder(t, a, created.ID)
		require.Equal(t, billing.OrderOpen, live.Status)
		require.NotNil(t, live.Payment.NextAction)
		require.NotEmpty(t, live.PaymentOptions)
		a.clock.Advance(orderTTL)

		expiredAsThePassLeavesIt(t, myOrder(t, a, created.ID))
		staff, err := a.alphaRemote.GetOrder(t.Context(), created.ID)
		require.NoError(t, err)
		expiredAsThePassLeavesIt(t, *staff)
		customer := created.CustomerID
		open, err := a.alphaRemote.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer, Status: billing.OrderOpen})
		require.NoError(t, err)
		require.Empty(t, open.Items)
		expired, err := a.alphaRemote.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer, Status: billing.OrderExpired})
		require.NoError(t, err)
		require.Len(t, expired.Items, 1)
		expiredAsThePassLeavesIt(t, expired.Items[0])
		byID, err := a.alphaRemote.ListOrders(t.Context(), billing.OrderListParams{IDs: []billing.OrderID{created.ID}})
		require.NoError(t, err)
		require.Len(t, byID.Items, 1)
		expiredAsThePassLeavesIt(t, byID.Items[0])
		status, raw = a.request(t, http.MethodGet, "/v1/me/orders?status=expired", a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var mine billing.ListPage[billing.Order]
		require.NoError(t, json.Unmarshal(raw, &mine))
		require.Len(t, mine.Items, 1)
		require.Equal(t, created.ID, mine.Items[0].ID)
		// The expiry closed it first: it is not canceled.
		status, raw = a.request(t, http.MethodPost, "/v1/me/orders/"+created.ID.String()+"/cancel", a.aliceToken, nil)
		require.Equal(t, http.StatusConflict, status, string(raw))
		require.Contains(t, string(raw), `"code":"order_not_cancelable"`)
		expiredAsThePassLeavesIt(t, myOrder(t, a, created.ID))
		require.Empty(t, p.effects(t))
	})

	t.Run("OR-J12-S70/card-save-past-its-cutoff-is-removed-before-the-pass", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		confirmed := saveMethod(t, a, p, a.aliceToken, "4242", "auth")
		require.Equal(t, billing.PaymentMethodRequiresAction, confirmed.Status)
		resaved := saveMethod(t, a, p, a.aliceToken, "5556", "auth")
		require.Equal(t, billing.PaymentMethodRequiresAction, resaved.Status)
		// The bank finished both challenges; the customer came back too late.
		for _, saved := range []*billing.PaymentMethod{confirmed, resaved} {
			setup, _, ok := strings.Cut(saved.NextAction.Payload["client_secret"], "_secret_")
			require.True(t, ok)
			require.True(t, p.core.AuthenticateSetup(setup))
		}
		a.clock.Advance(24*time.Hour + time.Second)

		status, raw := a.request(t, http.MethodPost, "/v1/me/payment-methods/"+confirmed.ID.String()+"/confirm", a.aliceToken, billing.ConfirmPaymentMethodParams{})
		require.Equal(t, http.StatusOK, status, string(raw))
		var removed billing.PaymentMethod
		require.NoError(t, json.Unmarshal(raw, &removed))
		require.Equal(t, confirmed.ID, removed.ID)
		require.Equal(t, billing.PaymentMethodRemoved, removed.Status)
		require.Nil(t, removed.NextAction)
		setups := p.core.Submitted("/v1/setup_intents")
		require.Len(t, setups, 2)
		status, raw = a.request(t, http.MethodPost, "/v1/me/payment-methods", a.aliceToken, billing.CreatePaymentMethodParams{PSPID: *resaved.PSPID, Token: setups[1].Form.Get("payment_method")})
		require.Equal(t, http.StatusCreated, status, string(raw))
		require.NoError(t, json.Unmarshal(raw, &removed))
		require.Equal(t, resaved.ID, removed.ID)
		require.Equal(t, billing.PaymentMethodRemoved, removed.Status)
		status, raw = a.request(t, http.MethodGet, "/v1/me/payment-methods", a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var methods billing.ListPage[billing.PaymentMethod]
		require.NoError(t, json.Unmarshal(raw, &methods))
		require.Len(t, methods.Items, 2)
		for _, method := range methods.Items {
			require.Equal(t, billing.PaymentMethodRemoved, method.Status)
		}
		require.Len(t, p.core.Submitted("/v1/setup_intents"), 2, "a lapsed save is never set up again")
		require.Empty(t, p.effects(t))
	})
}

// orderTTL is a customer order's lifetime, plus the instant that ends it.
const orderTTL = 24*time.Hour + time.Second
