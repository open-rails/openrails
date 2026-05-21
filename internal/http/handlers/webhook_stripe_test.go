package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/shared/webhookutil"
	"github.com/stretchr/testify/require"
)

func signStripe(secret string, body []byte) string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(body)))
	return fmt.Sprintf("t=%s,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

func TestPrepareStripeMultiSecret(t *testing.T) {
	body := []byte(`{"id":"evt_1","type":"checkout.session.completed","data":{"object":{}}}`)
	snapshot := "whsec_snapshot"
	thin := "whsec_thin"

	// Signed with the thin secret, but the snapshot secret is listed first:
	// verification must still succeed by trying each configured secret.
	prepared, err := prepareStripeMultiSecret(body, []string{snapshot, thin}, signStripe(thin, body), time.Minute)
	require.NoError(t, err)
	require.Equal(t, "evt_1", prepared.EventID)
	require.Equal(t, "checkout.session.completed", prepared.EventType)
	require.True(t, prepared.SignatureVerified)

	// No configured secret matches -> signature invalid.
	_, err = prepareStripeMultiSecret(body, []string{snapshot, thin}, signStripe("other", body), time.Minute)
	require.ErrorIs(t, err, webhookutil.ErrWebhookSignatureInvalid)

	// No secrets configured at all -> signature required.
	_, err = prepareStripeMultiSecret(body, nil, signStripe(thin, body), time.Minute)
	require.ErrorIs(t, err, webhookutil.ErrWebhookSignatureRequired)
}

func TestHydrateThinStripeEvent(t *testing.T) {
	ctx := context.Background()

	// Snapshot event (object already present) -> nothing to hydrate.
	out, err := hydrateThinStripeEvent(ctx, "sk_test", []byte(`{"id":"evt_1","type":"x","data":{"object":{"id":"cs_1"}}}`))
	require.NoError(t, err)
	require.Nil(t, out)

	// No related_object -> not a hydratable thin event, pass through.
	out, err = hydrateThinStripeEvent(ctx, "sk_test", []byte(`{"id":"evt_2","type":"x"}`))
	require.NoError(t, err)
	require.Nil(t, out)

	// Thin event but no secret key configured -> error (cannot fetch object).
	_, err = hydrateThinStripeEvent(ctx, "", []byte(`{"id":"evt_3","type":"x","related_object":{"id":"cs_1","type":"checkout.session","url":"https://api.stripe.com/v1/checkout/sessions/cs_1"}}`))
	require.Error(t, err)
}
