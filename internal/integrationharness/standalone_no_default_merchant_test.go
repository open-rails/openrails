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
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

func TestStandaloneNoDefaultMerchantResolvesRequestScopedMerchant(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")

	require.True(t, surface.app.Runtime.ConfiguredMerchant.IsZero(), "standalone must not carry a default merchant")

	status, body := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/service/merchant-configuration", surface.Token, nil)
	require.Equal(t, http.StatusOK, status, string(body))

	cp := embcp.Get(surface.app)
	require.NotNil(t, cp)
	secretStore, err := merchants.NewDBSecretStore(cp.Pool())
	require.NoError(t, err)
	const whsec = "whsec_no_default_merchant"
	_, err = secretStore.Put(ctx, dbtest.TestMerchantID, merchants.SecretStripeWebhookSigning, whsec)
	require.NoError(t, err)

	event := []byte(`{"id":"evt_no_default","type":"customer.created","data":{"object":{"id":"cus_no_default"}}}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, surface.BaseURL+"/v1/merchants/"+dbtest.TestMerchantSlug+"/webhooks/stripe", bytes.NewReader(event))
	require.NoError(t, err)
	req.Header.Set("Stripe-Signature", stripeSignature(whsec, event))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
}

func stripeSignature(secret string, body []byte) string {
	ts := fmt.Sprintf("%d", time.Now().Unix())
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "." + string(body)))
	return fmt.Sprintf("t=%s,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}
