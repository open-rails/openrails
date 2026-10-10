//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	authkitgin "github.com/open-rails/authkit/adapters/gin"
	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailsgin "github.com/open-rails/openrails/adapters/gin"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/fxfake"
	"github.com/open-rails/openrails/openrailstest/nmimock"
)

// A customer without access, signed out included, gets 402 and the page
// where they can buy it; buying through the routes billing-ui's Buy calls (a
// checkout session for the catalog price, paid with a new card) makes the
// content route serve them.
func TestGatedContent(t *testing.T) {
	app := startApp(t)

	t.Run("signed out", func(t *testing.T) {
		app.requireBuy(t, "", "/api/courses/css-101", "/courses/css-101/buy")
		app.requireBuy(t, "", "/api/members/qa", "/join")
	})
	t.Run("an unknown course", func(t *testing.T) {
		res := app.get(t, "", "/api/courses/no-such-course")
		require.Equal(t, http.StatusNotFound, res.StatusCode)
	})
	t.Run("a course bought alone", func(t *testing.T) {
		alice := app.signUp(t, "alice")
		app.requireBuy(t, alice, "/api/courses/css-101", "/courses/css-101/buy")
		app.buy(t, alice, "course-101", "purchase")
		app.requireContent(t, alice, "/api/courses/css-101", "The box model")
		app.requireBuy(t, alice, "/api/courses/tailwind-102", "/courses/tailwind-102/buy")
		app.requireBuy(t, alice, "/api/members/qa", "/join")
	})
	t.Run("a rental", func(t *testing.T) {
		dave := app.signUp(t, "dave")
		app.buy(t, dave, "course-102", "rent")
		app.requireContent(t, dave, "/api/courses/tailwind-102", "Utility classes")
	})
	t.Run("the bundle", func(t *testing.T) {
		bob := app.signUp(t, "bobby")
		app.buy(t, bob, "course-bundle", "purchase")
		app.requireContent(t, bob, "/api/courses/css-101", "The box model")
		app.requireContent(t, bob, "/api/courses/tailwind-102", "Utility classes")
	})
	t.Run("a member", func(t *testing.T) {
		carol := app.signUp(t, "carol")
		app.buy(t, carol, "channel-membership", "monthly")
		app.requireContent(t, carol, "/api/members/qa", "How do I center a div?")
		app.requireBuy(t, carol, "/api/courses/css-101", "/courses/css-101/buy")
	})
}

// The browser e2e in web/e2e against this server: the React app built into
// web/dist, signing in with auth-ui and buying with billing-ui.
func TestBrowser(t *testing.T) {
	playwright, err := filepath.Abs(filepath.Join("web", "node_modules", ".bin", "playwright"))
	require.NoError(t, err)
	if _, err := os.Stat(playwright); err != nil {
		t.Skip("the app is not installed: cd web && pnpm install && pnpm build")
	}
	app := startApp(t)
	users := []string{"reader1", "reader2", "reader3"}
	for _, name := range users {
		_, err := app.auth.CreateUser(context.Background(), iam.NewUser{Email: name + "@example.com", EmailVerified: true, Username: name, Password: password})
		require.NoError(t, err)
	}
	cmd := exec.Command(playwright, "test")
	cmd.Dir = "web"
	cmd.Env = append(os.Environ(), "E2E_BASE_URL="+app.URL, "E2E_USERS="+strings.Join(users, ","), "E2E_PASSWORD="+password)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	require.NoError(t, cmd.Run())
}

type app struct {
	*httptest.Server
	auth *authkit.Client
	nmi  *nmimock.Mock
}

// startApp serves main.go's mount (its admin group), gateContent and serveApp
// on a fresh database in OPENRAILS_E2E_DSN's server, with NMI played by
// nmimock.
func startApp(t *testing.T) *app {
	t.Helper()
	dsn := os.Getenv("OPENRAILS_E2E_DSN")
	if dsn == "" {
		t.Skip("OPENRAILS_E2E_DSN names no PostgreSQL 18 server")
	}
	ctx := context.Background()
	db := freshDatabase(t, dsn)

	rbac := authkit.NewRoles()
	customersRead := rbac.Root.Permission("customers", "read")
	customersUpdate := rbac.Root.Permission("customers", "update")
	rbac.Root.Role("support", customersRead, customersUpdate)
	ak, err := newAuth(ctx, db, rbac)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ak.Close(context.Background()) })

	// newBilling's configuration, with merchant.example.yaml's NMI account
	// played by nmimock.
	gateway := nmimock.New(nmimock.Options{})
	t.Cleanup(gateway.Close)
	merchant, err := openrails.ReadMerchantFile("merchant.example.yaml")
	require.NoError(t, err)
	products, err := catalog.ReadFile("catalog.yaml")
	require.NoError(t, err)
	// Exchange rates from a fake exchange-api: no test reaches the real one.
	fx := fxfake.New()
	t.Cleanup(fx.Close)
	bill, err := openrails.New(ctx, openrails.Config{
		Database:          openrails.DatabaseConfig{Schema: "billing"},
		TestMode:          openrails.Sandbox,
		ProviderWriteMode: openrails.ProviderWritesFull,
		ProviderSandbox:   &openrails.ProviderSandboxConfig{NMIGatewayURL: gateway.URL()},
		Merchant:          merchant,
		Catalog:           products,
	}, openrails.Deps{Postgres: db, UserInfo: ak.UserInfo(), FXTransport: fx.Transport()})
	require.NoError(t, err)
	t.Cleanup(func() { bill.Close(context.Background()) })
	require.NoError(t, ak.Start(ctx))
	require.NoError(t, bill.Start(ctx))

	gin.SetMode(gin.TestMode)
	r := gin.New()
	require.NoError(t, authkitgin.Mount(r, ak))
	staff, err := ak.Scope(ctx, iam.RootGroup())
	require.NoError(t, err)
	require.NoError(t, openrailsgin.Mount(r, bill, openrails.Routes{
		Auth:        ak.Authenticator(),
		Scope:       staff,
		Prefix:      "/billing",
		RouteGroups: openrails.RouteGroups{Admin: true},
		Permissions: openrails.Permissions{AdminRead: customersRead, AdminUpdate: customersUpdate},
	}))
	gateContent(r, ak, bill)
	serveApp(r, "web/dist")
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)
	return &app{Server: server, auth: ak, nmi: gateway}
}

// freshDatabase creates a database for this test on dsn's server and drops it
// afterwards.
func freshDatabase(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	name := "embedded_example_" + hex.EncodeToString(suffix)
	_, err = admin.Exec(ctx, "CREATE DATABASE "+name)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = admin.Close(context.Background())
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.ConnConfig.Database = name
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(db.Close)
	return db
}

// signUp creates a user with AuthKit's Go API (its sign-up route allows one
// per address a minute) and signs them in through its route, returning their
// access token.
func (a *app) signUp(t *testing.T, name string) string {
	t.Helper()
	_, err := a.auth.CreateUser(context.Background(), iam.NewUser{Email: name + "@example.com", EmailVerified: true, Username: name, Password: password})
	require.NoError(t, err)
	var out struct {
		TokenSet struct {
			AccessToken string `json:"access_token"`
		} `json:"token_set"`
	}
	a.call(t, "", http.MethodPost, "/api/v1/password/login", map[string]string{"identifier": name, "password": password}, &out)
	require.NotEmpty(t, out.TokenSet.AccessToken)
	return out.TokenSet.AccessToken
}

const password = "correct horse battery staple"

// buy pays for product's price as the buy page's Buy does: the signed-in
// customer mints a checkout session for the catalog price, and CheckoutModal
// reads and pays it by its id with a new card (checkoutSource without a
// customer base sends no token).
func (a *app) buy(t *testing.T, token, product, price string) {
	t.Helper()
	var session struct {
		ID string `json:"id"`
	}
	a.call(t, token, http.MethodPost, "/billing/v1/me/checkout-sessions", map[string]string{"product_key": product, "price_key": price}, &session)
	var page struct {
		Options []struct {
			ID     string `json:"id"`
			Driver string `json:"driver"`
		} `json:"options"`
	}
	a.call(t, "", http.MethodGet, "/billing/v1/checkout-sessions/"+session.ID, nil, &page)
	require.Len(t, page.Options, 1)
	require.Equal(t, "collect_js", page.Options[0].Driver)
	var paid struct {
		Status string `json:"status"`
	}
	a.call(t, "", http.MethodPost, "/billing/v1/checkout-sessions/"+session.ID+"/pay", map[string]any{
		"option_id":       page.Options[0].ID,
		"payment_token":   a.nmi.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242"}),
		"billing_details": map[string]any{"name": "Card Holder", "address": map[string]string{"country": "US", "postal_code": "10001"}},
	}, &paid)
	require.Equal(t, "succeeded", paid.Status)
}

// requireBuy: path answers 402 naming buy.
func (a *app) requireBuy(t *testing.T, token, path, buy string) {
	t.Helper()
	res := a.get(t, token, path)
	require.Equal(t, http.StatusPaymentRequired, res.StatusCode, path)
	var body struct{ Error, Buy string }
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
	require.Equal(t, "access_required", body.Error)
	require.Equal(t, buy, body.Buy)
}

// requireContent: path serves content listing item.
func (a *app) requireContent(t *testing.T, token, path, item string) {
	t.Helper()
	res := a.get(t, token, path)
	require.Equal(t, http.StatusOK, res.StatusCode, path)
	var content map[string][]string
	require.NoError(t, json.NewDecoder(res.Body).Decode(&content))
	require.Len(t, content, 1)
	for _, items := range content {
		require.Contains(t, items, item)
	}
}

// get requests path without following redirects.
func (a *app) get(t *testing.T, token, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, a.URL+path, nil)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// call sends a JSON request to path and decodes a 2xx answer into out.
func (a *app) call(t *testing.T, token, method, path string, body, out any) {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&payload).Encode(body))
	}
	req, err := http.NewRequest(method, a.URL+path, &payload)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", a.URL)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = res.Body.Close() }()
	var raw json.RawMessage
	_ = json.NewDecoder(res.Body).Decode(&raw)
	require.Less(t, res.StatusCode, 300, "%s %s: %d %s", method, path, res.StatusCode, raw)
	if out != nil {
		require.NoError(t, json.Unmarshal(raw, out))
	}
}
