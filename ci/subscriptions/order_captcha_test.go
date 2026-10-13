//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/engine"
)

func orderCaptchaConfig(cfg *config.Config) {
	cfg.Captcha = &config.CaptchaConfig{SiteKey: "e2e-site", SecretKey: "e2e-secret"}
	cfg.RateLimits = config.DefaultRateLimits()
	(*cfg.RateLimits)["purchase"].RequestsPerMinute = 100
}

// Order declines use the same captcha threshold as card saves. Replaying
// a payment or confirming an old failure adds no new failure.
func TestOrderDeclinesRequireCaptcha(t *testing.T) {
	for _, route := range []string{"create", "pay"} {
		t.Run(route, func(t *testing.T) {
			w := newWorld(t, orderCaptchaConfig)
			price := w.lifetime("captcha:orders", 6_000_000)
			c := w.newCustomer()
			c.ip = "198.51.100.53"
			path := "/orders"
			if route == "pay" {
				placed := c.order(http.MethodPost, path, "", map[string]any{"lines": []any{line(price, 0)}})
				require.Equal(t, http.StatusCreated, placed.status, "%v", placed.body)
				path += "/" + placed.body["id"].(string) + "/pay"
			}
			for i := range 6 {
				body := map[string]any{"expected_total": micros(6_000_000), "payment": map[string]any{
					"token": w.newCardToken("nmi", card{Brand: "visa", Last4: "0341", Decline: "202"}), "psp_id": w.psp["nmi"],
				}}
				if route == "create" {
					body["lines"] = []any{line(price, 0)}
				}
				key := uuid.NewString()
				declined := c.order(http.MethodPost, path, key, body)
				require.Equal(t, http.StatusPaymentRequired, declined.status, "decline %d: %v", i+1, declined.body)
				require.Equal(t, "card_declined", orderError(declined))
				count, _ := engine.Graph(w.rt).Runtime.AbuseState.Count(t.Context(), "card_fail:ip:"+c.ip+":fail", 0, 15*time.Minute)
				require.EqualValues(t, i+1, count, "one captcha failure per new provider decline")
				if i >= 2 {
					before, _ := w.providerWrites("nmi")
					blocked := c.order(http.MethodPost, path, uuid.NewString(), body)
					require.Equal(t, http.StatusForbidden, blocked.status, "%v", blocked.body)
					require.Equal(t, "captcha_required", orderError(blocked))
					after, _ := w.providerWrites("nmi")
					require.Equal(t, before, after, "a captcha challenge never calls the provider")
					// Simulate a solved challenge without calling the live captcha service.
					engine.Graph(w.rt).Runtime.CaptchaStore.ClearChallenged(t.Context(), "ip:"+c.ip)
				}
				replay := c.order(http.MethodPost, path, key, body)
				require.Equal(t, http.StatusPaymentRequired, replay.status, "%v", replay.body)
				require.True(t, replay.replayed)
				old := c.order(http.MethodPost, "/orders/"+orderOf(declined)["id"].(string)+"/confirm", "", nil)
				require.Equal(t, http.StatusOK, old.status, "%v", old.body)
				count, _ = engine.Graph(w.rt).Runtime.AbuseState.Count(t.Context(), "card_fail:ip:"+c.ip+":fail", 0, 15*time.Minute)
				require.EqualValues(t, i+1, count, "replay and old failed order add no failure")
				if route == "create" {
					canceled := c.order(http.MethodPost, "/orders/"+orderOf(declined)["id"].(string)+"/cancel", "", nil)
					require.Equal(t, http.StatusOK, canceled.status, "%v", canceled.body)
				}
			}
			before, _ := w.providerWrites("nmi")
			body := map[string]any{
				"expected_total": micros(6_000_000),
				"payment":        map[string]any{"token": w.newCardToken("nmi", visa), "psp_id": w.psp["nmi"]},
			}
			if route == "create" {
				body["lines"] = []any{line(price, 0)}
			}
			blocked := c.order(http.MethodPost, path, uuid.NewString(), body)
			require.Equal(t, http.StatusTooManyRequests, blocked.status, "%v", blocked.body)
			require.Equal(t, "card_attempts_blocked", orderError(blocked))
			after, _ := w.providerWrites("nmi")
			require.Equal(t, before, after, "six declines still block further provider calls")
		})
	}
}

func TestPaymentPageDeclinesRequireCaptcha(t *testing.T) {
	w := prepareWorld(t, 12, orderCaptchaConfig)
	var page *httptest.Server
	w.booted = func(e *openrails.Engine) {
		_, handler, err := e.PaymentPage("https://pay.e2e.test", fstest.MapFS{"index.html": {Data: []byte("<html></html>")}})
		require.NoError(t, err)
		page = httptest.NewServer(http.StripPrefix(mountPrefix, handler))
		t.Cleanup(page.Close)
	}
	w.start()
	price := w.lifetime("captcha:page", 6_000_000)
	c := w.newCustomer()
	c.ip = "198.51.100.54"
	order, err := w.client[embedded].CreateOrder(t.Context(), billing.CreateOrderParams{
		CustomerID: c.cid(), Lines: []billing.OrderLineParams{{PriceID: price.ID}}, IdempotencyKey: uuid.NewString(),
		PayURL: &billing.OrderPayURLParams{SuccessURL: "https://e2e.test/thanks"},
	})
	require.NoError(t, err)
	u, err := url.Parse(*order.PayURL)
	require.NoError(t, err)
	c.token = u.Fragment
	for i := range 4 {
		before, _ := w.providerWrites("nmi")
		out := c.orderAt(page.URL, http.MethodPost, "/orders/"+order.ID.String()+"/pay", uuid.NewString(), map[string]any{
			"expected_total": micros(order.Total), "payment": map[string]any{
				"token": w.newCardToken("nmi", card{Brand: "visa", Last4: "0341", Decline: "202"}), "psp_id": w.psp["nmi"],
			},
		})
		if i < 3 {
			require.Equal(t, http.StatusPaymentRequired, out.status, "%v", out.body)
		} else {
			require.Equal(t, http.StatusForbidden, out.status, "%v", out.body)
			require.Equal(t, "captcha_required", orderError(out))
			after, _ := w.providerWrites("nmi")
			require.Equal(t, before, after)
		}
	}
}

func TestOrderConfirmationDeclineRequiresCaptcha(t *testing.T) {
	w := newWorld(t, orderCaptchaConfig)
	price := w.lifetime("captcha:confirm", 6_000_000)
	c := w.newCustomer()
	c.ip = "198.51.100.55"
	rt := engine.Graph(w.rt).Runtime
	for i := range 3 {
		out := c.order(http.MethodPost, "/orders", uuid.NewString(), map[string]any{
			"lines": []any{line(price, 0)}, "expected_total": micros(6_000_000), "payment": map[string]any{
				"token": w.newCardToken("stripe", card{Brand: "visa", Last4: "3155", Decline: "auth"}), "psp_id": w.psp["stripe"],
			},
		})
		require.Equal(t, http.StatusCreated, out.status, "%v", out.body)
		require.Equal(t, "requires_action", paymentOf(out)["status"])
		count, _ := rt.AbuseState.Count(t.Context(), "card_fail:ip:"+c.ip+":fail", 0, 15*time.Minute)
		require.EqualValues(t, i, count, "an authentication challenge is not a card decline")
		pi := paymentOf(out)["next_action"].(map[string]any)["payload"].(map[string]any)["payment_intent_id"].(string)
		// The issuer refuses the card after its challenge. Serve that exact
		// PaymentIntent's final refusal when confirmation reads it.
		w.stripe.Intercept(func(r *http.Request) bool {
			return r.Method == http.MethodGet && r.URL.Path == "/v1/payment_intents/"+pi
		}, func(_ *http.Request, serve func() *http.Response) (*http.Response, error) {
			res := serve()
			defer res.Body.Close()
			var body map[string]any
			if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
				return nil, err
			}
			body["status"] = "canceled"
			body["last_payment_error"] = map[string]any{"code": "card_declined", "decline_code": "insufficient_funds"}
			raw, err := json.Marshal(body)
			if err != nil {
				return nil, err
			}
			res.Body = io.NopCloser(bytes.NewReader(raw))
			res.ContentLength = int64(len(raw))
			return res, nil
		})
		id := out.body["id"].(string)
		confirmed := c.order(http.MethodPost, "/orders/"+id+"/confirm", "", nil)
		require.Equal(t, http.StatusPaymentRequired, confirmed.status, "%v", confirmed.body)
		count, _ = rt.AbuseState.Count(t.Context(), "card_fail:ip:"+c.ip+":fail", 0, 15*time.Minute)
		require.EqualValues(t, i+1, count)
		if i == 2 {
			blocked := c.order(http.MethodPost, "/orders/"+id+"/confirm", "", nil)
			require.Equal(t, "captcha_required", orderError(blocked))
			rt.CaptchaStore.ClearChallenged(t.Context(), "ip:"+c.ip)
		}
		old := c.order(http.MethodPost, "/orders/"+id+"/confirm", "", nil)
		require.Equal(t, http.StatusOK, old.status, "%v", old.body)
		count, _ = rt.AbuseState.Count(t.Context(), "card_fail:ip:"+c.ip+":fail", 0, 15*time.Minute)
		require.EqualValues(t, i+1, count)
		canceled := c.order(http.MethodPost, "/orders/"+id+"/cancel", "", nil)
		require.Equal(t, http.StatusOK, canceled.status, "%v", canceled.body)
	}
}

func TestOrderCollectionDeclineCountsOnlyCustomerAttempt(t *testing.T) {
	for _, origin := range []string{"renewal", "usage"} {
		t.Run(origin, func(t *testing.T) {
			w := newWorld(t, orderCaptchaConfig)
			var c *customer
			var method, id, total string
			if origin == "renewal" {
				e := enroll(t, w, "nmi", remote)
				e.c.ip = "198.51.100.56"
				e.setDecline(visa.Last4, "", "202")
				e.toFreshPeriodEnd()
				w.runRenewals()
				open := e.openRenewal()
				c, method = e.c, e.method
				id, total = open.body["id"].(string), open.body["total"].(string)
			} else {
				w.settleRefreshes()
				w.jobs.PeriodicJobs().Clear()
				c = w.newCustomer()
				c.ip = "198.51.100.56"
				client := w.client[remote]
				declareDynamicMeter(t, client, "captcha-usage", "units")
				_, err := client.UpdateCustomer(t.Context(), c.cid(), billing.UpdateCustomerParams{CreditLimits: []billing.CreditLimit{{Currency: "USD", Amount: 100_000_000}}})
				require.NoError(t, err)
				_, err = recordUsage(t.Context(), client, billing.RecordUsageParams{Dimensions: map[string]int64{"units": 1}, CustomerID: c.cid(), Invoker: c.id, Currency: "USD", EventType: "captcha-usage", Amount: new(int64(50_000_000)), Source: "test", SourceID: uuid.NewString()})
				require.NoError(t, err)
				w.advance(time.Minute)
				job, err := w.jobs.Insert(t.Context(), invoicePass{}, &river.InsertOpts{Queue: openrails.QueueBilling})
				require.NoError(t, err)
				w.waitJob(job.Job.ID)
				page, err := client.ListOrders(t.Context(), billing.OrderListParams{Origin: billing.OrderOriginUsage, CustomerID: c.cid()})
				require.NoError(t, err)
				require.Len(t, page.Items, 1)
				w.settleCollectionScans()
				method = c.saveCard("nmi", visa)
				w.refreshProviders()
				w.settleRefreshes()
				w.nmi.SetDecline(visa.Last4, "202")
				id, total = page.Items[0].ID.String(), micros(page.Items[0].Total)
			}
			rt := engine.Graph(w.rt).Runtime
			count, _ := rt.AbuseState.Count(t.Context(), "card_fail:ip:"+c.ip+":fail", 0, 15*time.Minute)
			require.Zero(t, count, "background collection and saving a good card do not challenge the customer")
			key := uuid.NewString()
			body := map[string]any{"expected_total": total, "payment": map[string]any{"payment_method_id": method}}
			declined := c.order(http.MethodPost, "/orders/"+id+"/pay", key, body)
			require.Equal(t, http.StatusPaymentRequired, declined.status, "%v", declined.body)
			require.Equal(t, "card_declined", orderError(declined))
			replay := c.order(http.MethodPost, "/orders/"+id+"/pay", key, body)
			require.Equal(t, http.StatusPaymentRequired, replay.status, "%v", replay.body)
			require.True(t, replay.replayed)
			count, _ = rt.AbuseState.Count(t.Context(), "card_fail:ip:"+c.ip+":fail", 0, 15*time.Minute)
			require.EqualValues(t, 1, count)
		})
	}
}
