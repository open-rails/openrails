//go:build integration

package server_test

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/keys"
	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	"github.com/open-rails/openrails/server"
)

func extractionOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func extractionDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	raw := os.Getenv("OPENRAILS_EXTRACTION_DSN")
	if raw == "" {
		t.Fatal("OPENRAILS_EXTRACTION_DSN must name an owned disposable PostgreSQL server")
	}
	admin, err := pgxpool.New(t.Context(), raw)
	extractionOK(t, err)
	name := "or_extract_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	extractionOK(t, err)
	uri, err := url.Parse(raw)
	extractionOK(t, err)
	uri.Path = "/" + name
	pool, err := pgxpool.New(t.Context(), uri.String())
	extractionOK(t, err)
	t.Cleanup(func() {
		pool.Close()
		_, err := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize())
		if err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	return pool
}

func TestHostedStateRemovalRefusesUntransferredState(t *testing.T) {
	pool := extractionDatabase(t)
	chain, err := migratekit.LoadFromFS(postgresmigrations.FS)
	extractionOK(t, err)
	var before []migratekit.Migration
	var removal migratekit.Migration
	for _, m := range chain {
		if migratekit.Prefix(m.Name) == "116" {
			removal = m
		} else {
			before = append(before, m)
		}
	}
	if removal.Name == "" {
		t.Fatal("missing removal migration")
	}
	migrator, err := migratekit.NewPostgresFromPGXPool(pool, "openrails")
	extractionOK(t, err)
	defer migrator.Close()
	extractionOK(t, migrator.WithSchema("billing").ApplyMigrations(t.Context(), before))
	for _, tc := range []struct{ name, setup string }{
		{"cursor state is not empty data", `INSERT INTO billing.network_event_cursors(issuer,tenant_id,cursor) VALUES('https://accounts.invalid','00000000-0000-4000-8000-000000000001','')`},
		{"partial legacy pair", `ALTER TABLE billing.customer_links RENAME TO saved_customer_links`},
		{"conflicting destination", `CREATE SCHEMA saas; CREATE TABLE saas.customer_links(id uuid); CREATE TABLE saas.network_event_cursors(cursor text)`},
		{"missing pair is not presumed empty", `DROP TABLE billing.customer_links; DROP TABLE billing.network_event_cursors`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := pool.Begin(t.Context())
			extractionOK(t, err)
			defer tx.Rollback(context.Background())
			_, err = tx.Exec(t.Context(), tc.setup)
			extractionOK(t, err)
			_, err = tx.Exec(t.Context(), removal.Content)
			if err == nil {
				t.Fatal("unsafe state accepted")
			}
			if !strings.Contains(err.Error(), "transfer") {
				t.Fatalf("unexpected refusal: %v", err)
			}
		})
	}
	// A second transaction holding a source table must be refused without waiting.
	t.Run("busy source", func(t *testing.T) {
		hold, err := pool.Begin(t.Context())
		extractionOK(t, err)
		defer hold.Rollback(context.Background())
		_, err = hold.Exec(t.Context(), "LOCK TABLE billing.network_event_cursors IN ACCESS SHARE MODE")
		extractionOK(t, err)
		tx, err := pool.Begin(t.Context())
		extractionOK(t, err)
		defer tx.Rollback(context.Background())
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		_, err = tx.Exec(ctx, removal.Content)
		if err == nil || errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "could not obtain lock") {
			t.Fatalf("expected immediate lock refusal, got %v", err)
		}
	})
	// A complete explicit schema move preserves nonempty opaque reader state.
	t.Run("transferred cursor preserved", func(t *testing.T) {
		tx, err := pool.Begin(t.Context())
		extractionOK(t, err)
		defer tx.Rollback(context.Background())
		_, err = tx.Exec(t.Context(), `INSERT INTO billing.network_event_cursors(issuer,tenant_id,cursor) VALUES('https://accounts.invalid','00000000-0000-4000-8000-000000000001','opaque-bound-filter'); CREATE SCHEMA saas; ALTER TABLE billing.customer_links SET SCHEMA saas; ALTER TABLE billing.network_event_cursors SET SCHEMA saas`)
		extractionOK(t, err)
		_, err = tx.Exec(t.Context(), removal.Content)
		extractionOK(t, err)
		var cursor string
		err = tx.QueryRow(t.Context(), "SELECT cursor FROM saas.network_event_cursors").Scan(&cursor)
		extractionOK(t, err)
		if cursor != "opaque-bound-filter" {
			t.Fatal(cursor)
		}
	})
	extractionOK(t, migrator.WithSchema("billing").ApplyMigrations(t.Context(), chain))
	extractionOK(t, migrator.WithSchema("billing").ApplyMigrations(t.Context(), chain))
	var absent bool
	extractionOK(t, pool.QueryRow(t.Context(), "SELECT to_regclass('billing.customer_links') IS NULL AND to_regclass('billing.network_event_cursors') IS NULL").Scan(&absent))
	if !absent {
		t.Fatal("fresh core retained hosted tables")
	}
}

// Deliberately scoped to real public server, AuthKit and billing boundaries.
func TestCustomerAdmissionPolicyPreservesGenericSurfaces(t *testing.T) {
	pool := extractionDatabase(t)
	_, key, err := ed25519.GenerateKey(rand.Reader)
	extractionOK(t, err)
	signer, err := keys.SignerFromKey("extraction", key)
	extractionOK(t, err)
	var current atomic.Pointer[http.Handler]
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := current.Load(); h != nil {
			(*h).ServeHTTP(w, r)
		} else {
			http.Error(w, "starting", 503)
		}
	}))
	defer httpServer.Close()
	var deny atomic.Bool
	var rejectSubject string
	policy := func(ctx context.Context, v auth.Verified, b openrails.CustomerBinding) error {
		id := v.Identity()
		if id.Subject != b.CustomerID.String() || b.MerchantID.IsZero() {
			return auth.ErrForbidden
		}
		if deny.Load() && id.Subject == rejectSubject {
			return auth.ErrForbidden
		}
		return nil
	}
	srv, err := server.New(t.Context(), server.Config{
		Engine: openrails.Config{Database: openrails.DatabaseConfig{Schema: "isolated_billing", RiverSchema: "isolated_river"}, PublicBillingBaseURL: httpServer.URL, ProviderWriteMode: openrails.ProviderWritesFull, RateLimitsDisabled: true, CatalogReconciliationInterval: "0"},
		Auth: authkit.Config{DefaultTenantID: iam.NewTenantID(), Database: authkit.DatabaseConfig{Schema: "isolated_auth", RiverSchema: "isolated_river"}, Token: authkit.TokenConfig{Issuer: httpServer.URL, AllowPrivateNetworkJWKS: true}, Resource: authkit.ResourceConfig{ID: httpServer.URL, PublicURL: httpServer.URL, Scopes: map[string][]string{"app:link": {}, billing.ScopeSelf: {"merchant:*"}}},
			AuthorizationServer: authkit.AuthorizationServerConfig{Resources: []authkit.ResourceServerConfig{{ID: httpServer.URL, Scopes: []string{"app:link", billing.ScopeSelf}}}, GroupClients: authkit.GroupClientsConfig{Scopes: []authkit.GroupClientScope{{Name: "app:link", Description: "Link the host account", Resource: httpServer.URL}, {Name: billing.ScopeSelf, Description: "Read own billing", Resource: httpServer.URL}}}}, Registration: authkit.RegistrationConfig{AllowMissingSenders: true}, TwoFactor: authkit.TwoFactorConfig{Mode: iam.TwoFactorDisabled}, HTTP: &authkit.HTTPConfig{DirectPeerIP: true}, Agreements: []authkit.AgreementConfig{{Key: "network-terms", Version: "test", URL: "https://example.invalid/terms"}}},
		LocalSignIn: true, AdminConsole: &server.AdminConsole{Disabled: true}, RouteGroups: openrails.RouteGroups{Admin: true, Programmatic: true},
	}, server.Deps{CustomerAdmissionPolicy: policy, Engine: openrails.Deps{Postgres: pool}, Auth: authkit.Deps{Postgres: pool, KeySource: keys.Static{Active: signer, Public: map[string]crypto.PublicKey{signer.KID(): signer.Public()}}}})
	extractionOK(t, err)
	defer srv.Close(context.Background())
	handler := srv.Handler()
	current.Store(&handler)
	ak := srv.AuthKit()
	user := authtest.NewUser(t, ak)
	rejectSubject = user.ID
	merchant, err := srv.ProvisionMerchant(t.Context(), billing.ProvisionMerchantParams{Slug: "merchant"})
	extractionOK(t, err)
	token, err := ak.MintAccessToken(t.Context(), user.ID, iam.AccessTokenOptions{Group: iam.GroupByID(merchant.GroupID)})
	extractionOK(t, err)
	client, err := srv.Engine().Merchant(merchant.MerchantID)
	extractionOK(t, err)
	customer := billing.CustomerID(uuid.MustParse(user.ID))
	request := func(path, credential string) int {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+credential)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code >= 500 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
		return w.Code
	}
	deny.Store(true)
	if status := request("/v1/me/orders", token.Value); status != http.StatusForbidden {
		t.Fatalf("policy refusal=%d", status)
	}
	var count int
	extractionOK(t, pool.QueryRow(t.Context(), "SELECT count(*) FROM isolated_billing.customers WHERE merchant_id=$1 AND id=$2", merchant.MerchantID.UUID(), customer.UUID()).Scan(&count))
	if count != 0 {
		t.Fatal("refused proof allocated a customer")
	}
	deny.Store(false)
	if status := request("/v1/me/orders", token.Value); status != http.StatusOK {
		t.Fatalf("ordinary customer=%d", status)
	}
	deny.Store(true)
	if status := request("/v1/me/orders", token.Value); status != http.StatusForbidden {
		t.Fatalf("existing customer bypassed policy=%d", status)
	}
	owner, ok := server.MerchantRole("owner")
	if !ok {
		t.Fatal("owner role missing")
	}
	keyResult, err := ak.CreateAPIKey(t.Context(), iam.SystemIdentity(), iam.GroupByID(merchant.GroupID), iam.NewAPIKey{Name: "fixture", Role: owner})
	extractionOK(t, err)
	if status := request("/v1/admin/customers", keyResult.Secret); status != http.StatusOK {
		t.Fatalf("staff affected by customer policy=%d", status)
	}
	_, err = client.GetCustomer(t.Context(), customer)
	extractionOK(t, err)
	for _, path := range []string{"/v1/network/links", "/v1/me/network-link/challenges", "/pay/sign-in"} {
		if status := request(path, token.Value); status != 404 {
			t.Fatalf("removed route %s=%d", path, status)
		}
	}
	routes, err := srv.Engine().CustomerRoutes(openrails.CustomerRoutes{Prefix: "/account/me", Auth: ak.Authenticator(), Resolve: func(r *http.Request, v auth.Verified) (openrails.CustomerBinding, error) {
		if err := v.(auth.CredentialChecker).CheckCredential(r.Context()); err != nil {
			return openrails.CustomerBinding{}, err
		}
		return openrails.CustomerBinding{MerchantID: merchant.MerchantID, CustomerID: customer}, nil
	}})
	extractionOK(t, err)
	mux := http.NewServeMux()
	for _, r := range routes {
		mux.Handle(r.Method+" "+r.Path, r.Handler)
	}
	r := httptest.NewRequest(http.MethodGet, "/account/me/orders", nil)
	r.Header.Set("Authorization", "Bearer "+token.Value)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("mapped routes affected by ordinary policy: %d %s", w.Code, w.Body.String())
	}
	// A host-owned consent scope is usable, while a host cannot broaden the
	// canonical self scope into merchant administration.
	_, err = ak.SetGroupRole(t.Context(), iam.SystemIdentity(), iam.GroupByID(merchant.GroupID), iam.UserSubject(user.ID), owner)
	extractionOK(t, err)
	oauth, err := ak.CreateGroupOAuthClient(t.Context(), iam.SystemIdentity(), iam.GroupByID(merchant.GroupID), iam.NewOAuthClient{ClientName: "host link", RedirectURIs: []string{"https://merchant.invalid/callback"}, TokenEndpointAuthMethod: iam.OAuthClientSecretBasic, Scope: "openid app:link openrails:self"})
	extractionOK(t, err)
	if oauth.ClientSecret == nil {
		t.Fatal("confidential client has no secret")
	}
	as := authtest.Attach(t, ak, httpServer)
	issued := as.Authorize(t, user, authtest.CodeFlow{ClientID: oauth.ClientID, ClientSecret: *oauth.ClientSecret, RedirectURI: "https://merchant.invalid/callback", Resource: httpServer.URL, Scopes: []string{"openid", "app:link", billing.ScopeSelf}, Consent: true})
	if !strings.Contains(issued.Scope, "app:link") {
		t.Fatal("host scope was discarded")
	}
	deny.Store(false)
	if status := request("/v1/me/orders", issued.AccessToken); status != 200 {
		t.Fatalf("self scope refused: %d", status)
	}
	if status := request("/v1/admin/customers", issued.AccessToken); status != 403 {
		t.Fatalf("host overrode billing scope ceiling: %d", status)
	}
	// Historical producer kinds remain readable as raw retained facts.
	tx, err := pool.Begin(t.Context())
	extractionOK(t, err)
	_, err = tx.Exec(t.Context(), "SELECT set_config('openrails.merchant_id',$1,true)", merchant.MerchantID.String())
	extractionOK(t, err)
	_, err = tx.Exec(t.Context(), `INSERT INTO isolated_billing.host_outbox(merchant_id,event_type,subject_type,subject_id,occurred_at,data,dedupe_key) VALUES($1,'customer.network_linked','customer',$2,now(),'{"legacy":true}'::jsonb,'legacy:read')`, merchant.MerchantID.UUID(), customer.UUID())
	extractionOK(t, err)
	extractionOK(t, tx.Commit(t.Context()))
	events, err := client.ListEvents(t.Context(), billing.EventQuery{Types: []string{"customer.network_linked"}})
	extractionOK(t, err)
	if len(events.Events) != 1 || events.Events[0].Type != "customer.network_linked" {
		t.Fatalf("historical event changed: %#v", events)
	}
	var legacy struct {
		Legacy bool `json:"legacy"`
	}
	extractionOK(t, json.Unmarshal(events.Events[0].Data, &legacy))
	if !legacy.Legacy {
		t.Fatal("historical raw payload lost")
	}
	// Declaring a formerly activating agreement does not expose product configuration.
	r = httptest.NewRequest(http.MethodGet, "/v1/config", nil)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	var cfg map[string]json.RawMessage
	extractionOK(t, json.NewDecoder(w.Body).Decode(&cfg))
	if _, ok := cfg["network"]; ok {
		t.Fatal("public network activation survived")
	}
}
