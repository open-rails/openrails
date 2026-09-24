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
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// deliver posts one provider notification, signed as the provider signs it,
// to the mounted webhook route, and returns the HTTP status.
func (w *world) deliver(rail string, payload any) int {
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
	res, err := http.DefaultClient.Do(req)
	require.NoError(w.t, err)
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	w.t.Logf("webhook %s -> %d %s", rail, res.StatusCode, raw)
	w.settle()
	return res.StatusCode
}

func stripeEvent(kind string, object obj) obj {
	return obj{"id": "evt_" + uuid.NewString()[:12], "object": "event", "type": kind, "created": time.Now().Unix(), "livemode": false, "api_version": "2026-06-24.dahlia", "data": obj{"object": object}}
}

func nmiEvent(kind string, body obj) obj {
	body["merchant"] = obj{"id": nmiAcct, "name": "greenfield"}
	return obj{"event_id": uuid.NewString(), "event_type": kind, "event_body": body}
}

// refundNotice is the provider telling OpenRails about its latest refund.
func (w *world) refundNotice(rail string) obj {
	if rail == "stripe" {
		w.stripe.mu.Lock()
		defer w.stripe.mu.Unlock()
		for _, re := range w.stripe.refunds {
			ch := w.stripe.charges[fmt.Sprint(re["charge"])]
			return stripeEvent("charge.refunded", obj{"object": "charge", "id": ch["id"], "payment_intent": ch["payment_intent"], "amount": ch["amount"], "amount_refunded": ch["amount_refunded"], "refunded": ch["refunded"], "currency": ch["currency"], "customer": ch["customer"],
				"refunds": obj{"object": "list", "data": []obj{re}}})
		}
		w.t.Fatal("no Stripe refund to notify")
	}
	w.nmi.mu.Lock()
	defer w.nmi.mu.Unlock()
	for _, s := range w.nmi.sales {
		if s.RefundedCents > 0 {
			return nmiEvent("transaction.refund.success", obj{"transaction_id": s.RefundIDs[len(s.RefundIDs)-1], "transaction_type": "cc", "condition": "complete", "amount": fmt.Sprintf("%.2f", float64(s.RefundedCents)/100), "currency": "USD", "customer_vault_id": s.Vault,
				"action": obj{"action_type": "refund", "amount": fmt.Sprintf("%.2f", float64(s.RefundedCents)/100), "success": "1"}, "transaction": obj{"transaction_id": s.TransactionID}})
		}
	}
	w.t.Fatal("no NMI refund to notify")
	return nil
}
