//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

// checkoutOf is the staff read of an order's checkout facts.
func checkoutOf(t *testing.T, a *authorityWorld, id billing.OrderID) *billing.OrderCheckout {
	t.Helper()
	order, err := a.alpha.GetOrder(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, order.Checkout, "a checkout carries its facts on staff reads")
	return order.Checkout
}

// openCheckout creates alice's open order of price in person, with headers.
func openCheckout(t *testing.T, a *authorityWorld, price billing.PriceID, attribution *billing.OrderAttribution, headers map[string]string) billing.Order {
	t.Helper()
	status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price}}, Attribution: attribution}, headers)
	require.Contains(t, []int{http.StatusCreated, http.StatusOK}, status, string(raw))
	var order billing.Order
	require.NoError(t, json.Unmarshal(raw, &order))
	return order
}

func payCheckout(t *testing.T, a *authorityWorld, order billing.Order, payment billing.OrderPaymentParams, key string, headers map[string]string) (int, billing.Order) {
	t.Helper()
	all := map[string]string{"Idempotency-Key": key}
	for k, v := range headers {
		all[k] = v
	}
	status, raw := a.request(t, http.MethodPost, "/v1/me/orders/"+order.ID.String()+"/pay", a.aliceToken, billing.PayOrderParams{Payment: payment, ExpectedTotal: order.Total}, all)
	var paid billing.Order
	require.NoError(t, json.Unmarshal(raw, &paid), string(raw))
	return status, paid
}

func trustLoopback(w *world) {
	w.cfg.TrustedProxies = []string{"127.0.0.1/32", "::1/128"}
}

func TestCheckoutFunnel(t *testing.T) {
	t.Run("OR-J47-S01/steps-are-recorded-where-the-customer-takes-them", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		_, price := publicSale(t, a, "funnel-steps", 9_990_000, nil)
		created := openCheckout(t, a, price.ID, nil, nil)
		require.Nil(t, created.Checkout, "the customer never reads their own checkout facts")
		facts := checkoutOf(t, a, created.ID)
		require.Equal(t, 1, facts.PurchaseNumber)
		require.Nil(t, facts.OpenedAt, "creating the order is not opening the payment form")
		require.Nil(t, facts.SubmittedAt)

		a.clock.Advance(time.Minute)
		opened := a.clock.Now()
		status, raw := a.request(t, http.MethodGet, "/v1/me/orders/"+created.ID.String(), a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		facts = checkoutOf(t, a, created.ID)
		require.NotNil(t, facts.OpenedAt)
		require.WithinDuration(t, opened, *facts.OpenedAt, time.Second)
		a.clock.Advance(time.Minute)
		status, raw = a.request(t, http.MethodGet, "/v1/me/orders/"+created.ID.String(), a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		_, err := a.alpha.GetOrder(t.Context(), created.ID)
		require.NoError(t, err)
		again := checkoutOf(t, a, created.ID)
		require.Equal(t, facts.OpenedAt, again.OpenedAt, "only the first read in person opens it")

		a.clock.Advance(time.Minute)
		submitted := a.clock.Now()
		declined := p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "0002", Decline: "insufficient_funds"})
		status, _ = payCheckout(t, a, created, billing.OrderPaymentParams{Token: declined}, "funnel-declined", nil)
		require.Equal(t, http.StatusPaymentRequired, status)
		facts = checkoutOf(t, a, created.ID)
		require.NotNil(t, facts.SubmittedAt, "a declined pay request is still a submission")
		require.WithinDuration(t, submitted, *facts.SubmittedAt, time.Second)
		a.clock.Advance(time.Minute)
		status, paid := payCheckout(t, a, created, billing.OrderPaymentParams{Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}, "funnel-paid", nil)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, billing.OrderComplete, paid.Status)
		facts = checkoutOf(t, a, created.ID)
		require.Equal(t, again.OpenedAt, facts.OpenedAt)
		require.WithinDuration(t, submitted, *facts.SubmittedAt, time.Second, "the first submission is kept")

		// A one-call buy is opened and submitted when it is created.
		input := saleInput(t, a, p)
		status, raw = a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "funnel-one-call"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var bought billing.Order
		require.NoError(t, json.Unmarshal(raw, &bought))
		require.Equal(t, billing.OrderComplete, bought.Status)
		facts = checkoutOf(t, a, bought.ID)
		require.Equal(t, 2, facts.PurchaseNumber)
		require.NotNil(t, facts.OpenedAt)
		require.Equal(t, facts.OpenedAt, facts.SubmittedAt)

		// Renewals and other bills are not checkouts.
		page, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID))})
		require.NoError(t, err)
		require.Len(t, page.Items, 2)
		for _, order := range page.Items {
			require.NotNil(t, order.Checkout, "staff lists carry the facts")
		}
	})
	t.Run("OR-J47-S02/a-pay-url-is-opened-only-by-its-page", func(t *testing.T) {
		var page *httptest.Server
		a, p := newPaymentWorld(t, func(w *world) { page = declareCheckoutPage(t, &w.cfg, &w.deps) })
		serveCheckoutPage(t, page, a.engine)
		unopened, _ := hostPayOrder(t, a, "funnel-unopened", false)
		order, secret := hostPayOrder(t, a, "funnel-opened", false)
		require.Nil(t, checkoutOf(t, a, order.ID).OpenedAt, "the merchant creating the link is not the customer opening it")
		status, _, raw := hostPageRequest(t, page, http.MethodGet, "/v1/me/orders/"+order.ID.String(), secret, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.NotNil(t, checkoutOf(t, a, order.ID).OpenedAt)
		token := p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "0002", Decline: "insufficient_funds"})
		status, _, raw = hostPageRequest(t, page, http.MethodPost, "/v1/me/orders/"+order.ID.String()+"/pay", secret,
			billing.PayOrderParams{Payment: billing.OrderPaymentParams{Token: token}, ExpectedTotal: order.Total}, map[string]string{"Idempotency-Key": "funnel-page-declined"})
		require.Equal(t, http.StatusPaymentRequired, status, string(raw))

		a.clock.Advance(order.ExpiresAt.Sub(a.clock.Now()) + time.Minute)
		for _, id := range []billing.OrderID{unopened.ID, order.ID} {
			closed, err := a.alpha.GetOrder(t.Context(), id)
			require.NoError(t, err)
			require.Equal(t, billing.OrderExpired, closed.Status)
		}
		left := checkoutOf(t, a, unopened.ID)
		require.Nil(t, left.OpenedAt, "abandoned at created: the link was never opened")
		tried := checkoutOf(t, a, order.ID)
		require.NotNil(t, tried.OpenedAt)
		require.NotNil(t, tried.SubmittedAt, "abandoned after a submission")
	})
	t.Run("OR-J47-S04/purchase-number-counts-the-customers-paid-purchases", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		_, first := publicSale(t, a, "funnel-first", 5_000_000, nil)
		_, second := publicSale(t, a, "funnel-second", 6_000_000, nil)
		bought := publicPurchase(t, a, p, first.ID, 1, 5_000_000, "funnel-number-1")
		require.Equal(t, 1, checkoutOf(t, a, bought.ID).PurchaseNumber)
		abandoned, err := a.alphaRemote.CreateOrder(t.Context(), "funnel-number-abandoned", billing.CreateOrderParams{CustomerID: customer, Lines: []billing.OrderLineParams{{PriceID: second.ID}}})
		require.NoError(t, err)
		require.NotNil(t, abandoned.Checkout, "the application reads its own order's checkout facts")
		require.Equal(t, 2, abandoned.Checkout.PurchaseNumber)
		_, err = a.alphaRemote.CancelOrder(t.Context(), abandoned.ID)
		require.NoError(t, err)
		again := publicPurchase(t, a, p, second.ID, 1, 6_000_000, "funnel-number-2")
		require.Equal(t, 2, checkoutOf(t, a, again.ID).PurchaseNumber, "an abandoned checkout is not a purchase")
		third := publicPurchase(t, a, p, first.ID, 1, 5_000_000, "funnel-number-3")
		require.Equal(t, 3, checkoutOf(t, a, third.ID).PurchaseNumber)
		other, err := a.alphaRemote.CreateOrder(t.Context(), "funnel-number-bob", billing.CreateOrderParams{CustomerID: billing.CustomerID(uuid.MustParse(a.bob.ID)), Lines: []billing.OrderLineParams{{PriceID: first.ID}}})
		require.NoError(t, err)
		require.Equal(t, 1, other.Checkout.PurchaseNumber, "purchases are counted per customer")
	})
	t.Run("OR-J47-S05/the-country-is-the-billing-address-then-the-ip-never-the-address", func(t *testing.T) {
		a, p := newPaymentWorld(t, trustLoopback)
		_, price := publicSale(t, a, "funnel-country", 9_990_000, nil)
		_, other := publicSale(t, a, "funnel-country-ip", 9_990_000, nil)
		viaFrance := map[string]string{"CF-IPCountry": "FR"}
		order := openCheckout(t, a, price.ID, nil, viaFrance)
		facts := checkoutOf(t, a, order.ID)
		require.Equal(t, "FR", *facts.IPCountry)
		require.Equal(t, "FR", *facts.Country)
		require.Equal(t, billing.CountrySourceIP, *facts.CountrySource)
		billingDE := &billing.BillingDetails{Address: &billing.BillingAddress{Country: ptr("de")}}
		status, _ := payCheckout(t, a, order, billing.OrderPaymentParams{Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"}), BillingDetails: billingDE}, "funnel-country-pay", map[string]string{"CF-IPCountry": "US"})
		require.Equal(t, http.StatusOK, status)
		facts = checkoutOf(t, a, order.ID)
		require.Equal(t, "DE", *facts.BillingCountry)
		require.Equal(t, "DE", *facts.Country)
		require.Equal(t, billing.CountrySourceBillingAddress, *facts.CountrySource)
		require.Equal(t, "FR", *facts.IPCountry, "the first request's country is kept")

		// Unknown and malformed countries are no evidence.
		unknown := openCheckout(t, a, other.ID, nil, map[string]string{"CF-IPCountry": "XX"})
		facts = checkoutOf(t, a, unknown.ID)
		require.Nil(t, facts.IPCountry)
		require.Nil(t, facts.Country)
		require.Nil(t, facts.CountrySource)

		// A proxy header from a peer that is not a trusted proxy is ignored.
		u, _ := newPaymentWorld(t)
		_, untrusted := publicSale(t, u, "funnel-untrusted", 9_990_000, nil)
		spoofed := openCheckout(t, u, untrusted.ID, nil, viaFrance)
		require.Nil(t, checkoutOf(t, u, spoofed.ID).IPCountry)
	})
	t.Run("OR-J47-S06/the-language-is-the-first-in-person-requests", func(t *testing.T) {
		a, _ := newPaymentWorld(t)
		_, price := publicSale(t, a, "funnel-language", 9_990_000, nil)
		order := openCheckout(t, a, price.ID, nil, map[string]string{"Accept-Language": "pt-BR,pt;q=0.9,en;q=0.5"})
		require.Equal(t, "pt-BR", *checkoutOf(t, a, order.ID).Language)
		status, raw := a.request(t, http.MethodGet, "/v1/me/orders/"+order.ID.String(), a.aliceToken, nil, map[string]string{"Accept-Language": "de"})
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Equal(t, "pt-BR", *checkoutOf(t, a, order.ID).Language, "the first wins")
		for i, header := range []string{"*", "zh-Hant-TW", "es-419;q=0.8, fr;q=0.9", "not a tag!"} {
			want := []string{"", "zh-TW", "fr", ""}[i]
			_, each := publicSale(t, a, "funnel-language-"+string(rune('a'+i)), 9_990_000, nil)
			got := checkoutOf(t, a, openCheckout(t, a, each.ID, nil, map[string]string{"Accept-Language": header}).ID).Language
			if want == "" {
				require.Nil(t, got, header)
				continue
			}
			require.NotNil(t, got, header)
			require.Equal(t, want, *got, header)
		}
	})
	t.Run("OR-J47-S07/attribution-is-bounded-and-the-referrer-keeps-its-host", func(t *testing.T) {
		a, _ := newPaymentWorld(t)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		_, price := publicSale(t, a, "funnel-attribution", 9_990_000, nil)
		_, merchantPrice := publicSale(t, a, "funnel-attribution-app", 9_990_000, nil)
		given := &billing.OrderAttribution{Source: ptr(" newsletter "), Medium: ptr("email"), Campaign: ptr("spring-2026"),
			Referrer: ptr("https://Blog.Example.com/post/42?email=x"), Affiliate: ptr("partner-7")}
		order := openCheckout(t, a, price.ID, given, nil)
		got := checkoutOf(t, a, order.ID).Attribution
		require.Equal(t, "newsletter", *got.Source)
		require.Equal(t, "email", *got.Medium)
		require.Equal(t, "spring-2026", *got.Campaign)
		require.Equal(t, "blog.example.com", *got.Referrer, "never the path or query")
		require.Equal(t, "partner-7", *got.Affiliate)
		reopened := openCheckout(t, a, price.ID, &billing.OrderAttribution{Source: ptr("search")}, nil)
		require.Equal(t, order.ID, reopened.ID)
		require.Equal(t, "newsletter", *checkoutOf(t, a, order.ID).Attribution.Source, "the open order keeps its first attribution")

		app, err := a.alphaRemote.CreateOrder(t.Context(), "funnel-attribution-app", billing.CreateOrderParams{CustomerID: customer, Lines: []billing.OrderLineParams{{PriceID: merchantPrice.ID}},
			Attribution: &billing.OrderAttribution{Affiliate: ptr("reseller-1"), Referrer: ptr("partner.example.org")}})
		require.NoError(t, err)
		require.Equal(t, "reseller-1", *app.Checkout.Attribution.Affiliate)
		require.Equal(t, "partner.example.org", *app.Checkout.Attribution.Referrer)
		require.Nil(t, app.Checkout.Attribution.Source)

		for param, bad := range map[string]billing.OrderAttribution{
			"attribution.source":    {Source: ptr(strings.Repeat("a", 101))},
			"attribution.medium":    {Medium: ptr("line\nbreak")},
			"attribution.campaign":  {Campaign: ptr("alice@example.com")},
			"attribution.affiliate": {Affiliate: ptr("4242424242424242")},
			"attribution.referrer":  {Referrer: ptr("not a host/path")},
		} {
			status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: merchantPrice.ID}}, Attribution: &bad})
			require.Equal(t, http.StatusBadRequest, status, string(raw))
			require.Contains(t, string(raw), "invalid_param")
			require.Contains(t, string(raw), param)
		}
	})
	t.Run("OR-J47-S09/an-unpaid-checkout-keeps-its-facts-for-397-days", func(t *testing.T) {
		a, _ := newPaymentWorld(t, trustLoopback)
		w := a.world
		fleet := hostFleet(t, w)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, w.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		_, price := publicSale(t, a, "funnel-retained", 9_990_000, nil)
		order := openCheckout(t, a, price.ID, &billing.OrderAttribution{Campaign: ptr("retained")}, map[string]string{"CF-IPCountry": "JP", "Accept-Language": "ja"})
		status, raw := a.request(t, http.MethodGet, "/v1/me/orders/"+order.ID.String(), a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		w.clock.Advance(25 * time.Hour)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		w.clock.Advance(91 * 24 * time.Hour)
		runRegistered(t, fleet, "openrails.cleanup_expired_data", 1)
		facts := checkoutOf(t, a, order.ID)
		require.NotNil(t, facts.OpenedAt, "an abandoned checkout outlives 90 days")
		require.Equal(t, "JP", *facts.Country)
		require.Equal(t, "ja", *facts.Language)
		require.Equal(t, "retained", *facts.Attribution.Campaign)
		w.clock.Advance(306 * 24 * time.Hour)
		runRegistered(t, fleet, "openrails.cleanup_expired_data", 1)
		_, err := a.alpha.GetOrder(t.Context(), order.ID)
		require.ErrorIs(t, err, billing.ErrNotFound, "397 days after it closed it is gone")
	})
}
