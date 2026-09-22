//go:build integration

package integrationharness

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/stretchr/testify/require"
)

func cloneStripeNotification(t *testing.T, source map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(source)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

// Independent boundary review: enter through the real signed webhook HTTP
// route, including thin hydration and snapshot correlation. Every rejection
// must leave both financial state and the operation's River queue unchanged.
func reviewStripeEngineHTTPNotifications(t *testing.T, h *Harness, surface *Surface, owned OwnedMerchant, psp, operation uuid.UUID, now time.Time, queue string, payment map[string]any, knownPI bool, setResponses func(map[string]map[string]any)) func() {
	t.Helper()
	ctx := t.Context()
	rt := surface.App().Runtime
	pool := h.MerchantPool(owned.MerchantID.UUID())
	var account string
	require.NoError(t, pool.QueryRow(ctx, `SELECT account_id FROM billing.psps WHERE id=$1`, psp).Scan(&account))
	const secret = "whsec_independent_pi_boundary"
	name, err := merchants.PSPSecretName("stripe", "test", account, "webhook_signing_secret")
	require.NoError(t, err)
	_, err = rt.Merchants.Secrets().Put(ctx, owned.MerchantID, name, secret)
	require.NoError(t, err)
	post := func(event map[string]any, path, signingSecret string) (int, string) {
		t.Helper()
		raw, err := json.Marshal(event)
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, surface.BaseURL+path, bytes.NewReader(raw))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Stripe-Signature", stripeSignature(signingSecret, raw))
		response, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NotContains(t, string(body), "pi_signup_secret_private", "webhook response must not echo a browser credential")
		return response.StatusCode, string(body)
	}
	assertCacheSafe := func(kind, id string) {
		t.Helper()
		cached, err := rt.IdempotencyService.Get(ctx, "webhook.stripe."+kind+".psp."+psp.String(), id)
		require.NoError(t, err)
		require.NotNil(t, cached)
		require.NotContains(t, string(cached.Result), "client_secret", "webhook replay payload must not retain a browser credential")
		require.NotContains(t, string(cached.Result), "pi_signup_secret_private")
		require.Contains(t, string(cached.Result), operation.String(), "redaction retains immutable operation correlation")
	}

	path := "/v1/webhooks/stripe/" + account
	nextID := func() string { return "evt_" + strings.ReplaceAll(uuid.NewString(), "-", "") }
	snapshot := func(object map[string]any) map[string]any {
		return map[string]any{"id": nextID(), "object": "event", "type": "payment_intent.succeeded", "created": now.Unix(), "account": account, "data": map[string]any{"object": object}}
	}
	jobCount := func() int {
		var n int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM public.river_job WHERE queue=$1 AND args->>'intent_id'=$2`, queue, operation.String()).Scan(&n))
		return n
	}
	unchanged := func(before int) {
		t.Helper()
		require.Equal(t, before, jobCount(), "rejected notification cannot enqueue an operation")
		var status string
		require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM billing.rail_intents WHERE id=$1`, operation).Scan(&status))
		require.Equal(t, "unknown_needs_verify", status)
	}
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any, map[string]any)
	}{
		{"wrong_resource_type", func(e, p map[string]any) { p["object"] = "charge" }},
		{"wrong_envelope_account", func(e, p map[string]any) { e["account"] = "acct_other" }},
		{"wrong_envelope_context", func(e, p map[string]any) { e["context"] = "acct_other" }},
		{"wrong_customer", func(e, p map[string]any) { p["customer"] = "cus_other" }},
		{"wrong_method", func(e, p map[string]any) { p["payment_method"] = "pm_other" }},
		{"wrong_amount", func(e, p map[string]any) { p["amount"] = 1000 }},
		{"wrong_currency", func(e, p map[string]any) { p["currency"] = "eur" }},
		{"wrong_mode", func(e, p map[string]any) { p["livemode"] = true }},
		{"wrong_operation", func(e, p map[string]any) {
			p["metadata"].(map[string]any)["openrails_engine_operation"] = uuid.NewString()
		}},
		{"malformed_operation", func(e, p map[string]any) {
			p["metadata"].(map[string]any)["openrails_engine_operation"] = "not-an-operation"
		}},
		{"wrong_merchant", func(e, p map[string]any) { p["metadata"].(map[string]any)["openrails_merchant"] = uuid.NewString() }},
		{"wrong_psp", func(e, p map[string]any) { p["metadata"].(map[string]any)["openrails_psp"] = uuid.NewString() }},
		{"wrong_local_customer", func(e, p map[string]any) { p["metadata"].(map[string]any)["openrails_customer"] = uuid.NewString() }},
		{"wrong_original_payment", func(e, p map[string]any) { p["id"] = "pi_unmatched" }},
	} {
		if tc.name == "wrong_original_payment" && !knownPI {
			continue // The separate unretained-identity case below checks discovery.
		}
		t.Run("signed_snapshot_"+tc.name, func(t *testing.T) {
			object := cloneStripeNotification(t, payment)
			event := snapshot(object)
			tc.mutate(event, object)
			before := jobCount()
			status, body := post(event, path, secret)
			require.GreaterOrEqual(t, status, 400, body)
			unchanged(before)
		})
	}
	t.Run("wrong_signature", func(t *testing.T) {
		before := jobCount()
		status, body := post(snapshot(payment), path, "whsec_other")
		require.Equal(t, http.StatusUnauthorized, status, body)
		unchanged(before)
	})
	t.Run("unknown_account_route", func(t *testing.T) {
		before := jobCount()
		status, body := post(snapshot(payment), "/v1/webhooks/stripe/acct_missing", secret)
		require.GreaterOrEqual(t, status, 400, body)
		unchanged(before)
	})
	t.Run("wrong_merchant_route", func(t *testing.T) {
		other := surface.ProvisionOwnedMerchant("pi-cross-" + uuid.NewString()[:8])
		otherPSP := h.ArmLoopbackStripe(rt, other.MerchantID)
		otherPool := h.MerchantPool(other.MerchantID.UUID())
		var otherAccount string
		require.NoError(t, otherPool.QueryRow(ctx, `SELECT account_id FROM billing.psps WHERE id=$1`, otherPSP).Scan(&otherAccount))
		otherName, err := merchants.PSPSecretName("stripe", "test", otherAccount, "webhook_signing_secret")
		require.NoError(t, err)
		const otherSecret = "whsec_independent_other_merchant"
		_, err = rt.Merchants.Secrets().Put(ctx, other.MerchantID, otherName, otherSecret)
		require.NoError(t, err)
		event := snapshot(payment)
		event["account"] = otherAccount
		before := jobCount()
		status, body := post(event, "/v1/webhooks/stripe/"+otherAccount, otherSecret)
		require.GreaterOrEqual(t, status, 400, body)
		unchanged(before)
	})

	thin := func(object map[string]any, eventType string) map[string]any {
		return map[string]any{"id": nextID(), "object": "v2.core.event", "type": "v1." + eventType, "created": now.Format(time.RFC3339Nano), "context": account, "livemode": false, "related_object": map[string]any{"id": object["id"], "type": "payment_intent", "url": "/v1/payment_intents/" + object["id"].(string)}}
	}
	setThin := func(event, object, snap map[string]any) {
		responses := map[string]map[string]any{"/v2/core/events/" + event["id"].(string): event, "/v1/payment_intents/" + object["id"].(string): object}
		if snap != nil {
			responses["/v1/events/"+snap["id"].(string)] = snap
		}
		setResponses(responses)
	}
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any, map[string]any)
	}{
		{"wrong_context", func(e, p map[string]any) { e["context"] = "acct_other" }},
		{"wrong_account", func(e, p map[string]any) { e["account"] = "acct_other" }},
		{"wrong_related_type", func(e, p map[string]any) { e["related_object"].(map[string]any)["type"] = "charge" }},
		{"wrong_fetched_type", func(e, p map[string]any) { p["object"] = "charge" }},
		{"wrong_fetched_customer", func(e, p map[string]any) { p["customer"] = "cus_other" }},
	} {
		t.Run("signed_thin_"+tc.name, func(t *testing.T) {
			object := cloneStripeNotification(t, payment)
			event := thin(object, "payment_intent.succeeded")
			tc.mutate(event, object)
			setThin(event, object, nil)
			before := jobCount()
			status, body := post(event, path, secret)
			require.GreaterOrEqual(t, status, 400, body)
			unchanged(before)
		})
	}
	if !knownPI {
		// A lost response has no local PI reference yet. Even a signed wakeup
		// with matching immutable metadata cannot attach its supplied PI ID.
		object := cloneStripeNotification(t, payment)
		object["id"] = "pi_unretained"
		event := thin(object, "payment_intent.succeeded")
		setThin(event, object, nil)
		status, body := post(event, path, secret)
		require.Equal(t, http.StatusOK, status, body)
		var evidence string
		require.NoError(t, pool.QueryRow(ctx, `SELECT coalesce(result_evidence::text,'') FROM billing.rail_intents WHERE id=$1`, operation).Scan(&evidence))
		require.NotContains(t, evidence, "pi_unretained", "webhook must not install an unqualified recovery candidate")
	}

	t.Run("ignored_pi_event_is_redacted", func(t *testing.T) {
		event := snapshot(payment)
		event["type"] = "payment_intent.created"
		before := jobCount()
		status, body := post(event, path, secret)
		require.Equal(t, http.StatusOK, status, body)
		unchanged(before)
		assertCacheSafe("payment_intent.created", event["id"].(string))
	})

	// Failure/authentication signals may only request readback, never declare a
	// decline or make a new charge. The worker will see the canonical paid state.
	for _, kind := range []string{"payment_intent.payment_failed", "payment_intent.requires_action"} {
		object := cloneStripeNotification(t, payment)
		object["status"] = "requires_action"
		if strings.HasSuffix(kind, "payment_failed") {
			object["status"] = "requires_payment_method"
		}
		event := thin(object, kind)
		setThin(event, object, nil)
		status, body := post(event, path, secret)
		require.Equal(t, http.StatusOK, status, body)
		assertCacheSafe(kind, event["id"].(string))
	}
	snap := snapshot(payment)
	event := thin(payment, "payment_intent.succeeded")
	event["snapshot_event"] = snap["id"]
	setThin(event, payment, snap)
	before := jobCount()
	for range 2 {
		status, body := post(event, path, secret)
		require.Equal(t, http.StatusOK, status, body)
	}
	status, body := post(snap, path, secret)
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, before+1, jobCount(), "thin retry and overlapping snapshot share one completed webhook ID")
	assertCacheSafe("payment_intent.succeeded", snap["id"].(string))
	var due time.Time
	require.NoError(t, pool.QueryRow(ctx, `SELECT next_attempt_at FROM billing.rail_intents WHERE id=$1`, operation).Scan(&due))
	require.False(t, due.After(now), "notification must advance unknown verification without a SQL fixture time change")
	setResponses(nil)
	return func() {
		// A fresh late event for the now-terminal operation is acknowledged without
		// scheduling another financial attempt or changing the completed receipt.
		before := jobCount()
		late := snapshot(payment)
		status, body := post(late, path, secret)
		require.Equal(t, http.StatusOK, status, body)
		require.Equal(t, before, jobCount())
		assertCacheSafe("payment_intent.succeeded", late["id"].(string))
		var state string
		require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM billing.rail_intents WHERE id=$1`, operation).Scan(&state))
		require.Equal(t, "succeeded", state)
		var evidence string
		require.NoError(t, pool.QueryRow(ctx, `SELECT coalesce(result_evidence::text,'') FROM billing.rail_intents WHERE id=$1`, operation).Scan(&evidence))
		require.Contains(t, evidence, payment["id"])
		require.NotContains(t, evidence, "pi_unretained", "only provider readback may choose the recovered PI")
		t.Logf("verified original operation %s through signed HTTP and started River", operation)
	}
}
