package captcha

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
)

func TestVerifierPostsSiteVerifyForm(t *testing.T) {
	t.Parallel()

	type observedRequest struct {
		method      string
		contentType string
		form        url.Values
		err         error
	}
	observed := make(chan observedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		obs := observedRequest{method: r.Method, contentType: r.Header.Get("Content-Type")}
		if err := r.ParseForm(); err != nil {
			obs.err = err
		} else {
			obs.form = cloneValues(r.Form)
		}
		observed <- obs

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"success": true}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
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

	obs := <-observed
	require.NoError(t, obs.err)
	require.Equal(t, http.MethodPost, obs.method)
	require.Equal(t, "application/x-www-form-urlencoded", obs.contentType)
	require.Equal(t, "secret-key", obs.form.Get("secret"))
	require.Equal(t, "response-token", obs.form.Get("response"))
	require.Equal(t, "203.0.113.5", obs.form.Get("remoteip"))
}

func cloneValues(values url.Values) url.Values {
	cloned := make(url.Values, len(values))
	for key, vals := range values {
		cloned[key] = append([]string(nil), vals...)
	}
	return cloned
}

func TestVerifierRejectsLowScore(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"success": true, "score": 0.4}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	verifier := NewVerifier(&config.CaptchaConfig{
		Enabled:   true,
		Provider:  config.CaptchaProviderRecaptchaV3,
		SecretKey: "secret-key",
		VerifyURL: server.URL,
		MinScore:  0.5,
	}, server.Client())

	result, err := verifier.Verify(context.Background(), VerifyRequest{Token: "response-token"})
	require.NoError(t, err)
	require.False(t, result.Success)
	require.Contains(t, result.ErrorCodes, "low-score")
}

func TestVerifierValidatesRecaptchaV3Action(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		action     string
		wantResult bool
		wantCode   string
	}{
		{
			name:       "matching action accepted",
			action:     "billing_challenge",
			wantResult: true,
		},
		{
			name:       "mismatched action rejected",
			action:     "login",
			wantResult: false,
			wantCode:   "action-mismatch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]any{"success": true, "score": 0.9, "action": tt.action}); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
				}
			}))
			defer server.Close()

			verifier := NewVerifier(&config.CaptchaConfig{
				Enabled:   true,
				Provider:  config.CaptchaProviderRecaptchaV3,
				SecretKey: "secret-key",
				VerifyURL: server.URL,
				Action:    "billing_challenge",
				MinScore:  0.5,
			}, server.Client())

			result, err := verifier.Verify(context.Background(), VerifyRequest{Token: "response-token"})
			require.NoError(t, err)
			require.Equal(t, tt.wantResult, result.Success)
			if tt.wantCode != "" {
				require.Contains(t, result.ErrorCodes, tt.wantCode)
			}
		})
	}
}

func TestChallengeStorePrunesExpiredMemoryEntries(t *testing.T) {
	t.Parallel()

	store := NewChallengeStore(nil)
	store.challenged["expired"] = time.Now().Add(-time.Minute)

	require.NoError(t, store.MarkChallenged(context.Background(), "203.0.113.10", time.Minute))
	require.NotContains(t, store.challenged, "expired")
	require.Len(t, store.challenged, 1)
}

func TestChallengeStoreBoundsMemoryEntries(t *testing.T) {
	t.Parallel()

	store := NewChallengeStore(nil)
	for i := 0; i < maxMemoryEntries; i++ {
		store.challenged[strconv.Itoa(i)] = time.Now().Add(time.Hour)
	}

	require.NoError(t, store.MarkChallenged(context.Background(), "203.0.113.10", time.Minute))
	require.LessOrEqual(t, len(store.challenged), maxMemoryEntries)
}
