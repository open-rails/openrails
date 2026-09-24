package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/shared/iputil"
	"github.com/open-rails/openrails/internal/shared/webhookutil"
	"github.com/open-rails/openrails/internal/webhookauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

func signStripeAt(secret string, at time.Time, body []byte) string {
	ts := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(body)))
	return fmt.Sprintf("t=%s,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

// FC-7: the signature, not the routed merchant, is the trust boundary; any
// configured secret may sign (snapshot and thin destinations share an endpoint).
func TestStripeSignatureIsTheTrustBoundary(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"checkout.session.completed","data":{"object":{}}}`)
	secrets := []string{"whsec_snapshot", "whsec_thin"}
	now := time.Now()

	prepared, err := prepareStripeMultiSecret(body, secrets, signStripeAt("whsec_thin", now, body), time.Minute)
	require.NoError(t, err)
	require.Equal(t, []any{"evt_1", "checkout.session.completed", true}, []any{prepared.EventID, prepared.EventType, prepared.SignatureVerified})

	for _, tc := range []struct {
		name    string
		secrets []string
		header  string
		body    []byte
		want    error
	}{
		{"unknown secret", secrets, signStripeAt("other", now, body), body, webhookutil.ErrWebhookSignatureInvalid},
		{"no secrets configured", nil, signStripeAt("whsec_thin", now, body), body, webhookutil.ErrWebhookSignatureRequired},
		{"missing header", secrets, "", body, webhookutil.ErrWebhookSignatureMissing},
		{"body modified", secrets, signStripeAt("whsec_thin", now, body), []byte(strings.Replace(string(body), "evt_1", "evt_2", 1)), webhookutil.ErrWebhookSignatureInvalid},
		{"stale", secrets, signStripeAt("whsec_thin", now.Add(-time.Hour), body), body, nil},
	} {
		_, err := prepareStripeMultiSecret(tc.body, tc.secrets, tc.header, time.Minute)
		require.Error(t, err, tc.name)
		if tc.want != nil {
			require.ErrorIs(t, err, tc.want, tc.name)
		}
	}

	// A valid signature never replaces account scope.
	for _, field := range []string{"account", "context"} {
		foreign := []byte(`{"id":"evt_scope","type":"charge.refunded","` + field + `":"acct_other","data":{"object":{"id":"ch_scope"}}}`)
		prepared, err := prepareStripeMultiSecret(foreign, []string{"whsec_selected"}, signStripeAt("whsec_selected", now, foreign), time.Minute)
		require.NoError(t, err)
		_, err = hydrateThinStripeEvent(context.Background(), "sk_test", "acct_selected", prepared.Body, nil)
		require.ErrorContains(t, err, "does not match routed account")
	}
}

type stripeThinTransport func(*http.Request) (*http.Response, error)

func (f stripeThinTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func thinNotice(eventType, kind, id, collection string) map[string]any {
	return map[string]any{
		"id": "evt_thin", "object": "v2.core.event", "type": "v1." + eventType,
		"created": "2026-09-20T12:00:00.123Z", "context": "acct_test", "livemode": false,
		"related_object": map[string]any{"id": id, "type": kind, "url": "/v1/" + collection + "/" + id},
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// fakeStripe serves canned GETs and asserts every fetch goes to Stripe itself,
// with the merchant's key and no account-switching header.
func fakeStripe(t *testing.T, responses map[string]string, inspect func(*http.Request)) (*stripeapi.Factory, *[]string) {
	t.Helper()
	calls := []string{}
	return stripeapi.NewFactory(stripeThinTransport(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.URL.Path)
		require.Equal(t, "https://api.stripe.com", r.URL.Scheme+"://"+r.URL.Host)
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "Bearer sk_test", r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("Stripe-Context")+r.Header.Get("Stripe-Account"))
		if inspect != nil {
			inspect(r)
		}
		body, ok := responses[r.URL.Path]
		if !ok {
			return nil, fmt.Errorf("unexpected fetch %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})), &calls
}

// A thin notice hydrates into the same snapshot event (same dedupe id) the
// snapshot destination would deliver.
func TestThinStripeEventHydratesToSnapshot(t *testing.T) {
	for _, tc := range []struct{ eventType, kind, id, collection string }{
		{"invoice.paid", "invoice", "in_test", "invoices"},
		{"payment_intent.succeeded", "payment_intent", "pi_test", "payment_intents"},
		{"payment_intent.payment_failed", "payment_intent", "pi_test", "payment_intents"},
		{"payment_intent.requires_action", "payment_intent", "pi_test", "payment_intents"},
		{"refund.created", "refund", "re_test", "refunds"},
		{"charge.dispute.created", "dispute", "dp_test", "disputes"},
		{"customer.subscription.updated", "subscription", "sub_test", "subscriptions"},
		{"checkout.session.completed", "checkout.session", "cs_test", "checkout/sessions"},
		{"payment_method.detached", "payment_method", "pm_test", "payment_methods"},
	} {
		t.Run(tc.eventType, func(t *testing.T) {
			full := thinNotice(tc.eventType, tc.kind, tc.id, tc.collection)
			full["snapshot_event"] = "evt_snapshot"
			full["changes"] = map[string]any{"before": map[string]any{"customer": "cus_before"}}
			object := map[string]any{"id": tc.id, "object": tc.kind, "customer": "cus_current", "status": "succeeded", "amount": 1000}
			snapshot := map[string]any{"id": "evt_snapshot", "type": tc.eventType, "created": 1790000000, "data": map[string]any{"object": object, "previous_attributes": map[string]any{"customer": "cus_before"}}}
			clients, calls := fakeStripe(t, map[string]string{
				"/v1/account":                        `{"id":"acct_test"}`,
				"/v2/core/events/evt_thin":           string(mustJSON(t, full)),
				"/v1/events/evt_snapshot":            string(mustJSON(t, snapshot)),
				"/v1/" + tc.collection + "/" + tc.id: string(mustJSON(t, object)),
			}, func(r *http.Request) {
				want := stripeapi.APIVersion
				if strings.HasPrefix(r.URL.Path, "/v2/") {
					want = stripeThinEventAPIVersion
				}
				require.Equal(t, want, r.Header.Get(stripeapi.VersionHeader))
			})
			signed := mustJSON(t, thinNotice(tc.eventType, tc.kind, tc.id, tc.collection))
			prepared, err := prepareStripeMultiSecret(signed, []string{"whsec_thin"}, signStripeAt("whsec_thin", time.Now(), signed), 0)
			require.NoError(t, err)
			out, err := hydrateThinStripeEvent(context.Background(), "sk_test", "acct_test", prepared.Body, clients)
			require.NoError(t, err)
			id, typ, err := webhookutil.ParseStripeEventMeta(out)
			require.NoError(t, err)
			require.Equal(t, []string{"evt_snapshot", tc.eventType}, []string{id, typ})
			var got map[string]any
			require.NoError(t, json.Unmarshal(out, &got))
			require.Equal(t, "acct_test", got["context"])
			require.Equal(t, full["changes"], got["changes"])
			require.Equal(t, float64(1789905600), got["created"])
			require.Equal(t, "cus_before", got["data"].(map[string]any)["previous_attributes"].(map[string]any)["customer"])
			require.Len(t, *calls, 4)
		})
	}

	notice := thinNotice("refund.updated", "refund", "re_test", "refunds")
	clients, _ := fakeStripe(t, map[string]string{
		"/v1/account":              `{"id":"acct_test"}`,
		"/v2/core/events/evt_thin": string(mustJSON(t, notice)),
		"/v1/refunds/re_test":      `{"id":"re_test","object":"refund","status":"succeeded"}`,
	}, nil)
	out, err := hydrateThinStripeEvent(context.Background(), "sk_test", "acct_test", mustJSON(t, notice), clients)
	require.NoError(t, err)
	id, typ, err := webhookutil.ParseStripeEventMeta(out)
	require.NoError(t, err)
	require.Equal(t, []string{"evt_thin", "refund.updated"}, []string{id, typ}, "no snapshot correlation keeps the thin id")

	for name, body := range map[string]string{
		"snapshot event":    `{"id":"evt_1","type":"x","data":{"object":{"id":"cs_1"}}}`,
		"no related object": `{"id":"evt_2","type":"x"}`,
	} {
		out, err := hydrateThinStripeEvent(context.Background(), "sk_test", "acct_test", []byte(body), nil)
		require.NoError(t, err, name)
		require.Nil(t, out, name)
	}
	_, err = hydrateThinStripeEvent(context.Background(), "", "acct_test", mustJSON(t, notice), nil)
	require.Error(t, err, "a thin event cannot be hydrated without a secret key")
}

// SEC-24 item 5: the notice is attacker-shaped input; nothing in it can steer
// the fetch off Stripe, onto another object or another account.
func TestStripeRelatedObjectURLIsAlwaysAPath(t *testing.T) {
	refuse := func(t *testing.T, mutate func(map[string]any)) {
		t.Helper()
		n := thinNotice("refund.updated", "refund", "re_test", "refunds")
		mutate(n)
		clients, calls := fakeStripe(t, nil, nil)
		_, err := hydrateThinStripeEvent(context.Background(), "sk_test", "acct_test", mustJSON(t, n), clients)
		require.Error(t, err)
		require.Empty(t, *calls, "refused before any fetch")
	}
	for name, mutate := range map[string]func(map[string]any){
		"unsupported event":    func(n map[string]any) { n["type"] = "v1.payment_intent.created" },
		"unsupported v2":       func(n map[string]any) { n["type"] = "v2.core.account.updated" },
		"missing object":       func(n map[string]any) { delete(n, "related_object") },
		"wrong context":        func(n map[string]any) { n["context"] = "acct_other" },
		"organization context": func(n map[string]any) { n["context"] = "acct_test/acct_other" },
		"wrong account":        func(n map[string]any) { n["account"] = "acct_other" },
		"event id traversal":   func(n map[string]any) { n["id"] = "evt_x/../../charges" },
		"wrong kind":           func(n map[string]any) { n["related_object"].(map[string]any)["type"] = "charge" },
	} {
		t.Run(name, func(t *testing.T) { refuse(t, mutate) })
	}
	for _, url := range []string{
		"https://evil.example/collect", "https://api.stripe.com/v1/refunds/re_test", "//evil.example/x",
		"/v1/refunds/../charges/ch_x", "/v1/refunds/re_test?expand[]=customer", "/v1/refunds/re_test#x",
		"/v1/refunds/re_other", "/v1/refunds/%72e_test", "/v1/refunds/re_test\n",
	} {
		t.Run(url, func(t *testing.T) {
			refuse(t, func(n map[string]any) { n["related_object"].(map[string]any)["url"] = url })
		})
	}

	calls := 0
	redirecting := stripeapi.NewFactory(stripeThinTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, "api.stripe.com", r.URL.Host)
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://api.stripe.com.evil.example/collect"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	}))
	_, err := hydrateThinStripeEvent(context.Background(), "sk_test", "acct_test", mustJSON(t, thinNotice("refund.updated", "refund", "re_test", "refunds")), redirecting)
	require.Error(t, err)
	require.Equal(t, 1, calls, "a redirect is never followed with the key")
}

// Whatever Stripe returns must agree with the signed notice.
func TestThinStripeRejectsFetchedMismatch(t *testing.T) {
	for name, mutate := range map[string]func(map[string]string, map[string]any){
		"credential account": func(r map[string]string, _ map[string]any) { r["/v1/account"] = `{"id":"acct_other"}` },
		"fetch failure":      func(r map[string]string, _ map[string]any) { delete(r, "/v2/core/events/evt_thin") },
		"malformed":          func(r map[string]string, _ map[string]any) { r["/v2/core/events/evt_thin"] = `{` },
		"oversized": func(r map[string]string, _ map[string]any) {
			r["/v2/core/events/evt_thin"] = strings.Repeat(" ", int(maxStripeWebhookBytes)+1)
		},
		"event id":   func(_ map[string]string, n map[string]any) { n["id"] = "evt_other" },
		"event type": func(_ map[string]string, n map[string]any) { n["type"] = "v1.refund.created" },
		"context":    func(_ map[string]string, n map[string]any) { n["context"] = "acct_other" },
		"time":       func(_ map[string]string, n map[string]any) { n["created"] = "invalid" },
		"resource id": func(r map[string]string, _ map[string]any) {
			r["/v1/refunds/re_test"] = `{"id":"re_other","object":"refund"}`
		},
		"resource type": func(r map[string]string, _ map[string]any) {
			r["/v1/refunds/re_test"] = `{"id":"re_test","object":"charge"}`
		},
		"correlation id": func(_ map[string]string, n map[string]any) { n["snapshot_event"] = "evt_other/../../" },
		"related id": func(_ map[string]string, n map[string]any) {
			n["related_object"] = map[string]any{"id": "re_other", "type": "refund", "url": "/v1/refunds/re_other"}
		},
		"correlation type": func(r map[string]string, n map[string]any) {
			n["snapshot_event"] = "evt_snapshot"
			r["/v1/events/evt_snapshot"] = `{"id":"evt_snapshot","type":"refund.created"}`
		},
	} {
		t.Run(name, func(t *testing.T) {
			full := thinNotice("refund.updated", "refund", "re_test", "refunds")
			responses := map[string]string{"/v1/account": `{"id":"acct_test"}`, "/v2/core/events/evt_thin": "", "/v1/refunds/re_test": `{"id":"re_test","object":"refund"}`}
			mutate(responses, full)
			if v, ok := responses["/v2/core/events/evt_thin"]; ok && v == "" {
				responses["/v2/core/events/evt_thin"] = string(mustJSON(t, full))
			}
			clients, _ := fakeStripe(t, responses, nil)
			_, err := hydrateThinStripeEvent(context.Background(), "sk_test", "acct_test", mustJSON(t, thinNotice("refund.updated", "refund", "re_test", "refunds")), clients)
			require.Error(t, err)
		})
	}
}

// A webhook binds to exactly one persisted account; nothing falls back.
func TestWebhookAccountResolutionFailsClosed(t *testing.T) {
	for _, provider := range []string{"stripe", "nmi", "ccbill", "basistheory"} {
		r, rec := newTestRequest(http.MethodPost, "/", nil, nil)
		processResolvedMerchantWebhook(r, provider, merchant.ID(uuid.New()), " ")
		require.Equal(t, http.StatusBadRequest, rec.Code, provider)
		require.Contains(t, rec.Body.String(), "account_id is required")
	}
	for _, tenant := range []string{"", "tenant-other"} {
		r, rec := newTestRequest(http.MethodPost, "/", strings.NewReader(`{"id":"evt_1","type":"token.updated","tenant_id":"`+tenant+`"}`), &app.Runtime{})
		require.False(t, processMerchantBasisTheoryWebhook(r, merchant.ID(uuid.New()), "tenant-selected"))
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Contains(t, rec.Body.String(), "does not match payload")
	}
	for _, tc := range []struct {
		found  bool
		id     uuid.UUID
		err    error
		status int
	}{
		{true, uuid.New(), errors.New("database unavailable"), 500},
		{false, uuid.New(), nil, 401},
		{true, uuid.Nil, nil, 401},
		{true, uuid.New(), nil, 200},
	} {
		r, rec := newTestRequest(http.MethodPost, "/", nil, nil)
		bound := bindResolvedWebhookPSP(r, tc.id, tc.found, tc.err)
		require.Equal(t, tc.status, rec.Code)
		require.Equal(t, tc.status == 200, bound)
		want := uuid.Nil
		if bound {
			want = tc.id
		}
		require.Equal(t, want, db.PSPIDFromContext(r.Request.Context()))
	}
}

func stubCCBillProbe(t *testing.T, probe webhookauth.LiveRailProbe) {
	t.Helper()
	orig := ccbillLivePSPProbe
	ccbillLivePSPProbe = func(*httprequest.Request) webhookauth.LiveRailProbe { return probe }
	t.Cleanup(func() { ccbillLivePSPProbe = orig })
}

func ccbillProbe(presence merchants.LiveRailPresence, err error) webhookauth.LiveRailProbe {
	return func(context.Context) (merchants.LiveRailPresence, error) { return presence, err }
}

// #668/SEC-19: CCBill has no HMAC, so its source address is its only
// authentication. A declared dev source is honoured only in sandbox posture,
// only when a probe PROVES no live CCBill PSP exists, and only if declared.
func TestCCBillSourceAllowlist(t *testing.T) {
	declared := []string{"203.0.113.0/24"}
	const ccbillIP, devIP, strangerIP = "64.38.212.5", "203.0.113.5", "198.51.100.7"
	allowed := func(sandbox bool, allowlist []string, probe webhookauth.LiveRailProbe, ip string) bool {
		stubCCBillProbe(t, probe)
		posture := config.CredentialPostureLive
		if sandbox {
			posture = config.CredentialPostureSandbox
		}
		r, _ := newTestRequest(http.MethodPost, "/", nil, &app.Runtime{Config: &config.Config{TestMode: posture, CCBillWebhookIPAllowlist: allowlist}})
		return ccbillWebhookIPAllowed(r, ip)
	}
	present, absent := ccbillProbe(merchants.LiveRailPresent, nil), ccbillProbe(merchants.LiveRailAbsent, nil)
	for _, tc := range []struct {
		name      string
		sandbox   bool
		allowlist []string
		probe     webhookauth.LiveRailProbe
		ip        string
		want      bool
	}{
		{"CCBill range, live", false, nil, present, ccbillIP, true},
		{"CCBill range, sandbox", true, nil, present, ccbillIP, true},
		{"declared, live posture", false, declared, absent, devIP, false},
		{"declared, live PSP exists", true, declared, present, devIP, false},
		{"undeclared", true, nil, absent, devIP, false},
		{"outside declared range", true, declared, absent, strangerIP, false},
		{"probe proves nothing", true, declared, ccbillProbe(merchants.LiveRailUnknown, nil), devIP, false},
		{"probe errors", true, declared, ccbillProbe(merchants.LiveRailUnknown, errors.New("catalog unavailable")), devIP, false},
		{"probe errors but claims absent", true, declared, ccbillProbe(merchants.LiveRailAbsent, errors.New("catalog unavailable")), devIP, false},
		{"no merchants service", true, declared, nil, devIP, false},
		{"all three conditions", true, declared, absent, devIP, true},
	} {
		require.Equal(t, tc.want, allowed(tc.sandbox, tc.allowlist, tc.probe, tc.ip), tc.name)
	}
	r, _ := newTestRequest(http.MethodPost, "/", nil, nil)
	require.False(t, ccbillWebhookIPAllowed(r, devIP), "no runtime never bypasses")
}

// #746: the allowlist sees the resolved client address through a trusted
// proxy only; a spoofed X-Forwarded-For from anyone else changes nothing.
func TestCCBillWebhookDispatchResolvesClientIP(t *testing.T) {
	stubCCBillProbe(t, ccbillProbe(merchants.LiveRailAbsent, nil))
	for _, tc := range []struct {
		name, peer, xff string
		trusted         []string
		want            int
	}{
		// 503 = past the IP gate, stopped only by the absent merchants service.
		{"behind trusted proxy", "10.0.0.5:443", "64.38.212.5", []string{"10.0.0.0/8"}, http.StatusServiceUnavailable},
		{"spoofed XFF, no trusted proxies", "203.0.113.9:443", "64.38.212.5", nil, http.StatusForbidden},
		{"spoofed XFF from untrusted peer", "203.0.113.9:443", "64.38.212.5", []string{"10.0.0.0/8"}, http.StatusForbidden},
	} {
		body := `{"eventType":"NewSaleSuccess","clientAccnum":"900000","clientSubacc":"0000","transactionId":"1"}`
		req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/ccbill/900000-0000?eventType=NewSaleSuccess", strings.NewReader(body))
		req.RemoteAddr = tc.peer
		req.Header.Set("X-Forwarded-For", tc.xff)
		req.SetPathValue("provider", "ccbill")
		req.SetPathValue("account_id", "900000-0000")
		rt := &app.Runtime{Config: &config.Config{TestMode: config.CredentialPostureLive, TrustedProxies: tc.trusted}, TrustedProxies: iputil.ParseTrustedProxies(tc.trusted)}
		rt.SetConfiguredMerchant(merchant.ID(uuid.New()))
		rec := httptest.NewRecorder()
		Webhook(httprequest.NewHTTP(rec, req, rt))
		require.Equal(t, tc.want, rec.Code, "%s: %s", tc.name, rec.Body.String())
	}
}

func TestCCBillWebhookIdentity(t *testing.T) {
	for body, want := range map[string]string{
		`{"clientAccnum":"900000","clientSubacc":"0000"}`: "900000-0000",
		`{"clientAccnum":900000,"clientSubacc":3}`:        "900000-3",
		`{"clientAccnum":"900000"}`:                       "900000",
		`{"clientSubacc":"0000"}`:                         "",
		`not json`:                                        "",
	} {
		require.Equal(t, want, ccbillWebhookAccountID([]byte(body)), body)
	}
	prepared, err := webhookutil.PrepareCCBill([]byte(`{"eventType":"NewSaleSuccess","clientAccnum":"900000","transactionId":"1"}`), "NewSaleSuccess")
	require.NoError(t, err)
	msg := ccbillWebhookMessage("64.38.212.5", prepared, "900000-0000")
	require.Nil(t, msg.SignatureValid, "an unsigned event never claims a valid signature")
	require.Equal(t, "900000-0000", msg.PspID)
}

func TestWebhookBodyLimit(t *testing.T) {
	for size, ok := range map[int64]bool{maxCCBillWebhookBytes - 1: true, maxCCBillWebhookBytes: true, maxCCBillWebhookBytes + 1: false} {
		payload := strings.Repeat("a", int(size))
		r, rec := newTestRequest(http.MethodPost, "/", strings.NewReader(payload), nil)
		body, accepted := readLimitedWebhookBody(r, maxCCBillWebhookBytes)
		require.Equal(t, ok, accepted, size)
		if ok {
			require.Equal(t, payload, string(body))
		} else {
			require.Nil(t, body)
			require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
		}
	}
}
