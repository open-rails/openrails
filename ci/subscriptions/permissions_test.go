//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	auth "github.com/open-rails/helpers/auth"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
)

// A staff route acting on a merchant admits its merchant: permission held in
// that merchant's scope, or its root: twin held in the Authenticator's root
// scope, and neither anywhere else; the access read answers the same levels
// for both.
func TestStaffHoldMerchantOrRootPermission(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	shop, other := w.client[embedded].MerchantID().String(), uuid.NewString()
	person := func(role string) string {
		return w.auth.issue(t, grant{subject: uuid.NewString(), role: role})
	}
	get := func(role, path string) (int, map[string]any) {
		return w.merchantJSON(person(role), http.MethodGet, path, nil)
	}
	refused := func(name, role, method, path string, body any) {
		t.Helper()
		status, answer := w.merchantJSON(person(role), method, path, body)
		require.Equal(t, http.StatusForbidden, status, "%s: %v", name, answer)
		require.Equal(t, billing.CodePermissionRequired, answer["error"].(map[string]any)["code"], name)
	}

	for name, role := range map[string]string{
		"merchant:billing:read at the merchant": heldIn(shop, billing.MerchantBillingRead),
		"root:billing:read in the root scope":   "only:" + string(billing.RootBillingRead),
	} {
		status, body := get(role, "/v1/admin/customers")
		require.Equal(t, http.StatusOK, status, "%s: %v", name, body)
	}
	for name, role := range map[string]string{
		"merchant:billing:read at another merchant": heldIn(other, billing.MerchantBillingRead),
		"merchant:billing:read in the root scope":   "only:" + string(billing.MerchantBillingRead),
		"root:billing:read at the merchant":         heldIn(shop, billing.RootBillingRead),
		"neither":                                   "nobody",
	} {
		refused(name, role, http.MethodGet, "/v1/admin/customers", nil)
	}

	// A read's name opens no change.
	for name, role := range map[string]string{
		"merchant:billing:read alone": heldIn(shop, billing.MerchantBillingRead),
		"root:billing:read alone":     "only:" + string(billing.RootBillingRead),
	} {
		refused(name, role, http.MethodPost, "/v1/admin/credit-grants", map[string]any{})
	}

	// The access read answers one level, whichever name is held.
	for _, tc := range []struct {
		merchant, root billing.Permission
		want           map[string]any
	}{
		{billing.MerchantBillingRead, billing.RootBillingRead, map[string]any{"admin": "read", "catalog": false, "merchant_config": false, "metrics": false}},
		{billing.MerchantCatalogManage, billing.RootCatalogManage, map[string]any{"admin": "none", "catalog": true, "merchant_config": false, "metrics": false}},
		{billing.MerchantConfigManage, billing.RootConfigManage, map[string]any{"admin": "none", "catalog": false, "merchant_config": true, "metrics": false}},
		{billing.MerchantMetricsRead, billing.RootMetricsRead, map[string]any{"admin": "none", "catalog": false, "merchant_config": false, "metrics": true}},
	} {
		tc.want["scope_id"] = shop
		for _, role := range []string{heldIn(shop, tc.merchant), "only:" + string(tc.root)} {
			status, access := get(role, "/v1/admin/access")
			require.Equal(t, http.StatusOK, status, "%s: %v", role, access)
			require.Equal(t, tc.want, access, role)
		}
	}
	_, access := get(heldIn(other, billing.MerchantBillingRead), "/v1/admin/access")
	require.Equal(t, "none", access["admin"], "held at another merchant")
}

// rootless is the harness verifier without its root scope: an Authenticator
// that is no helpers/auth Root.
type rootless struct{ v *verifier }

func (r rootless) Authenticate(req *http.Request) (auth.Verified, error) {
	return r.v.Authenticate(req)
}

// knowing is the harness verifier knowing only these permissions
// (helpers/auth PermissionCatalog).
type knowing struct {
	*verifier
	known []billing.Permission
}

func (k knowing) KnownPermission(p string) bool {
	return slices.Contains(k.known, billing.Permission(p))
}

// Mount refuses a staff or programmatic group behind an Authenticator with
// no root scope to hold root: permissions in, and a route neither of whose
// permissions the Authenticator knows, naming it. One known name is enough,
// and the routes that ask no permission need neither.
func TestMountNeedsARootAuthenticator(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	mount := func(routes openrails.Routes) error {
		routes.Prefix = "/" + uuid.NewString()[:8]
		return openrailshttp.Mount(http.NewServeMux(), w.rt, routes)
	}
	admin := openrails.RouteGroups{Admin: true}
	require.NoError(t, mount(openrails.Routes{Auth: rootless{w.auth}}), "public, customer and webhook routes ask no permission")
	for name, groups := range map[string]openrails.RouteGroups{"a staff group": admin, "the programmatic routes": {Programmatic: true}} {
		require.ErrorContains(t, mount(openrails.Routes{Auth: rootless{w.auth}, RouteGroups: groups}), "helpers/auth Root", name)
	}

	err := mount(openrails.Routes{Auth: knowing{w.auth, []billing.Permission{billing.RootBillingRead}}, RouteGroups: admin})
	require.ErrorContains(t, err, `knows none of its permissions ["merchant:billing:manage" "root:billing:manage"]`)
	require.Regexp(t, `^openrails: (POST|PUT|PATCH|DELETE) /v1/admin/`, err.Error(), "the error names the route")

	for name, known := range map[string][]billing.Permission{
		"the root: names":     {billing.RootBillingRead, billing.RootBillingManage},
		"the merchant: names": {billing.MerchantBillingRead, billing.MerchantBillingManage},
		"one of each":         {billing.MerchantBillingRead, billing.RootBillingManage},
	} {
		require.NoError(t, mount(openrails.Routes{Auth: knowing{w.auth, known}, RouteGroups: admin}), name)
	}
}

// Findings are an operator inbox. Billing read/manage grants open neither
// reads nor resolutions; either correctly scoped config grant opens all three,
// and resolving a finding does not require a recent sign-in.
func TestFindingsRequireMerchantConfiguration(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	mid := w.client[embedded].MerchantID()
	for _, tc := range []struct {
		name, role string
		allowed    bool
	}{
		{"billing reader", heldIn(mid.String(), billing.MerchantBillingRead), false},
		{"billing manager", heldIn(mid.String(), billing.MerchantBillingManage), false},
		{"root billing manager", "only:" + string(billing.RootBillingManage), false},
		{"other merchant config", heldIn(uuid.NewString(), billing.MerchantConfigManage), false},
		{"merchant config", heldIn(mid.String(), billing.MerchantConfigManage), true},
		{"root config", "only:" + string(billing.RootConfigManage), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var id string
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`INSERT INTO billing.reconciliation_findings
    (merchant_id, finding_type, subject_key, severity, status)
    VALUES ($1, 'consistency.test.authorization', $2, 'high', 'requires_review') RETURNING id::text`),
				mid.UUID(), uuid.NewString()).Scan(&id))
			token := w.auth.issue(t, grant{subject: uuid.NewString(), role: tc.role, signedIn: time.Now().Add(-time.Hour)})
			for _, op := range []struct {
				method, path string
				body         any
			}{
				{http.MethodGet, "/v1/admin/findings", nil},
				{http.MethodGet, "/v1/admin/findings/fnd_" + id, nil},
				{http.MethodPost, "/v1/admin/findings/fnd_" + id + "/resolve", map[string]any{"outcome": "ignore", "notes": "Reviewed by the operator"}},
			} {
				status, body := w.merchantJSON(token, op.method, op.path, op.body)
				want := http.StatusForbidden
				if tc.allowed {
					want = http.StatusOK
				}
				require.Equal(t, want, status, "%s %s: %v", op.method, op.path, body)
				if !tc.allowed {
					require.Equal(t, billing.CodePermissionRequired, body["error"].(map[string]any)["code"])
				}
			}
		})
	}
}
