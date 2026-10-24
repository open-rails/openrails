package serverfixture_test

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/keys"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/server"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type config struct {
	Schema, Ready, ControlKey, FXURL, RedisURL string
	At                                         time.Time
}
type merchant struct {
	ID            billing.MerchantID
	Key           string
	CustomerID    billing.CustomerID
	CustomerToken string
	OAuth         iam.OAuthClientCreated
}
type ready struct {
	URL, ControlURL                      string
	AliceID, BobID, AliceToken, BobToken string
	Merchants                            map[string]merchant
}
type command struct {
	Op, Merchant, Account, Nonce string
	Scopes                       []string
	ID                           string
	Body                         json.RawMessage
	reply                        chan answer
}
type answer struct {
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}
type wire struct {
	target *url.URL
	base   http.RoundTripper
}

func (w wire) RoundTrip(r *http.Request) (*http.Response, error) {
	c := r.Clone(r.Context())
	c.URL.Scheme, c.URL.Host, c.Host = w.target.Scheme, w.target.Host, w.target.Host
	return w.base.RoundTrip(c)
}

// Only a parent critical scenario launches this owned process. Setup uses
// the shipped Server, AuthKit, and merchant client interfaces exclusively.
func TestServerFixture(t *testing.T) {
	path := os.Getenv("OPENRAILS_CRITICAL_SERVER_CONFIG")
	if path == "" {
		t.Fatal("server fixture needs an owned parent configuration")
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var cfg config
	require.NoError(t, json.Unmarshal(data, &cfg))
	pool, err := pgxpool.New(t.Context(), os.Getenv("OPENRAILS_CRITICAL_DSN"))
	require.NoError(t, err)
	defer pool.Close()
	var current atomic.Pointer[http.Handler]
	calls := make(chan command)
	controlPath := "/__critical/" + cfg.ControlKey
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == controlPath {
			var cmd command
			if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&cmd) != nil {
				http.Error(w, "invalid fixture command", http.StatusBadRequest)
				return
			}
			cmd.reply = make(chan answer, 1)
			select {
			case calls <- cmd:
			case <-r.Context().Done():
				return
			}
			select {
			case result := <-cmd.reply:
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(result)
			case <-r.Context().Done():
			}
			return
		}
		if h := current.Load(); h != nil {
			(*h).ServeHTTP(w, r)
			return
		}
		http.Error(w, "fixture starting", http.StatusServiceUnavailable)
	}))
	defer httpServer.Close()
	fx, err := url.Parse(cfg.FXURL)
	require.NoError(t, err)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := keys.SignerFromKey("standalone-fixture", key)
	require.NoError(t, err)
	clock := clockwork.NewFakeClockAt(cfg.At)
	var shared redis.UniversalClient
	if cfg.RedisURL != "" {
		options, err := redis.ParseURL(cfg.RedisURL)
		require.NoError(t, err)
		shared = redis.NewClient(options)
		defer shared.Close()
		require.NoError(t, shared.Ping(t.Context()).Err())
	}
	srv, err := server.New(t.Context(), server.Config{
		Engine: openrails.Config{Database: openrails.DatabaseConfig{Schema: cfg.Schema, RiverSchema: cfg.Schema}, PublicBillingBaseURL: httpServer.URL, ProviderWriteMode: openrails.ProviderWritesFull, RateLimitsDisabled: true, CatalogReconciliationInterval: "0"},
		Auth: authkit.Config{
			DefaultTenantID: iam.NewTenantID(), Database: authkit.DatabaseConfig{Schema: cfg.Schema + "_auth", RiverSchema: cfg.Schema},
			Token: authkit.TokenConfig{Issuer: httpServer.URL, AllowPrivateNetworkJWKS: true}, Resource: authkit.ResourceConfig{ID: httpServer.URL, PublicURL: httpServer.URL},
			Registration: authkit.RegistrationConfig{AllowMissingSenders: true}, TwoFactor: authkit.TwoFactorConfig{Mode: iam.TwoFactorDisabled},
			Agreements: []authkit.AgreementConfig{{Key: "network-terms", Version: "critical-v1", URL: "https://critical.invalid/network-terms"}},
			HTTP:       &authkit.HTTPConfig{DirectPeerIP: true},
		},
		LocalSignIn: true, AdminConsole: &server.AdminConsole{Disabled: true}, RouteGroups: openrails.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true, Programmatic: true},
	}, server.Deps{Engine: openrails.Deps{Postgres: pool, Redis: shared, Clock: clock, FXTransport: wire{fx, transport}}, Auth: authkit.Deps{Postgres: pool, KeySource: keys.Static{Active: signer, Public: map[string]crypto.PublicKey{signer.KID(): signer.Public()}}}})
	require.NoError(t, err)
	defer srv.Close(context.Background())
	handler := srv.Handler()
	current.Store(&handler)
	ak := srv.AuthKit()
	users := map[string]authtest.User{"alice": authtest.NewUser(t, ak), "bob": authtest.NewUser(t, ak)}
	for _, user := range users {
		require.NoError(t, ak.AcceptAgreements(t.Context(), user.ID, []iam.AgreementRef{{Key: "network-terms", Version: "critical-v1"}}))
	}
	alice := authtest.SignIn(t, ak, users["alice"])
	bob := authtest.SignIn(t, ak, users["bob"])
	as := authtest.Attach(t, ak, httpServer)
	out := ready{URL: httpServer.URL, ControlURL: httpServer.URL + controlPath, AliceID: users["alice"].ID, BobID: users["bob"].ID, AliceToken: alice.AccessToken, BobToken: bob.AccessToken, Merchants: map[string]merchant{}}
	clients := map[string]*openrails.Client{}
	for _, slug := range []string{"alpha", "beta"} {
		created, err := srv.ProvisionMerchant(t.Context(), billing.ProvisionMerchantParams{Slug: slug})
		require.NoError(t, err)
		group := iam.GroupByID(created.GroupID)
		owner, ok := server.MerchantRole("owner")
		require.True(t, ok)
		apiKey, err := ak.CreateAPIKey(t.Context(), iam.SystemIdentity(), group, iam.NewAPIKey{Name: "critical backend", Role: owner})
		require.NoError(t, err)
		_, assertionKey, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		der, err := x509.MarshalPKIXPublicKey(assertionKey.Public())
		require.NoError(t, err)
		issuer := "https://" + slug + ".critical.invalid"
		app, err := ak.UpsertRemoteApplication(t.Context(), iam.SystemIdentity(), group, iam.RemoteApplication{Issuer: issuer, Mode: iam.RemoteApplicationModeStatic, PublicKeys: []iam.RemoteApplicationKey{{KID: "merchant-key", PublicKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))}}, Enabled: true})
		require.NoError(t, err)
		_, err = ak.SetGroupRole(t.Context(), iam.SystemIdentity(), group, iam.RemoteApplicationSubject(app.ID), owner)
		require.NoError(t, err)
		registered, err := ak.CreateGroupOAuthClient(t.Context(), iam.SystemIdentity(), group, iam.NewOAuthClient{ClientName: slug + " sign-in", RedirectURIs: []string{"https://" + slug + ".critical.invalid/callback"}, TokenEndpointAuthMethod: iam.OAuthClientSecretBasic, Scope: "openid openrails:self openrails:link"})
		require.NoError(t, err)
		remote, err := openrails.NewRemote(httpServer.URL, openrails.RemoteOptions{Credentials: map[billing.MerchantID]openrails.RemoteCredential{created.MerchantID: {APIKey: apiKey.Secret, Assertion: &openrails.AssertionKey{Issuer: issuer, KeyID: "merchant-key", Signer: assertionKey, TokenURL: httpServer.URL + iam.OAuthTokenPath}}}})
		require.NoError(t, err)
		defer remote.Close(context.Background())
		client, err := remote.Merchant(created.MerchantID)
		require.NoError(t, err)
		customer := billing.CustomerID(uuid.New())
		token, err := client.CustomerToken(t.Context(), billing.CustomerTokenParams{CustomerID: customer, Email: slug + "@critical.invalid", EmailVerified: true})
		require.NoError(t, err)
		out.Merchants[slug] = merchant{ID: created.MerchantID, Key: apiKey.Secret, CustomerID: customer, CustomerToken: token.AccessToken, OAuth: registered}
		clients[slug] = client
	}
	data, err = json.Marshal(out)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cfg.Ready, data, 0600))
	for {
		select {
		case <-t.Context().Done():
			return
		case cmd := <-calls:
			result := answer{}
			switch cmd.Op {
			case "authorize":
				shop, ok := out.Merchants[cmd.Merchant]
				require.True(t, ok)
				user, ok := users[cmd.Account]
				require.True(t, ok)
				scopes := cmd.Scopes
				if len(scopes) == 0 {
					scopes = []string{"openid", "openrails:link"}
				}
				result.Result = as.Authorize(t, user, authtest.CodeFlow{ClientID: shop.OAuth.ClientID, ClientSecret: *shop.OAuth.ClientSecret, RedirectURI: "https://" + cmd.Merchant + ".critical.invalid/callback", Resource: httpServer.URL, Scopes: scopes, Nonce: cmd.Nonce, Consent: true})
			case "stale-sign-in":
				// A freshly issued/renewed token may carry an older sign-in.
				// Preserve the real session and every authority claim, changing
				// only issuer-owned auth_time at this negative time boundary.
				raw := alice.AccessToken
				if cmd.Account == "bob" {
					raw = bob.AccessToken
				}
				claims := jwt.MapClaims{}
				original, _, err := jwt.NewParser().ParseUnverified(raw, claims)
				require.NoError(t, err)
				claims["auth_time"] = time.Now().Add(-16 * time.Minute).Unix()
				token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
				token.Header = original.Header
				signed, err := token.SignedString(key)
				require.NoError(t, err)
				result.Result = map[string]string{"access_token": signed}
			case "customer-token":
				id, err := billing.ParseCustomerID(cmd.ID)
				require.NoError(t, err)
				token, err := clients[cmd.Merchant].CustomerToken(t.Context(), billing.CustomerTokenParams{CustomerID: id})
				require.NoError(t, err)
				result.Result = token
			case "soft-delete":
				require.NoError(t, srv.Start(t.Context()))
				rows, err := ak.DeleteUsers(t.Context(), iam.SystemIdentity(), []string{cmd.ID})
				require.NoError(t, err)
				for _, row := range rows {
					require.NoError(t, row.Err)
				}
				user, err := ak.User(t.Context(), iam.UserByID(cmd.ID), authkit.IncludeDeleted())
				require.NoError(t, err)
				result.Result = user
			case "create-product":
				var p billing.CreateProductParams
				require.NoError(t, json.Unmarshal(cmd.Body, &p))
				value, err := clients[cmd.Merchant].CreateProduct(t.Context(), p)
				result.Result = value
				if err != nil {
					result.Error = err.Error()
				}
			default:
				result.Error = "unknown fixture command"
			}
			cmd.reply <- result
		}
	}
}
