//go:build greenfield && integration

package subscriptions_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
)

// cardSave submits one NMI card save for c from client address ip and returns
// the status and body.
func (c *customer) cardSave(ip string, cd card) (int, string) {
	c.w.t.Helper()
	body, err := json.Marshal(map[string]any{"provider": "nmi", "psp_id": c.w.psp["nmi"], "payment_token": c.w.nmi.Tokenize(cd), "name_on_card": "Card Holder"})
	require.NoError(c.w.t, err)
	req, err := http.NewRequestWithContext(c.w.t.Context(), http.MethodPost, c.w.server.URL+mountPrefix+"/v1/me/payment-methods", bytes.NewReader(body))
	require.NoError(c.w.t, err)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", ip)
	res, err := http.DefaultClient.Do(req)
	require.NoError(c.w.t, err)
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	require.NoError(c.w.t, err)
	return res.StatusCode, string(raw)
}

// SEC: card-testing attack mode belongs to the merchant under attack. A wave
// of declines at one merchant puts a captcha in front of that merchant's card
// routes only. Another merchant on the same Redis keeps its card routes and
// its merchant API, and no merchant's server-to-server API ever meets a
// captcha, the attacked merchant's included.
func TestSecurityCardAttackModeIsPerMerchant(t *testing.T) {
	t.Parallel()
	addr := strings.TrimSpace(os.Getenv("OPENRAILS_GREENFIELD_REDIS_ADDR"))
	if addr == "" {
		t.Fatal("OPENRAILS_GREENFIELD_REDIS_ADDR must point at a disposable Redis")
	}
	guarded := func(cfg *config.Config) {
		cfg.Redis = &config.RedisConfig{Addr: addr}
		cfg.Captcha = &config.CaptchaConfig{Provider: config.CaptchaProviderTurnstile, SiteKey: "greenfield-site", SecretKey: "greenfield-secret"}
	}
	attacked, bystander := newWorld(t, guarded), newWorld(t, guarded)
	// Fresh client addresses per run: Redis outlives the test.
	prefix := fmt.Sprintf("2001:db8:%x:%x::", rand.Uint32N(1<<16), rand.Uint32N(1<<16))
	ip := func(i int) string { return fmt.Sprintf("%s%x", prefix, i+1) }

	// Card testers spread 100 declines over fresh accounts and addresses, so no
	// one subject is challenged and every attempt reaches the gateway.
	for i := range 100 {
		status, body := attacked.newCustomer().cardSave(ip(i), refusedCard)
		require.Equal(t, http.StatusBadRequest, status, "decline %d: %s", i, body)
	}

	status, body := attacked.newCustomer().cardSave(ip(100), visa)
	require.Equal(t, http.StatusForbidden, status, "the attacked merchant's card routes ask everyone for a captcha: %s", body)
	require.Contains(t, body, "captcha_required")
	status, body = attacked.staff(http.MethodGet, "/v1/merchant/findings")
	require.Equal(t, http.StatusOK, status, "its server-to-server API never meets a captcha: %s", body)

	status, body = bystander.staff(http.MethodGet, "/v1/merchant/findings")
	require.Equal(t, http.StatusOK, status, "another merchant's API is untouched: %s", body)
	status, body = bystander.newCustomer().cardSave(ip(101), visa)
	require.Equal(t, http.StatusOK, status, "another merchant's card routes are untouched: %s", body)
}
