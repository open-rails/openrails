//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/bootstrap/serverboot"
	embcp "github.com/open-rails/openrails/internal/operator"
	"github.com/stretchr/testify/require"
)

func TestStandaloneAccountDeletionAndExplicitRecovery(t *testing.T) {
	t.Run("api_only", func(t *testing.T) { testStandaloneAccountRecovery(t, false) })
	t.Run("with_workers", func(t *testing.T) { testStandaloneAccountRecovery(t, true) })
}

func testStandaloneAccountRecovery(t *testing.T, workers bool) {
	h := New(t, t.Context())
	var opts []StandaloneOption
	if workers {
		opts = append(opts, WithWorkers())
	}
	surface := h.StartStandalone("USD", opts...)
	cp := embcp.Get(surface.App())
	name := "recovery-" + uuid.NewString()[:8]
	password := "Correct-standalone-recovery-password-1"
	user, err := cp.Core().CreateUser(t.Context(), name+"@example.test", name)
	require.NoError(t, err)
	require.NoError(t, cp.Core().AdminSetPassword(t.Context(), user.ID, password))
	call := func(method, path, token string, body any, status int) map[string]any {
		t.Helper()
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(t.Context(), method, surface.BaseURL+"/auth"+path, bytes.NewReader(raw))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.Equal(t, status, response.StatusCode, "%s %s: %s", method, path, data)
		if status == http.StatusNoContent {
			require.Empty(t, data)
			return nil
		}
		var value map[string]any
		require.NoError(t, json.Unmarshal(data, &value))
		return value
	}
	credentials := map[string]string{"identifier": name + "@example.test", "password": password}
	login := call(http.MethodPost, "/password/login", "", credentials, http.StatusOK)
	call(http.MethodDelete, "/user", login["access_token"].(string), map[string]string{"password": password}, http.StatusNoContent)
	var generation string
	var deleted, purge time.Time
	require.NoError(t, h.Pool().QueryRow(t.Context(), "SELECT id::text,deleted_at,purge_at FROM profiles.account_deletions WHERE user_id=$1::uuid AND state='deleted'", user.ID).Scan(&generation, &deleted, &purge))
	require.Equal(t, 30*24*time.Hour, purge.Sub(deleted))
	var jobs int
	require.NoError(t, h.Pool().QueryRow(t.Context(), "SELECT count(*) FROM public.river_job WHERE kind='authkit_account_finalize' AND args->>'deletion_id'=$1", generation).Scan(&jobs))
	require.Equal(t, 1, jobs, "the standalone control-plane producer must be bound to its running fleet")
	required := call(http.MethodPost, "/password/login", "", credentials, http.StatusConflict)
	require.NotContains(t, required, "access_token")
	envelope := required["error"].(map[string]any)
	require.Equal(t, "account_recovery_required", envelope["code"])
	recovery := envelope["metadata"].(map[string]any)["recovery"].(map[string]any)
	confirmation := map[string]any{"token": recovery["token"]}
	call(http.MethodPost, "/account/recovery/confirm", "", confirmation, http.StatusNoContent)
	call(http.MethodPost, "/account/recovery/confirm", "", confirmation, http.StatusUnauthorized)
	call(http.MethodPost, "/token", "", map[string]any{"grant_type": "refresh_token", "refresh_token": login["refresh_token"]}, http.StatusUnauthorized)
	fresh := call(http.MethodPost, "/password/login", "", credentials, http.StatusOK)
	require.NotEmpty(t, fresh["access_token"])
	require.NotEqual(t, login["refresh_token"], fresh["refresh_token"])
	if !workers {
		var pending int
		require.NoError(t, h.Pool().QueryRow(t.Context(), "SELECT count(*) FROM profiles.account_deletion_deliveries WHERE deletion_id=$1::uuid AND completed_at IS NULL", generation).Scan(&pending))
		require.Equal(t, 2, pending, "API-only startup binds producers without executing callbacks")
		// A distinct worker application uses the same complete contribution set,
		// identity issuer and database as the API's unstarted producer client.
		worker, err := serverboot.NewWorker(t.Context(), &config.Config{
			Env: "dev", APIURL: surface.BaseURL, TestMode: config.CredentialPostureSandbox,
			DB: &config.DBConfig{URL: h.DSN}, Redis: &config.RedisConfig{Addr: h.Redis.Options().Addr},
			Auth:                 &config.AuthConfig{Issuer: surface.App().Config.Auth.Issuer, KeysPath: surface.App().Config.Auth.KeysPath, DirectPeerIP: true},
			MerchantConfigSource: config.MerchantConfigSourceAPI, SecretBackend: config.SecretBackendDB,
		}, nil)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, worker.Close(context.Background())) })
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- worker.Runtime.RunWorkers(ctx) }()
		t.Cleanup(func() {
			cancel()
			err := <-done
			require.True(t, err == nil || errors.Is(err, context.Canceled), "%v", err)
		})
	}
	require.Eventually(t, func() bool {
		var pending int
		err := h.Pool().QueryRow(t.Context(), "SELECT count(*) FROM profiles.account_deletion_deliveries WHERE deletion_id=$1::uuid AND completed_at IS NULL", generation).Scan(&pending)
		return err == nil && pending == 0
	}, 10*time.Second, 25*time.Millisecond, "the shared standalone fleet must execute AuthKit's ordered lifecycle deliveries")
}
