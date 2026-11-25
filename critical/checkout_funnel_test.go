//go:build integration

package critical_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/open-rails/openrails/internal/captcha"
	"github.com/open-rails/openrails/internal/modules/metrics"
	"github.com/open-rails/openrails/internal/sqlschema"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

// checkoutOf is the staff read of an invoice's checkout facts.
func checkoutOf(t *testing.T, a *authorityWorld, id billing.InvoiceID) *billing.InvoiceCheckout {
	t.Helper()
	invoice, err := a.alpha.GetInvoice(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, invoice.Checkout, "a checkout carries its facts on staff reads")
	return invoice.Checkout
}

// openCheckout creates alice's open invoice of price in person, with headers.
func openCheckout(t *testing.T, a *authorityWorld, price billing.PriceID, attribution *billing.InvoiceAttribution, headers map[string]string) billing.Invoice {
	t.Helper()
	status, raw := a.request(t, http.MethodPost, "/v1/me/invoices", a.aliceToken, billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: price}}, Attribution: attribution}, headers)
	require.Contains(t, []int{http.StatusCreated, http.StatusOK}, status, string(raw))
	var invoice billing.Invoice
	require.NoError(t, json.Unmarshal(raw, &invoice))
	return invoice
}

func payCheckout(t *testing.T, a *authorityWorld, invoice billing.Invoice, payment billing.InvoicePaymentParams, key string, headers map[string]string) (int, billing.Invoice) {
	t.Helper()
	all := map[string]string{"Idempotency-Key": key}
	for k, v := range headers {
		all[k] = v
	}
	status, raw := a.request(t, http.MethodPost, "/v1/me/invoices/"+invoice.ID.String()+"/pay", a.aliceToken, billing.PayInvoiceParams{Payment: payment, ExpectedTotal: invoice.Total}, all)
	var paid billing.Invoice
	require.NoError(t, json.Unmarshal(raw, &paid), string(raw))
	return status, paid
}

// cloudflareEdge is an address in Cloudflare's published egress ranges.
const cloudflareEdge = "173.245.48.1"

// behindCloudflare puts the test client behind the merchant's own proxy
// (loopback, a trusted proxy), which Cloudflare fronts.
func behindCloudflare(w *world) {
	w.cfg.TrustedProxies = []string{"127.0.0.1/32", "::1/128"}
}

// viaCloudflare is the X-Forwarded-For of client's request through
// Cloudflare's edge and the merchant's proxy.
func viaCloudflare(client string) string { return client + ", " + cloudflareEdge }

// underAttack is the precondition of a card-testing attack on the world's
// merchant: its captcha challenges every pay request until cleared.
func underAttack(t *testing.T, a *authorityWorld) (clear func()) {
	t.Helper()
	store := captcha.NewChallengeStore(slog.New(slog.DiscardHandler), abusestate.New(slog.New(slog.DiscardHandler), a.deps.Redis))
	subject := captcha.CardAttackModeSubject(a.alpha.MerchantID().UUID())
	store.MarkChallenged(t.Context(), subject, time.Hour)
	return func() { store.ClearChallenged(t.Context(), subject) }
}

// readIndexes is the indexes the plan of sql reads billing.invoices through,
// with sequential scans off: a sequential scan of the merchant's invoices
// fails it.
func readIndexes(t *testing.T, a *authorityWorld, sql string, args []any) []string {
	t.Helper()
	sql, err := sqlschema.Rewrite(sql, a.schema)
	require.NoError(t, err)
	tx, err := a.pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(t.Context()) }()
	_, err = tx.Exec(t.Context(), "SET LOCAL enable_seqscan = off")
	require.NoError(t, err)
	var explained string
	require.NoError(t, tx.QueryRow(t.Context(), "EXPLAIN (FORMAT JSON) "+sql, args...).Scan(&explained))
	var plans []struct {
		Plan map[string]any `json:"Plan"`
	}
	require.NoError(t, json.Unmarshal([]byte(explained), &plans))
	var out []string
	var walk func(n map[string]any)
	walk = func(n map[string]any) {
		require.False(t, n["Node Type"] == "Seq Scan" && n["Relation Name"] == "invoices", "invoices are scanned whole: %s", explained)
		if index, ok := n["Index Name"].(string); ok && strings.HasPrefix(index, "invoices_") {
			out = append(out, index)
		}
		children, _ := n["Plans"].([]any)
		for _, c := range children {
			if child, ok := c.(map[string]any); ok {
				walk(child)
			}
		}
	}
	walk(plans[0].Plan)
	require.NotEmpty(t, out, "invoices are read through an index: %s", explained)
	return out
}

func TestCheckoutFunnel(t *testing.T) {
	t.Run("OR-J47-S01/steps-are-recorded-where-the-customer-takes-them", func(t *testing.T) {
		a, p := newPaymentWorld(t, func(w *world) {
			w.deps.Redis = criticalRedis(t)
			w.cfg.Captcha = billing.CaptchaAccount{Provider: "turnstile", SiteKey: "critical-site", SecretKey: "critical-secret"}
			w.cfg.RateLimits = billing.RateLimitsConfig{Purchase: &billing.RateLimit{RequestsPerMinute: 1000}}
		})
		_, price := publicSale(t, a, "funnel-steps", 9_990_000, nil)
		created := openCheckout(t, a, price.ID, nil, nil)
		require.Nil(t, created.Checkout, "the customer never reads their own checkout facts")
		facts := checkoutOf(t, a, created.ID)
		require.Equal(t, 1, facts.PurchaseNumber)
		require.Nil(t, facts.OpenedAt, "creating the invoice is not opening the payment form")
		require.Nil(t, facts.SubmittedAt)

		a.clock.Advance(time.Minute)
		opened := a.clock.Now()
		status, raw := a.request(t, http.MethodGet, "/v1/me/invoices/"+created.ID.String(), a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		facts = checkoutOf(t, a, created.ID)
		require.NotNil(t, facts.OpenedAt)
		require.WithinDuration(t, opened, *facts.OpenedAt, time.Second)
		a.clock.Advance(time.Minute)
		status, raw = a.request(t, http.MethodGet, "/v1/me/invoices/"+created.ID.String(), a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		_, err := a.alpha.GetInvoice(t.Context(), created.ID)
		require.NoError(t, err)
		again := checkoutOf(t, a, created.ID)
		require.Equal(t, facts.OpenedAt, again.OpenedAt, "only the first read in person opens it")

		// The attack captcha refuses the pay request ahead of its handler; the
		// customer still asked to pay.
		a.clock.Advance(time.Minute)
		submitted := a.clock.Now()
		clear := underAttack(t, a)
		token := p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})
		status, raw = a.request(t, http.MethodPost, "/v1/me/invoices/"+created.ID.String()+"/pay", a.aliceToken,
			billing.PayInvoiceParams{Payment: billing.InvoicePaymentParams{Token: token}, ExpectedTotal: created.Total}, map[string]string{"Idempotency-Key": "funnel-challenged"})
		require.Equal(t, http.StatusForbidden, status, string(raw))
		require.Contains(t, string(raw), "captcha_required")
		require.Empty(t, p.effects(t), "the challenged request charged nothing")
		facts = checkoutOf(t, a, created.ID)
		require.NotNil(t, facts.SubmittedAt, "a challenged pay request is still a submission")
		require.WithinDuration(t, submitted, *facts.SubmittedAt, time.Second)
		clear()

		a.clock.Advance(time.Minute)
		declined := p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "0002", Decline: "insufficient_funds"})
		status, _ = payCheckout(t, a, created, billing.InvoicePaymentParams{Token: declined}, "funnel-declined", nil)
		require.Equal(t, http.StatusPaymentRequired, status)
		facts = checkoutOf(t, a, created.ID)
		require.WithinDuration(t, submitted, *facts.SubmittedAt, time.Second, "the first submission is kept")
		a.clock.Advance(time.Minute)
		status, paid := payCheckout(t, a, created, billing.InvoicePaymentParams{Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}, "funnel-paid", nil)
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, billing.InvoiceComplete, paid.Status)
		facts = checkoutOf(t, a, created.ID)
		require.Equal(t, again.OpenedAt, facts.OpenedAt)
		require.WithinDuration(t, submitted, *facts.SubmittedAt, time.Second, "the first submission is kept")

		// A one-call buy is opened and submitted when it is created.
		input := saleInput(t, a, p)
		status, raw = a.request(t, http.MethodPost, "/v1/me/invoices", a.aliceToken, input, map[string]string{"Idempotency-Key": "funnel-one-call"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var bought billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &bought))
		require.Equal(t, billing.InvoiceComplete, bought.Status)
		facts = checkoutOf(t, a, bought.ID)
		require.Equal(t, 2, facts.PurchaseNumber)
		require.NotNil(t, facts.OpenedAt)
		require.Equal(t, facts.OpenedAt, facts.SubmittedAt)

		// Renewals and other bills are not checkouts.
		page, err := a.alpha.ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID))})
		require.NoError(t, err)
		require.Len(t, page.Items, 2)
		for _, invoice := range page.Items {
			require.NotNil(t, invoice.Checkout, "staff lists carry the facts")
		}
	})
	t.Run("OR-J47-S02/a-pay-url-is-opened-only-by-its-page", func(t *testing.T) {
		var page *httptest.Server
		a, p := newPaymentWorld(t, func(w *world) { page = declareCheckoutPage(t, &w.cfg, &w.deps) })
		serveCheckoutPage(t, page, a.engine)
		unopened, _ := hostPayInvoice(t, a, "funnel-unopened", false)
		invoice, secret := hostPayInvoice(t, a, "funnel-opened", false)
		require.Nil(t, checkoutOf(t, a, invoice.ID).OpenedAt, "the merchant creating the link is not the customer opening it")
		status, _, raw := hostPageRequest(t, page, http.MethodGet, "/v1/me/invoices/"+invoice.ID.String(), secret, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.NotNil(t, checkoutOf(t, a, invoice.ID).OpenedAt)
		token := p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "0002", Decline: "insufficient_funds"})
		status, _, raw = hostPageRequest(t, page, http.MethodPost, "/v1/me/invoices/"+invoice.ID.String()+"/pay", secret,
			billing.PayInvoiceParams{Payment: billing.InvoicePaymentParams{Token: token}, ExpectedTotal: invoice.Total}, map[string]string{"Idempotency-Key": "funnel-page-declined"})
		require.Equal(t, http.StatusPaymentRequired, status, string(raw))

		a.clock.Advance(invoice.ExpiresAt.Sub(a.clock.Now()) + time.Minute)
		for _, id := range []billing.InvoiceID{unopened.ID, invoice.ID} {
			closed, err := a.alpha.GetInvoice(t.Context(), id)
			require.NoError(t, err)
			require.Equal(t, billing.InvoiceExpired, closed.Status)
		}
		left := checkoutOf(t, a, unopened.ID)
		require.Nil(t, left.OpenedAt, "abandoned at created: the link was never opened")
		tried := checkoutOf(t, a, invoice.ID)
		require.NotNil(t, tried.OpenedAt)
		require.NotNil(t, tried.SubmittedAt, "abandoned after a submission")
	})
	t.Run("OR-J47-S03/checkout-measures-read-the-funnel-as-it-happened", func(t *testing.T) {
		a, p := newPaymentWorld(t, behindCloudflare)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		sale := func(key string) billing.PriceID {
			_, price := publicSale(t, a, key, 10_000_000, nil)
			return price.ID
		}
		via := func(country string, more map[string]string) map[string]string {
			out := map[string]string{"CF-IPCountry": country, "X-Forwarded-For": viaCloudflare("203.0.113.90")}
			for k, v := range more {
				out[k] = v
			}
			return out
		}
		as := func(token string, method, path string, body any) {
			status, raw := a.request(t, method, path, token, body)
			require.Equal(t, http.StatusOK, status, string(raw))
		}
		card := func() string { return p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"}) }

		// Alice buys at once from France, from a newsletter: her first purchase.
		input := billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: sale("measures-first")}}, ExpectedTotal: ptr(int64(10_000_000)), Reusable: ptr(true),
			Payment: &billing.InvoicePaymentParams{Token: card()}, Attribution: &billing.InvoiceAttribution{Source: ptr("newsletter")}}
		status, raw := a.request(t, http.MethodPost, "/v1/me/invoices", a.aliceToken, input, via("FR", map[string]string{"Idempotency-Key": "measures-first"}))
		require.Equal(t, http.StatusCreated, status, string(raw))
		// She opens a second checkout from Germany and pays it.
		second := openCheckout(t, a, sale("measures-second"), &billing.InvoiceAttribution{Source: ptr("search")}, via("DE", nil))
		as(a.aliceToken, http.MethodGet, "/v1/me/invoices/"+second.ID.String(), nil)
		status, _ = payCheckout(t, a, second, billing.InvoicePaymentParams{Token: card()}, "measures-second", nil)
		require.Equal(t, http.StatusOK, status)
		// Bob opens one and leaves.
		status, raw = a.request(t, http.MethodPost, "/v1/me/invoices", a.bobToken, billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: sale("measures-left")}}}, via("FR", nil))
		require.Equal(t, http.StatusCreated, status, string(raw))
		var left billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &left))
		as(a.bobToken, http.MethodGet, "/v1/me/invoices/"+left.ID.String(), nil)
		as(a.bobToken, http.MethodPost, "/v1/me/invoices/"+left.ID.String()+"/cancel", nil)
		// Alice tries a card that is declined, and leaves.
		tried := openCheckout(t, a, sale("measures-tried"), nil, nil)
		as(a.aliceToken, http.MethodGet, "/v1/me/invoices/"+tried.ID.String(), nil)
		status, _ = payCheckout(t, a, tried, billing.InvoicePaymentParams{Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "0002", Decline: "insufficient_funds"})}, "measures-tried", nil)
		require.Equal(t, http.StatusPaymentRequired, status)
		as(a.aliceToken, http.MethodPost, "/v1/me/invoices/"+tried.ID.String()+"/cancel", nil)
		// A link the merchant sent her expires unopened.
		_, err := a.alphaRemote.CreateInvoice(t.Context(), "measures-unopened", billing.CreateInvoiceParams{CustomerID: customer,
			Lines: []billing.InvoiceLineParams{{PriceID: sale("measures-unopened")}}, ExpiresAt: ptr(a.clock.Now().Add(time.Hour))})
		require.NoError(t, err)
		a.clock.Advance(2 * time.Hour)
		// One is open, its form loaded.
		pending := openCheckout(t, a, sale("measures-open"), nil, nil)
		as(a.aliceToken, http.MethodGet, "/v1/me/invoices/"+pending.ID.String(), nil)

		window := &billing.MetricsRange{Last: "7d"}
		usd := map[string][]string{"currency": {"USD"}}
		funnel := map[string]int64{}
		for _, row := range metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"checkouts"}, By: []string{"step", "outcome"}, Range: window}) {
			if n := whole(t, row["checkouts"]); n != 0 {
				funnel[fmt.Sprint(row["step"])+"/"+fmt.Sprint(row["outcome"])] = n
			}
		}
		require.Equal(t, map[string]int64{"paid/paid": 2, "submitted/abandoned": 1, "opened/abandoned": 1, "created/abandoned": 1, "opened/open": 1}, funnel)
		rates := only(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"checkout_conversion_rate", "checkout_submission_rate", "customer_conversion_rate"}, Range: window}))
		require.InDelta(t, 2.0/4, fraction(t, rates["checkout_conversion_rate"]), 1e-12, "paid / settled opened: the unopened link and the open one are out")
		require.InDelta(t, 3.0/4, fraction(t, rates["checkout_submission_rate"]), 1e-12)
		require.InDelta(t, 1.0/2, fraction(t, rates["customer_conversion_rate"]), 1e-12, "alice paid, bob left")
		require.Equal(t, map[string]int64{"paid": 20_000_000, "abandoned": 30_000_000, "open": 10_000_000},
			split(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"checkout_value"}, By: []string{"outcome"}, Filters: usd, Range: window}), "outcome", "checkout_value"))

		sales := only(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"sales", "sales_customers", "repeat_purchase_rate"}, Range: window}))
		require.EqualValues(t, 2, whole(t, sales["sales"]))
		require.EqualValues(t, 1, whole(t, sales["sales_customers"]))
		require.InDelta(t, 1.0, fraction(t, sales["repeat_purchase_rate"]), 1e-12, "her second purchase makes her a repeat buyer")
		require.Equal(t, map[string]int64{"1": 1, "2": 1},
			split(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"sales"}, By: []string{"purchase_number"}, Range: window}), "purchase_number", "sales"))

		// Where purchases and cash come from: country and source.
		require.Equal(t, map[string]int64{"FR": 10_000_000, "DE": 10_000_000},
			split(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"cash"}, By: []string{"country"}, Filters: usd, Range: window}), "country", "cash"))
		require.Equal(t, map[string]int64{"newsletter": 1, "search": 1},
			split(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"sales"}, By: []string{"attribution_source"}, Range: window}), "attribution_source", "sales"))
		require.Equal(t, map[string]int64{"newsletter": 10_000_000, "search": 10_000_000},
			split(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"cash"}, By: []string{"attribution_source"}, Filters: usd, Range: window}), "attribution_source", "cash"))

		// Checkouts and sales read their own indexes, never every invoice.
		for measure, index := range map[string][]string{"checkouts": {"invoices_checkout_created_at_idx", "invoices_created_at_id_idx"}, "sales": {"invoices_sale_completed_at_idx"}} {
			q := billing.QueryMetricsParams{Measures: []string{measure}, By: []string{"country"}, Range: window}
			plan, refused := metrics.ValidateIn(&q, a.clock.Now(), time.UTC)
			require.Nil(t, refused)
			statements, err := metrics.Statements(plan, a.alpha.MerchantID().UUID())
			require.NoError(t, err)
			for _, st := range statements {
				require.Subset(t, index, readIndexes(t, a, st.SQL, st.Args), "%s: %s", measure, st.SQL)
			}
		}

		// The overview carries the checkout section, and each of its widgets answers.
		board, err := a.alpha.GetDashboard(t.Context())
		require.NoError(t, err)
		require.True(t, board.Profile.Checkouts)
		ids := map[string]bool{}
		for _, w := range board.Widgets {
			ids[w.ID] = true
		}
		for _, id := range []string{"kpi-checkout-conversion", "section-checkouts", "checkout-funnel", "customers-by-type", "repeat-purchase-rate", "sales-by-country", "sales-by-source"} {
			require.True(t, ids[id], id)
		}
		overviewAnswers(t, a.alpha, board.Widgets, "USD")
		_, err = a.alpha.SetDashboard(t.Context(), billing.SetDashboardParams{Widgets: board.Widgets})
		require.NoError(t, err, "the overview is a layout the merchant may save")
		notFunnel := billing.DashboardWidget{ID: "cash-funnel", Title: "Cash", Viz: "funnel", Grid: billing.DashboardGrid{W: 6, H: 4},
			Query: billing.QueryMetricsParams{Measures: []string{"cash"}, By: []string{"cash_kind"}, Range: window}}
		_, err = a.alpha.SetDashboard(t.Context(), billing.SetDashboardParams{Widgets: []billing.DashboardWidget{notFunnel}})
		var refusal *billing.StatusError
		require.ErrorAs(t, err, &refusal)
		require.Equal(t, billing.CodeDashboardInvalid, refusal.Code)
		require.Contains(t, fmt.Sprint(refusal.Metadata), "funnel_shape", "a funnel draws a count, never money")
	})
	t.Run("OR-J47-S04/purchase-number-counts-the-customers-paid-purchases", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		_, first := publicSale(t, a, "funnel-first", 5_000_000, nil)
		_, second := publicSale(t, a, "funnel-second", 6_000_000, nil)
		bought := publicPurchase(t, a, p, first.ID, 1, 5_000_000, "funnel-number-1")
		require.Equal(t, 1, checkoutOf(t, a, bought.ID).PurchaseNumber)
		abandoned, err := a.alphaRemote.CreateInvoice(t.Context(), "funnel-number-abandoned", billing.CreateInvoiceParams{CustomerID: customer, Lines: []billing.InvoiceLineParams{{PriceID: second.ID}}})
		require.NoError(t, err)
		require.NotNil(t, abandoned.Checkout, "the application reads its own invoice's checkout facts")
		require.Equal(t, 2, abandoned.Checkout.PurchaseNumber)
		_, err = a.alphaRemote.CancelInvoice(t.Context(), abandoned.ID)
		require.NoError(t, err)
		again := publicPurchase(t, a, p, second.ID, 1, 6_000_000, "funnel-number-2")
		require.Equal(t, 2, checkoutOf(t, a, again.ID).PurchaseNumber, "an abandoned checkout is not a purchase")

		// A refunded purchase was still bought.
		_, err = a.alpha.RefundPayment(t.Context(), "funnel-number-refund", *again.Payment.PaymentID, billing.RefundPaymentParams{Full: true, Reason: "Refund the second purchase"})
		require.NoError(t, err)
		third := publicPurchase(t, a, p, first.ID, 1, 5_000_000, "funnel-number-3")
		require.Equal(t, 3, checkoutOf(t, a, third.ID).PurchaseNumber)

		// Two checkouts open at once share the number neither has paid yet.
		left, err := a.alphaRemote.CreateInvoice(t.Context(), "funnel-number-open-a", billing.CreateInvoiceParams{CustomerID: customer, Lines: []billing.InvoiceLineParams{{PriceID: first.ID}}})
		require.NoError(t, err)
		right, err := a.alphaRemote.CreateInvoice(t.Context(), "funnel-number-open-b", billing.CreateInvoiceParams{CustomerID: customer, Lines: []billing.InvoiceLineParams{{PriceID: second.ID}}})
		require.NoError(t, err)
		require.Equal(t, 4, left.Checkout.PurchaseNumber)
		require.Equal(t, 4, right.Checkout.PurchaseNumber)

		other, err := a.alphaRemote.CreateInvoice(t.Context(), "funnel-number-bob", billing.CreateInvoiceParams{CustomerID: billing.CustomerID(uuid.MustParse(a.bob.ID)), Lines: []billing.InvoiceLineParams{{PriceID: first.ID}}})
		require.NoError(t, err)
		require.Equal(t, 1, other.Checkout.PurchaseNumber, "purchases are counted per customer")
	})
	t.Run("OR-J47-S05/the-country-is-the-billing-address-then-the-ip-never-the-address", func(t *testing.T) {
		a, p := newPaymentWorld(t, behindCloudflare)
		_, price := publicSale(t, a, "funnel-country", 9_990_000, nil)
		_, other := publicSale(t, a, "funnel-country-ip", 9_990_000, nil)
		// The client's own address reaches OpenRails only in X-Forwarded-For.
		const client = "203.0.113.77"
		viaFrance := map[string]string{"CF-IPCountry": "FR", "X-Forwarded-For": viaCloudflare(client)}
		invoice := openCheckout(t, a, price.ID, nil, viaFrance)
		facts := checkoutOf(t, a, invoice.ID)
		require.Equal(t, "FR", *facts.IPCountry)
		require.Equal(t, "FR", *facts.Country)
		require.Equal(t, billing.CountrySourceIP, *facts.CountrySource)
		require.Nil(t, facts.CardCountry, "card country is not recorded yet")
		billingDE := &billing.BillingDetails{Address: &billing.BillingAddress{Country: ptr("de")}}
		status, _ := payCheckout(t, a, invoice, billing.InvoicePaymentParams{Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"}), BillingDetails: billingDE}, "funnel-country-pay", map[string]string{"CF-IPCountry": "US", "X-Forwarded-For": viaCloudflare(client)})
		require.Equal(t, http.StatusOK, status)
		facts = checkoutOf(t, a, invoice.ID)
		require.Equal(t, "DE", *facts.BillingCountry)
		require.Equal(t, "DE", *facts.Country)
		require.Equal(t, billing.CountrySourceBillingAddress, *facts.CountrySource)
		require.Equal(t, "FR", *facts.IPCountry, "the first request's country is kept")

		// Unknown and malformed countries are no evidence.
		unknown := openCheckout(t, a, other.ID, nil, map[string]string{"CF-IPCountry": "XX", "X-Forwarded-For": viaCloudflare(client)})
		facts = checkoutOf(t, a, unknown.ID)
		require.Nil(t, facts.IPCountry)
		require.Nil(t, facts.Country)
		require.Nil(t, facts.CountrySource)

		// A request that did not come through Cloudflare's edge is never read
		// for a country, though a trusted proxy relays the header.
		_, untrusted := publicSale(t, a, "funnel-untrusted", 9_990_000, nil)
		spoofed := openCheckout(t, a, untrusted.ID, nil, map[string]string{"CF-IPCountry": "FR", "X-Forwarded-For": client})
		require.Nil(t, checkoutOf(t, a, spoofed.ID).IPCountry)

		// Neither the invoice nor its archived row holds the address; the export
		// comes last, as it leaves the merchant's writes readonly.
		staff, raw := a.request(t, http.MethodGet, "/v1/admin/invoices/"+invoice.ID.String(), a.alphaKey.Secret, nil)
		require.Equal(t, http.StatusOK, staff, string(raw))
		require.NotContains(t, string(raw), client)
		table, archived := "", 0
		_, err := archivewire.Read(bytes.NewReader(archiveBytes(t, a.alpha)), func(archivewire.Header) error { return nil }, func(r archivewire.Record) error {
			switch {
			case r.Kind == "table":
				table = r.Table
			case r.Kind == "row" && table == "invoices":
				archived++
				for _, v := range r.Values {
					if v != nil {
						require.NotContains(t, *v, client)
					}
				}
			}
			return nil
		})
		require.NoError(t, err)
		require.Positive(t, archived)
	})
	t.Run("OR-J47-S06/the-language-is-the-first-in-person-requests", func(t *testing.T) {
		a, _ := newPaymentWorld(t)
		_, price := publicSale(t, a, "funnel-language", 9_990_000, nil)
		invoice := openCheckout(t, a, price.ID, nil, map[string]string{"Accept-Language": "pt-BR,pt;q=0.9,en;q=0.5"})
		require.Equal(t, "pt-BR", *checkoutOf(t, a, invoice.ID).Language)
		status, raw := a.request(t, http.MethodGet, "/v1/me/invoices/"+invoice.ID.String(), a.aliceToken, nil, map[string]string{"Accept-Language": "de"})
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Equal(t, "pt-BR", *checkoutOf(t, a, invoice.ID).Language, "the first wins")
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
	t.Run("OR-J47-S07/attribution-is-kept-bounded-and-never-refuses-a-sale", func(t *testing.T) {
		a, _ := newPaymentWorld(t)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		_, price := publicSale(t, a, "funnel-attribution", 9_990_000, nil)
		_, creator := publicSale(t, a, "funnel-attribution-creator", 9_990_000, nil)
		_, merchantPrice := publicSale(t, a, "funnel-attribution-app", 9_990_000, nil)
		given := &billing.InvoiceAttribution{Source: ptr(" newsletter "), Medium: ptr("email"), Campaign: ptr("spring%2D2026"),
			Term: ptr("running shoes"), Content: ptr("banner%20a"), Referrer: ptr("https://Blog.Example.com/post/42?email=x"), Affiliate: ptr("partner-7")}
		invoice := openCheckout(t, a, price.ID, given, nil)
		got := checkoutOf(t, a, invoice.ID).Attribution
		require.Equal(t, "newsletter", *got.Source)
		require.Equal(t, "email", *got.Medium)
		require.Equal(t, "spring-2026", *got.Campaign, "percent escapes are decoded")
		require.Equal(t, "running shoes", *got.Term)
		require.Equal(t, "banner a", *got.Content)
		require.Equal(t, "blog.example.com", *got.Referrer, "never the path or query")
		require.Equal(t, "partner-7", *got.Affiliate)
		reopened := openCheckout(t, a, price.ID, &billing.InvoiceAttribution{Source: ptr("search")}, nil)
		require.Equal(t, invoice.ID, reopened.ID)
		require.Equal(t, "newsletter", *checkoutOf(t, a, invoice.ID).Attribution.Source, "the open invoice keeps its first attribution")

		// An @creator source still makes the invoice, without that field.
		status, raw := a.request(t, http.MethodPost, "/v1/me/invoices", a.aliceToken, billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: creator.ID}},
			Attribution: &billing.InvoiceAttribution{Source: ptr("@creator"), Campaign: ptr("launch")}})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var made billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &made))
		kept := checkoutOf(t, a, made.ID).Attribution
		require.Nil(t, kept.Source)
		require.Equal(t, "launch", *kept.Campaign)

		app, err := a.alphaRemote.CreateInvoice(t.Context(), "funnel-attribution-app", billing.CreateInvoiceParams{CustomerID: customer, Lines: []billing.InvoiceLineParams{{PriceID: merchantPrice.ID}},
			Attribution: &billing.InvoiceAttribution{Affiliate: ptr("reseller-1"), Referrer: ptr("partner_shop.example.org:8443/landing")}})
		require.NoError(t, err)
		require.Equal(t, "reseller-1", *app.Checkout.Attribution.Affiliate)
		require.Equal(t, "partner_shop.example.org", *app.Checkout.Attribution.Referrer)
		require.Nil(t, app.Checkout.Attribution.Source)

		// Each of these is left out; the rest of the invoice stands.
		for i, bad := range []billing.InvoiceAttribution{
			{Source: ptr(strings.Repeat("a", 101))},
			{Medium: ptr("line\nbreak")},
			{Medium: ptr("zero\u200bwidth")},
			{Medium: ptr("bidi\u202eflip")},
			{Campaign: ptr("alice@example.com")},
			{Campaign: ptr("alice\uff20example.com")},
			{Campaign: ptr("alice\ufe6bexample.com")},
			{Campaign: ptr("alice%40example.com")},
			{Campaign: ptr("alice%2540example.com")},
			{Term: ptr("alice@example.com")},
			{Content: ptr(strings.Repeat(" ", 4096) + "short")},
			{Affiliate: ptr("4242424242424242")},
			{Affiliate: ptr("4242.4242.4242.4242")},
			{Affiliate: ptr("4242/4242/4242/4242")},
			{Affiliate: ptr("\uff14\uff12\uff14\uff12\uff14\uff12\uff14\uff12\uff14\uff12\uff14\uff12\uff14\uff12\uff14\uff12")},
			{Referrer: ptr("https://[2001:db8::1]/")},
			{Referrer: ptr("https://exa%zzmple.com/")},
			{Referrer: ptr("not a host")},
		} {
			bad.Source = coalesce(bad.Source, ptr("kept"))
			made, err := a.alphaRemote.CreateInvoice(t.Context(), "funnel-attribution-bad-"+string(rune('a'+i)), billing.CreateInvoiceParams{CustomerID: customer, Lines: []billing.InvoiceLineParams{{PriceID: merchantPrice.ID}}, Attribution: &bad})
			require.NoError(t, err, "case %d", i)
			got := made.Checkout.Attribution
			for _, field := range []*string{got.Medium, got.Campaign, got.Term, got.Content, got.Referrer, got.Affiliate} {
				require.Nil(t, field, "case %d is left out", i)
			}
			if i == 0 {
				require.Nil(t, got.Source)
			} else {
				require.Equal(t, "kept", *got.Source, "case %d keeps the other fields", i)
			}
		}
		require.Equal(t, "50%off", *firstCampaign(t, a, customer, merchantPrice.ID, "50%off"), "a lone % stays as written")
	})
	t.Run("OR-J47-S09/an-unpaid-checkout-keeps-its-facts-for-397-days", func(t *testing.T) {
		a, _ := newPaymentWorld(t, behindCloudflare)
		w := a.world
		fleet := hostFleet(t, w)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, w.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		_, price := publicSale(t, a, "funnel-retained", 9_990_000, nil)
		invoice := openCheckout(t, a, price.ID, &billing.InvoiceAttribution{Campaign: ptr("retained")}, map[string]string{"CF-IPCountry": "JP", "X-Forwarded-For": viaCloudflare("203.0.113.81"), "Accept-Language": "ja"})
		status, raw := a.request(t, http.MethodGet, "/v1/me/invoices/"+invoice.ID.String(), a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		w.clock.Advance(25 * time.Hour)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		w.clock.Advance(91 * 24 * time.Hour)
		runRegistered(t, fleet, "openrails.cleanup_expired_data", 1)
		facts := checkoutOf(t, a, invoice.ID)
		require.NotNil(t, facts.OpenedAt, "an abandoned checkout outlives 90 days")
		require.Equal(t, "JP", *facts.Country)
		require.Equal(t, "ja", *facts.Language)
		require.Equal(t, "retained", *facts.Attribution.Campaign)
		// The merchant's archive carries the facts to a restore.
		_, restored := archiveTarget(t, w)
		_, err := restored.ImportBillingArchive(t.Context(), bytes.NewReader(archiveBytes(t, a.alpha)))
		require.NoError(t, err)
		moved, err := restored.GetInvoice(t.Context(), invoice.ID)
		require.NoError(t, err)
		require.Equal(t, facts, moved.Checkout)
		w.clock.Advance(306 * 24 * time.Hour)
		runRegistered(t, fleet, "openrails.cleanup_expired_data", 1)
		_, err = a.alpha.GetInvoice(t.Context(), invoice.ID)
		require.ErrorIs(t, err, billing.ErrNotFound, "397 days after it closed it is gone")
	})
	t.Run("OR-J47-S08/renewals-name-the-checkout-that-bought-their-subscription", func(t *testing.T) {
		a, p := newPaymentWorld(t, func(w *world) { w.clock.Advance(-30 * 24 * time.Hour) }, behindCloudflare)
		product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "funnel-membership", DisplayName: "Membership", Entitlements: []string{"membership:paid"}})
		require.NoError(t, err)
		price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "funnel-membership-day", UnitAmount: 10_000_000, Currency: "USD", BillingIntervalHours: ptr(24), AccessDurationHours: ptr(24)})
		require.NoError(t, err)
		input := billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: price.ID}}, ExpectedTotal: ptr(int64(10_000_000)), Reusable: ptr(true),
			Payment: &billing.InvoicePaymentParams{Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}, Attribution: &billing.InvoiceAttribution{Campaign: ptr("renewals")}}
		status, raw := a.request(t, http.MethodPost, "/v1/me/invoices", a.aliceToken, input, map[string]string{"Idempotency-Key": "funnel-membership", "CF-IPCountry": "SE", "X-Forwarded-For": viaCloudflare("203.0.113.80")})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var bought billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &bought))
		require.Equal(t, billing.InvoiceComplete, bought.Status)
		require.NotNil(t, bought.Lines[0].SubscriptionID)
		sub, err := a.alpha.GetSubscription(t.Context(), *bought.Lines[0].SubscriptionID)
		require.NoError(t, err)

		due := *sub.CurrentPeriodEndsAt
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		renewalSettled(t, a, p, sub, due)
		bills, err := a.alpha.ListInvoices(t.Context(), billing.InvoiceListParams{SubscriptionID: sub.ID})
		require.NoError(t, err)
		require.Len(t, bills.Items, 1)
		renewal := bills.Items[0]
		require.Equal(t, billing.InvoiceOriginSubscription, renewal.Origin)
		require.Nil(t, renewal.Checkout, "a renewal is not a checkout")
		require.NotNil(t, renewal.Renewal.CheckoutInvoiceID)
		require.Equal(t, bought.ID, *renewal.Renewal.CheckoutInvoiceID)
		checkout := checkoutOf(t, a, *renewal.Renewal.CheckoutInvoiceID)
		require.Equal(t, "SE", *checkout.Country)
		require.Equal(t, "renewals", *checkout.Attribution.Campaign)
		// The renewal's cash counts where and from what its checkout came.
		window := &billing.MetricsRange{From: due.Add(-48 * time.Hour).Format(time.DateOnly), To: due.Add(24 * time.Hour).Format(time.DateOnly)}
		usd := map[string][]string{"currency": {"USD"}}
		require.Equal(t, map[string]int64{"SE": 20_000_000},
			split(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"cash"}, By: []string{"country"}, Filters: usd, Range: window}), "country", "cash"))
		require.Equal(t, map[string]int64{"renewals": 20_000_000},
			split(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"cash"}, By: []string{"attribution_campaign"}, Filters: usd, Range: window}), "attribution_campaign", "cash"))
		require.Equal(t, map[string]int64{"new": 10_000_000, "": 10_000_000},
			split(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"cash"}, By: []string{"customer_type"}, Filters: usd, Range: window}), "customer_type", "cash"),
			"a renewal's cash bought nothing new: no customer type")

		// An upgrade's membership keeps the checkout, and so do its bills.
		higher := upgradePrice(t, a, sub)
		a.clock.Advance(12 * time.Hour)
		status, raw = a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/change", a.aliceToken, map[string]any{"price_id": higher.ID}, map[string]string{"Idempotency-Key": "funnel-upgrade"})
		require.Equal(t, http.StatusOK, status, string(raw))
		var changed billing.SubscriptionChange
		require.NoError(t, json.Unmarshal(raw, &changed))
		require.Equal(t, "succeeded", changed.Status, string(raw))
		require.NotNil(t, changed.SubscriptionID)
		require.NotEqual(t, sub.ID, *changed.SubscriptionID)
		upgraded, err := a.alpha.GetSubscription(t.Context(), *changed.SubscriptionID)
		require.NoError(t, err)
		upgradeDue := *upgraded.CurrentPeriodEndsAt
		renewedAt(t, a, fleet, upgraded.ID, upgradeDue, upgradeDue)
		bills, err = a.alpha.ListInvoices(t.Context(), billing.InvoiceListParams{SubscriptionID: upgraded.ID})
		require.NoError(t, err)
		renewed := 0
		for _, bill := range bills.Items {
			if bill.Origin != billing.InvoiceOriginSubscription || bill.Renewal == nil {
				continue
			}
			require.NotNil(t, bill.Renewal.CheckoutInvoiceID, "%s names the checkout", bill.ID)
			require.Equal(t, bought.ID, *bill.Renewal.CheckoutInvoiceID)
			if bill.Renewal.PeriodStartsAt.Equal(upgradeDue) {
				renewed++
			}
		}
		require.Equal(t, 1, renewed, "the upgraded membership's renewal")
	})
	t.Run("OR-J47-S10/the-customer-type-is-new-returning-or-reactivated", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		card := func() string { return p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"}) }
		product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "type-membership", DisplayName: "Membership", Entitlements: []string{"membership:paid"}})
		require.NoError(t, err)
		daily, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "type-membership-day", UnitAmount: 10_000_000, Currency: "USD", BillingIntervalHours: ptr(24), AccessDurationHours: ptr(24)})
		require.NoError(t, err)
		_, book := publicSale(t, a, "type-book", 5_000_000, nil)
		_, extra := publicSale(t, a, "type-extra", 6_000_000, nil)

		// A first purchase by a customer who never subscribed is new.
		input := billing.CreateMyInvoiceParams{Lines: []billing.InvoiceLineParams{{PriceID: daily.ID}}, ExpectedTotal: ptr(int64(10_000_000)), Reusable: ptr(true),
			Payment: &billing.InvoicePaymentParams{Token: card()}}
		status, raw := a.request(t, http.MethodPost, "/v1/me/invoices", a.aliceToken, input, map[string]string{"Idempotency-Key": "type-membership"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var joined billing.Invoice
		require.NoError(t, json.Unmarshal(raw, &joined))
		require.Nil(t, joined.Checkout, "the customer never reads its own type")
		require.Equal(t, billing.CustomerTypeNew, checkoutOf(t, a, joined.ID).CustomerType)
		require.NotNil(t, joined.Lines[0].SubscriptionID)
		sub := *joined.Lines[0].SubscriptionID

		// A subscriber buying more is returning.
		more := publicPurchase(t, a, p, book.ID, 1, 5_000_000, "type-more")
		require.Equal(t, billing.CustomerTypeReturning, checkoutOf(t, a, more.ID).CustomerType)

		// Once its subscription ended, a former subscriber's purchase reactivates it.
		_, err = a.alpha.CancelSubscription(t.Context(), sub, billing.CancelSubscriptionParams{Reason: "Membership ends now", RevokeAccess: true})
		require.NoError(t, err)
		back := publicPurchase(t, a, p, extra.ID, 1, 6_000_000, "type-back")
		require.Equal(t, billing.CustomerTypeReactivated, checkoutOf(t, a, back.ID).CustomerType)

		// Another customer's first checkout is new, whoever started it.
		bob, err := a.alphaRemote.CreateInvoice(t.Context(), "type-bob", billing.CreateInvoiceParams{CustomerID: billing.CustomerID(uuid.MustParse(a.bob.ID)), Lines: []billing.InvoiceLineParams{{PriceID: book.ID}}})
		require.NoError(t, err)
		require.Equal(t, billing.CustomerTypeNew, bob.Checkout.CustomerType)

		// Sales and their cash split by it.
		now := a.clock.Now()
		window := &billing.MetricsRange{From: now.Add(-48 * time.Hour).Format(time.DateOnly), To: now.Add(48 * time.Hour).Format(time.DateOnly)}
		byType := func(measure string, filters map[string][]string) map[string]int64 {
			return split(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{measure}, By: []string{"customer_type"}, Filters: filters, Range: window}), "customer_type", measure)
		}
		require.Equal(t, map[string]int64{"new": 1, "returning": 1, "reactivated": 1}, byType("sales", nil))
		require.Equal(t, map[string]int64{"new": 10_000_000, "returning": 5_000_000, "reactivated": 6_000_000},
			byType("cash", map[string][]string{"currency": {"USD"}, "cash_kind": {"payment"}}))
		_, err = a.alpha.QueryMetrics(t.Context(), billing.QueryMetricsParams{Measures: []string{"sales"}, By: []string{"subscriber_type"}, Range: window})
		require.Equal(t, "customer_type", metricRefusals(t, err)["by[0]"]["did_you_mean"])
		// A measure without a customer type names no replacement.
		_, err = a.alpha.QueryMetrics(t.Context(), billing.QueryMetricsParams{Measures: []string{"mrr"}, By: []string{"subscriber_type"}, Range: window})
		removed := metricRefusals(t, err)["by[0]"]
		require.Nil(t, removed["did_you_mean"])
		require.Contains(t, removed["message"], "was removed")
	})
	t.Run("OR-J47-S11/spending-a-merchants-units-is-not-a-checkout", func(t *testing.T) {
		s, _ := unitPaymentWorld(t, 0)
		a := s.a
		gold := s.pack(t, "funnel-gold", "GOLD", 400, 365, unitPrice{"usd", 20_000_000, "USD"})
		sword := s.item(t, "funnel-sword", catalog.OwnershipConsumable, 50, "GOLD", "shop:sword")

		pack := s.buy(t, gold["usd"], 20_000_000, "funnel-gold")
		require.Equal(t, 1, checkoutOf(t, a, pack.ID).PurchaseNumber)
		for i := range 3 {
			status, raw := s.spend(t, sword, fmt.Sprintf("funnel-sword-%d", i))
			require.Equal(t, http.StatusCreated, status, string(raw))
			var spent billing.Invoice
			require.NoError(t, json.Unmarshal(raw, &spent))
			require.Equal(t, billing.InvoiceComplete, spent.Status)
			read, err := a.alpha.GetInvoice(t.Context(), spent.ID)
			require.NoError(t, err)
			require.Nil(t, read.Checkout, "spending units is not a checkout")
		}
		again := s.buy(t, gold["usd"], 20_000_000, "funnel-gold-again")
		facts := checkoutOf(t, a, again.ID)
		require.Equal(t, 2, facts.PurchaseNumber, "spends are no purchases")
		require.Equal(t, billing.CustomerTypeReturning, facts.CustomerType)

		window := &billing.MetricsRange{Last: "7d"}
		funnel := map[string]int64{}
		for _, row := range metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"checkouts"}, By: []string{"step", "outcome"}, Range: window}) {
			if n := whole(t, row["checkouts"]); n != 0 {
				funnel[fmt.Sprint(row["step"])+"/"+fmt.Sprint(row["outcome"])] = n
			}
		}
		require.Equal(t, map[string]int64{"paid/paid": 2}, funnel)
		read := only(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"checkout_conversion_rate", "sales", "sales_customers", "repeat_purchase_rate"}, Range: window}))
		require.InDelta(t, 1.0, fraction(t, read["checkout_conversion_rate"]), 1e-12)
		require.EqualValues(t, 2, whole(t, read["sales"]))
		require.EqualValues(t, 1, whole(t, read["sales_customers"]))
		require.InDelta(t, 1.0, fraction(t, read["repeat_purchase_rate"]), 1e-12)
		require.Equal(t, map[string]int64{"1": 1, "2": 1},
			split(t, metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"sales"}, By: []string{"purchase_number"}, Range: window}), "purchase_number", "sales"))
	})
	t.Run("OR-J47-S12/an-erasure-clears-a-checkouts-attribution", func(t *testing.T) {
		a, _ := newPaymentWorld(t, func(w *world) { w.deps.UserInfo = nil })
		provisioner, err := a.alphaRemote.CreateProvisioningToken(t.Context(), billing.CreateProvisioningTokenParams{Name: "owned directory"})
		require.NoError(t, err)
		id := uuid.NewString()
		user := map[string]any{"schemas": []string{hostSCIMUserSchema}, "externalId": id, "userName": "erased-buyer", "displayName": "Erased Buyer",
			"emails": []any{map[string]any{"value": "erased-buyer@directory.invalid", "primary": true}}, "active": true}
		status, raw := a.request(t, http.MethodPost, "/v1/app/scim/v2/Users", provisioner.Token, user)
		require.Equal(t, http.StatusCreated, status, string(raw))
		customer := billing.CustomerID(uuid.MustParse(id))
		_, price := publicSale(t, a, "funnel-erased", 9_990_000, nil)
		given := billing.InvoiceAttribution{Source: ptr("newsletter"), Medium: ptr("email"), Campaign: ptr("spring"), Term: ptr("recipient-4711"),
			Content: ptr("variant-b"), Referrer: ptr("news.example.org"), Affiliate: ptr("partner-7")}
		made, err := a.alphaRemote.CreateInvoice(t.Context(), "funnel-erased", billing.CreateInvoiceParams{CustomerID: customer, Lines: []billing.InvoiceLineParams{{PriceID: price.ID}}, Attribution: &given})
		require.NoError(t, err)
		require.Equal(t, "recipient-4711", *made.Checkout.Attribution.Term)

		status, raw = a.request(t, http.MethodDelete, "/v1/app/scim/v2/Users/"+id, provisioner.Token, nil)
		require.Equal(t, http.StatusNoContent, status, string(raw))
		facts := checkoutOf(t, a, made.ID)
		require.Equal(t, billing.InvoiceAttribution{}, facts.Attribution, "an erasure leaves nothing attribution said of the customer")
		require.Equal(t, 1, facts.PurchaseNumber, "the checkout itself stands")
	})
}

func coalesce(v, fallback *string) *string {
	if v != nil {
		return v
	}
	return fallback
}

// firstCampaign is the campaign a new merchant invoice of price keeps.
func firstCampaign(t *testing.T, a *authorityWorld, customer billing.CustomerID, price billing.PriceID, campaign string) *string {
	t.Helper()
	made, err := a.alphaRemote.CreateInvoice(t.Context(), "funnel-campaign-"+campaign, billing.CreateInvoiceParams{CustomerID: customer, Lines: []billing.InvoiceLineParams{{PriceID: price}},
		Attribution: &billing.InvoiceAttribution{Campaign: &campaign}})
	require.NoError(t, err)
	return made.Checkout.Attribution.Campaign
}
