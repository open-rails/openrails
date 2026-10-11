//go:build e2e && integration

package ci_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
)

// retiredMerchantHeader is the request header that once named a merchant:
// tests send it to show it names nothing.
const retiredMerchantHeader = "OpenRails" + "-Merchant"

// every route group a host can turn on.
var everyGroup = openrails.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true, Programmatic: true}

// bindingCall is one request through a mount: its answer and the merchant it
// says it acted on.
type bindingCall struct {
	status int
	body   string
	bound  string
}

func (c bindingCall) code(t *testing.T) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(c.body), &body), c.body)
	return body.Error.Code
}

// send serves one JSON request with token; header adds request headers.
func send(h http.Handler, token, method, path string, body any, header ...string) bindingCall {
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", uuid.NewString())
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return bindingCall{w.Code, w.Body.String(), w.Header().Get("OpenRails-Bound-Merchant")}
}

// merchantPerms are every merchant: permission, as a merchant's own staff or
// key holds them in its group.
func merchantPerms() []billing.Permission {
	var out []billing.Permission
	for _, p := range billing.Permissions() {
		if p.Persona() == billing.MerchantGroupPersona {
			out = append(out, p)
		}
	}
	return out
}

// boundTo issues credentials bound to merchant mid's group (helpers/auth
// Bound), as a host's AuthKit does: its staff member, an API key and a
// customer token holding nothing, a group-bound mint.
type boundTo struct {
	mid                  billing.MerchantID
	staff, key, customer string
	customerID           billing.CustomerID
}

func bind(auth *authtest.Fake, mid billing.MerchantID) boundTo {
	scope := authtest.MerchantScope(mid)
	b := boundTo{mid: mid, customerID: billing.CustomerID(uuid.New())}
	b.staff = auth.Issue(authtest.Grant{Identity: authtest.User(uuid.NewString()), Permissions: merchantPerms(), Scope: scope, Bound: scope})
	b.key = auth.Issue(authtest.Grant{Identity: authtest.Application("key-" + mid.String()), Permissions: merchantPerms(), Scope: scope, Bound: scope})
	b.customer = auth.Issue(authtest.Grant{Identity: authtest.User(b.customerID.String()), Bound: scope})
	return b
}

// declaredCatalog is a catalog of one product, key, granting key.
func declaredCatalog(key string) *catalog.Application {
	return &catalog.Application{SchemaVersion: 1, Products: map[string]catalog.ApplyProduct{
		key: {DisplayName: catalog.Value(key), Entitlements: catalog.Value([]string{key})},
	}}
}

// One engine and one mount serve two merchants, each declared with its own
// catalog. A credential bound to one merchant's group reads that merchant's
// rows on every tier and none of the other's, and the other's object ids are
// not found; a credential bound to neither reaches neither.
func TestEmbeddedEngineServesTwoMerchants(t *testing.T) {
	f := newFixture(t)
	ka, kb := "a-"+uuid.NewString()[:8], "b-"+uuid.NewString()[:8]
	cfg := f.config()
	cfg.Merchants = map[string]openrails.MerchantDeclaration{
		ka: {DisplayName: "A", Catalog: declaredCatalog(ka)},
		kb: {DisplayName: "B", Catalog: declaredCatalog(kb)},
	}
	bill, err := openrails.New(t.Context(), cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bill.Close(context.Background())) })
	shopA, err := bill.MerchantBySlug(ka)
	require.NoError(t, err)
	shopB, err := bill.MerchantBySlug(kb)
	require.NoError(t, err)
	auth := &authtest.Fake{}
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, bill, openrails.Routes{Auth: auth, RouteGroups: everyGroup}))
	a, b := bind(auth, shopA.MerchantID()), bind(auth, shopB.MerchantID())

	productOf := map[billing.MerchantID]billing.ProductID{}
	for _, s := range []struct {
		shop *openrails.Client
		key  string
		who  boundTo
	}{{shopA, ka, a}, {shopB, kb, b}} {
		products, err := s.shop.ListProducts(t.Context(), billing.ProductListParams{})
		require.NoError(t, err)
		require.Len(t, products.Items, 1, "each merchant's catalog applied from its declaration")
		require.Equal(t, s.key, products.Items[0].Key)
		productOf[s.who.mid] = products.Items[0].ID
		_, err = s.shop.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{{CustomerID: s.who.customerID, ProductID: products.Items[0].ID}}})
		require.NoError(t, err)
	}

	for _, tc := range []struct {
		who, other boundTo
		key        string
	}{{a, b, ka}, {b, a, kb}} {
		mine := tc.who.mid.String()
		staff := send(mux, tc.who.staff, http.MethodGet, "/v1/admin/catalog/products", nil)
		require.Equal(t, http.StatusOK, staff.status, staff.body)
		require.Equal(t, mine, staff.bound)
		require.Contains(t, staff.body, tc.key)
		require.NotContains(t, staff.body, `"key":"`+map[string]string{ka: kb, kb: ka}[tc.key]+`"`)
		gone := send(mux, tc.who.staff, http.MethodGet, "/v1/admin/catalog/products/"+productOf[tc.other.mid].String(), nil)
		require.Equal(t, http.StatusNotFound, gone.status, "the other merchant's product, by id: %s", gone.body)

		app := send(mux, tc.who.key, http.MethodPost, "/v1/app/entitlements/check", map[string]any{"customer_id": tc.who.customerID.String(), "entitlements": []string{tc.key}})
		require.Equal(t, http.StatusOK, app.status, app.body)
		require.Equal(t, mine, app.bound)
		require.Contains(t, app.body, `true`, "its own customer holds its own entitlement")
		app = send(mux, tc.who.key, http.MethodPost, "/v1/app/entitlements/check", map[string]any{"customer_id": tc.other.customerID.String(), "entitlements": []string{map[string]string{ka: kb, kb: ka}[tc.key]}})
		require.Equal(t, http.StatusOK, app.status, app.body)
		require.NotContains(t, app.body, `true`, "the other merchant's customer holds nothing here")
		other := send(mux, tc.who.key, http.MethodGet, "/v1/admin/customers/"+tc.other.customerID.String(), nil)
		require.Equal(t, http.StatusNotFound, other.status, "the other merchant's customer, by id: %s", other.body)

		me := send(mux, tc.who.customer, http.MethodGet, "/v1/me/entitlements", nil)
		require.Equal(t, http.StatusOK, me.status, me.body)
		require.Equal(t, mine, me.bound)
		require.Contains(t, me.body, tc.key)

		access := send(mux, tc.who.staff, http.MethodGet, "/v1/admin/access", nil)
		require.Equal(t, http.StatusOK, access.status, access.body)
		require.Contains(t, access.body, `"scope_id":"`+mine+`"`)
	}

	// SCIM: each merchant's provisioning token reaches its own directory.
	for _, s := range []struct {
		shop  *openrails.Client
		other *openrails.Client
	}{{shopA, shopB}, {shopB, shopA}} {
		minted, err := s.shop.CreateProvisioningToken(t.Context(), billing.CreateProvisioningTokenParams{Name: "directory"})
		require.NoError(t, err)
		user := uuid.NewString()
		scim := scimClient{t: t, handler: mux, base: "/v1/app/scim/v2", token: minted.Token}
		scim.must(http.MethodPost, "/Users", scimUser(user, "u-"+user[:8], user[:8]+"@example.test", ""), http.StatusCreated)
		got, err := s.shop.GetCustomer(t.Context(), billing.CustomerID(uuid.MustParse(user)))
		require.NoError(t, err)
		require.NotNil(t, got.Contact, "its own merchant's customer")
		_, err = s.other.GetCustomer(t.Context(), billing.CustomerID(uuid.MustParse(user)))
		require.ErrorIs(t, err, billing.ErrNotFound, "the other merchant knows nothing of it")
	}

	// Bound to neither, no deployment default fills in.
	unbound := auth.Person(uuid.NewString(), billing.RootBillingRead)
	for _, path := range []string{"/v1/admin/payments", "/v1/me"} {
		got := send(mux, unbound, http.MethodGet, path, nil)
		require.Equal(t, http.StatusForbidden, got.status, "%s: %s", path, got.body)
		require.Equal(t, billing.CodeMerchantUnresolved, got.code(t), path)
		require.Empty(t, got.bound)
	}
}

// A bound scope that is not a live merchant's in the Authenticator's root
// authority is refused on every gated tier, never read as the issuer
// binding: one of no group, of a deleted merchant and of another authority,
// also when the credential's issuer is the one the merchant declares.
func TestBoundScopeOfNoMerchantIsRefused(t *testing.T) {
	f := newFixture(t)
	slug, gone := "bound-"+uuid.NewString()[:8], "gone-"+uuid.NewString()[:8]
	cfg := f.config()
	cfg.Merchants = map[string]openrails.MerchantDeclaration{slug: {DisplayName: slug, Issuer: authtest.Issuer}, gone: {DisplayName: gone}}
	bill, err := openrails.New(t.Context(), cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bill.Close(context.Background())) })
	deleted, err := bill.MerchantBySlug(gone)
	require.NoError(t, err)
	_, err = f.pool.Exec(t.Context(), "UPDATE "+pgx.Identifier{f.schema, "merchants"}.Sanitize()+" SET status = 'deleted', deleted_at = now() WHERE id = $1", deleted.MerchantID().UUID())
	require.NoError(t, err)
	auth := &authtest.Fake{}
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, bill, openrails.Routes{Auth: auth, RouteGroups: everyGroup}))

	routes := []struct{ method, path string }{
		{http.MethodGet, "/v1/me"},
		{http.MethodGet, "/v1/admin/findings"},
		{http.MethodGet, "/v1/admin/access"},
		{http.MethodGet, "/v1/app/host-events"},
		{http.MethodGet, "/v1/app/scim/v2/Users"},
	}
	for name, scope := range map[string]billingauth.Scope{
		"no merchant's group":  {Authority: authtest.Issuer, ID: uuid.NewString()},
		"a deleted merchant's": authtest.MerchantScope(deleted.MerchantID()),
		"another authority's":  {Authority: "https://other.example", ID: deleted.MerchantID().String()},
		"the root group":       authtest.Root,
		"not a canonical id":   {Authority: authtest.Issuer, ID: strings.ToUpper(deleted.MerchantID().String())},
	} {
		for _, who := range []billingauth.Identity{authtest.User(uuid.NewString()), authtest.Application("app")} {
			token := auth.Issue(authtest.Grant{Identity: who, Permissions: billing.Permissions(), Scope: scope, Bound: scope})
			for _, r := range routes {
				got := send(mux, token, r.method, r.path, nil)
				require.Equal(t, http.StatusForbidden, got.status, "%s, %s %s: %s", name, r.method, r.path, got.body)
				if !strings.Contains(r.path, "/scim/") {
					require.Equal(t, billing.CodeMerchantUnresolved, got.code(t), "%s, %s %s", name, r.method, r.path)
				}
				require.Empty(t, got.bound)
			}
		}
	}
}

// Nothing in the request names its merchant: with A's credential, a header,
// a Host, a query parameter and a body field naming B each leave the answer
// A's (or refuse the unknown field), and no path names a merchant.
func TestRequestCannotNameItsMerchant(t *testing.T) {
	f := newFixture(t)
	ka, kb := "a-"+uuid.NewString()[:8], "b-"+uuid.NewString()[:8]
	cfg := f.config()
	cfg.Merchants = map[string]openrails.MerchantDeclaration{ka: {DisplayName: "A", Issuer: authtest.Issuer}, kb: {DisplayName: "B"}}
	bill, err := openrails.New(t.Context(), cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bill.Close(context.Background())) })
	shopA, err := bill.MerchantBySlug(ka)
	require.NoError(t, err)
	shopB, err := bill.MerchantBySlug(kb)
	require.NoError(t, err)
	a, b := shopA.MerchantID().String(), shopB.MerchantID().String()
	auth := &authtest.Fake{}
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, bill, openrails.Routes{Auth: auth, RouteGroups: everyGroup}))
	staff := auth.Person(uuid.NewString(), billing.RootBillingRead, billing.RootBillingManage)
	customer := auth.Person(uuid.NewString())

	for name, got := range map[string]bindingCall{
		"a header":      send(mux, staff, http.MethodGet, "/v1/admin/findings", nil, retiredMerchantHeader, "id:"+b),
		"a slug header": send(mux, staff, http.MethodGet, "/v1/admin/findings", nil, retiredMerchantHeader, kb),
		"a Host":        send(mux, staff, http.MethodGet, "/v1/admin/findings", nil, "Host", kb+".api.billing.example"),
		"a customer's":  send(mux, customer, http.MethodGet, "/v1/me", nil, retiredMerchantHeader, "id:"+b),
		"a query":       send(mux, staff, http.MethodGet, "/v1/admin/payments?merchant="+b, nil),
	} {
		if got.status == http.StatusBadRequest {
			require.Equal(t, billing.CodeInvalidQuery, got.code(t), "%s: %s", name, got.body)
			continue
		}
		require.Equal(t, http.StatusOK, got.status, "%s: %s", name, got.body)
		require.Equal(t, a, got.bound, "%s: the answer is A's", name)
	}
	body := send(mux, staff, http.MethodPatch, "/v1/admin/customers/"+uuid.NewString(), map[string]any{"merchant_id": b})
	require.Equal(t, http.StatusBadRequest, body.status, body.body)
	require.Equal(t, billing.CodeUnknownField, body.code(t))
	path := send(mux, staff, http.MethodGet, "/v1/merchants/"+b+"/admin/findings", nil)
	require.Equal(t, http.StatusNotFound, path.status, path.body)
}

// One merchant whose host declares its own issuer: the host's tokens, bound
// to no group, act there as its customers, its own (customers.issuer NULL),
// and as its staff; a token of another issuer reaches no merchant.
func TestIssuerBindsASingleMerchantHost(t *testing.T) {
	f := newFixture(t)
	slug := "issued-" + uuid.NewString()[:8]
	cfg := f.config()
	cfg.Merchants = map[string]openrails.MerchantDeclaration{slug: {DisplayName: slug, Issuer: authtest.Issuer}}
	bill, err := openrails.New(t.Context(), cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bill.Close(context.Background())) })
	shop, err := bill.MerchantBySlug(slug)
	require.NoError(t, err)
	auth := &authtest.Fake{}
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, bill, openrails.Routes{Auth: auth, RouteGroups: everyGroup}))

	user := uuid.NewString()
	me := send(mux, auth.Person(user), http.MethodGet, "/v1/me", nil)
	require.Equal(t, http.StatusOK, me.status, me.body)
	require.Equal(t, shop.MerchantID().String(), me.bound)
	var issuer *string
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT issuer FROM "+pgx.Identifier{f.schema, "customers"}.Sanitize()+" WHERE merchant_id = $1 AND id = $2",
		shop.MerchantID().UUID(), uuid.MustParse(user)).Scan(&issuer))
	require.Nil(t, issuer, "the host's own user is the merchant's own customer")
	staff := send(mux, auth.Person(uuid.NewString(), billing.RootBillingRead), http.MethodGet, "/v1/admin/payments", nil)
	require.Equal(t, http.StatusOK, staff.status, staff.body)

	foreign := authtest.User(uuid.NewString())
	foreign.Issuer, foreign.Invoker.Issuer = "https://other.example", "https://other.example"
	for _, path := range []string{"/v1/me", "/v1/admin/payments"} {
		got := send(mux, auth.Issue(authtest.Grant{Identity: foreign, Permissions: billing.Permissions()}), http.MethodGet, path, nil)
		require.Equal(t, http.StatusForbidden, got.status, "%s: %s", path, got.body)
		require.Equal(t, billing.CodeMerchantUnresolved, got.code(t), path)
	}
}

// Two hosts with their own issuers serve one merchant over one schema: a
// person with one subject is one customer there, the merchant's own, and what
// one host grants the other reads.
func TestTwoIssuersShareOneMerchant(t *testing.T) {
	f := newFixture(t)
	slug := "shared-" + uuid.NewString()[:8]
	host := func(issuer string) (*openrails.Engine, http.Handler, *authtest.Fake) {
		cfg := f.config()
		cfg.Merchants = map[string]openrails.MerchantDeclaration{slug: {DisplayName: slug, Issuer: issuer}}
		bill, err := openrails.New(t.Context(), cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, bill.Close(context.Background())) })
		auth := &authtest.Fake{}
		mux := http.NewServeMux()
		require.NoError(t, openrailshttp.Mount(mux, bill, openrails.Routes{Auth: auth}))
		return bill, mux, auth
	}
	one, oneMux, oneAuth := host("https://one.example")
	_, twoMux, twoAuth := host("https://two.example")
	person := uuid.NewString()
	as := func(auth *authtest.Fake, issuer string) string {
		id := authtest.User(person)
		id.Issuer, id.Invoker.Issuer = issuer, issuer
		return auth.Issue(authtest.Grant{Identity: id})
	}
	viaOne, viaTwo := as(oneAuth, "https://one.example"), as(twoAuth, "https://two.example")
	for name, got := range map[string]bindingCall{
		"one": send(oneMux, viaOne, http.MethodGet, "/v1/me", nil),
		"two": send(twoMux, viaTwo, http.MethodGet, "/v1/me", nil),
	} {
		require.Equal(t, http.StatusOK, got.status, "%s: %s", name, got.body)
	}
	shop, err := one.MerchantBySlug(slug)
	require.NoError(t, err)
	var rows int
	var issuer *string
	require.NoError(t, f.pool.QueryRow(t.Context(), "SELECT count(*), max(issuer) FROM "+pgx.Identifier{f.schema, "customers"}.Sanitize()+" WHERE merchant_id = $1 AND id = $2",
		shop.MerchantID().UUID(), uuid.MustParse(person)).Scan(&rows, &issuer))
	require.Equal(t, 1, rows, "one customer row for the person")
	require.Nil(t, issuer, "the merchant's own customer")

	key := "member-" + uuid.NewString()[:8]
	product, err := shop.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, Entitlements: []string{key}})
	require.NoError(t, err)
	_, err = shop.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{{CustomerID: billing.CustomerID(uuid.MustParse(person)), ProductID: product.ID}}})
	require.NoError(t, err)
	held := send(twoMux, viaTwo, http.MethodGet, "/v1/me/entitlements", nil)
	require.Equal(t, http.StatusOK, held.status, held.body)
	require.Contains(t, held.body, key, "what one host granted, the other reads")
}

// An issuer binds its credentials to one merchant of an engine: declared by
// two, New fails before opening anything; a merchant file never carries one.
func TestIssuerDeclaredTwiceFailsNew(t *testing.T) {
	f := newFixture(t)
	cfg := f.config()
	cfg.Merchants = map[string]openrails.MerchantDeclaration{
		"a-" + uuid.NewString()[:8]: {Issuer: "https://shop.example"},
		"b-" + uuid.NewString()[:8]: {Issuer: "https://shop.example"},
	}
	bill, err := openrails.New(t.Context(), cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
	require.Nil(t, bill)
	require.ErrorContains(t, err, "an issuer binds its credentials to one merchant")
	_, err = openrails.ParseMerchantDeclaration([]byte("slug: shop\nissuer: https://shop.example\n"))
	require.ErrorContains(t, err, "issuer is not a merchant file's")
}
