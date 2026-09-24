package captcha

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
)

// siteverify fakes the provider endpoint and records the posted form.
func siteverify(t *testing.T, provider string, status int, body string) (Verifier, *url.Values) {
	t.Helper()
	var form url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/x-www-form-urlencoded", r.Header.Get("Content-Type"))
		assert.NoError(t, r.ParseForm())
		form = r.PostForm
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	v := NewVerifier(&config.CaptchaConfig{Provider: provider, SiteKey: "site-key", SecretKey: " secret-key "}, srv.Client())
	v.(*siteVerifyVerifier).verifyURLOverride = srv.URL
	return v, &form
}

// FC-13: anything short of a clean provider success is not a pass.
func TestVerifierFailsClosed(t *testing.T) {
	ctx := context.Background()
	v, form := siteverify(t, config.CaptchaProviderTurnstile, 200, `{"success":true}`)
	res, err := v.Verify(ctx, VerifyRequest{Token: " tok ", RemoteIP: "203.0.113.5", Bucket: "checkout"})
	require.NoError(t, err)
	require.True(t, res.Success)
	require.Equal(t, url.Values{"secret": {"secret-key"}, "response": {"tok"}, "remoteip": {"203.0.113.5"}}, *form)

	for _, tc := range []struct {
		name, provider, body string
		status               int
		wantErr              bool
		wantCode             string
	}{
		{"provider says no", config.CaptchaProviderTurnstile, `{"success":false,"error-codes":["invalid-input-response"]}`, 200, false, "invalid-input-response"},
		{"recaptcha low score", config.CaptchaProviderRecaptchaV3, `{"success":true,"score":0.4,"action":"billing_challenge"}`, 200, false, "low-score"},
		{"recaptcha wrong action", config.CaptchaProviderRecaptchaV3, `{"success":true,"score":0.9,"action":"login"}`, 200, false, "action-mismatch"},
		{"non-2xx", config.CaptchaProviderHCaptcha, `{"success":true}`, 502, true, ""},
		{"malformed body", config.CaptchaProviderHCaptcha, `{"success":tru`, 200, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v, _ := siteverify(t, tc.provider, tc.status, tc.body)
			res, err := v.Verify(ctx, VerifyRequest{Token: "tok"})
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.False(t, res.Success)
			require.Contains(t, res.ErrorCodes, tc.wantCode)
		})
	}

	v, _ = siteverify(t, config.CaptchaProviderRecaptchaV3, 200, `{"success":true,"score":0.5,"action":"billing_challenge"}`)
	res, err = v.Verify(ctx, VerifyRequest{Token: "tok"})
	require.NoError(t, err)
	require.True(t, res.Success, "score at the threshold with the expected action passes")

	v, form = siteverify(t, config.CaptchaProviderTurnstile, 200, `{"success":true}`)
	*form = nil
	res, err = v.Verify(ctx, VerifyRequest{Token: "  "})
	require.NoError(t, err)
	require.False(t, res.Success)
	require.Nil(t, *form, "a blank token never reaches the provider")
}

func TestPolicy(t *testing.T) {
	on := &config.CaptchaConfig{SiteKey: "s", SecretKey: "k"}
	for bucket, want := range map[string]bool{"checkout": true, " Payment-Methods ": true, "subscriptions": true, "webhook": false, "default": false, "": false} {
		require.Equal(t, want, ShouldApply(on, bucket), bucket)
	}
	require.False(t, ShouldApply(&config.CaptchaConfig{SiteKey: "s"}, "checkout"))
	require.Nil(t, NewVerifier(nil, nil))
	require.Equal(t, 30, ExtremeThreshold(&config.RateLimit{RequestsPerMinute: 10}, on))
	require.Equal(t, 180, ExtremeThreshold(nil, on))
}

func TestChallengeStore(t *testing.T) {
	ctx := context.Background()
	s := NewChallengeStore(nil)
	require.NoError(t, s.MarkChallenged(ctx, "ip:203.0.113.1", time.Minute))
	on, err := s.IsChallenged(ctx, "ip:203.0.113.1")
	require.NoError(t, err)
	require.True(t, on)
	require.NoError(t, s.ClearChallenged(ctx, "ip:203.0.113.1"))
	on, _ = s.IsChallenged(ctx, "ip:203.0.113.1")
	require.False(t, on)

	s.challenged["expired"] = time.Now().Add(-time.Second)
	on, _ = s.IsChallenged(ctx, "expired")
	require.False(t, on)
	require.NotContains(t, s.challenged, "expired")

	// Memory is bounded: expired entries are pruned, then the oldest evicted.
	s.challenged["stale"] = time.Now().Add(-time.Minute)
	for i := 0; len(s.challenged) < maxMemoryEntries; i++ {
		s.challenged[strconv.Itoa(i)] = time.Now().Add(time.Hour)
	}
	require.NoError(t, s.MarkChallenged(ctx, "user:new", 0))
	require.NotContains(t, s.challenged, "stale")
	require.LessOrEqual(t, len(s.challenged), maxMemoryEntries)
	require.WithinDuration(t, time.Now().Add(15*time.Minute), s.challenged["user:new"], time.Minute, "ttl <= 0 defaults to 15m")
}
