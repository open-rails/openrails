//go:build greenfield && integration

package subscriptions_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type webhookTarget struct {
	server, path, header, scheme, secret string
}

func (w *world) webhookTarget(rail string) webhookTarget {
	if rail == "stripe" {
		return webhookTarget{w.server.URL, "/v1/webhooks/stripe/" + stripeAcct, "Stripe-Signature", "v1", whsecStripe}
	}
	return webhookTarget{w.server.URL, "/v1/webhooks/nmi/" + nmiAcct, "Webhook-Signature", "s", whsecNMI}
}

func (r *rival) webhookTarget(rail string) webhookTarget {
	if rail == "stripe" {
		return webhookTarget{r.server.URL, "/v1/webhooks/stripe/acct_rival", "Stripe-Signature", "v1", "whsec_rival"}
	}
	return webhookTarget{r.server.URL, "/v1/webhooks/nmi/rival-nmi", "Webhook-Signature", "s", "nmi_webhook_rival"}
}

func signature(target webhookTarget, secret string, at time.Time, body []byte) string {
	ts := strconv.FormatInt(at.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	return fmt.Sprintf("t=%s,%s=%s", ts, target.scheme, hex.EncodeToString(mac.Sum(nil)))
}

func (w *world) postWebhook(target webhookTarget, header string, body []byte) (int, string) {
	w.t.Helper()
	req, err := http.NewRequestWithContext(w.t.Context(), http.MethodPost, target.server+mountPrefix+target.path, bytes.NewReader(body))
	require.NoError(w.t, err)
	req.Header.Set("Content-Type", "application/json")
	if header != "" {
		req.Header.Set(target.header, header)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(w.t, err)
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	w.settle()
	return res.StatusCode, string(raw)
}

// SEC: webhook authenticity. A provider renewal notice extends a provider-owned
// membership only when it is signed with this merchant's secret for this
// merchant's endpoint, fresh and unmodified. A second merchant on the same
// deployment, holding its own valid secret, cannot move another merchant's
// memberships by naming their provider ids. Responses never echo secrets.
func TestSecurityWebhookAuthenticity(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			l := importLegacy(t, w, rail, embedded)
			w.converge()
			r := w.rival()
			end := l.periodEnd()
			w.advance(end.Sub(w.clock.Now()) + time.Hour)
			notice := l.providerRenewal(true)
			body, err := json.Marshal(notice)
			require.NoError(t, err)
			tampered := bytes.Replace(body, []byte(`9.99`), []byte(`0.01`), 1)
			if rail == "stripe" {
				tampered = bytes.Replace(body, []byte(`"amount_paid":999`), []byte(`"amount_paid":1`), 1)
			}
			own, foreign := w.webhookTarget(rail), r.webhookTarget(rail)
			now := time.Now()
			paid := len(completed(w.payments(embedded, l.c.id)))

			for _, tc := range []struct {
				name   string
				target webhookTarget
				header string
				body   []byte
			}{
				{"unsigned", own, "", body},
				{"garbage signature", own, "t=1,v1=00,s=00", body},
				{"wrong secret", own, signature(own, "whsec_attacker", now, body), body},
				{"another merchant's secret", own, signature(own, foreign.secret, now, body), body},
				{"stale timestamp", own, signature(own, own.secret, now.Add(-time.Hour), body), body},
				{"future timestamp", own, signature(own, own.secret, now.Add(time.Hour), body), body},
				{"body modified after signing", own, signature(own, own.secret, now, body), tampered},
				{"another merchant's endpoint and secret", foreign, signature(foreign, foreign.secret, now, body), body},
			} {
				status, raw := w.postWebhook(tc.target, tc.header, tc.body)
				t.Logf("%s -> %d %s", tc.name, status, raw)
				if tc.target == own {
					require.GreaterOrEqual(t, status, 400, tc.name)
				}
				for _, secret := range []string{own.secret, foreign.secret, "sk_test_greenfield", "greenfield-nmi-key"} {
					require.NotContains(t, raw, secret, tc.name)
				}
				require.True(t, l.periodEnd().Equal(end), "%s moved the membership", tc.name)
				require.Len(t, completed(w.payments(embedded, l.c.id)), paid, "%s recorded a payment", tc.name)
			}

			status, raw := w.postWebhook(own, signature(own, own.secret, now, body), body)
			require.Equal(t, http.StatusOK, status, raw)
			require.True(t, l.periodEnd().After(end), "the genuine notice renews")
			require.Len(t, completed(w.payments(embedded, l.c.id)), paid+1)
		})
	}
}
