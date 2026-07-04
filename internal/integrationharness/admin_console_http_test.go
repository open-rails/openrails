//go:build integration

package integrationharness

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/bootstrap/serverboot"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
)

// #740/#754 admin console serving. The #754 mount rule: /admin exists ONLY
// when assets are present AND admin_console.enabled; enabled without assets
// refuses boot. Assets are a tiny fstest fixture — never a real npm build.

func getRaw(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body), resp.Header
}

const fixtureIndexHTML = `<!doctype html><html><head><title>fixture-console</title></head><body>fixture</body></html>`

func fixtureConsoleAssets() fstest.MapFS {
	return fstest.MapFS{
		"index.html":            &fstest.MapFile{Data: []byte(fixtureIndexHTML)},
		"assets/app-fixture.js": &fstest.MapFile{Data: []byte(`console.log("fixture")`)},
	}
}

func TestAdminConsoleServing(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)

	t.Run("no assets, disabled: silently absent", func(t *testing.T) {
		surface := h.StartStandalone("usd")
		status, _, _ := getRaw(t, surface.BaseURL+"/admin/")
		require.Equal(t, http.StatusNotFound, status, "admin console must be absent when disabled")
		status, _, _ = getRaw(t, surface.BaseURL+"/admin/config.json")
		require.Equal(t, http.StatusNotFound, status)
	})

	t.Run("assets, disabled: not mounted", func(t *testing.T) {
		surface := h.StartStandalone("usd", WithConsoleAssets(fixtureConsoleAssets()))
		status, _, _ := getRaw(t, surface.BaseURL+"/admin/")
		require.Equal(t, http.StatusNotFound, status, "assets without the config gate must not mount /admin")
		status, _, _ = getRaw(t, surface.BaseURL+"/admin/config.json")
		require.Equal(t, http.StatusNotFound, status)
	})

	t.Run("no assets, enabled: loud boot error", func(t *testing.T) {
		// The boot error surfaces from serverboot.NewServer, so this drives it
		// directly with the same config shape the harness boots.
		_, appDSN := dbtest.SharedRLSPostgres(t)
		cfg := &config.Config{
			Env:               "dev",
			TestMode:          config.CredentialPostureSandbox,
			MerchantSource:    config.MerchantSourceAPI,
			ProviderWriteMode: config.ProviderWriteModeFull,
			Host:              "127.0.0.1",
			Port:              0,
			DB:                &config.DBConfig{URL: appDSN},
			Auth:              &config.AuthConfig{Issuer: "https://controlplane.openrails.test"},
			AdminConsole:      &config.AdminConsoleConfig{Enabled: true},
		}
		if h.Redis != nil {
			cfg.Redis = &config.RedisConfig{Addr: h.Redis.Options().Addr}
		}
		_, err := serverboot.NewServer(cfg, &serverboot.Options{})
		require.Error(t, err, "admin_console.enabled without assets must refuse boot")
		require.Contains(t, err.Error(), "no console assets")
		require.Contains(t, err.Error(), "build-admin-console.sh", "boot error must name the build step")
		require.Contains(t, err.Error(), "console_assets", "boot error must name the build tag")
	})

	t.Run("assets, enabled: serves", func(t *testing.T) {
		surface := h.StartStandalone("usd",
			WithConsoleAssets(fixtureConsoleAssets()),
			WithConfig(func(cfg *config.Config) {
				cfg.AdminConsole = &config.AdminConsoleConfig{Enabled: true}
			}))

		// config.json bootstrap carries the standalone defaults.
		status, body, hdr := getRaw(t, surface.BaseURL+"/admin/config.json")
		require.Equal(t, http.StatusOK, status)
		require.Contains(t, hdr.Get("Content-Type"), "application/json")
		var boot struct {
			AuthBaseURL string `json:"auth_base_url"`
			APIBaseURL  string `json:"api_base_url"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &boot))
		require.Equal(t, "/auth", boot.AuthBaseURL)
		require.Equal(t, "/v1", boot.APIBaseURL)

		// Index serves the supplied assets, SPA fallback covers client routes.
		status, index, hdr := getRaw(t, surface.BaseURL+"/admin/")
		require.Equal(t, http.StatusOK, status)
		require.Contains(t, hdr.Get("Content-Type"), "text/html")
		require.Contains(t, index, "fixture-console")
		status, fallback, _ := getRaw(t, surface.BaseURL+"/admin/customers/some-client-route")
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, index, fallback, "SPA fallback must serve index.html")

		// Static files under assets/ serve immutable-cacheable.
		status, js, hdr := getRaw(t, surface.BaseURL+"/admin/assets/app-fixture.js")
		require.Equal(t, http.StatusOK, status)
		require.Contains(t, js, "fixture")
		require.Contains(t, hdr.Get("Cache-Control"), "immutable")

		// Bare /admin redirects into the SPA root.
		req, err := http.NewRequest(http.MethodGet, surface.BaseURL+"/admin", nil)
		require.NoError(t, err)
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		require.Equal(t, http.StatusMovedPermanently, resp.StatusCode)
		require.Equal(t, "/admin/", resp.Header.Get("Location"))
	})

	t.Run("custom bases", func(t *testing.T) {
		surface := h.StartStandalone("usd",
			WithConsoleAssets(fixtureConsoleAssets()),
			WithConfig(func(cfg *config.Config) {
				cfg.AdminConsole = &config.AdminConsoleConfig{
					Enabled:     true,
					AuthBaseURL: "https://auth.example.test/auth",
					APIBaseURL:  "/billing/v1",
				}
			}))
		status, body, _ := getRaw(t, surface.BaseURL+"/admin/config.json")
		require.Equal(t, http.StatusOK, status)
		require.Contains(t, body, `"auth_base_url":"https://auth.example.test/auth"`)
		require.Contains(t, body, `"api_base_url":"/billing/v1"`)
	})
}

// TestAdminCustomersSearch: GET /v1/merchant/customers list/search — subject
// prefix, id prefix, subscription-email substring, paging clamp, and RLS
// isolation across two merchants.
func TestAdminCustomersSearch(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")

	aToken := surface.MintAPIKey(dbtest.TestMerchantSlug, "cust-a-"+uuid.NewString(),
		[]string{controlplane.PermMerchantCustomerSettingsRead})
	b := surface.ProvisionOwnedMerchant("custb" + strings.ReplaceAll(uuid.NewString(), "-", ""))
	bToken := surface.MintAPIKey(b.MerchantSlug, "cust-b-"+uuid.NewString(),
		[]string{controlplane.PermMerchantCustomerSettingsRead})

	pool := h.sharedPool() // super pool: cross-merchant fixture seeding bypasses RLS

	seedCustomer := func(merchantID uuid.UUID, subject string) uuid.UUID {
		id := uuid.New()
		_, err := pool.Exec(ctx,
			`INSERT INTO openrails.customers (id, merchant_id, subject) VALUES ($1, $2, $3)`,
			id, merchantID, subject)
		require.NoError(t, err)
		return id
	}
	// Merchant A: two customers, one with a subscription email.
	aliceSubject := "alice-" + uuid.NewString()
	alice := seedCustomer(uuid.UUID(dbtest.TestMerchantID), aliceSubject)
	bobSubject := "bob-" + uuid.NewString()
	bob := seedCustomer(uuid.UUID(dbtest.TestMerchantID), bobSubject)
	// Merchant B: a customer that must be invisible to A.
	eveSubject := "eve-" + uuid.NewString()
	seedCustomer(uuid.UUID(b.MerchantID), eveSubject)

	// A subscription carrying alice's email (email lives on subscriptions).
	productID := uuid.New()
	_, err := pool.Exec(ctx,
		`INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, 'Cust Search Product', $3)`,
		productID, "cust-search-"+uuid.NewString(), uuid.UUID(dbtest.TestMerchantID))
	require.NoError(t, err)
	aliceEmail := fmt.Sprintf("alice-%s@example.test", uuid.NewString()[:8])
	_, err = pool.Exec(ctx,
		`INSERT INTO openrails.subscriptions (product_id, status, rail, user_email, merchant_id, customer_id)
		 VALUES ($1, 'active', 'nmi', $2, $3, $4)`,
		productID, aliceEmail, uuid.UUID(dbtest.TestMerchantID), alice)
	require.NoError(t, err)

	type listResp struct {
		Object string `json:"object"`
		Data   []struct {
			ID      string  `json:"id"`
			Subject *string `json:"subject"`
			Email   *string `json:"email"`
		} `json:"data"`
		Total   int64 `json:"total"`
		Limit   int   `json:"limit"`
		Offset  int   `json:"offset"`
		HasMore bool  `json:"has_more"`
	}
	search := func(token, query string) listResp {
		t.Helper()
		status, body := requestJSON(t, http.MethodGet,
			surface.BaseURL+"/v1/merchant/customers?"+query, token, nil)
		require.Equalf(t, http.StatusOK, status, "customers search: %s", string(body))
		var out listResp
		require.NoError(t, json.Unmarshal(body, &out))
		require.Equal(t, "list", out.Object)
		return out
	}

	// Subject prefix.
	res := search(aToken, "q="+aliceSubject[:10])
	require.Len(t, res.Data, 1)
	require.Equal(t, alice.String(), res.Data[0].ID)
	require.NotNil(t, res.Data[0].Email)
	require.Equal(t, aliceEmail, *res.Data[0].Email)

	// ID prefix.
	res = search(aToken, "q="+alice.String()[:13])
	require.NotEmpty(t, res.Data)
	require.Equal(t, alice.String(), res.Data[0].ID)

	// Second customer resolves independently (no email on file).
	res = search(aToken, "q="+bobSubject[:10])
	require.Len(t, res.Data, 1)
	require.Equal(t, bob.String(), res.Data[0].ID)
	require.Nil(t, res.Data[0].Email)

	// Email substring.
	res = search(aToken, "q="+aliceEmail[:12])
	require.Len(t, res.Data, 1)
	require.Equal(t, alice.String(), res.Data[0].ID)

	// Paging: limit clamps to 200; bad limit is a 400.
	res = search(aToken, "q=&limit=100000")
	require.Equal(t, 200, res.Limit)
	status, _ := requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/customers?limit=nope", aToken, nil)
	require.Equal(t, http.StatusBadRequest, status)

	// RLS isolation: A never sees B's customer, and vice versa.
	res = search(aToken, "q="+eveSubject[:10])
	require.Empty(t, res.Data, "merchant A must not see merchant B's customers")
	res = search(bToken, "q="+eveSubject[:10])
	require.Len(t, res.Data, 1)
	res = search(bToken, "q="+aliceSubject[:10])
	require.Empty(t, res.Data, "merchant B must not see merchant A's customers")

	// Auth gate: no bearer is a 401. (Every merchant catalog role — owner/
	// support/viewer — carries customer-settings:read, so there is no valid
	// merchant credential without it; the gate itself is the assertion.)
	status, _ = requestJSON(t, http.MethodGet, surface.BaseURL+"/v1/merchant/customers?q=x", "", nil)
	require.Equal(t, http.StatusUnauthorized, status)
}
