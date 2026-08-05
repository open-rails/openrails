//go:build integration

package tests

// NMI merchant-webhook signature verification over real HTTP (#694 GAP 2).
// Full stack: real Postgres-backed merchant resolution, the seeded
// webhook-signing secret loaded through the merchant secret store, the
// sigverify HMAC scheme (t=<unix>,s=hex(hmac-sha256(secret, ts+"."+body))),
// and the real webhook dispatcher.
//
//	valid signature   ⇒ 202-path "accepted" AND (#684) the subscription is
//	                    marked dirty: a coalesced fetch-and-converge job is
//	                    enqueued; the payload itself moves NO state (webhooks
//	                    are wake-up signals, provider truth decides)
//	tampered/foreign  ⇒ 401, no state change
//	missing signature ⇒ 401, no state change

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// signNMIWebhook produces the Webhook-Signature header value NMI sends:
// t=<unix>,s=<hex hmac-sha256(secret, ts + "." + body)>.
func signNMIWebhook(secret string, body []byte) string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(body)))
	return fmt.Sprintf("t=%s,s=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

func postNMIMerchantWebhook(t *testing.T, suite *TestContainerSuite, body []byte, signature string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	// or#893: standalone serves ONE webhook surface. The merchant comes from the
	// payload's own gateway identity (event_body.merchant.id), never a URL slug.
	req, err := http.NewRequest(http.MethodPost, "/v1/webhooks/nmi", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if signature != "" {
		req.Header.Set("Webhook-Signature", signature)
	}
	suite.Server.Handler().ServeHTTP(w, req)
	return w
}

func TestNMIMerchantWebhookSignatureHTTP(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := suite.MerchantCtx()

	// Seed the webhook signing secret for the suite's active NMI account — the
	// exact secret LoadNMIWebhookSigningSecret resolves on the merchant surface.
	const signingSecret = "nmi-e2e-webhook-signing-secret"
	nmiEnv := config.ExpectedProviderEnvironment(suite.Config.IsTestMode())
	secretName, err := merchants.PSPSecretName("nmi", nmiEnv, testNMIRailMerchantAccountID(), "webhook_signing_secret")
	require.NoError(t, err)
	_, err = suite.App.Runtime.Merchants.PutCredential(ctx, dbtest.TestMerchantID, secretName, signingSecret)
	require.NoError(t, err)

	// A live NMI subscription the delete event should cancel.
	products := suite.SeedProducts()
	userID := uuid.New().String()
	railSubID := "nmi-sig-e2e-" + uuid.New().String()[:8]
	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:    userID,
		PriceID:   products[0].Prices[0].ID,
		Status:    models.StatusActive,
		Rail:      models.RailNMI,
		RailSubID: railSubID,
	})

	body := []byte(fmt.Sprintf(
		`{"event_id":"evt_nmi_sig_%s","event_type":"recurring.subscription.delete","event_body":{"merchant":{"id":"%s"},"subscription_id":"%s"}}`,
		uuid.New().String()[:8], testNMIRailMerchantAccountID(), railSubID))

	t.Run("missing signature rejected", func(t *testing.T) {
		w := postNMIMerchantWebhook(t, suite, body, "")
		assert.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
	})

	t.Run("tampered signature rejected", func(t *testing.T) {
		// Signed with the WRONG secret (equivalently: body tampered after signing).
		w := postNMIMerchantWebhook(t, suite, body, signNMIWebhook("attacker-secret", body))
		assert.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
	})

	t.Run("signed-then-modified body rejected", func(t *testing.T) {
		sig := signNMIWebhook(signingSecret, body)
		tampered := bytes.Replace(body, []byte(railSubID), []byte("some-other-sub"), 1)
		w := postNMIMerchantWebhook(t, suite, tampered, sig)
		assert.Equal(t, http.StatusUnauthorized, w.Code, "body: %s", w.Body.String())
	})

	// Nothing above may have touched the subscription.
	require.Equal(t, models.StatusActive, suite.GetSubscription(sub.ID).Status,
		"rejected webhooks must not change state")

	t.Run("valid signature accepted and converge enqueued", func(t *testing.T) {
		w := postNMIMerchantWebhook(t, suite, body, signNMIWebhook(signingSecret, body))
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
		assert.Contains(t, w.Body.String(), "accepted")

		// #684: the handler is identity-only — the payload moved no state...
		assert.Equal(t, models.StatusActive, suite.GetSubscription(sub.ID).Status,
			"the webhook payload itself must not move subscription state")

		// ...and the dirty mark is durably queued: ONE coalesced
		// fetch-and-converge job for this subscription (provider truth, not the
		// payload, will decide the transition when the worker fetches).
		var queued int
		require.NoError(t, suite.Pool.QueryRow(context.Background(),
			`SELECT count(*) FROM `+config.RiverSchema+`.river_job
			 WHERE kind = $1 AND args->>'subscription_reference' = $2
			   AND state IN ('available','scheduled','pending','running','retryable')`,
			riverjobs.KindSubscriptionConverge, railSubID).Scan(&queued))
		assert.Equal(t, 1, queued, "valid webhook must enqueue exactly one converge job")
	})

	// Don't leak a converge job that would retry against the real NMI API for
	// the rest of the shared suite.
	t.Cleanup(func() {
		_, _ = suite.Pool.Exec(context.Background(),
			`DELETE FROM `+config.RiverSchema+`.river_job WHERE kind = $1 AND args->>'subscription_reference' = $2`,
			riverjobs.KindSubscriptionConverge, railSubID)
	})
}
