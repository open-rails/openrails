//go:build greenfield && integration

package greenfield_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/embed/controlplane"
	hostconfig "github.com/open-rails/openrails/hostauth/config"
	"github.com/open-rails/openrails/internal/standalonedb"
)

// SEC: a merchant owner cannot claim the deployment's own hosts as its
// api_host. The shared API host carries every merchant's traffic; pinning it
// to one merchant would refuse every other merchant's credentials there. A
// host another merchant holds stays with that merchant.
func TestSecurityMerchantCannotClaimTheSharedHost(t *testing.T) {
	const shared, console = "api.greenfield.test", "console.greenfield.test"
	f := newFixture(t)
	ctx := t.Context()
	require.NoError(t, standalonedb.ApplyAuthKit(ctx, f.pool, f.pool))
	rt, err := embed.New(ctx, embed.Options{
		Config: &config.Config{
			TestMode:             config.CredentialPostureSandbox,
			ProviderWriteMode:    config.ProviderWriteModeReadOnly,
			MerchantConfigHTTP:   true,
			PublicBillingBaseURL: "https://" + shared,
			DashboardBaseURL:     "https://" + console,
			DB:                   &config.DBConfig{URL: f.dsn(t), Schema: f.schema},
			ReturnOrigins:        []string{"https://greenfield.test"},
		},
		PGXPool: f.pool,
		River:   embed.RiverManagedByOpenRails(f.schema),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	cp, err := controlplane.Attach(ctx, rt, controlplane.Options{Auth: &hostconfig.AuthConfig{
		Issuer: "http://127.0.0.1/" + f.schema, AllowMemory: true, AllowMissingSenders: true,
		AllowEphemeralSigningKey: true, AllowLoopbackHTTP: true, DirectPeerIP: true, KeysPath: t.TempDir(),
	}})
	require.NoError(t, err)
	handler, err := cp.Handler()
	require.NoError(t, err)

	on := func(host, token, method, path, selector string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var payload bytes.Buffer
		if body != nil {
			require.NoError(t, json.NewEncoder(&payload).Encode(body))
		}
		r := httptest.NewRequest(method, "https://"+host+path, &payload)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		if selector != "" {
			r.Header.Set("X-OpenRails-Merchant", selector)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	type shop struct{ slug, session string }
	provision := func(prefix string) shop {
		owner := newAccount(t, cp)
		slug := uniqueName(prefix)
		_, err := cp.ProvisionMerchant(ctx, controlplane.ProvisionMerchantRequest{Slug: slug, OwnerUserID: owner.ID})
		require.NoError(t, err)
		return shop{slug, authtest.SignIn(t, cp.Core(), owner).AccessToken}
	}
	victim, attacker := provision("victim"), provision("attacker")
	w := on(shared, victim.session, http.MethodPost, "/v1/merchant/api-keys", victim.slug, map[string]string{"name": "backend", "role": "owner"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	key := map[string]any{}
	require.NoError(t, json.NewDecoder(w.Body).Decode(&key))
	victimKey := key["secret"].(string)
	victimWorks := func(host string) {
		t.Helper()
		w := on(host, victim.session, http.MethodGet, "/v1/merchant/team", victim.slug, nil)
		require.Equal(t, http.StatusOK, w.Code, "victim console on %s: %s", host, w.Body.String())
		w = on(host, victimKey, http.MethodGet, "/v1/merchant/findings", "", nil)
		require.Equal(t, http.StatusOK, w.Code, "victim API key on %s: %s", host, w.Body.String())
	}
	claim := func(s shop, host string) *httptest.ResponseRecorder {
		return on(shared, s.session, http.MethodPut, "/v1/merchant/api-host", s.slug, map[string]string{"api_host": host})
	}
	apply := func(s shop, host string) *httptest.ResponseRecorder {
		w := on(shared, s.session, http.MethodGet, "/v1/merchant/configuration", s.slug, nil)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		state := map[string]any{}
		require.NoError(t, json.NewDecoder(w.Body).Decode(&state))
		return on(shared, s.session, http.MethodPost, "/v1/merchant/configuration/applications", s.slug,
			map[string]any{"application_id": uuid.NewString(), "expected_revision": state["revision"], "api_host": host})
	}

	victimWorks(shared)
	for _, host := range []string{shared, "API.Greenfield.Test:443", console, "127.0.0.1"} {
		w := claim(attacker, host)
		require.Equal(t, http.StatusBadRequest, w.Code, "claim %s: %s", host, w.Body.String())
		require.Contains(t, w.Body.String(), "api_host_reserved")
		w = apply(attacker, host)
		require.Equal(t, http.StatusBadRequest, w.Code, "apply %s: %s", host, w.Body.String())
		require.Contains(t, w.Body.String(), "api_host_reserved")
	}
	victimWorks(shared)

	const victimHost = "shop.victim.greenfield.test"
	w = claim(victim, victimHost)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	w = claim(attacker, victimHost)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "api_host_taken")
	w = apply(attacker, victimHost)
	require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	require.Contains(t, w.Body.String(), "api_host_taken")
	victimWorks(victimHost)
	victimWorks(shared)

	w = claim(attacker, "shop.attacker.greenfield.test")
	require.Equal(t, http.StatusOK, w.Code, "control: a host of its own: %s", w.Body.String())
}
