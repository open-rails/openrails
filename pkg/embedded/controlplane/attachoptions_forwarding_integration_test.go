//go:build integration

package controlplane_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	authcore "github.com/open-rails/authkit/embedded"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

// postJSONWithHeaders is postJSON (provision_integration_test.go) plus
// caller-supplied headers (needed here to set X-Forwarded-For).
func postJSONWithHeaders(t *testing.T, url, body string, headers map[string]string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestFrontendOverride_PasswordResetLinkUsesProductFrontend proves #743's
// headline finding is fixed: with no override, AuthKit pins Frontend.BaseURL
// at the control plane's own token issuer — an API host that serves no pages
// — so an emailed password-reset link 404s. AttachOptions.Frontend lets a
// hosted product point the link at its OWN frontend instead.
func TestFrontendOverride_PasswordResetLinkUsesProductFrontend(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	issuer := "https://controlplane.openrails.test"
	cfg := hostedTestConfig(dsn, issuer)
	e := newHostApp(t, cfg)

	sender := &captureEmailSender{}
	const frontendBase = "https://app.example.test"
	require.NoError(t, embcp.AttachWithOptions(ctx, e.App(), cfg, nil, embcp.AttachOptions{
		EmailSender: sender,
		Frontend: authcore.FrontendConfig{
			BaseURL:           frontendBase,
			PasswordResetPath: "/account/reset",
		},
	}))
	srv := mountAuthRoutes(t, e)

	cp := embcp.Get(e.App())
	require.NotNil(t, cp)
	email := "reset-target@example.test"
	_, err := cp.Core().CreateUser(ctx, email, "resettarget")
	require.NoError(t, err)

	status, body := postJSON(t, srv.URL+"/email/password/reset/request", `{"email":"`+email+`"}`)
	require.Equal(t, http.StatusAccepted, status, "%v", body)

	link := sender.resetLink(email)
	require.NotEmpty(t, link, "reset link delivered to the host sender")
	require.True(t, strings.HasPrefix(link, frontendBase+"/account/reset"),
		"reset link must use the overridden product frontend + path, not the issuer: %s", link)
	require.NotContains(t, link, issuer, "reset link must not point at the control-plane issuer")
}

// TestFrontendAbsent_KeepsPreviousIssuerBasedDefault pins the "absent -> current
// behavior" contract: with no Frontend override, BaseURL still pins at the
// issuer exactly as before #743 (so existing self-hosted deployments, which
// never set this, see no behavior change).
func TestFrontendAbsent_KeepsPreviousIssuerBasedDefault(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	issuer := "https://controlplane2.openrails.test"
	cfg := hostedTestConfig(dsn, issuer)
	e := newHostApp(t, cfg)

	sender := &captureEmailSender{}
	require.NoError(t, embcp.AttachWithOptions(ctx, e.App(), cfg, nil, embcp.AttachOptions{
		EmailSender: sender,
	}))
	srv := mountAuthRoutes(t, e)

	cp := embcp.Get(e.App())
	email := "reset-default@example.test"
	_, err := cp.Core().CreateUser(ctx, email, "resetdefault")
	require.NoError(t, err)

	status, body := postJSON(t, srv.URL+"/email/password/reset/request", `{"email":"`+email+`"}`)
	require.Equal(t, http.StatusAccepted, status, "%v", body)

	link := sender.resetLink(email)
	require.NotEmpty(t, link)
	require.True(t, strings.HasPrefix(link, issuer+"/reset"),
		"with no override, the link must still use the issuer + authcore's default reset path: %s", link)
}

// TestTrustedProxies_ClientIPBucketsByForwardedHeader proves #743's second
// finding is fixed: AttachOptions.TrustedProxies reaches authhttp's per-client
// rate limiter. Without it, every request collapses onto the loopback peer
// address (as seen by any test/prod reverse proxy or LB) and shares ONE
// bucket; with the peer declared trusted, distinct X-Forwarded-For clients
// get independent buckets.
func TestTrustedProxies_ClientIPBucketsByForwardedHeader(t *testing.T) {
	ctx := context.Background()
	// RLPasswordResetRequest defaults to Limit:6/hour (authhttp.DefaultRateLimits) —
	// one more request than that must 429 when every request shares one bucket.
	const requests = 7

	run := func(t *testing.T, trustedProxies []string) []int {
		dsn := dbtest.SharedPostgresDSN(t)
		cfg := hostedTestConfig(dsn, "https://controlplane.openrails.test")
		e := newHostApp(t, cfg)
		require.NoError(t, embcp.AttachWithOptions(ctx, e.App(), cfg, nil, embcp.AttachOptions{
			EmailSender:    &captureEmailSender{},
			TrustedProxies: trustedProxies,
		}))
		srv := mountAuthRoutes(t, e)

		codes := make([]int, 0, requests)
		for i := 0; i < requests; i++ {
			email := fmt.Sprintf("reset-%d-%s@example.test", i, strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")))
			status := postJSONWithHeaders(t, srv.URL+"/email/password/reset/request",
				`{"email":"`+email+`"}`,
				map[string]string{"X-Forwarded-For": fmt.Sprintf("203.0.113.%d", i+1)})
			codes = append(codes, status)
		}
		return codes
	}

	t.Run("untrusted_proxy_shares_one_bucket", func(t *testing.T) {
		codes := run(t, nil)
		sawRateLimited := false
		for _, c := range codes {
			if c == http.StatusTooManyRequests {
				sawRateLimited = true
			}
		}
		require.True(t, sawRateLimited,
			"with no TrustedProxies, every request shares the loopback peer's bucket and must eventually 429: %v", codes)
	})

	t.Run("trusted_proxy_keys_on_forwarded_ip", func(t *testing.T) {
		codes := run(t, []string{"127.0.0.1/32", "::1/128"})
		for i, c := range codes {
			require.Equalf(t, http.StatusAccepted, c,
				"request %d must not be rate limited once TrustedProxies keys the limiter on its own forwarded IP: %v", i, codes)
		}
	})
}

// TestTrustedProxies_InvalidCIDRFailsAttach proves the option actually reaches
// authhttp.WithTrustedProxies (an invalid CIDR is authhttp's own construction
// error, not something OpenRails could produce by accident) rather than being
// silently dropped.
func TestTrustedProxies_InvalidCIDRFailsAttach(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	cfg := hostedTestConfig(dsn, "https://controlplane.openrails.test")
	e := newHostApp(t, cfg)

	err := embcp.AttachWithOptions(ctx, e.App(), cfg, nil, embcp.AttachOptions{
		EmailSender:    &captureEmailSender{},
		TrustedProxies: []string{"not-a-cidr"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid trusted proxy CIDR")
}
