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
	"strings"
	"time"

	"github.com/stretchr/testify/require"
)

// refundGateServed parks the rail's next refund response after the provider
// has already refunded, as a slow network would.
func (w *world) refundGateServed(rail string) *gate {
	g := newGate(func(r *http.Request) bool {
		if rail == "stripe" {
			return r.Method == http.MethodPost && r.URL.Path == "/v1/refunds"
		}
		return strings.HasSuffix(r.URL.Path, "/refund")
	}, false)
	g.served = true
	if rail == "stripe" {
		return w.stripe.hold(g)
	}
	return w.nmi.hold(g)
}

// deliverNow posts a signed provider notice without waiting for engine work,
// for use while an operation is held at the provider.
func (w *world) deliverNow(rail string, payload any) int {
	w.t.Helper()
	body, err := json.Marshal(payload)
	require.NoError(w.t, err)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	path, header, secret, scheme := "/v1/webhooks/stripe/"+stripeAcct, "Stripe-Signature", whsecStripe, "v1"
	if rail == "nmi" {
		path, header, secret, scheme = "/v1/webhooks/nmi/"+nmiAcct, "Webhook-Signature", whsecNMI, "s"
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(body)))
	req, err := http.NewRequestWithContext(w.t.Context(), http.MethodPost, w.server.URL+mountPrefix+path, bytes.NewReader(body))
	require.NoError(w.t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(header, fmt.Sprintf("t=%s,%s=%s", ts, scheme, hex.EncodeToString(mac.Sum(nil))))
	res, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	require.NoError(w.t, err, "a provider notice is never blocked by an operation in flight")
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	w.t.Logf("webhook %s -> %d %s", rail, res.StatusCode, raw)
	return res.StatusCode
}
