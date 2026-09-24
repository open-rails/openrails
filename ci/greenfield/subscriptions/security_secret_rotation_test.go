//go:build greenfield && integration

package subscriptions_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/embed"
)

// declarePrevious declares a rotated-out webhook secret for rail, with an
// overlap expiry when expires is non-zero.
func declarePrevious(rail, old string, expires time.Time) func(map[string]embed.PSPConfig) {
	return func(psps map[string]embed.PSPConfig) {
		account := psps[rail][rail]
		account.Secrets["webhook_signing_secret_previous"] = old
		if !expires.IsZero() {
			settings := map[string]any{"webhook_overlap_expires_at": expires.Format(time.RFC3339)}
			for k, v := range account.Settings {
				settings[k] = v
			}
			account.Settings = settings
		}
		psps[rail][rail] = account
	}
}

// SEC-29: a rotated-out webhook signing secret verifies only until its
// overlap expires. After that, a notice signed with the old secret, as a
// leaked secret would sign a forged "paid" notice, is refused and changes
// nothing; the current secret keeps working. A previous secret declared with
// no expiry is refused outright.
func TestSecurityRotatedWebhookSecretExpires(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		old := "whsec_rotated_out"
		if rail == "nmi" {
			old = "nmi_webhook_rotated_out"
		}
		t.Run(rail+"/bounded", func(t *testing.T) {
			t.Parallel()
			w := prepareWorld(t, 12)
			expires := w.clock.Now().Add(25 * day)
			w.declare = declarePrevious(rail, old, expires)
			w.start()
			l := importLegacy(t, w, rail, embedded)
			w.converge()
			own := w.webhookTarget(rail)
			paid := len(completed(w.payments(embedded, l.c.id)))

			end := l.periodEnd()
			w.advance(end.Sub(w.clock.Now()) + time.Hour)
			require.True(t, w.clock.Now().Before(expires))
			body, err := json.Marshal(l.providerRenewal(true))
			require.NoError(t, err)
			status, raw := w.postWebhook(own, signature(own, old, time.Now(), body), body)
			require.Equal(t, http.StatusOK, status, "inside the overlap the old secret verifies: %s", raw)
			require.True(t, l.periodEnd().After(end))
			require.Len(t, completed(w.payments(embedded, l.c.id)), paid+1)

			renewed := l.periodEnd()
			w.advance(renewed.Sub(w.clock.Now()) + time.Hour)
			require.True(t, w.clock.Now().After(expires))
			body, err = json.Marshal(l.providerRenewal(true))
			require.NoError(t, err)
			status, raw = w.postWebhook(own, signature(own, old, time.Now(), body), body)
			require.Equal(t, http.StatusUnauthorized, status, "after the overlap the old secret is refused: %s", raw)
			require.True(t, l.periodEnd().Equal(renewed), "a notice signed with the expired secret moves nothing")
			require.Len(t, completed(w.payments(embedded, l.c.id)), paid+1, "and records no payment")
			require.Len(t, completed(w.payments(remote, l.c.id)), paid+1)

			status, raw = w.postWebhook(own, signature(own, own.secret, time.Now(), body), body)
			require.Equal(t, http.StatusOK, status, raw)
			require.True(t, l.periodEnd().After(renewed), "the current secret still verifies")
		})
		t.Run(rail+"/no_expiry", func(t *testing.T) {
			t.Parallel()
			w := prepareWorld(t, 12)
			w.declare = declarePrevious(rail, old, time.Time{})
			w.start()
			l := importLegacy(t, w, rail, embedded)
			w.converge()
			own := w.webhookTarget(rail)
			end := l.periodEnd()
			w.advance(end.Sub(w.clock.Now()) + time.Hour)
			body, err := json.Marshal(l.providerRenewal(true))
			require.NoError(t, err)
			status, raw := w.postWebhook(own, signature(own, old, time.Now(), body), body)
			require.Equal(t, http.StatusUnauthorized, status, "a previous secret without an expiry never verifies: %s", raw)
			require.True(t, l.periodEnd().Equal(end))
		})
	}
}

// SEC-33: an unknown provider account and a bad signature for a real one get
// the same answer, so webhook routes cannot enumerate the accounts served.
func TestSecurityWebhookResponsesRevealNoAccounts(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, rail := range rails {
		own := w.webhookTarget(rail)
		unknown := own
		unknown.path = "/v1/webhooks/" + rail + "/not-a-configured-account"
		body := []byte(`{"id":"evt_probe","type":"invoice.paid","event_id":"probe","event_type":"transaction.sale.success","event_body":{"merchant":{"id":"not-a-configured-account"}}}`)
		if rail == "nmi" {
			body = []byte(`{"event_id":"probe","event_type":"transaction.sale.success","event_body":{"merchant":{"id":"` + nmiAcct + `"}}}`)
		}
		knownStatus, knownBody := w.postWebhook(own, signature(own, "whsec_attacker", time.Now(), body), body)
		if rail == "nmi" {
			body = []byte(`{"event_id":"probe","event_type":"transaction.sale.success","event_body":{"merchant":{"id":"not-a-configured-account"}}}`)
		}
		unknownStatus, unknownBody := w.postWebhook(unknown, signature(unknown, "whsec_attacker", time.Now(), body), body)
		require.Equal(t, http.StatusUnauthorized, knownStatus, "%s: %s", rail, knownBody)
		require.Equal(t, knownStatus, unknownStatus, "%s: %s", rail, unknownBody)
		require.Equal(t, withoutRequestID(t, knownBody), withoutRequestID(t, unknownBody), rail)
	}
}

func withoutRequestID(t *testing.T, raw string) map[string]any {
	t.Helper()
	out := map[string]any{}
	require.NoError(t, json.Unmarshal([]byte(raw), &out), raw)
	if e, ok := out["error"].(map[string]any); ok {
		delete(e, "request_id")
	}
	return out
}
