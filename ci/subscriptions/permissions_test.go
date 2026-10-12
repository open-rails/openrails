//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	auth "github.com/open-rails/authkit/auth"
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
		"merchant:customer:read at the merchant": heldIn(shop, billing.MerchantCustomerRead),
		"root:customer:read in the root scope":   "only:" + string(billing.RootCustomerRead),
	} {
		status, body := get(role, "/v1/admin/customers")
		require.Equal(t, http.StatusOK, status, "%s: %v", name, body)
	}
	for name, role := range map[string]string{
		"merchant:customer:read at another merchant": heldIn(other, billing.MerchantCustomerRead),
		"merchant:customer:read in the root scope":   "only:" + string(billing.MerchantCustomerRead),
		"root:customer:read at the merchant":         heldIn(shop, billing.RootCustomerRead),
		"neither":                                    "nobody",
	} {
		refused(name, role, http.MethodGet, "/v1/admin/customers", nil)
	}

	// A read's name opens no change.
	for name, role := range map[string]string{
		"merchant:customer:read alone": heldIn(shop, billing.MerchantCustomerRead),
		"root:customer:read alone":     "only:" + string(billing.RootCustomerRead),
	} {
		refused(name, role, http.MethodPost, "/v1/admin/credit-grants", map[string]any{})
	}

	// The access read answers one level, whichever name is held. A write-only
	// grant does not expose a console section without its separate read grant.
	for _, tc := range []struct {
		merchant, root billing.Permission
		want           map[string]any
	}{
		{billing.MerchantCustomerRead, billing.RootCustomerRead, map[string]any{"admin": "read", "catalog": "none", "merchant_config": "none", "metrics": false}},
		{billing.MerchantCatalogRead, billing.RootCatalogRead, map[string]any{"admin": "none", "catalog": "read", "merchant_config": "none", "metrics": false}},
		{billing.MerchantCatalogManage, billing.RootCatalogManage, map[string]any{"admin": "none", "catalog": "none", "merchant_config": "none", "metrics": false}},
		{billing.MerchantConfigRead, billing.RootConfigRead, map[string]any{"admin": "none", "catalog": "none", "merchant_config": "read", "metrics": false}},
		{billing.MerchantConfigManage, billing.RootConfigManage, map[string]any{"admin": "none", "catalog": "none", "merchant_config": "none", "metrics": false}},
		{billing.MerchantMetricsRead, billing.RootMetricsRead, map[string]any{"admin": "none", "catalog": "none", "merchant_config": "none", "metrics": true}},
	} {
		tc.want["scope_id"] = shop
		tc.want["assistants"] = map[string]any{
			"catalog_copilot": false, "catalog_drafting": false,
		}
		tc.want["application_permissions"] = []any{}
		for _, role := range []string{heldIn(shop, tc.merchant), "only:" + string(tc.root)} {
			status, access := get(role, "/v1/admin/access")
			require.Equal(t, http.StatusOK, status, "%s: %v", role, access)
			require.Equal(t, tc.want, access, role)
		}
	}
	_, access := get(heldIn(other, billing.MerchantCustomerRead), "/v1/admin/access")
	require.Equal(t, "none", access["admin"], "held at another merchant")
}

// rootless is the harness verifier without its root scope: an Authenticator
// that is no authkit/auth Root.
type rootless struct{ v *verifier }

func (r rootless) Authenticate(req *http.Request) (auth.Verified, error) {
	return r.v.Authenticate(req)
}

// knowing is the harness verifier knowing only these permissions
// (authkit/auth PermissionCatalog).
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
		require.ErrorContains(t, mount(openrails.Routes{Auth: rootless{w.auth}, RouteGroups: groups}), "authkit/auth Root", name)
	}

	err := mount(openrails.Routes{Auth: knowing{w.auth, []billing.Permission{billing.RootCustomerRead}}, RouteGroups: admin})
	require.ErrorContains(t, err, `knows none of its permissions ["merchant:customer:manage" "root:customer:manage"]`)
	require.Regexp(t, `^openrails: (POST|PUT|PATCH|DELETE) /v1/admin/`, err.Error(), "the error names the route")

	for name, known := range map[string][]billing.Permission{
		"the root: names":     {billing.RootCustomerRead, billing.RootCustomerManage},
		"the merchant: names": {billing.MerchantCustomerRead, billing.MerchantCustomerManage},
		"one of each":         {billing.MerchantCustomerRead, billing.RootCustomerManage},
	} {
		require.NoError(t, mount(openrails.Routes{Auth: knowing{w.auth, known}, RouteGroups: admin}), name)
	}
}

// Findings are an operator inbox. Billing read/manage grants open neither
// reads nor resolutions. ConfigRead opens inspection and ConfigManage opens
// resolution; resolving a finding does not require a recent sign-in.
func TestFindingsRequireMerchantConfiguration(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	mid := w.client[embedded].MerchantID()
	for _, tc := range []struct {
		name, role   string
		read, manage bool
	}{
		{"billing reader", heldIn(mid.String(), billing.MerchantCustomerRead), false, false},
		{"billing manager", heldIn(mid.String(), billing.MerchantCustomerManage), false, false},
		{"root billing manager", "only:" + string(billing.RootCustomerManage), false, false},
		{"other merchant config", heldIn(uuid.NewString(), billing.MerchantConfigManage), false, false},
		{"merchant config read", heldIn(mid.String(), billing.MerchantConfigRead), true, false},
		{"merchant config manage", heldIn(mid.String(), billing.MerchantConfigManage), false, true},
		{"root config read", "only:" + string(billing.RootConfigRead), true, false},
		{"root config manage", "only:" + string(billing.RootConfigManage), false, true},
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
				{http.MethodGet, "/v1/merchant/findings", nil},
				{http.MethodGet, "/v1/merchant/findings/fnd_" + id, nil},
				{http.MethodPost, "/v1/merchant/findings/fnd_" + id + "/resolve", map[string]any{"outcome": "ignore", "notes": "Reviewed by the operator"}},
			} {
				status, body := w.merchantJSON(token, op.method, op.path, op.body)
				allowed := tc.read
				if op.method == http.MethodPost {
					allowed = tc.manage
				}
				want := http.StatusForbidden
				if allowed {
					want = http.StatusOK
				}
				require.Equal(t, want, status, "%s %s: %v", op.method, op.path, body)
				if !allowed {
					require.Equal(t, billing.CodePermissionRequired, body["error"].(map[string]any)["code"])
				}
			}
		})
	}
}
