//go:build e2e && integration

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
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/config"
)

// cardSave submits one NMI card save for c from client address ip and returns
// the status and body.
func (c *customer) cardSave(ip string, cd card) (int, string) {
	c.w.t.Helper()
	return c.cardSaveJSON(ip, map[string]any{"psp_id": c.w.psp["nmi"], "payment_token": c.w.nmi.Tokenize(cd), "billing_details": map[string]any{"name": "Card Holder"}})
}

// cardSaveJSON submits a card save request body for c from client address ip.
func (c *customer) cardSaveJSON(ip string, payload map[string]any) (int, string) {
	c.w.t.Helper()
	body, err := json.Marshal(payload)
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

// freshAddresses names client addresses no earlier run used (Redis outlives
// the test), each its own /64: the ledger counts an IPv6 client by its /64.
func freshAddresses() func(int) string {
	prefix := fmt.Sprintf("2001:db8:%x:", rand.Uint32N(1<<16))
	return func(i int) string { return fmt.Sprintf("%s%x::1", prefix, i+1) }
}

// SEC: card-testing attack mode belongs to the merchant under attack. A wave
// of declines at one merchant puts a captcha in front of that merchant's card
// routes only. Another merchant on the same Redis keeps its card routes and
// its merchant API, and no merchant's server-to-server API ever meets a
// captcha, the attacked merchant's included.
func TestSecurityCardAttackModeIsPerMerchant(t *testing.T) {
	t.Parallel()
	addr := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_REDIS_ADDR"))
	if addr == "" {
		t.Fatal("OPENRAILS_E2E_REDIS_ADDR must point at a disposable Redis")
	}
	guarded := func(cfg *config.Config) {
		cfg.Redis = &config.RedisConfig{Addr: addr}
		cfg.Captcha = &config.CaptchaConfig{Provider: config.CaptchaProviderTurnstile, SiteKey: "e2e-site", SecretKey: "e2e-secret"}
	}
	attacked, bystander := newWorld(t, guarded), newWorld(t, guarded)
	ip := freshAddresses()

	// Card testers spread 100 declines over fresh accounts and addresses, so no
	// one subject is challenged and every attempt reaches the gateway.
	for i := range 100 {
		status, body := attacked.newCustomer().cardSave(ip(i), refusedCard)
		require.Equal(t, http.StatusBadGateway, status, "decline %d: %s", i, body)
	}

	status, body := attacked.newCustomer().cardSave(ip(100), visa)
	require.Equal(t, http.StatusForbidden, status, "the attacked merchant's card routes ask everyone for a captcha: %s", body)
	require.Contains(t, body, "captcha_required")
	status, body = attacked.staff(http.MethodGet, "/v1/merchant/findings")
	require.Equal(t, http.StatusOK, status, "its server-to-server API never meets a captcha: %s", body)

	status, body = bystander.staff(http.MethodGet, "/v1/merchant/findings")
	require.Equal(t, http.StatusOK, status, "another merchant's API is untouched: %s", body)
	status, body = bystander.newCustomer().cardSave(ip(101), visa)
	require.Equal(t, http.StatusCreated, status, "another merchant's card routes are untouched: %s", body)
}

// SEC: without a captcha (the embedded default: Redis on, no captcha keys),
// attack mode never refuses a buyer with no recent decline. A wave of declines
// from fresh accounts and addresses blocks only the subjects that just
// declined, before the gateway sees them; a fresh buyer still saves a card and
// an earlier buyer still confirms a checkout with a saved one. Requests the
// provider never saw, and declines from a few accounts and addresses, never
// make an attack.
func TestSecurityCardAttackModeWithoutCaptcha(t *testing.T) {
	t.Parallel()
	addr := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_REDIS_ADDR"))
	if addr == "" {
		t.Fatal("OPENRAILS_E2E_REDIS_ADDR must point at a disposable Redis")
	}
	redisOnly := func(cfg *config.Config) { cfg.Redis = &config.RedisConfig{Addr: addr} }
	t.Run("wave", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t, redisOnly)
		ip := freshAddresses()
		price := w.membership("vip", 9_990_000)
		buyer := w.newCustomer()
		method := buyer.saveCard("nmi", visa)

		var tester *customer
		for i := range 100 {
			tester = w.newCustomer()
			status, body := tester.cardSave(ip(i), refusedCard)
			require.Equal(t, http.StatusBadGateway, status, "decline %d: %s", i, body)
		}
		status, body := tester.cardSave(ip(99), refusedCard)
		require.Equal(t, http.StatusTooManyRequests, status, "in attack mode one recent decline blocks: %s", body)
		require.Contains(t, body, "card_attempts_blocked")
		require.Equal(t, 100, w.nmi.RefusedSaves(), "a blocked attempt never reaches the gateway")

		status, body = w.newCustomer().cardSave(ip(100), visa)
		require.Equal(t, http.StatusCreated, status, "a fresh buyer saves a card: %s", body)
		buyer.subscribe(embedded, "nmi", price.ID, "vip", method) // confirms a checkout with the saved card
		require.True(t, buyer.entitled("vip"))
	})
	t.Run("not_an_attack", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t, redisOnly)
		ip := freshAddresses()
		for i := range 100 {
			status, body := w.newCustomer().cardSaveJSON(ip(i), map[string]any{"payment_token": "x"})
			require.Equal(t, http.StatusBadRequest, status, "junk %d: %s", i, body)
		}
		few := make([]*customer, 10)
		for i := range few {
			few[i] = w.newCustomer()
		}
		for round := range 2 {
			for i, c := range few {
				for range 5 {
					status, body := c.cardSave(ip(200+i), refusedCard)
					require.Equal(t, http.StatusBadGateway, status, "round %d, customer %d: %s", round, i, body)
				}
			}
			w.advance(16 * time.Minute)
		}
		require.Equal(t, 100, w.nmi.RefusedSaves())

		c := w.newCustomer()
		status, body := c.cardSave(ip(300), refusedCard)
		require.Equal(t, http.StatusBadGateway, status, body)
		status, body = c.cardSave(ip(300), visa)
		require.Equal(t, http.StatusCreated, status, "a customer who just declined once is not blocked: %s", body)
	})
}
