//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

func signNMIWebhook(secret string, body []byte) string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(body)))
	return fmt.Sprintf("t=%s,s=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

// A valid NMI webhook wakes the provider reconciler; it cannot itself assert
// cancellation. Missing, foreign or altered signatures cannot move any state.
func TestNMIMerchantWebhookSignatureHTTP(t *testing.T) {
	h := New(t, t.Context())
	surface := h.StartStandalone("USD", WithConfig(func(cfg *config.Config) {
		cfg.MerchantSource = config.MerchantSourceAPI
		cfg.SecretBackend = config.SecretBackendDB
	}))
	owned := surface.ProvisionOwnedMerchant("nmi-signature-" + uuid.NewString()[:8])
	account := fmt.Sprint(100000 + uuid.New().ID()%2_000_000_000)
	const secret = "synthetic-nmi-webhook-secret"
	SeedPSPs(t.Context(), t, surface.App().Runtime, owned.MerchantID, config.PSPSet{"nmi": {Rail: "nmi", AccountID: account, NMI: &config.NMIRailConfig{SecurityKey: "synthetic-key", WebhookSigningSecret: secret}}})
	pool := h.MerchantPool(owned.MerchantID.UUID())
	rows := seedMerchantBillingRows(t, t.Context(), pool, owned.MerchantID)
	// A raw numeric provider ID above 2^53 must retain every digit, just like
	// its quoted representation. A float round-trip would route the wrong job.
	railSub := strconv.FormatUint(9_007_199_254_740_993+uint64(uuid.New().ID()), 10)
	_, err := pool.Exec(t.Context(), `UPDATE billing.subscriptions SET rail_subscription_id=$2 WHERE id=$1`, rows.subscriptionID, railSub)
	require.NoError(t, err)
	body := []byte(fmt.Sprintf(`{"event_id":"evt_%s","event_type":"recurring.subscription.delete","event_body":{"merchant":{"id":"%s"},"subscription_id":%s}}`, uuid.NewString(), account, railSub))
	post := func(body []byte, signature string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, surface.BaseURL+"/v1/webhooks/nmi", bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		if signature != "" {
			req.Header.Set("Webhook-Signature", signature)
		}
		response, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer response.Body.Close()
		raw, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		return response.StatusCode, raw
	}
	for _, row := range []struct {
		body      []byte
		signature string
	}{
		{body, ""}, {body, signNMIWebhook("wrong-account-secret", body)},
		{bytes.Replace(body, []byte(railSub), []byte("9007199254740992"), 1), signNMIWebhook(secret, body)},
	} {
		status, raw := post(row.body, row.signature)
		require.Equal(t, http.StatusUnauthorized, status, string(raw))
	}
	var state string
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT status FROM billing.subscriptions WHERE id=$1`, rows.subscriptionID).Scan(&state))
	require.Equal(t, "active", state)
	t.Cleanup(func() {
		_, err := h.Pool().Exec(context.Background(), `DELETE FROM public.river_job WHERE kind=$1 AND args->>'subscription_reference'=$2`, riverjobs.KindSubscriptionConverge, railSub)
		require.NoError(t, err, "clean up this subscription's queued wakeup before another worker runs")
	})
	quoted := bytes.Replace(body, []byte(`"subscription_id":`+railSub), []byte(`"subscription_id":"`+railSub+`"`), 1)
	for _, payload := range [][]byte{body, quoted} {
		status, raw := post(payload, signNMIWebhook(secret, payload))
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Contains(t, string(raw), "accepted")
	}
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT status FROM billing.subscriptions WHERE id=$1`, rows.subscriptionID).Scan(&state))
	require.Equal(t, "active", state, "provider payload cannot directly cancel paid access")
	var jobs int
	require.NoError(t, h.Pool().QueryRow(t.Context(), `SELECT count(*) FROM public.river_job WHERE kind=$1 AND args->>'subscription_reference'=$2 AND state IN('available','scheduled','pending','running','retryable')`, riverjobs.KindSubscriptionConverge, railSub).Scan(&jobs))
	require.Equal(t, 1, jobs, "valid duplicate delivery produces one durable coalesced wakeup")
}
