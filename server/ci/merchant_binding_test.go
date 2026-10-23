//go:build e2e && integration

package ci_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/server"
)

// A credential is an access token of this resource or none: a token for
// another audience, one typed JWT rather than at+jwt and one of an issuer no
// merchant trusts are each refused 401 on every gated tier.
func TestBoundTokensAreAccessTokensForThisResource(t *testing.T) {
	f := newFixture(t)
	srv := f.resourceServer(t)
	mid := provision(t, srv, uniqueName("tokens"))
	host := newIssuerKey(t, "https://tokens-"+uuid.NewString()[:8]+".e2e.test")
	trust(t, srv, mid, host.app(t, "owner", nil))
	stranger := newIssuerKey(t, "https://stranger-"+uuid.NewString()[:8]+".e2e.test")
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)

	customer := func(c jwt.MapClaims) {
		c["sub"], c["scope"] = uuid.NewString(), billing.ScopeSelf
		delete(c, "permissions")
	}
	machine := func(c jwt.MapClaims) {
		c["sub"], c["client_id"] = "backend", "backend"
		c["permissions"] = []string{"merchant:*"}
	}
	typed := func(k issuerKey, edit func(jwt.MapClaims), typ string) string {
		claims := jwt.MapClaims{"iss": k.iss, "aud": resourceID, "sub": uuid.NewString(), "client_id": "admin-ui",
			"iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix(), "jti": uuid.NewString(), "scope": billing.ScopeMerchant}
		edit(claims)
		token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		token.Header["typ"], token.Header["kid"] = typ, k.kid
		signed, err := token.SignedString(k.key)
		require.NoError(t, err)
		return signed
	}
	routes := map[string]func(jwt.MapClaims){"/v1/me": customer, "/v1/merchant/findings": machine, "/v1/app/events": machine}
	for path, shape := range routes {
		require.Equal(t, http.StatusOK, serve(handler, rsRequest{path: path, authorization: "Bearer " + host.mint(t, shape)}).Code, "control: %s", path)
		for name, token := range map[string]string{
			"another audience": host.mint(t, func(c jwt.MapClaims) { shape(c); c["aud"] = "https://elsewhere.e2e.test" }),
			"typed JWT":        typed(host, shape, "JWT"),
			"an untrusted iss": stranger.mint(t, shape),
		} {
			w := serve(handler, rsRequest{path: path, authorization: "Bearer " + token})
			require.Equal(t, http.StatusUnauthorized, w.Code, "%s at %s: %s", name, path, w.Body.String())
		}
	}
}

// One issuer belongs to one merchant: registering a trusted issuer in a
// second merchant's group is refused, so its tokens bind one merchant.
func TestRemoteIssuerBelongsToOneMerchant(t *testing.T) {
	f := newFixture(t)
	srv := f.resourceServer(t)
	a, b := provision(t, srv, uniqueName("issuer-a")), provision(t, srv, uniqueName("issuer-b"))
	backend := newIssuerKey(t, "https://one-"+uuid.NewString()[:8]+".e2e.test")
	trust(t, srv, a, backend.app(t, "viewer", nil))
	err := srv.AuthKit().DeclareRemoteApplications(t.Context(), iam.GroupByID(b.String()), []iam.RemoteApplication{backend.app(t, "viewer", nil)})
	require.ErrorIs(t, err, iam.ErrRemoteApplicationIssuerConflict)
}

// A credential bound to merchant A reads A's rows on every tier and nothing
// of B's: its customer token, staff API key and programmatic key; B's object
// ids are not found with A's key. (The server's SCIM directory is AuthKit's;
// an embedded engine's provisioning tokens are checked in ci.)
func TestCredentialsReachOnlyTheirMerchant(t *testing.T) {
	f := newFixture(t)
	srv := f.resourceServer(t)
	a, b := provision(t, srv, uniqueName("reach-a")), provision(t, srv, uniqueName("reach-b"))
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	backendA := newIssuerKey(t, "https://reach-a-"+uuid.NewString()[:8]+".e2e.test")
	trust(t, srv, a, backendA.app(t, "viewer", nil))
	keyA := merchantKey(t, srv, a, "owner").Secret
	shopA, shopB := handle(t, srv, a), handle(t, srv, b)

	alice, bob := billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New())
	grantTo(t, shopA, alice)
	grantTo(t, shopB, bob)
	product, err := shopB.CreateProduct(t.Context(), billing.CreateProductParams{Key: "b-only", DisplayName: "B only", Entitlements: []string{"b-only"}})
	require.NoError(t, err)
	_, err = shopB.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{{CustomerID: alice, ProductID: product.ID}}})
	require.NoError(t, err)

	// The customer token of alice at A sees nothing B granted her.
	w := f.redeem(t, handler, backendA.assertion(t, f.tokenURL(), alice.String()))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	token := decode[boundToken](t, w).AccessToken
	me := serve(handler, rsRequest{path: "/v1/me/entitlements", authorization: "Bearer " + token})
	require.Equal(t, http.StatusOK, me.Code, me.Body.String())
	require.Equal(t, a.String(), me.Header().Get("OpenRails-Bound-Merchant"))
	require.NotContains(t, me.Body.String(), "b-only")

	// A's staff key: A's customers, B's by id not found.
	list := call(t, handler, keyA, http.MethodGet, "/v1/admin/customers", nil)
	require.Equal(t, http.StatusOK, list.Code, list.Body.String())
	require.Contains(t, list.Body.String(), alice.String())
	require.NotContains(t, list.Body.String(), bob.String())
	for _, path := range []string{"/v1/admin/customers/" + bob.String(), "/v1/catalog/products/" + product.ID.String()} {
		require.Equal(t, http.StatusNotFound, call(t, handler, keyA, http.MethodGet, path, nil).Code, path)
	}

	// A's programmatic key: B's grant to alice is not A's.
	check := call(t, handler, keyA, http.MethodPost, "/v1/app/entitlements/check", map[string]any{"customer_id": alice.String(), "entitlements": []string{"b-only"}})
	require.Equal(t, http.StatusOK, check.Code, check.Body.String())
	require.NotContains(t, check.Body.String(), "true")
}

// A host's group-bound mint is its user acting for itself at the merchant:
// a customer there, no staff member; a console token, granted the merchant
// scope and not the customer one, is no customer.
func TestGroupMintIsACustomer(t *testing.T) {
	f := newFixture(t)
	srv := f.resourceServer(t)
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	owner, session := newOwner(t, srv)
	mid := provisionOwned(t, srv, uniqueName("mint"), owner)
	buyer := newAccount(t, srv)
	minted, err := srv.AuthKit().MintAccessToken(t.Context(), buyer.ID, iam.AccessTokenOptions{Group: iam.GroupByID(mid.String())})
	require.NoError(t, err)

	me := serve(handler, rsRequest{path: "/v1/me", authorization: "Bearer " + minted.Value})
	require.Equal(t, http.StatusOK, me.Code, me.Body.String())
	require.Equal(t, mid.String(), me.Header().Get("OpenRails-Bound-Merchant"))
	admin := serve(handler, rsRequest{path: "/v1/merchant/findings", authorization: "Bearer " + minted.Value})
	require.Equal(t, http.StatusForbidden, admin.Code, admin.Body.String())
	require.Equal(t, billing.CodePermissionRequired, errorCode(t, admin))

	console := f.exchanged(t, handler, session, mid)
	w := console.call(t, handler, http.MethodGet, "/v1/me", nil)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	require.Equal(t, billing.CodePermissionRequired, errorCode(t, w))
}

// Every merchant-bound answer names the merchant it acted on, and the Go
// client checks it: API keys filed under each other's merchants fail every
// call with a BoundMerchantError.
func TestSwappedCredentialsFailEveryCall(t *testing.T) {
	f := newFixture(t)
	srv := f.newServer(t, nil)
	a, b := provision(t, srv, uniqueName("swap-a")), provision(t, srv, uniqueName("swap-b"))
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	api := record(t, handler)
	keyA, keyB := merchantKey(t, srv, a, "owner").Secret, merchantKey(t, srv, b, "owner").Secret
	remote, err := openrails.NewRemote(api.URL, openrails.RemoteOptions{
		Credentials: map[billing.MerchantID]openrails.RemoteCredential{a: {APIKey: keyB}, b: {APIKey: keyA}}, Timeout: 30 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = remote.Close(context.Background()) })
	for filed, bound := range map[billing.MerchantID]billing.MerchantID{a: b, b: a} {
		h, err := remote.Merchant(filed)
		require.NoError(t, err)
		for name, call := range map[string]func() error{
			"list customers": func() error { _, err := h.ListCustomers(t.Context(), billing.CustomerListParams{}); return err },
			"read config":    func() error { _, err := h.GetMerchantConfiguration(t.Context()); return err },
			"list products":  func() error { _, err := h.ListProducts(t.Context(), billing.ProductListParams{}); return err },
		} {
			err := call()
			var mismatch *openrails.BoundMerchantError
			require.True(t, errors.As(err, &mismatch), "%s: %v", name, err)
			require.Equal(t, openrails.BoundMerchantError{Merchant: filed, Bound: bound}, *mismatch, name)
		}
	}
}

// Nothing in the request names its merchant on the standalone server: with
// A's key, a header, a merchant Host and a query naming B leave the answer
// A's, a body field naming B is refused as unknown, and no path names one.
func TestRequestCannotNameItsMerchant(t *testing.T) {
	f := newFixture(t)
	srv := f.newServer(t, nil)
	a, b := provision(t, srv, uniqueName("named-a")), provision(t, srv, uniqueName("named-b"))
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	keyA := merchantKey(t, srv, a, "owner").Secret
	for name, w := range map[string]*httptest.ResponseRecorder{
		"a header":        callWith(t, handler, keyA, http.MethodGet, "/v1/merchant/findings", nil, retiredMerchantHeader, "id:"+b.String()),
		"a merchant Host": callWith(t, handler, keyA, http.MethodGet, "/v1/merchant/findings", nil, "Host", "shop-b.api.billing.example"),
		"a query":         callWith(t, handler, keyA, http.MethodGet, "/v1/merchant/findings?merchant="+b.String(), nil),
	} {
		if w.Code == http.StatusBadRequest {
			require.Equal(t, billing.CodeInvalidQuery, errorCode(t, w), name)
			continue
		}
		require.Equal(t, http.StatusOK, w.Code, "%s: %s", name, w.Body.String())
		require.Equal(t, a.String(), w.Header().Get("OpenRails-Bound-Merchant"), name)
	}
	body := call(t, handler, keyA, http.MethodPatch, "/v1/admin/customers/"+uuid.NewString(), map[string]any{"merchant_id": b.String()})
	require.Equal(t, http.StatusBadRequest, body.Code, body.Body.String())
	require.Equal(t, billing.CodeUnknownField, errorCode(t, body))
	require.Equal(t, http.StatusNotFound, call(t, handler, keyA, http.MethodGet, "/v1/merchants/"+b.String()+"/merchant/findings", nil).Code)
}

// Secrets stay where they are given: through a customer token's redemption
// and use, a console exchange, API key calls and a payment page, no
// access token, assertion, API key or pay URL secret reaches a log line or
// a request URL; a pay URL carries its secret in the fragment alone,
// and the answers carrying one are not stored.
func TestBoundTokensStayOutOfLogsAndURLs(t *testing.T) {
	hook := logtest.NewGlobal()
	var slogged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{w: &slogged}, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	s := newPayShop(t)
	var mu sync.Mutex
	var urls []string
	inner := s.handler
	s.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		urls = append(urls, r.URL.String())
		mu.Unlock()
		inner.ServeHTTP(w, r)
	})
	var secrets []string

	// A payment page: the order's answer carries the URL, its secret in
	// the fragment, and is not stored.
	created := s.call(serverHost, http.MethodPost, "/v1/app/orders", s.backend, uuid.NewString(), map[string]any{
		"customer_id": uuid.NewString(), "lines": []map[string]any{{"price_id": s.price()}}, "pay_url": map[string]any{"success_url": "https://shop.e2e.test/thanks"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.Body.String())
	require.Equal(t, "no-store", created.Header().Get("Cache-Control"))
	order := decode[billing.Order](t, created)
	page, secret, ok := strings.Cut(*order.PayURL, "#")
	require.True(t, ok)
	require.NotContains(t, page, secret)
	read := s.call(paymentHost, http.MethodGet, "/v1/me/orders/"+order.ID.String(), secret, "", nil)
	require.Equal(t, http.StatusOK, read.Code, read.Body.String())
	require.Equal(t, "no-store", read.Header().Get("Cache-Control"))
	secrets = append(secrets, secret, s.backend)

	// A merchant backend's assertion, its customer token, an API key and a
	// console token.
	backend := newIssuerKey(t, "https://secrets-"+uuid.NewString()[:8]+".e2e.test")
	trust(t, s.srv, s.mid, backend.app(t, "viewer", nil))
	f := s.f
	assertion := backend.assertion(t, f.tokenURL(), uuid.NewString())
	redeemed := f.redeem(t, s.handler, assertion)
	require.Equal(t, http.StatusOK, redeemed.Code, redeemed.Body.String())
	require.Equal(t, "no-store", redeemed.Header().Get("Cache-Control"), "a token endpoint answer is not stored")
	customer := decode[boundToken](t, redeemed).AccessToken
	require.Equal(t, http.StatusOK, serve(s.handler, rsRequest{path: "/v1/me", authorization: "Bearer " + customer}).Code)
	key := merchantKey(t, s.srv, s.mid, "owner").Secret
	require.Equal(t, http.StatusOK, call(t, s.handler, key, http.MethodGet, "/v1/merchant/findings", nil).Code)
	owner := newAccount(t, s.srv)
	role, ok := server.MerchantRole("owner")
	require.True(t, ok)
	authtest.GrantRole(t, s.srv.AuthKit(), iam.GroupByID(s.mid.String()), iam.UserSubject(owner.ID), role)
	session := authtest.SignIn(t, s.srv.AuthKit(), owner).AccessToken
	console := f.exchanged(t, s.handler, session, s.mid)
	require.Equal(t, http.StatusOK, console.call(t, s.handler, http.MethodGet, "/v1/admin/access", nil).Code)
	// A refusal logs too: a token presented at a merchant it is not bound to.
	require.Equal(t, http.StatusForbidden, serve(s.handler, rsRequest{path: "/v1/merchant/findings", authorization: "Bearer " + session}).Code)
	secrets = append(secrets, assertion, customer, key, session, console.AccessToken)

	var logged strings.Builder
	for _, entry := range hook.AllEntries() {
		line, err := entry.String()
		require.NoError(t, err)
		logged.WriteString(line)
	}
	logged.WriteString(slogged.String())
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, urls)
	for _, secret := range secrets {
		require.NotContains(t, logged.String(), secret, "a secret in a log line")
		for _, u := range urls {
			require.NotContains(t, u, secret, "a secret in a request URL")
		}
	}
}

// lockedWriter serializes writes from concurrent loggers.
type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
