//go:build e2e && integration

package ci_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authtest"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/open-rails/openrails/server"
)

// ownerAuth is a hosting platform's Auth for its own billing surface,
// /api/v1/merchants/{slug}/billing/me: the signed-in owner of the hosted
// merchant {slug} acts as that merchant, the platform's customer. Anyone else
// is refused.
type ownerAuth struct {
	openrails.Auth
	owners map[string]hosted
}

type hosted struct {
	owner string
	id    billing.MerchantID
}

type hostedCustomerKey struct{}

func (a ownerAuth) Required() func(http.Handler) http.Handler {
	required := a.Auth.Required()
	return func(next http.Handler) http.Handler {
		return required(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, _ := a.Auth.Identity(r.Context())
			merchant, ok := a.owners[r.PathValue("slug")]
			if !ok || merchant.owner != user.Subject {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			customer := user
			customer.Subject = merchant.id.String()
			customer.Invoker = openrails.Invoker{Issuer: user.Issuer, ID: customer.Subject}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), hostedCustomerKey{}, customer)))
		}))
	}
}

func (a ownerAuth) Identity(ctx context.Context) (openrails.Identity, bool) {
	who, ok := ctx.Value(hostedCustomerKey{}).(openrails.Identity)
	return who, ok
}

// A customer whose identity a host's customer surface defines pays a Stripe
// Elements checkout session with its saved card on that surface: the
// surface's Auth is the customer's proof. The session id alone still cannot
// spend the card, and no other owner, user, merchant or surface can pay it.
func TestServerProfilePaysCheckoutAsItsCustomer(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	stripe := stripemock.NewUnstarted(stripemock.Options{})
	ctx := t.Context()
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	srv := f.newServer(t, func(cfg *server.Config, deps *server.Deps) {
		cfg.Engine.ProviderWriteMode = openrails.ProviderWritesFull
		cfg.Engine.SecretBackend = openrails.SecretBackendDB
		cfg.Engine.Encryption = &openrails.EncryptionConfig{MasterKey: base64.StdEncoding.EncodeToString(key)}
		cfg.Engine.TrustedProxies = []string{"127.0.0.1/32"}
		cfg.Auth.DirectPeerIP = false
		deps.Engine.StripeTransport = stripe
	})
	engine := srv.Client()
	auth := srv.AuthKit()

	// The platform sells Pro to its hosted merchants; another merchant sells
	// on its own.
	type seller struct {
		id    billing.MerchantID
		slug  string
		price billing.PriceID
	}
	sell := func(name, account string) seller {
		slug := name + "-" + uuid.NewString()[:8]
		m, err := srv.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: slug})
		require.NoError(t, err)
		at := openrails.ForMerchantID(m.MerchantID)
		_, err = engine.CreatePSP(ctx, billing.CreatePSPParams{OperationID: uuid.New(), Key: "stripe", Rail: billing.RailStripe, AccountID: account,
			Settings:    map[string]any{"publishable_key": "pk_test_e2e"},
			Credentials: map[string]string{"secret_key": "sk_test_" + account, "webhook_signing_secret": whsecStripe}}, at)
		require.NoError(t, err)
		product, err := engine.CreateProduct(ctx, billing.CreateProductParams{Key: "pro", DisplayName: "Pro", Entitlements: []string{"platform:pro"}}, at)
		require.NoError(t, err)
		hours := monthHours
		price, err := engine.CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: "pro-usd", UnitAmount: 29_000_000, Currency: "USD", BillingIntervalHours: &hours, AccessDurationHours: &hours}, at)
		require.NoError(t, err)
		return seller{m.MerchantID, slug, price.ID}
	}
	platform, other := sell("platform", "acct_platform"), sell("other", "acct_other")

	// Two hosted merchants, each with its owner, and a user who owns none.
	olivia, oscar, mallory := authtest.NewUser(t, auth), authtest.NewUser(t, auth), authtest.NewUser(t, auth)
	owners := map[string]hosted{}
	host := func(name string, owner authtest.User) (string, billing.MerchantID) {
		slug := name + "-" + uuid.NewString()[:8]
		m, err := srv.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: slug, OwnerUserID: owner.ID})
		require.NoError(t, err)
		owners[slug] = hosted{owner: owner.ID, id: m.MerchantID}
		return slug, m.MerchantID
	}
	acme, acmeID := host("acme", olivia)
	rival, _ := host("rival", oscar)
	token := func(u authtest.User) string { return authtest.SignIn(t, auth, u).AccessToken }
	oliviaToken, oscarToken, malloryToken := token(olivia), token(oscar), token(mallory)

	routes, err := srv.Routes(
		openrails.CustomerRoutes{Prefix: "/api/v1/merchants/{slug}/billing/me", Merchant: platform.slug, Auth: ownerAuth{Auth: auth, owners: owners}},
		openrails.CustomerRoutes{Prefix: "/billing/v1/me", Auth: auth},
	)
	require.NoError(t, err)
	mux := http.NewServeMux()
	for _, r := range routes {
		mux.Handle(r.Method+" "+r.Path, r.Handler)
	}
	web := httptest.NewServer(mux)
	t.Cleanup(web.Close)
	address := 0
	call := func(token, method, path, selector string, body any) (int, map[string]any) {
		t.Helper()
		var data io.Reader
		if body != nil {
			raw, err := json.Marshal(body)
			require.NoError(t, err)
			data = bytes.NewReader(raw)
		}
		req, err := http.NewRequestWithContext(ctx, method, web.URL+path, data)
		require.NoError(t, err)
		address++
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", address%250+1))
		req.Header.Set("Content-Type", "application/json")
		if method == http.MethodPost {
			req.Header.Set("Idempotency-Key", uuid.NewString())
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if selector != "" {
			req.Header.Set("OpenRails-Merchant", selector)
		}
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		out := map[string]any{}
		if len(raw) > 0 && strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
			require.NoError(t, json.Unmarshal(raw, &out), "%s %s: %s", method, path, raw)
		}
		return res.StatusCode, out
	}
	surface := func(slug string) string { return "/api/v1/merchants/" + slug + "/billing/me" }
	refused := func(status int, out map[string]any, want int, code, what string) {
		t.Helper()
		require.Equal(t, want, status, "%s: %v", what, out)
		if code != "" {
			require.Equal(t, code, hostedErrorCode(out), "%s: %v", what, out)
		}
	}

	// Olivia saves a card for Acme on the platform's surface.
	status, setup := call(oliviaToken, http.MethodPost, surface(acme)+"/payment-method-setups", "", map[string]any{"psp_id": pspOf(t, engine, platform.id), "consent": true})
	require.Equal(t, http.StatusOK, status, "%v", setup)
	setup = unwrap(setup)
	stripe.CompleteSetup(strings.TrimSuffix(setup["client_secret"].(string), "_secret_gf"), visa)
	status, confirmed := call(oliviaToken, http.MethodPost, surface(acme)+"/payment-method-setups/"+setup["id"].(string)+"/confirm", "", map[string]any{})
	require.Equal(t, http.StatusOK, status, "%v", confirmed)
	card := unwrap(confirmed)["payment_method_id"].(string)

	// The platform mints Acme's Pro session.
	mint := func(s seller, customer billing.CustomerID) string {
		link, err := engine.CreateCheckoutSession(ctx, billing.CreateCheckoutSessionParams{Customer: billing.CheckoutCustomerIdentity{ID: customer}, PriceID: s.price}, openrails.ForMerchantID(s.id))
		require.NoError(t, err)
		return link.ID
	}
	acmeCustomer := billing.CustomerID(acmeID.UUID())
	session := mint(platform, acmeCustomer)
	status, doc := call("", http.MethodGet, "/v1/checkout-sessions/"+session, "", nil)
	require.Equal(t, http.StatusOK, status, "%v", doc)
	require.Empty(t, doc["saved_methods"], "the capability alone sees no saved cards")
	option := ""
	for _, raw := range doc["options"].([]any) {
		if o := raw.(map[string]any); o["rail"] == "stripe" {
			require.Equal(t, "stripe_elements", o["driver"], "%v", o)
			option = o["id"].(string)
		}
	}
	require.NotEmpty(t, option)
	pay := map[string]any{"option_id": option, "payment_method_id": card}

	// The capability alone cannot spend Acme's card.
	status, out := call("", http.MethodPost, "/v1/checkout-sessions/"+session+"/pay", "", pay)
	refused(status, out, http.StatusForbidden, "customer_proof_required", "the anonymous capability")
	// Nor can a signed-in owner over the capability: the server's own Auth
	// knows the user, not the merchant it bills.
	status, out = call(oliviaToken, http.MethodPost, "/v1/checkout-sessions/"+session+"/pay", "", pay)
	require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, status, "%v", out)

	// Nobody else pays it on the platform's surface: another merchant's owner
	// on Acme's path or on their own, a user who owns nothing.
	for _, tc := range []struct {
		token, slug string
		status      int
		what        string
	}{
		{oscarToken, acme, http.StatusForbidden, "another merchant's owner on Acme's surface"},
		{malloryToken, acme, http.StatusForbidden, "a user who owns nothing"},
		{oscarToken, rival, http.StatusNotFound, "another merchant's owner as their own merchant"},
		{"", acme, http.StatusUnauthorized, "no credential"},
	} {
		status, out := call(tc.token, http.MethodGet, surface(tc.slug)+"/checkout-sessions/"+session, "", nil)
		require.Equal(t, tc.status, status, "read: %s: %v", tc.what, out)
		status, out = call(tc.token, http.MethodPost, surface(tc.slug)+"/checkout-sessions/"+session+"/pay", "", pay)
		require.Equal(t, tc.status, status, "pay: %s: %v", tc.what, out)
	}

	// Another merchant's session for the same customer is not on this
	// surface, which serves the platform.
	foreign := mint(other, acmeCustomer)
	status, out = call(oliviaToken, http.MethodGet, surface(acme)+"/checkout-sessions/"+foreign, "", nil)
	refused(status, out, http.StatusNotFound, "checkout_session_not_found", "another merchant's session")
	status, out = call(oliviaToken, http.MethodPost, surface(acme)+"/checkout-sessions/"+foreign+"/pay", "", pay)
	refused(status, out, http.StatusNotFound, "checkout_session_not_found", "paying another merchant's session")

	// On a surface serving the merchant each request selects, the session's
	// merchant must be the selected one.
	own := mint(platform, billing.CustomerID(uuid.MustParse(mallory.ID)))
	status, out = call(malloryToken, http.MethodGet, "/billing/v1/me/checkout-sessions/"+own, other.slug, nil)
	refused(status, out, http.StatusNotFound, "checkout_session_not_found", "a session of a merchant not selected")
	status, out = call(malloryToken, http.MethodGet, "/billing/v1/me/checkout-sessions/"+own, platform.slug, nil)
	require.Equal(t, http.StatusOK, status, "%v", out)
	status, out = call(oliviaToken, http.MethodGet, "/billing/v1/me/checkout-sessions/"+own, platform.slug, nil)
	refused(status, out, http.StatusNotFound, "checkout_session_not_found", "another customer's session")

	// Olivia, as Acme, sees and pays it with Acme's saved card.
	status, doc = call(oliviaToken, http.MethodGet, surface(acme)+"/checkout-sessions/"+session, "", nil)
	require.Equal(t, http.StatusOK, status, "%v", doc)
	saved := doc["saved_methods"].([]any)
	require.Len(t, saved, 1, "%v", doc)
	require.Equal(t, card, saved[0].(map[string]any)["id"])
	charges := len(stripe.Ledger(""))
	status, out = call(oliviaToken, http.MethodPost, surface(acme)+"/checkout-sessions/"+session+"/pay", "", pay)
	require.Equal(t, http.StatusOK, status, "%v", out)
	require.Equal(t, "succeeded", out["status"], "%v", out)
	require.Equal(t, charges+1, len(stripe.Ledger("")))
	subs, err := engine.ListSubscriptions(ctx, billing.SubscriptionListParams{CustomerID: acmeCustomer, Status: billing.SubscriptionActive}, openrails.ForMerchantID(platform.id))
	require.NoError(t, err)
	require.Len(t, subs.Items, 1)
	require.Equal(t, platform.price, subs.Items[0].PriceID)
	none, err := engine.ListSubscriptions(ctx, billing.SubscriptionListParams{CustomerID: acmeCustomer}, openrails.ForMerchantID(other.id))
	require.NoError(t, err)
	require.Empty(t, none.Items, "the other merchant's session stays unpaid")
	require.Empty(t, stripe.Unexpected(), "Stripe calls the mock does not model")
}

// pspOf is the merchant's one PSP.
func pspOf(t *testing.T, engine *openrails.Client, merchant billing.MerchantID) billing.PSPID {
	t.Helper()
	psps, err := engine.ListPSPs(t.Context(), billing.PSPListParams{}, openrails.ForMerchantID(merchant))
	require.NoError(t, err)
	require.Len(t, psps.Items, 1)
	return psps.Items[0].ID
}

// unwrap is a response's data member, or the response.
func unwrap(v map[string]any) map[string]any {
	if data, ok := v["data"].(map[string]any); ok {
		return data
	}
	return v
}

// hostedErrorCode is an error response's code.
func hostedErrorCode(body map[string]any) string {
	detail, _ := body["error"].(map[string]any)
	code, _ := detail["code"].(string)
	return code
}
