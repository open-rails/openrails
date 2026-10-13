//go:build e2e && integration

package ci_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	hostKeys "github.com/open-rails/authkit/keys"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/customernetwork"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/paymentpage"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/internal/vaultfake"
	"github.com/open-rails/openrails/server"
	"github.com/open-rails/openrails/server/internal/bootstrap/serverboot"
	"github.com/open-rails/openrails/server/internal/network"
	"github.com/open-rails/openrails/server/internal/operator"
)

const (
	networkTerms      = "2026-10"
	paymentPageReturn = "https://" + paymentHost + paymentpage.SignInPath
	merchantReturn    = "https://shop.e2e.test/openrails/callback"
)

// world is openrails.dev: a standalone server running the customer network,
// its AuthKit the OpenID provider merchants' clients sign accounts in at,
// served over HTTPS at its issuer, with a payment origin whose page's
// client the server declares. The shop has its own login (a trusted issuer)
// and an NMI PSP; the rival has its own login. The mart has an NMI PSP too,
// at a gateway of its own.
type world struct {
	t                    *testing.T
	srv                  *server.Server
	handler              http.Handler
	as                   *authtest.AuthorizationServer
	gateway, martGateway *nmimock.Mock
	now                  func() time.Time

	shop, rival, mart       billing.MerchantID
	shopLogin, rivalLogin   issuerKey
	shopClient, rivalClient iam.OAuthClientCreated
	shopKey, rivalKey       iam.APIKeyCreated
}

func newWorld(t *testing.T) *world { return newWorldAt(t, nil) }

// newWorldAt is a world whose engine and gateway keep clock's time; nil is
// the wall clock.
func newWorldAt(t *testing.T, clock *clockwork.FakeClock) *world {
	t.Helper()
	f := newFixture(t)
	ctx := t.Context()
	vault := vaultfake.New("e2e-root")
	t.Cleanup(vault.Close)
	now := time.Now
	if clock != nil {
		now = clock.Now
	}
	gateway, martGateway := nmimock.New(nmimock.Options{Clock: now}), nmimock.New(nmimock.Options{Clock: now})
	t.Cleanup(gateway.Close)
	t.Cleanup(martGateway.Close)
	// The sandbox has one NMI URL; each PSP's security key reaches its own
	// gateway, so neither sees the other's transactions.
	gateways := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		form, _ := url.ParseQuery(string(body))
		if form.Get("security_key") == "mart" || r.URL.Query().Get("security_key") == "mart" || r.Header.Get("Authorization") == "mart" {
			martGateway.ServeHTTP(rw, r)
			return
		}
		gateway.ServeHTTP(rw, r)
	}))
	t.Cleanup(gateways.Close)
	var served atomic.Pointer[http.Handler]
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := served.Load(); h != nil {
			(*h).ServeHTTP(w, r)
			return
		}
		http.Error(w, "starting", http.StatusServiceUnavailable)
	}))
	t.Cleanup(ts.Close)
	srv := f.newServer(t, func(cfg *server.Config, deps *server.Deps) {
		cfg.Engine.PublicBillingBaseURL = "https://" + serverHost
		cfg.PaymentOrigin = "https://" + paymentHost
		deps.PaymentPageAssets = paymentPage
		cfg.Auth.Token.Issuer = ts.URL
		cfg.Auth.Resource = authkit.ResourceConfig{ID: resourceID, PublicURL: rsOrigin}
		cfg.Auth.Agreements = []authkit.AgreementConfig{{Key: network.TermsAgreement, Version: networkTerms, URL: "https://openrails.e2e.test/terms"}}
		cfg.Engine.ProviderWriteMode = openrails.ProviderWritesFull
		cfg.Engine.Vault = &openrails.VaultConfig{Address: vault.URL(), Token: vault.Token}
		cfg.Engine.ProviderSandbox = &openrails.ProviderSandboxConfig{NMIGatewayURL: gateways.URL}
		if clock != nil {
			deps.Engine.Clock = clock
		}
	})
	shop, mart := uniqueName("network-shop"), uniqueName("network-mart")
	manifest := filepath.Join(t.TempDir(), "merchants.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte(fmt.Sprintf(`version: 1
merchants:
  %s:
    display_name: Network Shop
    sandbox: true
    psps:
      nmi:
        rail: nmi
        environment: sandbox
        gateway_id: test
        secrets: {security_key: test, webhook_signing_secret: test}
        tokenization_key: test
  %s:
    display_name: Network Mart
    sandbox: true
    psps:
      nmi:
        rail: nmi
        environment: sandbox
        gateway_id: mart
        secrets: {security_key: mart, webhook_signing_secret: mart}
        tokenization_key: mart
`, shop, mart)), 0o600))
	graph, plane := operator.Of(srv)
	require.NoError(t, serverboot.ReconcileBootMerchantManifest(ctx, graph.Config, graph, plane, manifest, nil, ""))
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	served.Store(&handler)
	require.NoError(t, srv.Start(ctx), "River delivers AuthKit's account events")

	m, err := graph.Runtime.Merchants.GetBySlug(ctx, shop)
	require.NoError(t, err)
	mm, err := graph.Runtime.Merchants.GetBySlug(ctx, mart)
	require.NoError(t, err)
	label := strings.ReplaceAll(f.schema, "_", "-")
	w := &world{t: t, srv: srv, handler: handler, as: authtest.Attach(t, srv.AuthKit(), ts), gateway: gateway, martGateway: martGateway, now: now,
		shop: m.ID, mart: mm.ID, rival: provision(t, srv, uniqueName("network-rival")),
		shopLogin: newIssuerKey(t, "https://shop-"+label+".e2e.test"), rivalLogin: newIssuerKey(t, "https://rival-"+label+".e2e.test")}
	trust(t, srv, w.shop, w.shopLogin.app(t, "viewer", nil))
	trust(t, srv, w.rival, w.rivalLogin.app(t, "viewer", nil))
	w.shopClient, w.rivalClient = w.client(w.shop), w.client(w.rival)
	w.shopKey, w.rivalKey = merchantKey(t, srv, w.shop, "owner"), merchantKey(t, srv, w.rival, "owner")
	return w
}

// client registers "Sign in with openrails.dev" for a merchant.
func (w *world) client(mid billing.MerchantID) iam.OAuthClientCreated {
	w.t.Helper()
	c, err := w.srv.AuthKit().CreateGroupOAuthClient(w.t.Context(), iam.SystemIdentity(), iam.GroupByID(mid.String()), iam.NewOAuthClient{
		ClientName: "Sign in", RedirectURIs: []string{merchantReturn}, TokenEndpointAuthMethod: iam.OAuthClientSecretBasic,
		Scope: "openid email openrails:self openrails:link"})
	require.NoError(w.t, err)
	return c
}

// account is an openrails.dev account; terms accepts the network terms.
func (w *world) account(terms bool) authtest.User {
	w.t.Helper()
	u := newAccount(w.t, w.srv)
	if terms {
		require.NoError(w.t, w.srv.AuthKit().AcceptAgreements(w.t.Context(), u.ID, []iam.AgreementRef{{Key: network.TermsAgreement, Version: networkTerms}}))
	}
	return u
}

// gatewayOf is the merchant's NMI gateway.
func (w *world) gatewayOf(mid billing.MerchantID) *nmimock.Mock {
	if mid == w.mart {
		return w.martGateway
	}
	return w.gateway
}

// customer is a customer token from a merchant's own login.
func (w *world) customer(login issuerKey, sub string, edit func(jwt.MapClaims)) string {
	return "Bearer " + login.mint(w.t, func(c jwt.MapClaims) {
		c["sub"], c["scope"] = sub, billing.ScopeSelf
		delete(c, "permissions")
		if edit != nil {
			edit(c)
		}
	})
}

func (w *world) me(method, path, authorization string, body any) *httptest.ResponseRecorder {
	w.t.Helper()
	q := rsRequest{method: method, path: "/v1/me" + path, authorization: authorization}
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(w.t, err)
		q.body = string(raw)
	}
	if method == http.MethodPost && strings.HasPrefix(path, "/orders") {
		q.idempotencyKey = uuid.NewString()
	}
	return serve(w.handler, q)
}

func (w *world) challenge(authorization string) billing.NetworkLinkChallenge {
	w.t.Helper()
	r := w.me(http.MethodPost, "/network-link/challenges", authorization, nil)
	require.Equal(w.t, http.StatusCreated, r.Code, r.Body.String())
	var c billing.NetworkLinkChallenge
	require.NoError(w.t, json.Unmarshal(r.Body.Bytes(), &c))
	require.True(w.t, strings.HasPrefix(c.ID, "lnc_"))
	require.WithinDuration(w.t, w.now().Add(network.ChallengeTTL), c.ExpiresAt, 10*time.Second)
	return c
}

// signIn signs u in afresh at a merchant's client, answering nonce.
func (w *world) signIn(u authtest.User, c iam.OAuthClientCreated, nonce, resource string, scopes ...string) authtest.OAuthTokens {
	w.t.Helper()
	return w.as.AuthorizeAs(w.t, authtest.SignIn(w.t, w.srv.AuthKit(), u), authtest.CodeFlow{ClientID: c.ClientID, ClientSecret: *c.ClientSecret,
		RedirectURI: merchantReturn, Resource: resource, Scopes: scopes, Nonce: nonce, Consent: true})
}

// paymentPageSignIn signs u in afresh on the payment page.
func (w *world) paymentPageSignIn(u authtest.User, nonce string) authtest.OAuthTokens {
	w.t.Helper()
	return w.as.AuthorizeAs(w.t, authtest.SignIn(w.t, w.srv.AuthKit(), u), authtest.CodeFlow{ClientID: network.PaymentPageClientID,
		RedirectURI: paymentPageReturn, Scopes: []string{"openid", "email"}, Nonce: nonce})
}

func (w *world) redeem(authorization, challenge, idToken string) *httptest.ResponseRecorder {
	w.t.Helper()
	return w.me(http.MethodPost, "/network-link", authorization, billing.CreateNetworkLinkParams{ChallengeID: challenge, IDToken: idToken})
}

// connect links the customer authorization proves to u from the merchant's
// site: a challenge, u's fresh sign-in at the merchant's client, the redeem.
func (w *world) connect(authorization string, c iam.OAuthClientCreated, u authtest.User) billing.CustomerLink {
	w.t.Helper()
	ch := w.challenge(authorization)
	r := w.redeem(authorization, ch.ID, w.signIn(u, c, ch.Nonce, "", "openid", "email", network.ScopeLink).IDToken)
	require.Equal(w.t, http.StatusCreated, r.Code, r.Body.String())
	var link billing.CustomerLink
	require.NoError(w.t, json.Unmarshal(r.Body.Bytes(), &link))
	return link
}

func (w *world) refused(r *httptest.ResponseRecorder, status int, code, why string) {
	w.t.Helper()
	require.Equal(w.t, status, r.Code, "%s: %s", why, r.Body.String())
	require.Equal(w.t, code, errorCode(w.t, r), why)
}

// adminCustomer is the merchant's read of its customer, raw and decoded.
func (w *world) adminCustomer(key iam.APIKeyCreated, id string) (billing.Customer, map[string]json.RawMessage) {
	w.t.Helper()
	r := call(w.t, w.handler, key.Secret, http.MethodGet, "/v1/admin/customers/"+id, nil)
	require.Equal(w.t, http.StatusOK, r.Code, r.Body.String())
	var c billing.Customer
	require.NoError(w.t, json.Unmarshal(r.Body.Bytes(), &c))
	var raw map[string]json.RawMessage
	require.NoError(w.t, json.Unmarshal(r.Body.Bytes(), &raw))
	return c, raw
}

// events are the merchant's unacknowledged host events of kind.
func (w *world) events(key iam.APIKeyCreated, kind billing.HostEventType) []billing.HostEvent {
	w.t.Helper()
	r := call(w.t, w.handler, key.Secret, http.MethodGet, "/v1/app/host-events?type="+string(kind), nil)
	require.Equal(w.t, http.StatusOK, r.Code, r.Body.String())
	var page billing.ListPage[billing.HostEvent]
	require.NoError(w.t, json.Unmarshal(r.Body.Bytes(), &page))
	return page.Items
}

func (w *world) linkedEvent(key iam.APIKeyCreated, kind billing.HostEventType, customer string) *billing.CustomerNetworkHostEvent {
	w.t.Helper()
	for _, e := range w.events(key, kind) {
		if e.CustomerNetwork != nil && e.CustomerNetwork.CustomerID.String() == customer {
			return e.CustomerNetwork
		}
	}
	return nil
}

// A merchant with its own login links its customer to an openrails.dev
// account from its site: the customer's token asks for a challenge, the
// account signs in afresh at the merchant's client with openrails:link, and
// the redeem proves both sides at once. The merchant sees one stable id and
// how it was linked, finds its customer by it, and hears of it.
func TestCustomerLinkFromTheMerchantsSite(t *testing.T) {
	w := newWorld(t)
	sub := uuid.NewString()
	token := w.customer(w.shopLogin, sub, nil)
	require.Equal(t, http.StatusOK, w.me(http.MethodGet, "", token, nil).Code)
	ada := w.account(true)

	ch := w.challenge(token)
	idToken := w.signIn(ada, w.shopClient, ch.Nonce, "", "openid", "email", network.ScopeLink).IDToken
	r := w.redeem(token, ch.ID, idToken)
	require.Equal(t, http.StatusCreated, r.Code, r.Body.String())
	var link billing.CustomerLink
	require.NoError(t, json.Unmarshal(r.Body.Bytes(), &link))
	require.Equal(t, billing.CustomerLinkConnect, link.Method)
	require.Equal(t, ada.ID, link.AccountID.String())
	require.Equal(t, sub, link.CustomerID.String())
	require.Equal(t, w.shop, link.MerchantID)
	require.Equal(t, billing.CustomerLinkActive, link.Status)
	require.True(t, strings.HasPrefix(link.ID.String(), "lnk_"))

	w.refused(w.redeem(token, ch.ID, idToken), http.StatusBadRequest, billing.CodeNetworkLinkChallengeInvalid, "a challenge is spent once")
	again := w.challenge(token)
	r = w.redeem(token, again.ID, w.signIn(ada, w.shopClient, again.Nonce, "", "openid", network.ScopeLink).IDToken)
	require.Equal(t, http.StatusOK, r.Code, "linking the same pair again answers the link: %s", r.Body.String())
	var same billing.CustomerLink
	require.NoError(t, json.Unmarshal(r.Body.Bytes(), &same))
	require.Equal(t, link.ID, same.ID)

	customer, raw := w.adminCustomer(w.shopKey, sub)
	require.NotNil(t, customer.Network)
	require.Equal(t, ada.ID, customer.Network.AccountID.String())
	require.Equal(t, billing.CustomerLinkConnect, customer.Network.Method)
	var network map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw["network"], &network))
	require.Len(t, network, 3, "the merchant sees the account id, when and how: %s", raw["network"])
	for _, key := range []string{"account_id", "linked_at", "method"} {
		require.Contains(t, network, key)
	}

	lookup := func(key iam.APIKeyCreated, account string) []billing.Customer {
		r := call(t, w.handler, key.Secret, http.MethodGet, "/v1/app/customers?network_account_id="+url.QueryEscape(account), nil)
		require.Equal(t, http.StatusOK, r.Code, r.Body.String())
		var page billing.ListPage[billing.Customer]
		require.NoError(t, json.Unmarshal(r.Body.Bytes(), &page))
		return page.Items
	}
	found := lookup(w.shopKey, ada.ID)
	require.Len(t, found, 1)
	require.Equal(t, sub, found[0].ID.String())
	require.Empty(t, lookup(w.rivalKey, ada.ID), "another merchant learns nothing of the account")
	r = call(t, w.handler, w.shopKey.Secret, http.MethodGet, "/v1/app/customers", nil)
	w.refused(r, http.StatusBadRequest, billing.CodeInvalidQuery, "the lookup names an account")

	got, err := handle(w.t, w.srv, w.shop).ListNetworkCustomers(t.Context(), billing.NetworkCustomerListParams{NetworkAccountID: link.AccountID})
	require.NoError(t, err)
	require.Len(t, got.Items, 1, "the Go client finds it too")

	event := w.linkedEvent(w.shopKey, billing.HostEventCustomerNetworkLinked, sub)
	require.NotNil(t, event, "customer.network_linked")
	require.Equal(t, ada.ID, event.AccountID.String())
	require.Equal(t, billing.CustomerLinkConnect, *event.Method)
	require.Nil(t, event.RevokedBy)

	links, err := w.srv.CustomerLinks(t.Context(), link.AccountID)
	require.NoError(t, err)
	require.Len(t, links, 1, "the account's side lists the link")
}

// A merchant without its own login signs customers in with openrails.dev:
// the account itself is the customer, native to the merchant and linked from
// its first request. A token granted only openrails:link is no customer.
func TestCustomerLinkBySigningIn(t *testing.T) {
	w := newWorld(t)
	sam := w.account(true)
	tokens := w.signIn(sam, w.shopClient, "n-"+uuid.NewString(), resourceID, "openid", billing.ScopeSelf)
	r := w.me(http.MethodGet, "", "Bearer "+tokens.AccessToken, nil)
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())

	customer, _ := w.adminCustomer(w.shopKey, sam.ID)
	require.NotNil(t, customer.Network, "linked by its sign-in")
	require.Equal(t, sam.ID, customer.Network.AccountID.String())
	require.Equal(t, billing.CustomerLinkSignIn, customer.Network.Method)
	event := w.linkedEvent(w.shopKey, billing.HostEventCustomerNetworkLinked, sam.ID)
	require.NotNil(t, event)
	require.Equal(t, billing.CustomerLinkSignIn, *event.Method)
	require.Equal(t, http.StatusOK, w.me(http.MethodGet, "", "Bearer "+tokens.AccessToken, nil).Code, "and again")

	linkOnly := w.signIn(w.account(true), w.shopClient, "n-"+uuid.NewString(), resourceID, "openid", network.ScopeLink)
	w.refused(w.me(http.MethodGet, "", "Bearer "+linkOnly.AccessToken, nil), http.StatusForbidden, billing.CodePermissionRequired, "openrails:link alone acts as no customer")

	// One person is one customer per merchant: an account linked to the
	// merchant's own customer is not a second one.
	ada := w.account(true)
	w.connect(w.customer(w.shopLogin, uuid.NewString(), nil), w.shopClient, ada)
	native := w.signIn(ada, w.shopClient, "n-"+uuid.NewString(), resourceID, "openid", billing.ScopeSelf)
	w.refused(w.me(http.MethodGet, "", "Bearer "+native.AccessToken, nil), http.StatusConflict, billing.CodeNetworkAccountLinked, "a second customer for one account")
	r = call(t, w.handler, w.shopKey.Secret, http.MethodGet, "/v1/admin/customers/"+ada.ID, nil)
	require.Equal(t, http.StatusNotFound, r.Code, "none was made: %s", r.Body.String())
}

// The payment page (hosted or embedded) proves the merchant side by the order's
// pay URL: on the payment origin its secret asks for the challenge and
// redeems it, answered only by the payment page's client; the account must
// have accepted the network terms.
func TestCustomerLinkOnThePaymentPage(t *testing.T) {
	w := newWorld(t)
	shop := handle(w.t, w.srv, w.shop)
	key := "linked-" + uuid.NewString()[:8]
	product, err := shop.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: "Course " + key})
	require.NoError(t, err)
	price, err := shop.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: key, Currency: "USD", UnitAmount: 5_000_000})
	require.NoError(t, err)
	host := func(host, method, path, bearer string, body any) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		req := httptest.NewRequest(method, "https://"+host+path, strings.NewReader(string(raw)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearer)
		if path == "/v1/app/orders" {
			req.Header.Set("Idempotency-Key", uuid.NewString())
		}
		res := httptest.NewRecorder()
		w.handler.ServeHTTP(res, req)
		return res
	}
	// payURL is the secret of a pay URL the shop's backend made for a
	// customer of its own.
	payURL := func() (billing.Order, string) {
		t.Helper()
		sub := uuid.NewString()
		require.Equal(t, http.StatusOK, w.me(http.MethodGet, "", w.customer(w.shopLogin, sub, nil), nil).Code)
		r := host(serverHost, http.MethodPost, "/v1/app/orders", w.shopKey.Secret, map[string]any{"customer_id": sub,
			"lines": []map[string]any{{"price_id": price.ID}}, "pay_url": map[string]any{"success_url": "https://shop.e2e.test/thanks"}})
		require.Equal(t, http.StatusCreated, r.Code, r.Body.String())
		var o billing.Order
		require.NoError(t, json.Unmarshal(r.Body.Bytes(), &o))
		_, secret, ok := strings.Cut(*o.PayURL, "#")
		require.True(t, ok)
		return o, secret
	}
	challenge := func(secret string) billing.NetworkLinkChallenge {
		t.Helper()
		r := host(paymentHost, http.MethodPost, "/v1/me/network-link/challenges", secret, nil)
		require.Equal(t, http.StatusCreated, r.Code, r.Body.String())
		var c billing.NetworkLinkChallenge
		require.NoError(t, json.Unmarshal(r.Body.Bytes(), &c))
		return c
	}
	redeem := func(secret, challenge, idToken string) *httptest.ResponseRecorder {
		return host(paymentHost, http.MethodPost, "/v1/me/network-link", secret, billing.CreateNetworkLinkParams{ChallengeID: challenge, IDToken: idToken})
	}

	order, secret := payURL()
	r := host(paymentHost, http.MethodGet, "/v1/me/orders/"+order.ID.String(), secret, nil)
	require.Equal(t, http.StatusOK, r.Code, "the merchant's own login's customer pays on the payment origin: %s", r.Body.String())
	ch := challenge(secret)
	fay := w.account(true)
	r = redeem(secret, ch.ID, w.paymentPageSignIn(fay, ch.Nonce).IDToken)
	require.Equal(t, http.StatusCreated, r.Code, r.Body.String())
	var link billing.CustomerLink
	require.NoError(t, json.Unmarshal(r.Body.Bytes(), &link))
	require.Equal(t, billing.CustomerLinkPaymentPage, link.Method)
	require.Equal(t, fay.ID, link.AccountID.String())
	require.Equal(t, order.CustomerID, link.CustomerID, "the order's customer")
	customer, _ := w.adminCustomer(w.shopKey, order.CustomerID.String())
	require.NotNil(t, customer.Network)
	require.Equal(t, billing.CustomerLinkPaymentPage, customer.Network.Method)

	_, secret = payURL()
	ch = challenge(secret)
	w.refused(redeem(secret, ch.ID, w.signIn(w.account(true), w.shopClient, ch.Nonce, "", "openid", network.ScopeLink).IDToken),
		http.StatusBadRequest, billing.CodeNetworkLinkChallengeInvalid, "a merchant's client never answers a pay URL")
	token := w.customer(w.shopLogin, uuid.NewString(), nil)
	ch = w.challenge(token)
	w.refused(w.redeem(token, ch.ID, w.paymentPageSignIn(w.account(true), ch.Nonce).IDToken),
		http.StatusBadRequest, billing.CodeNetworkLinkChallengeInvalid, "the payment page's client answers only a pay URL")
	// The page's sign-in itself asks for the network terms.
	gus := w.account(false)
	dpop := authtest.NewDPoPKey(t)
	id := w.as.BeginAuthorization(t, authtest.CodeFlow{ClientID: network.PaymentPageClientID, RedirectURI: paymentPageReturn, Scopes: []string{"openid"},
		Nonce: "n-" + uuid.NewString(), DPoP: dpop}, "verifier-"+uuid.NewString(), "state")
	r = serve(w.handler, rsRequest{method: http.MethodPost, path: w.srv.AuthKit().APIBase() + "/oauth2/authorizations/" + url.PathEscape(id) + "/approve",
		authorization: "Bearer " + authtest.SignIn(t, w.srv.AuthKit(), gus).AccessToken})
	require.Equal(t, http.StatusConflict, r.Code, r.Body.String())
	require.Contains(t, r.Body.String(), "agreement_required", "an account that never accepted the network terms")
	r = host(paymentHost, http.MethodGet, paymentpage.SignInPath+"?code=c&state=s", "", nil)
	require.Equal(t, http.StatusOK, r.Code, "the page answers its sign-in's return")
	require.Contains(t, r.Header().Get("Content-Security-Policy"), "connect-src 'self' ")
	require.Regexp(t, `connect-src [^;]*`+regexp.QuoteMeta(w.as.URL), r.Header().Get("Content-Security-Policy"), "the page redeems its code at the server's AuthKit")
	r = host(serverHost, http.MethodPost, "/v1/me/network-link/challenges", secret, nil)
	require.Equal(t, http.StatusUnauthorized, r.Code, "a pay URL opens nothing on the API host: %s", r.Body.String())
}

// Nothing but both sides proven in one exchange links: not a stale sign-in,
// another merchant's credential or client, another challenge's nonce, a
// client without openrails:link, nor an email the customer shares with an
// account. One person is one customer per merchant, both ways.
func TestCustomerLinkRefusals(t *testing.T) {
	w := newWorld(t)
	sub := uuid.NewString()
	token := w.customer(w.shopLogin, sub, nil)
	require.Equal(t, http.StatusOK, w.me(http.MethodGet, "", token, nil).Code)
	rival := w.customer(w.rivalLogin, uuid.NewString(), nil)
	require.Equal(t, http.StatusOK, w.me(http.MethodGet, "", rival, nil).Code)
	bob := w.account(true)
	invalid := func(r *httptest.ResponseRecorder, why string) {
		t.Helper()
		w.refused(r, http.StatusBadRequest, billing.CodeNetworkLinkChallengeInvalid, why)
	}

	signedIn := authtest.SignIn(t, w.srv.AuthKit(), bob)
	time.Sleep(1100 * time.Millisecond)
	ch := w.challenge(token)
	stale := w.as.AuthorizeAs(t, signedIn, authtest.CodeFlow{ClientID: w.shopClient.ClientID, ClientSecret: *w.shopClient.ClientSecret,
		RedirectURI: merchantReturn, Scopes: []string{"openid", network.ScopeLink}, Nonce: ch.Nonce, Consent: true})
	invalid(w.redeem(token, ch.ID, stale.IDToken), "a sign-in older than the challenge")

	ch = w.challenge(token)
	invalid(w.redeem(rival, ch.ID, w.signIn(bob, w.shopClient, ch.Nonce, "", "openid", network.ScopeLink).IDToken), "another merchant's credential redeems the shop's challenge")
	ch = w.challenge(rival)
	invalid(w.redeem(rival, ch.ID, w.signIn(bob, w.shopClient, ch.Nonce, "", "openid", network.ScopeLink).IDToken), "the shop's client answers the rival's challenge")
	ch = w.challenge(token)
	invalid(w.redeem(token, ch.ID, w.signIn(bob, w.rivalClient, ch.Nonce, "", "openid", network.ScopeLink).IDToken), "an ID token for another merchant's client")
	first, second := w.challenge(token), w.challenge(token)
	invalid(w.redeem(token, second.ID, w.signIn(bob, w.shopClient, first.Nonce, "", "openid", network.ScopeLink).IDToken), "another challenge's nonce")
	ch = w.challenge(token)
	invalid(w.redeem(token, ch.ID, w.signIn(w.account(true), w.shopClient, ch.Nonce, "", "openid", "email").IDToken), "no openrails:link consent")
	ch = w.challenge(token)
	invalid(w.redeem(token, ch.ID, "not-an-id-token"), "not an ID token")
	invalid(w.redeem(token, "lnc_unknown", w.signIn(bob, w.shopClient, "n", "", "openid", network.ScopeLink).IDToken), "an unknown challenge")

	// An account sharing the customer's verified email is not its account.
	dora := w.account(true)
	twin := uuid.NewString()
	require.Equal(t, http.StatusOK, w.me(http.MethodGet, "", w.customer(w.shopLogin, twin, func(c jwt.MapClaims) {
		c["email"], c["email_verified"] = dora.Email, true
	}), nil).Code)
	customer, _ := w.adminCustomer(w.shopKey, twin)
	require.NotNil(t, customer.Contact)
	require.Equal(t, dora.Email, *customer.Contact.Email)
	require.Nil(t, customer.Network, "an email match links nothing")
	links, err := w.srv.CustomerLinks(t.Context(), billing.NetworkAccountID(uuid.MustParse(dora.ID)))
	require.NoError(t, err)
	require.Empty(t, links)

	// Conflicts both ways.
	w.connect(token, w.shopClient, bob)
	ch = w.challenge(token)
	w.refused(w.redeem(token, ch.ID, w.signIn(w.account(true), w.shopClient, ch.Nonce, "", "openid", network.ScopeLink).IDToken),
		http.StatusConflict, billing.CodeNetworkLinkConflict, "the customer is linked to another account")
	other := w.customer(w.shopLogin, uuid.NewString(), nil)
	require.Equal(t, http.StatusOK, w.me(http.MethodGet, "", other, nil).Code)
	ch = w.challenge(other)
	w.refused(w.redeem(other, ch.ID, w.signIn(bob, w.shopClient, ch.Nonce, "", "openid", network.ScopeLink).IDToken),
		http.StatusConflict, billing.CodeNetworkAccountLinked, "the account is linked to another customer here")
	w.connect(rival, w.rivalClient, bob)
	links, err = w.srv.CustomerLinks(t.Context(), billing.NetworkAccountID(uuid.MustParse(bob.ID)))
	require.NoError(t, err)
	require.Len(t, links, 2, "one customer at each merchant")
}

// subscribe buys a monthly plan as the customer: a card saved at the
// merchant's NMI PSP and a purchase paid with it.
func (w *world) subscribe(mid billing.MerchantID, authorization string) billing.SubscriptionID {
	w.t.Helper()
	ctx, shop := w.t.Context(), handle(w.t, w.srv, mid)
	hours := 720
	key := "members-" + uuid.NewString()[:8]
	product, err := shop.CreateProduct(ctx, billing.CreateProductParams{Key: key, DisplayName: "Members", Entitlements: []string{"content:" + key}})
	require.NoError(w.t, err)
	price, err := shop.CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-monthly", Currency: "USD", UnitAmount: 9_990_000, BillingIntervalHours: &hours, AccessDurationHours: &hours})
	require.NoError(w.t, err)
	psps, err := shop.ListPSPs(ctx, billing.PSPListParams{})
	require.NoError(w.t, err)
	ok := func(r *httptest.ResponseRecorder) map[string]any {
		w.t.Helper()
		require.Less(w.t, r.Code, 300, r.Body.String())
		out := map[string]any{}
		require.NoError(w.t, json.Unmarshal(r.Body.Bytes(), &out))
		return out
	}
	card := ok(w.me(http.MethodPost, "/payment-methods", authorization, map[string]any{"psp_id": psps.Items[0].ID, "token": w.gatewayOf(mid).Tokenize(nmimock.Card{Brand: "visa", Last4: "4242"}),
		"billing_details": map[string]any{"name": "Network Customer", "address": map[string]any{"postal_code": "10001", "country": "US"}}}))["id"]
	paid := ok(w.me(http.MethodPost, "/orders", authorization, map[string]any{"lines": []any{map[string]any{"price_id": price.ID}}, "expected_total": "9990000", "payment": map[string]any{"payment_method_id": card}}))
	require.Equal(w.t, "complete", paid["status"], "%v", paid)
	for _, raw := range ok(w.me(http.MethodGet, "/subscriptions", authorization, nil))["data"].([]any) {
		if sub := raw.(map[string]any); sub["price_id"] == price.ID.String() {
			id, err := billing.ParseSubscriptionID(sub["id"].(string))
			require.NoError(w.t, err)
			return id
		}
	}
	w.t.Fatal("no subscription to the plan")
	return billing.SubscriptionID{}
}

// Unlinking is refused while the customer's cards still pay subscriptions
// at that merchant, and so are deleting the account and withdrawing the
// merchant's consent. Once nothing charges, the account unlinks: the
// merchant's clients lose its consent, the customer and its subscriptions
// stay, and the merchant hears of it. A withdrawn consent, a deleted account
// and a deleted merchant end links too.
func TestUnlinkingCustomers(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	apiBase := w.srv.AuthKit().APIBase()
	sub := uuid.NewString()
	token := w.customer(w.shopLogin, sub, nil)
	subscription := w.subscribe(w.shop, token)
	ann := w.account(true)
	link := w.connect(token, w.shopClient, ann)
	account := link.AccountID

	_, err := w.srv.UnlinkCustomer(ctx, account, link.ID)
	require.ErrorIs(t, err, billing.ErrNetworkLinkInUse)
	require.ErrorIs(t, err, billing.ErrConflict)
	session := "Bearer " + authtest.SignIn(t, w.srv.AuthKit(), ann).AccessToken
	r := serve(w.handler, rsRequest{method: http.MethodDelete, path: apiBase + "/me/oauth-consents/" + url.PathEscape(w.shopClient.ClientID), authorization: session})
	require.Equal(t, http.StatusConflict, r.Code, "withdrawing the shop's consent unlinks it: %s", r.Body.String())
	require.Contains(t, r.Body.String(), "consent_revocation_refused")
	r = serve(w.handler, rsRequest{method: http.MethodDelete, path: apiBase + "/me", authorization: session})
	require.Equal(t, http.StatusConflict, r.Code, "deleting the account: %s", r.Body.String())
	require.Contains(t, r.Body.String(), "deletion_refused")

	canceled, err := handle(w.t, w.srv, w.shop).CancelSubscription(ctx, subscription, billing.CancelSubscriptionParams{Reason: "moving on", RevokeAccess: true})
	require.NoError(t, err)
	require.Equal(t, billing.SubscriptionCanceled, canceled.Status)
	unlinked, err := w.srv.UnlinkCustomer(ctx, account, link.ID)
	require.NoError(t, err)
	require.Equal(t, billing.CustomerLinkRevoked, unlinked.Status)
	require.Equal(t, billing.CustomerLinkRevokedByCustomer, *unlinked.RevokedBy)
	consents, err := w.srv.AuthKit().OAuthConsents(ctx, ann.ID)
	require.NoError(t, err)
	for _, c := range consents {
		require.NotEqual(t, w.shop.String(), c.GroupID, "the shop's clients lost the account's consent")
	}
	customer, _ := w.adminCustomer(w.shopKey, sub)
	require.Nil(t, customer.Network)
	subs, err := handle(w.t, w.srv, w.shop).ListSubscriptions(ctx, billing.SubscriptionListParams{CustomerID: billing.CustomerID(uuid.MustParse(sub))})
	require.NoError(t, err)
	require.Len(t, subs.Items, 1, "the customer keeps its subscriptions")
	event := w.linkedEvent(w.shopKey, billing.HostEventCustomerNetworkUnlinked, sub)
	require.NotNil(t, event, "customer.network_unlinked")
	require.Equal(t, billing.CustomerLinkRevokedByCustomer, *event.RevokedBy)
	again, err := w.srv.UnlinkCustomer(ctx, account, link.ID)
	require.NoError(t, err, "an ended link answers as it stands")
	require.Equal(t, billing.CustomerLinkRevoked, again.Status)
	_, err = w.srv.UnlinkCustomer(ctx, account, billing.CustomerLinkID(uuid.New()))
	require.ErrorIs(t, err, billing.ErrNotFound, "a link the account never held")

	gone := func(id string) func() bool {
		return func() bool {
			c, _ := w.adminCustomer(w.shopKey, id)
			return c.Network == nil
		}
	}
	// Withdrawing the merchant's consent in AuthKit unlinks it.
	sid := w.account(true)
	signed := w.signIn(sid, w.shopClient, "n-"+uuid.NewString(), resourceID, "openid", billing.ScopeSelf)
	require.Equal(t, http.StatusOK, w.me(http.MethodGet, "", "Bearer "+signed.AccessToken, nil).Code)
	session = "Bearer " + authtest.SignIn(t, w.srv.AuthKit(), sid).AccessToken
	r = serve(w.handler, rsRequest{method: http.MethodDelete, path: apiBase + "/me/oauth-consents/" + url.PathEscape(w.shopClient.ClientID), authorization: session})
	require.Equal(t, http.StatusNoContent, r.Code, r.Body.String())
	require.Eventually(t, gone(sid.ID), 30*time.Second, 100*time.Millisecond, "oauth_consent.revoked unlinks")

	// A deleted account's links end at its purge.
	kim := w.account(true)
	kept := uuid.NewString()
	w.connect(w.customer(w.shopLogin, kept, nil), w.shopClient, kim)
	_, err = w.srv.AuthKit().DeleteUsers(ctx, iam.SystemIdentity(), []string{kim.ID})
	require.NoError(t, err)
	_, err = w.srv.AuthKit().PurgeUsers(ctx, []string{kim.ID})
	require.NoError(t, err)
	require.Eventually(t, gone(kept), 30*time.Second, 100*time.Millisecond, "the purge revokes the account's links")
	event = w.linkedEvent(w.shopKey, billing.HostEventCustomerNetworkUnlinked, kept)
	require.NotNil(t, event)
	require.Equal(t, billing.CustomerLinkRevokedByAccountDeleted, *event.RevokedBy)

	// A merchant leaving ends its links: they are openrails.dev's.
	lee := w.account(true)
	w.connect(w.customer(w.rivalLogin, uuid.NewString(), nil), w.rivalClient, lee)
	_, err = w.srv.DeleteMerchant(ctx, w.rival)
	require.NoError(t, err)
	links, err := w.srv.CustomerLinks(ctx, billing.NetworkAccountID(uuid.MustParse(lee.ID)))
	require.NoError(t, err)
	require.Empty(t, links)
}

// unlinkedEvents counts the merchant's customer.network_unlinked events for
// customer.
func (w *world) unlinkedEvents(key iam.APIKeyCreated, customer string) (n int) {
	w.t.Helper()
	for _, e := range w.events(key, billing.HostEventCustomerNetworkUnlinked) {
		if e.CustomerNetwork != nil && e.CustomerNetwork.CustomerID.String() == customer {
			n++
		}
	}
	return n
}

// Unlinks racing to end one link, as the consent event an unlink raises can:
// the link ends once, by the customer, the merchant hears once, and every
// unlink answers the link as it stands. A row lock holds both unlinks at the
// link's update until both have read it active.
func TestUnlinksAnswerTheLinkAsItStands(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	sub := uuid.NewString()
	link := w.connect(w.customer(w.shopLogin, sub, nil), w.shopClient, w.account(true))
	// The hold and the probe have connections of their own: the server's
	// pool keeps every one it has for the unlinks.
	graph, _ := operator.Of(w.srv)
	dial := func() *pgx.Conn {
		cfg, err := pgxpool.ParseConfig(os.Getenv("OPENRAILS_E2E_DSN"))
		require.NoError(t, err)
		conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close(context.WithoutCancel(ctx)) })
		return conn
	}
	holding, probe := dial(), dial()
	hold, err := holding.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = hold.Rollback(context.WithoutCancel(ctx)) }()
	var holder int
	require.NoError(t, hold.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&holder))
	table := pgx.Identifier{graph.Config.Database.Schema, "customer_links"}.Sanitize()
	_, err = hold.Exec(ctx, "SELECT 1 FROM "+table+" WHERE merchant_id = $1 AND id = $2 FOR UPDATE", w.shop.UUID(), link.ID.UUID())
	require.NoError(t, err)

	type answer struct {
		link *billing.CustomerLink
		err  error
	}
	answers := make(chan answer, 2)
	for range 2 {
		go func() {
			l, err := w.srv.UnlinkCustomer(ctx, link.AccountID, link.ID)
			answers <- answer{l, err}
		}()
	}
	require.Eventually(t, func() bool {
		var waiting int
		err := probe.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity a WHERE $1 = ANY(pg_blocking_pids(a.pid))
			OR EXISTS (SELECT 1 FROM pg_stat_activity b WHERE b.pid = ANY(pg_blocking_pids(a.pid)) AND $1 = ANY(pg_blocking_pids(b.pid)))`, holder).Scan(&waiting)
		return err == nil && waiting == 2
	}, 30*time.Second, 20*time.Millisecond, "both unlinks wait at the link")
	require.NoError(t, hold.Commit(ctx))
	for range 2 {
		a := <-answers
		require.NoError(t, a.err)
		require.Equal(t, billing.CustomerLinkRevoked, a.link.Status)
		require.Equal(t, billing.CustomerLinkRevokedByCustomer, *a.link.RevokedBy)
	}
	require.Equal(t, 1, w.unlinkedEvents(w.shopKey, sub), "the merchant hears once")
}

// An unlink that ended the link but left a consent behind is finished by
// unlinking again. Here withdrawing one of the merchant's clients' consents
// ends the link while another client still holds the account's consent; the
// account's unlink then revokes that one too. A link made since keeps its
// consents.
func TestUnlinkingAgainFinishesTheUnlink(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	sub := uuid.NewString()
	token := w.customer(w.shopLogin, sub, nil)
	ann := w.account(true)
	link := w.connect(token, w.shopClient, ann)
	app := w.client(w.shop)
	w.signIn(ann, app, "n-"+uuid.NewString(), "", "openid", "email")
	held := func(clientID string) bool {
		consents, err := w.srv.AuthKit().OAuthConsents(ctx, ann.ID)
		require.NoError(t, err)
		for _, c := range consents {
			if c.ClientID == clientID {
				return true
			}
		}
		return false
	}
	require.True(t, held(app.ClientID))

	require.NoError(t, w.srv.AuthKit().RevokeConsent(ctx, ann.ID, w.shopClient.ClientID))
	require.Eventually(t, func() bool { c, _ := w.adminCustomer(w.shopKey, sub); return c.Network == nil }, 30*time.Second, 100*time.Millisecond,
		"oauth_consent.revoked ends the link")
	require.True(t, held(app.ClientID), "the merchant's other client still holds the account's consent")

	unlinked, err := w.srv.UnlinkCustomer(ctx, link.AccountID, link.ID)
	require.NoError(t, err)
	require.Equal(t, billing.CustomerLinkRevoked, unlinked.Status)
	require.Equal(t, billing.CustomerLinkRevokedByCustomer, *unlinked.RevokedBy)
	require.False(t, held(app.ClientID), "unlinking again revoked it")
	require.Equal(t, 1, w.unlinkedEvents(w.shopKey, sub))

	relinked := w.connect(token, w.shopClient, ann)
	require.NotEqual(t, link.ID, relinked.ID)
	_, err = w.srv.UnlinkCustomer(ctx, link.AccountID, link.ID)
	require.NoError(t, err)
	require.True(t, held(w.shopClient.ClientID), "the new link keeps its consent")
	links, err := w.srv.CustomerLinks(ctx, link.AccountID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	require.Equal(t, relinked.ID, links[0].ID)
}

// A revoked link is kept 24 months, then the cleanup deletes it.
func TestRevokedLinksAreDeletedAfterRetention(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	sub := uuid.NewString()
	link := w.connect(w.customer(w.shopLogin, sub, nil), w.shopClient, w.account(true))
	_, err := w.srv.UnlinkCustomer(ctx, link.AccountID, link.ID)
	require.NoError(t, err)
	graph, _ := operator.Of(w.srv)
	count := func() (n int) {
		require.NoError(t, graph.Runtime.DB.DataPool().QueryRow(ctx, "SELECT count(*) FROM billing.customer_links WHERE merchant_id = $1", w.shop.UUID()).Scan(&n))
		return n
	}
	require.Equal(t, 1, count())
	sweep := func(at time.Time) {
		_, err := cleanupWorker(graph, at).Sweep(context.WithoutCancel(ctx))
		require.NoError(t, err)
	}
	sweep(time.Now().Add(700 * 24 * time.Hour))
	require.Equal(t, 1, count(), "kept within 24 months")
	sweep(time.Now().Add(740 * 24 * time.Hour))
	require.Zero(t, count(), "deleted after them")
}

// cleanupWorker is the retention sweep at a clock fixed at at.
func cleanupWorker(graph *app.App, at time.Time) riverjobs.CleanupExpiredDataWorker {
	return riverjobs.CleanupExpiredDataWorker{DB: graph.Runtime.DB, Clock: clockwork.NewFakeClockAt(at), Config: riverjobs.DefaultCleanupConfig()}
}

// Challenges live in Redis, which every instance shares: one minted on one
// instance is redeemed on another. In memory they serve one instance only.
func TestLinkChallengesCrossInstancesThroughRedis(t *testing.T) {
	w := newWorld(t)
	ctx := t.Context()
	addr := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_REDIS_ADDR"))
	if addr == "" {
		t.Fatal("OPENRAILS_E2E_REDIS_ADDR must point at a disposable Redis")
	}
	graph, _ := operator.Of(w.srv)
	instance := func(rdb redis.UniversalClient) *network.Service {
		s, err := network.New(graph.Runtime.DB, w.srv.AuthKit(), abusestate.New(rdb), nil)
		require.NoError(t, err)
		return s
	}
	shared := func() *redis.Client {
		rdb := redis.NewClient(&redis.Options{Addr: addr})
		t.Cleanup(func() { _ = rdb.Close() })
		return rdb
	}
	proof := func() customernetwork.Proof {
		sub := uuid.NewString()
		require.Equal(t, http.StatusOK, w.me(http.MethodGet, "", w.customer(w.shopLogin, sub, nil), nil).Code)
		return customernetwork.Proof{Merchant: w.shop, Customer: billing.CustomerID(uuid.MustParse(sub)), Credential: customernetwork.PayURL, Order: billing.OrderID(uuid.New())}
	}
	redeem := func(a, b *network.Service) error {
		p := proof()
		ch, err := a.Challenge(ctx, p)
		require.NoError(t, err)
		_, _, err = b.Link(ctx, p, billing.CreateNetworkLinkParams{ChallengeID: ch.ID, IDToken: w.paymentPageSignIn(w.account(true), ch.Nonce).IDToken})
		return err
	}
	require.NoError(t, redeem(instance(shared()), instance(shared())), "minted on one instance, redeemed on another")
	err := redeem(instance(nil), instance(nil))
	var coded interface{ ErrorCode() string }
	require.ErrorAs(t, err, &coded)
	require.Equal(t, billing.CodeNetworkLinkChallengeInvalid, coded.ErrorCode(), "another instance's memory holds no challenge")
}

// New network terms: an account that lets a merchant's client sign it in is
// that merchant's customer at once, and linked when it accepts them, by the
// consent it gave the client, never by a match on the merchant's records.
func TestNativeCustomersLinkWhenTheyAcceptNewTerms(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	var served atomic.Pointer[http.Handler]
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := served.Load(); h != nil {
			(*h).ServeHTTP(w, r)
			return
		}
		http.Error(w, "starting", http.StatusServiceUnavailable)
	}))
	t.Cleanup(ts.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	source, err := hostKeys.StaticFromPEM("e2e-terms", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})), nil)
	require.NoError(t, err)
	keys, totp := t.TempDir(), make([]byte, 32)
	_, _ = rand.Read(totp)
	require.NoError(t, os.WriteFile(filepath.Join(keys, "totp.key"), []byte(hex.EncodeToString(totp)), 0o600))
	terms := func(version string) func(*server.Config, *server.Deps) {
		return func(cfg *server.Config, deps *server.Deps) {
			cfg.Auth.Token.Issuer = ts.URL
			cfg.Auth.Resource = authkit.ResourceConfig{ID: resourceID, PublicURL: rsOrigin}
			cfg.Auth.Agreements = []authkit.AgreementConfig{{Key: network.TermsAgreement, Version: version, URL: "https://openrails.e2e.test/terms", Reaccept: true}}
			cfg.Auth.Keys = authkit.KeysConfig{Path: keys}
			deps.Auth.KeySource = source
		}
	}

	before := f.newServer(t, terms("2026-10"))
	handler, err := standaloneHandler(before)
	require.NoError(t, err)
	served.Store(&handler)
	shop := provision(t, before, uniqueName("terms-shop"))
	c, err := before.AuthKit().CreateGroupOAuthClient(ctx, iam.SystemIdentity(), iam.GroupByID(shop.String()), iam.NewOAuthClient{
		ClientName: "Sign in", RedirectURIs: []string{merchantReturn}, TokenEndpointAuthMethod: iam.OAuthClientSecretBasic, Scope: "openid openrails:self"})
	require.NoError(t, err)
	sam := newAccount(t, before)
	require.NoError(t, before.AuthKit().AcceptAgreements(ctx, sam.ID, []iam.AgreementRef{{Key: network.TermsAgreement, Version: "2026-10"}}))
	tokens := authtest.Attach(t, before.AuthKit(), ts).AuthorizeAs(t, authtest.SignIn(t, before.AuthKit(), sam), authtest.CodeFlow{ClientID: c.ClientID,
		ClientSecret: *c.ClientSecret, RedirectURI: merchantReturn, Resource: resourceID, Scopes: []string{"openid", billing.ScopeSelf}, Nonce: "n-" + uuid.NewString(), Consent: true})
	require.NoError(t, before.Close(ctx))

	after := f.newServer(t, terms("2026-11"))
	handler, err = standaloneHandler(after)
	require.NoError(t, err)
	served.Store(&handler)
	require.NoError(t, after.Start(ctx), "River delivers the acceptance")
	r := serve(handler, rsRequest{path: "/v1/me", authorization: "Bearer " + tokens.AccessToken})
	require.Equal(t, http.StatusOK, r.Code, r.Body.String())
	owner := merchantKey(t, after, shop, "owner")
	linked := func() *billing.CustomerNetwork {
		r := call(t, handler, owner.Secret, http.MethodGet, "/v1/admin/customers/"+sam.ID, nil)
		require.Equal(t, http.StatusOK, r.Code, r.Body.String())
		var c billing.Customer
		require.NoError(t, json.Unmarshal(r.Body.Bytes(), &c))
		return c.Network
	}
	require.Nil(t, linked(), "the new terms are due: a customer, not yet linked")
	require.NoError(t, after.AuthKit().AcceptAgreements(ctx, sam.ID, []iam.AgreementRef{{Key: network.TermsAgreement, Version: "2026-11"}}))
	require.Eventually(t, func() bool { n := linked(); return n != nil && n.Method == billing.CustomerLinkSignIn }, 30*time.Second, 100*time.Millisecond,
		"user.agreement_accepted links the merchants whose client the account let act as itself")
}

// The payment page's client is the server's: an operator declaring it is
// refused at boot.
func TestTheServerDeclaresThePaymentPageClient(t *testing.T) {
	f := newFixture(t)
	_, err := f.buildServer(t, func(cfg *server.Config, deps *server.Deps) {
		cfg.Auth.Resource = authkit.ResourceConfig{ID: resourceID, PublicURL: rsOrigin}
		cfg.Auth.Agreements = []authkit.AgreementConfig{{Key: network.TermsAgreement, Version: networkTerms, URL: "https://openrails.e2e.test/terms"}}
		cfg.Auth.AuthorizationServer.Clients = []authkit.OAuthClientConfig{{ID: network.PaymentPageClientID, RedirectURIs: []string{paymentPageReturn}}}
		cfg.Engine.PublicBillingBaseURL = "https://" + serverHost
		cfg.PaymentOrigin = "https://" + paymentHost
		deps.PaymentPageAssets = paymentPage
	})
	require.ErrorContains(t, err, "declares "+network.PaymentPageClientID)
}
