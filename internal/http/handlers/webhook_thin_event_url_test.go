package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/shared/webhookutil"
	"github.com/stretchr/testify/require"
)

type stripeThinTransport func(*http.Request) (*http.Response, error)

func (f stripeThinTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func thinFixture(eventType, kind, id, collection string) map[string]any {
	return map[string]any{
		"id": "evt_thin", "object": "v2.core.event", "type": "v1." + eventType,
		"created": "2026-09-20T12:00:00.123Z", "context": "acct_test", "livemode": false,
		"related_object": map[string]any{"id": id, "type": kind, "url": "/v1/" + collection + "/" + id},
	}
}
func thinBytes(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	require.NoError(t, err)
	return b
}

func installThinTransport(t *testing.T, responses map[string]string, inspect func(*http.Request)) *[]string {
	t.Helper()
	calls := []string{}
	stripeapi.SetBaseTransport(stripeThinTransport(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.URL.Path)
		require.Equal(t, "https", r.URL.Scheme)
		require.Equal(t, "api.stripe.com", r.URL.Host)
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "Bearer sk_test", r.Header.Get("Authorization"))
		require.Empty(t, r.Header.Get("Stripe-Context"))
		require.Empty(t, r.Header.Get("Stripe-Account"))
		if inspect != nil {
			inspect(r)
		}
		body, ok := responses[r.URL.Path]
		if !ok {
			return nil, fmt.Errorf("unexpected fetch %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}))
	t.Cleanup(func() { stripeapi.SetBaseTransport(nil) })
	return &calls
}

func TestThinStripeSupportedFinancialAndLegacyNotifications(t *testing.T) {
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
			notice := thinFixture(tc.eventType, tc.kind, tc.id, tc.collection)
			full := thinFixture(tc.eventType, tc.kind, tc.id, tc.collection)
			full["snapshot_event"] = "evt_snapshot"
			full["changes"] = map[string]any{"before": map[string]any{"customer": "cus_before"}}
			full["reason"] = map[string]any{"type": "request"}
			object := map[string]any{"id": tc.id, "object": tc.kind, "customer": "cus_current", "status": "succeeded", "amount": 1000, "subscription": "sub_test"}
			snapshot := map[string]any{"id": "evt_snapshot", "type": tc.eventType, "created": 1790000000, "data": map[string]any{"object": object, "previous_attributes": map[string]any{"customer": "cus_before"}}}
			calls := installThinTransport(t, map[string]string{
				"/v1/account":                        `{"id":"acct_test"}`,
				"/v2/core/events/evt_thin":           string(thinBytes(t, full)),
				"/v1/events/evt_snapshot":            string(thinBytes(t, snapshot)),
				"/v1/" + tc.collection + "/" + tc.id: string(thinBytes(t, object)),
			}, func(r *http.Request) {
				want := stripeapi.APIVersion
				if strings.HasPrefix(r.URL.Path, "/v2/") {
					want = stripeThinEventAPIVersion
				}
				require.Equal(t, want, r.Header.Get(stripeapi.VersionHeader))
			})
			signed := thinBytes(t, notice)
			prepared, err := prepareStripeMultiSecret(signed, []string{"whsec_thin"}, signStripe("whsec_thin", signed), 0)
			require.NoError(t, err)
			out, err := hydrateThinStripeEvent(context.Background(), "sk_test", "acct_test", prepared.Body)
			require.NoError(t, err)
			id, typ, err := webhookutil.ParseStripeEventMeta(out)
			require.NoError(t, err)
			require.Equal(t, "evt_snapshot", id)
			require.Equal(t, tc.eventType, typ)
			// Both snapshot and thin now reach the existing deduper with the same key.
			snapshotID, _, err := webhookutil.ParseStripeEventMeta(thinBytes(t, snapshot))
			require.NoError(t, err)
			require.Equal(t, snapshotID, id)
			var got map[string]any
			require.NoError(t, json.Unmarshal(out, &got))
			require.Equal(t, "acct_test", got["context"])
			require.Equal(t, full["changes"], got["changes"])
			require.Equal(t, float64(1789905600), got["created"])
			data := got["data"].(map[string]any)
			require.Equal(t, "cus_before", data["previous_attributes"].(map[string]any)["customer"])
			require.Equal(t, 4, len(*calls))
		})
	}
}

func TestThinStripeNoSnapshotCorrelation(t *testing.T) {
	notice := thinFixture("refund.updated", "refund", "re_test", "refunds")
	installThinTransport(t, map[string]string{
		"/v1/account":              `{"id":"acct_test"}`,
		"/v2/core/events/evt_thin": string(thinBytes(t, notice)),
		"/v1/refunds/re_test":      `{"id":"re_test","object":"refund","status":"succeeded"}`,
	}, nil)
	out, err := hydrateThinStripeEvent(context.Background(), "sk_test", "acct_test", thinBytes(t, notice))
	require.NoError(t, err)
	id, typ, err := webhookutil.ParseStripeEventMeta(out)
	require.NoError(t, err)
	require.Equal(t, "evt_thin", id)
	require.Equal(t, "refund.updated", typ)
}

func TestThinStripeRejectsBeforeFetching(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"unsupported payment intent event", func(n map[string]any) { n["type"] = "v1.payment_intent.created" }},
		{"unsupported v2", func(n map[string]any) { n["type"] = "v2.core.account.updated" }},
		{"missing object", func(n map[string]any) { delete(n, "related_object") }},
		{"wrong context", func(n map[string]any) { n["context"] = "acct_other" }},
		{"organization context", func(n map[string]any) { n["context"] = "acct_test/acct_other" }},
		{"wrong account", func(n map[string]any) { n["account"] = "acct_other" }},
		{"event path traversal", func(n map[string]any) { n["id"] = "evt_x/../../charges" }},
		{"wrong kind", func(n map[string]any) { n["related_object"].(map[string]any)["type"] = "charge" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := thinFixture("refund.updated", "refund", "re_test", "refunds")
			tc.mutate(n)
			calls := installThinTransport(t, nil, nil)
			_, err := hydrateThinStripeEvent(context.Background(), "sk_test", "acct_test", thinBytes(t, n))
			require.Error(t, err)
			require.Empty(t, *calls)
		})
	}
	for _, url := range []string{"https://evil.example/collect", "https://api.stripe.com/v1/refunds/re_test", "//evil.example/x", "/v1/refunds/../charges/ch_x", "/v1/refunds/re_test?expand[]=customer", "/v1/refunds/re_test#x", "/v1/refunds/re_other", "/v1/refunds/%72e_test", "/v1/refunds/re_test\n"} {
		t.Run(url, func(t *testing.T) {
			n := thinFixture("refund.updated", "refund", "re_test", "refunds")
			n["related_object"].(map[string]any)["url"] = url
			calls := installThinTransport(t, nil, nil)
			_, err := hydrateThinStripeEvent(context.Background(), "sk_test", "acct_test", thinBytes(t, n))
			require.Error(t, err)
			require.Empty(t, *calls)
		})
	}
}

func TestThinStripeRejectsFetchedMismatchAndFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]string, map[string]any)
	}{
		{"wrong credential account", func(r map[string]string, n map[string]any) { r["/v1/account"] = `{"id":"acct_other"}` }},
		{"fetch failure", func(r map[string]string, n map[string]any) { delete(r, "/v2/core/events/evt_thin") }},
		{"malformed", func(r map[string]string, n map[string]any) { r["/v2/core/events/evt_thin"] = `{` }},
		{"oversized", func(r map[string]string, n map[string]any) {
			r["/v2/core/events/evt_thin"] = strings.Repeat(" ", int(maxStripeWebhookBytes)+1)
		}},
		{"event id", func(r map[string]string, n map[string]any) { n["id"] = "evt_other" }},
		{"event type", func(r map[string]string, n map[string]any) { n["type"] = "v1.refund.created" }},
		{"context", func(r map[string]string, n map[string]any) { n["context"] = "acct_other" }},
		{"related id", func(r map[string]string, n map[string]any) {
			n["related_object"].(map[string]any)["id"] = "re_other"
			n["related_object"].(map[string]any)["url"] = "/v1/refunds/re_other"
		}},
		{"time", func(r map[string]string, n map[string]any) { n["created"] = "invalid" }},
		{"resource id", func(r map[string]string, n map[string]any) {
			r["/v1/refunds/re_test"] = `{"id":"re_other","object":"refund"}`
		}},
		{"resource type", func(r map[string]string, n map[string]any) {
			r["/v1/refunds/re_test"] = `{"id":"re_test","object":"charge"}`
		}},
		{"correlation id", func(r map[string]string, n map[string]any) { n["snapshot_event"] = "evt_other/../../" }},
		{"correlation type", func(r map[string]string, n map[string]any) {
			n["snapshot_event"] = "evt_snapshot"
			r["/v1/events/evt_snapshot"] = `{"id":"evt_snapshot","type":"refund.created"}`
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notice := thinFixture("refund.updated", "refund", "re_test", "refunds")
			full := thinFixture("refund.updated", "refund", "re_test", "refunds")
			responses := map[string]string{"/v1/account": `{"id":"acct_test"}`, "/v2/core/events/evt_thin": "generate", "/v1/refunds/re_test": `{"id":"re_test","object":"refund"}`}
			tc.mutate(responses, full)
			if responses["/v2/core/events/evt_thin"] == "generate" {
				responses["/v2/core/events/evt_thin"] = string(thinBytes(t, full))
			}
			installThinTransport(t, responses, nil)
			_, err := hydrateThinStripeEvent(context.Background(), "sk_test", "acct_test", thinBytes(t, notice))
			require.Error(t, err)
		})
	}
}

func TestThinStripeRefusesRedirectWithoutForwardingKey(t *testing.T) {
	calls := 0
	stripeapi.SetBaseTransport(stripeThinTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, "api.stripe.com", r.URL.Host)
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://api.stripe.com.evil.example/collect"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	}))
	t.Cleanup(func() { stripeapi.SetBaseTransport(nil) })
	_, err := hydrateThinStripeEvent(context.Background(), "sk_test", "acct_test", thinBytes(t, thinFixture("refund.updated", "refund", "re_test", "refunds")))
	require.Error(t, err)
	require.Equal(t, 1, calls)
}
