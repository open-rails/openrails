//go:build integration

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/pkg/merchant"
)

// pspFixture describes one PSP to arm for a merchant in this test.
type pspFixture struct {
	Rail      string
	Key       string
	AccountID string
	Archived  bool
	Settings  map[string]any
}

// TestCheckoutConfigPublicDiscoveryHTTP drives GET /v1/checkout-config over
// REAL HTTP against the real standalone server (#829).
//
// It asserts four things at once:
//   - an armed merchant gets its PSPs, keyed by the value checkout's
//     payment.rail selector takes;
//   - a rail with no non-archived psps row is ABSENT (never advertised);
//   - a second merchant on a different Host gets its own config and never the
//     first merchant's;
//   - and the decisive one: NO SECRET VALUE can appear in the response. Every
//     credential slot rails.All() declares is seeded, per merchant, with a
//     unique sentinel — so adding a credential key to the registry extends this
//     assertion automatically rather than opening a leak.
func TestCheckoutConfigPublicDiscoveryHTTP(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	surface := h.StartStandalone("usd")
	rt := surface.App().Runtime
	require.NotNil(t, rt.Merchants, "merchants service must be armed")

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]

	// --- merchant A: NMI (tokenize) + Stripe (redirect) armed, CCBill ARCHIVED,
	// Solana never declared.
	acme := surface.ProvisionOwnedMerchant("ckcfg-acme-" + suffix)
	acmeHost := "api.ckcfg-acme-" + suffix + ".test"
	assignAPIHost(t, surface.BaseURL, acme.APIKey, acmeHost)
	armPSPs(ctx, t, rt, acme.MerchantID, "acme", []pspFixture{
		{Rail: string(models.RailNMI), Key: "mobius-sandbox", AccountID: "acme-nmi-" + suffix, Settings: map[string]any{
			"tokenization_key": "acme-public-collect-key",
			"tokenization_url": "https://collect.acme.test/Collect.js",
		}},
		{Rail: string(models.RailStripe), Key: "stripe", AccountID: "acct_acme" + suffix},
		// Archived == drain-only == NOT armed for new work: must not appear.
		{Rail: string(models.RailCCBill), Key: "ccbill", AccountID: "111111-0000", Archived: true},
	})

	// --- merchant B: CCBill only, on its own Host.
	other := surface.ProvisionOwnedMerchant("ckcfg-other-" + suffix)
	otherHost := "api.ckcfg-other-" + suffix + ".test"
	assignAPIHost(t, surface.BaseURL, other.APIKey, otherHost)
	armPSPs(ctx, t, rt, other.MerchantID, "other", []pspFixture{
		{Rail: string(models.RailCCBill), Key: "ccbill", AccountID: "222222-0000"},
	})

	// ---- merchant A's document, fetched UNAUTHENTICATED, by Host alone.
	status, hdr, raw := getPublic(t, surface.BaseURL+"/v1/checkout-config", acmeHost)
	require.Equal(t, http.StatusOK, status, string(raw))
	require.Contains(t, hdr.Get("Cache-Control"), "max-age=", "checkout config must be cacheable")
	require.NotEmpty(t, hdr.Get("ETag"))

	acmeDoc := decodeCheckoutConfig(t, raw)
	byKey := map[string]merchants.PublicPSPConfig{}
	for _, p := range acmeDoc.PSPs {
		byKey[p.Key] = p
	}

	nmi, ok := byKey["mobius-sandbox"]
	require.True(t, ok, "armed NMI PSP must be advertised under its PSP key: %s", raw)
	require.Equal(t, string(models.RailNMI), nmi.Rail)
	require.Equal(t, merchants.FlowTokenize, nmi.Flow)
	require.Equal(t, "acme-public-collect-key", nmi.Config["tokenization_key"])
	require.Equal(t, "https://collect.acme.test/Collect.js", nmi.Config["tokenization_url"])
	require.Len(t, nmi.Config, 2, "only the whitelisted NMI fields may be served")

	stripe, ok := byKey["stripe"]
	require.True(t, ok, "armed Stripe PSP must be advertised: %s", raw)
	require.Equal(t, merchants.FlowRedirect, stripe.Flow)
	require.Empty(t, stripe.Config, "a hosted-redirect rail needs no browser key")

	require.NotContains(t, byKey, "ccbill", "an ARCHIVED PSP is not armed and must be absent: %s", raw)
	for _, p := range acmeDoc.PSPs {
		require.NotEqual(t, string(models.RailSolana), p.Rail, "an undeclared rail must never be advertised")
	}

	// ---- merchant B's document, same server, different Host.
	status, _, otherRaw := getPublic(t, surface.BaseURL+"/v1/checkout-config", otherHost)
	require.Equal(t, http.StatusOK, status, string(otherRaw))
	otherDoc := decodeCheckoutConfig(t, otherRaw)
	require.Len(t, otherDoc.PSPs, 1, "merchant B has exactly one armed PSP: %s", otherRaw)
	require.Equal(t, "ccbill", otherDoc.PSPs[0].Key)
	require.Equal(t, merchants.FlowRedirect, otherDoc.PSPs[0].Flow)

	// Cross-merchant isolation, both directions.
	require.NotContains(t, string(otherRaw), "acme-public-collect-key")
	require.NotContains(t, string(otherRaw), "collect.acme.test")
	require.NotContains(t, string(otherRaw), "mobius-sandbox")
	require.NotContains(t, string(raw), "222222-0000")

	// ---- THE DECISIVE ASSERTION: not one seeded secret reaches either body.
	for _, tenant := range []string{"acme", "other"} {
		for _, sentinel := range secretSentinels(tenant) {
			require.NotContainsf(t, string(raw), sentinel,
				"merchant A's checkout config leaked the %s secret %q", tenant, sentinel)
			require.NotContainsf(t, string(otherRaw), sentinel,
				"merchant B's checkout config leaked the %s secret %q", tenant, sentinel)
		}
	}
	// The operator-declared account ids are not secret, but they are not the
	// browser's business either — nothing publishes them.
	for _, accountID := range []string{"acme-nmi-" + suffix, "acct_acme" + suffix, "111111-0000", "222222-0000"} {
		require.NotContains(t, string(raw), accountID)
		require.NotContains(t, string(otherRaw), accountID)
	}
}

// secretSentinels is the per-merchant sentinel value for every credential slot
// ANY rail declares. rails.All() is compile-time complete, so a new secret key
// is seeded — and therefore asserted absent — without touching this test.
func secretSentinels(tenant string) []string {
	var out []string
	for _, d := range rails.All() {
		for _, k := range d.CredentialKeys {
			out = append(out, secretSentinel(tenant, string(d.Rail), k.Name))
		}
	}
	return out
}

func secretSentinel(tenant, rail, key string) string {
	return fmt.Sprintf("SECRET-MUST-NEVER-LEAK-%s-%s-%s", tenant, rail, key)
}

// armPSPs writes psps rows + a FULL set of scoped secrets for each fixture,
// through the same store production writers use. Every credential slot the
// fixture's rail declares gets a per-merchant sentinel value, so the response
// assertions have something real to catch.
func armPSPs(ctx context.Context, t *testing.T, rt *app.Runtime, mid merchant.ID, tenant string, fixtures []pspFixture) {
	t.Helper()
	store := rt.Merchants.Secrets()
	require.NotNil(t, store, "merchant secret store must be armed")
	mctx := merchant.WithID(ctx, mid)

	for _, f := range fixtures {
		id, nRail, nEnv, nAccount := merchants.PSPNaturalKey(f.Rail, "test", f.AccountID)
		var evidence []byte
		if len(f.Settings) > 0 {
			raw, err := json.Marshal(map[string]any{"settings": f.Settings})
			require.NoError(t, err)
			evidence = raw
		}
		key, archived := f.Key, f.Archived
		require.NoError(t, rt.DB.RunInMerchantConn(mctx, func(cctx context.Context) error {
			_, err := rt.DB.Gen(cctx).UpsertPSP(cctx, gen.UpsertPSPParams{
				ID:          id,
				MerchantID:  mid.UUID(),
				Rail:        nRail,
				Environment: &nEnv,
				AccountID:   nAccount,
				Key:         &key,
				Archived:    &archived,
				Evidence:    evidence,
			})
			return err
		}), "arm psp %s/%s", f.Rail, f.AccountID)

		for _, k := range rails.CredentialKeys(models.Rail(nRail)) {
			name, err := merchants.PSPSecretName(nRail, nEnv, nAccount, k.Name)
			require.NoError(t, err)
			_, err = store.Put(ctx, mid, name, secretSentinel(tenant, nRail, k.Name))
			require.NoError(t, err, "seed secret %s", name)
		}
	}
}

func assignAPIHost(t *testing.T, baseURL, apiKey, host string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"api_host": host})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPut, baseURL+"/v1/merchant/api-host", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, "assign api_host: %s", raw)
}

// getPublic issues an UNAUTHENTICATED GET with an explicit Host header — the
// #734 mechanism every public route resolves its merchant through.
func getPublic(t *testing.T, url, host string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header, raw
}

func decodeCheckoutConfig(t *testing.T, raw []byte) merchants.PublicCheckoutConfig {
	t.Helper()
	var doc merchants.PublicCheckoutConfig
	require.NoError(t, json.Unmarshal(raw, &doc), "decode: %s", raw)
	require.Equal(t, "checkout_config", doc.Object)
	return doc
}
