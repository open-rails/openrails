//go:build e2e && integration

package subscriptions_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
)

// One PSP whose credentials cannot be checked never takes purchase down: the
// customer's configuration still lists every other PSP with its public
// values, and that one as temporarily unavailable, without them.
func TestPublicConfigDegradesOnePSP(t *testing.T) {
	t.Parallel()
	w := newVaultWorld(t)
	// An edit outside OpenRails leaves the CCBill DataLink pair half set.
	w.editDoc("psps/ccbill", func(doc map[string]any) {
		secrets, _ := doc["secrets"].(map[string]any)
		secrets["datalink_username"] = "half-a-pair"
	})

	for _, tp := range []topology{embedded} {
		cfg := customerConfig(t, w.rt)
		byRail := map[string]billing.PSPPaymentConfig{}
		for _, psp := range cfg.Payment.PSPs {
			byRail[psp.Rail] = psp
		}
		require.Empty(t, byRail["stripe"].Status, "%s: the other PSPs stay available", tp)
		ccbill, ok := byRail["ccbill"]
		require.True(t, ok, "%s: the failing PSP is still listed: %+v", tp, cfg.Payment.PSPs)
		require.Equal(t, billing.PSPTemporarilyUnavailable, ccbill.Status, tp)
		require.Positive(t, ccbill.RetryAfter, tp)
		require.Empty(t, ccbill.Config, "%s: no values to drive an unavailable PSP", tp)
	}
}

// A host renders billing-ui from its own page response: Engine.PublicConfig
// is the document the mount it names serves at GET /v1/config.
func TestEnginePublicConfigIsTheMountsDocument(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	served := func(routes openrails.Routes) string {
		mux := http.NewServeMux()
		require.NoError(t, openrailshttp.Mount(mux, w.rt, routes))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, routes.Prefix+"/v1/config", nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		return rec.Body.String()
	}
	staff := openrails.Routes{Auth: w.auth, Prefix: mountPrefix, RouteGroups: routeGroups}
	public := openrails.Routes{Auth: authtest.Deny{}}
	docs := map[string]*billing.PublicConfig{}
	for name, routes := range map[string]openrails.Routes{"staff": staff, "public": public} {
		doc, err := w.rt.PublicConfig(routes)
		require.NoError(t, err)
		encoded, err := json.Marshal(doc)
		require.NoError(t, err)
		require.JSONEq(t, served(routes), string(encoded), name)
		docs[name] = doc
	}
	require.True(t, docs["staff"].Capabilities.RouteGroups["admin"])
	require.False(t, docs["public"].Capabilities.RouteGroups["admin"], "the mount it names, not the engine's")
}

// Engine.CustomerConfig is the document the mount serves at GET /v1/me/config
// to the customer the host's own request is signed in as, refusals included.
func TestEngineCustomerConfigIsTheMountsDocument(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	routes := openrails.Routes{Auth: w.auth, Prefix: mountPrefix, RouteGroups: routeGroups}
	buyer := w.newCustomer()
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, w.rt, routes))
	served := httptest.NewRequest(http.MethodGet, mountPrefix+"/v1/me/config", nil)
	served.Header.Set("Authorization", "Bearer "+buyer.token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, served)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// The host's own page request, carrying the same session.
	page := httptest.NewRequest(http.MethodPost, "/api/subscribe", strings.NewReader(`{"plan":"monthly"}`))
	page.Header.Set("Authorization", "Bearer "+buyer.token)
	page.Header.Set("Content-Type", "application/json")
	doc, err := w.rt.CustomerConfig(routes, page)
	require.NoError(t, err)
	encoded, err := json.Marshal(doc)
	require.NoError(t, err)
	require.JSONEq(t, rec.Body.String(), string(encoded))
	require.NotEmpty(t, doc.Payment.PSPs)

	_, err = w.rt.CustomerConfig(routes, httptest.NewRequest(http.MethodGet, "/api/subscribe", nil))
	var refused *billing.StatusError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, http.StatusUnauthorized, refused.Status, "signed out: the mount's refusal")
}
