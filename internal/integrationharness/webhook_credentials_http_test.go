//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/alerting"
	"github.com/open-rails/openrails/internal/shared/httpx"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func TestWebhookCredentialHTTPWorkflow(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd", WithConfig(func(cfg *config.Config) {
		cfg.Encryption = &config.EncryptionConfig{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="}
	}))
	rt := surface.App().Runtime
	cp := embcp.Get(surface.App())
	actor := h.ensureAPIKeyActor(cp, dbtest.TestMerchantSlug)
	_, viewer, err := cp.Core().MintAPIKeyWithOptions(ctx, controlplane.MerchantGroup(dbtest.TestMerchantSlug), authkit.APIKeyMintOptions{Name: "webhook-reader", Role: controlplane.MerchantRoleViewer, CreatedBy: actor})
	require.NoError(t, err)
	var mu sync.Mutex
	var paths []string
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.RequestURI())
		mu.Unlock()
		w.WriteHeader(204)
	}))
	t.Cleanup(sink.Close)
	// Only the outbound test transport permits this owned loopback sink.
	rt.AlertService = alerting.NewService(alerting.Deps{DB: rt.DB, Secrets: rt.Merchants.Secrets(), Outbound: httpx.Policy{Allow: httpx.AllowLoopback}, WebhookBackoff: time.Millisecond})
	rawURL := sink.URL + "/hook/path-secret?token=query-secret"
	base := surface.BaseURL + "/v1/merchant"
	status, raw := requestJSON(t, http.MethodPost, base+"/webhooks", surface.Token, map[string]any{"name": "synthetic sink", "url": rawURL, "format": "slack"})
	require.Equal(t, 201, status, string(raw))
	require.NotContains(t, string(raw), "path-secret")
	require.NotContains(t, string(raw), "query-secret")
	require.NotContains(t, string(raw), `"url"`)
	var hook alerting.Webhook
	require.NoError(t, json.Unmarshal(raw, &hook))
	require.NotEqual(t, uuid.Nil, hook.ID)
	name := merchants.AlertWebhookURLSecretName(hook.ID)
	var stored string
	require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT value FROM openrails.merchant_secrets WHERE merchant_id=$1 AND name=$2`, dbtest.TestMerchantID.UUID(), name).Scan(&stored))
	require.NotContains(t, stored, "path-secret")
	require.NotContains(t, stored, "query-secret")
	require.NotContains(t, stored, sink.URL)
	status, raw = requestJSON(t, http.MethodGet, base+"/webhooks", viewer, nil)
	require.Equal(t, 200, status, string(raw))
	require.NotContains(t, string(raw), "secret")
	require.NotContains(t, string(raw), `"url"`)
	status, raw = requestJSON(t, http.MethodPost, base+"/alerts/rules", surface.Token, map[string]any{"name": "delivery", "template": "chargeback_rate_by_rail_account", "params": map[string]any{"threshold": 0.1}, "channels": []map[string]any{{"type": "webhook", "webhook_id": hook.ID}}})
	require.Equal(t, 201, status, string(raw))
	var rule alerting.Rule
	require.NoError(t, json.Unmarshal(raw, &rule))
	testURL := base + "/alerts/rules/" + rule.ID.String() + "/test"
	status, _ = requestJSON(t, http.MethodPost, testURL, viewer, nil)
	require.Equal(t, 403, status)
	rotateURL := base + "/webhooks/" + hook.ID.String() + "/url"
	status, _ = requestJSON(t, http.MethodPut, rotateURL, viewer, map[string]any{"url": rawURL})
	require.Equal(t, 403, status)
	other := surface.ProvisionOwnedMerchant("webhook-other-" + uuid.NewString()[:8])
	status, _ = requestJSON(t, http.MethodPut, rotateURL, other.APIKey, map[string]any{"url": rawURL})
	require.Equal(t, 404, status)
	nextURL := sink.URL + "/hook/rotated-secret?token=rotated-query"
	status, raw = requestJSON(t, http.MethodPut, rotateURL, surface.Token, map[string]any{"url": nextURL})
	require.Equal(t, 200, status, string(raw))
	require.NotContains(t, string(raw), "rotated-secret")
	var rotated alerting.Webhook
	require.NoError(t, json.Unmarshal(raw, &rotated))
	require.Equal(t, hook.ID, rotated.ID)
	status, raw = requestJSON(t, http.MethodPost, testURL, surface.Token, nil)
	require.Equal(t, 200, status, string(raw))
	require.Contains(t, string(raw), `"ok":true`)
	mu.Lock()
	require.Equal(t, []string{"/hook/rotated-secret?token=rotated-query"}, paths)
	mu.Unlock()
	// A secret update whose metadata write did not commit must not deliver the
	// newer credential. Repeating the update repairs the exact version binding.
	newerURL := sink.URL + "/hook/pending-secret"
	_, err = rt.Merchants.Secrets().Put(ctx, dbtest.TestMerchantID, name, newerURL)
	require.NoError(t, err)
	status, raw = requestJSON(t, http.MethodPost, testURL, surface.Token, nil)
	require.Equal(t, 200, status, string(raw))
	require.Contains(t, string(raw), `"ok":false`)
	require.NotContains(t, string(raw), "pending-secret")
	status, raw = requestJSON(t, http.MethodPut, rotateURL, surface.Token, map[string]any{"url": newerURL})
	require.Equal(t, 200, status, string(raw))
	status, raw = requestJSON(t, http.MethodPost, testURL, surface.Token, nil)
	require.Equal(t, 200, status, string(raw))
	require.Contains(t, string(raw), `"ok":true`)
	// Closed local server causes a real transport error whose URL must never be logged.
	sink.Close()
	var logs bytes.Buffer
	old := log.StandardLogger().Out
	log.SetOutput(&logs)
	status, raw = requestJSON(t, http.MethodPost, testURL, surface.Token, nil)
	log.SetOutput(old)
	require.Equal(t, 200, status, string(raw))
	require.NotContains(t, logs.String(), "pending-secret")
	require.NotContains(t, string(raw), "pending-secret")
	status, raw = requestJSON(t, http.MethodDelete, base+"/webhooks/"+hook.ID.String(), surface.Token, nil)
	require.Equal(t, 200, status, string(raw))
	_, err = rt.Merchants.Secrets().Get(ctx, dbtest.TestMerchantID, name)
	require.ErrorIs(t, err, merchants.ErrSecretNotFound)
	// Owned metadata does not contain any hidden URL column.
	var count int
	require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_schema='openrails' AND table_name='merchant_webhooks' AND column_name='url'`).Scan(&count))
	require.Zero(t, count)
}
