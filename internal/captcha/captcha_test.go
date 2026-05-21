package captcha

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
)

func TestVerifierPostsSiteVerifyForm(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "application/x-www-form-urlencoded", r.Header.Get("Content-Type"))
		require.NoError(t, r.ParseForm())
		require.Equal(t, "secret-key", r.Form.Get("secret"))
		require.Equal(t, "response-token", r.Form.Get("response"))
		require.Equal(t, "203.0.113.5", r.Form.Get("remoteip"))

		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"success": true}))
	}))
	defer server.Close()

	verifier := NewVerifier(&config.CaptchaConfig{
		Enabled:   true,
		Provider:  config.CaptchaProviderTurnstile,
		SecretKey: "secret-key",
		VerifyURL: server.URL,
	}, server.Client())

	result, err := verifier.Verify(context.Background(), VerifyRequest{Token: "response-token", RemoteIP: "203.0.113.5", Bucket: "checkout"})
	require.NoError(t, err)
	require.True(t, result.Success)
}

func TestVerifierRejectsLowScore(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"success": true, "score": 0.4}))
	}))
	defer server.Close()

	verifier := NewVerifier(&config.CaptchaConfig{
		Enabled:   true,
		Provider:  config.CaptchaProviderRecaptcha,
		SecretKey: "secret-key",
		VerifyURL: server.URL,
		MinScore:  0.5,
	}, server.Client())

	result, err := verifier.Verify(context.Background(), VerifyRequest{Token: "response-token"})
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Contains(t, result.ErrorCodes, "low-score")
}

func TestChallengeStorePrunesExpiredMemoryEntries(t *testing.T) {
	t.Parallel()

	store := NewChallengeStore(nil)
	store.challenged["expired"] = time.Now().Add(-time.Minute)

	require.NoError(t, store.MarkChallenged(context.Background(), "checkout", "203.0.113.10", time.Minute))
	require.NotContains(t, store.challenged, "expired")
	require.Len(t, store.challenged, 1)
}

func TestChallengeStoreBoundsMemoryEntries(t *testing.T) {
	t.Parallel()

	store := NewChallengeStore(nil)
	for i := 0; i < maxMemoryEntries; i++ {
		store.challenged[strconv.Itoa(i)] = time.Now().Add(time.Hour)
	}

	require.NoError(t, store.MarkChallenged(context.Background(), "checkout", "203.0.113.10", time.Minute))
	require.LessOrEqual(t, len(store.challenged), maxMemoryEntries)
}
