//go:build integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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

// A visitor without a key that unlocks a video, signed out included, gets
// 402 with the products on sale that unlock it and its buy page. Buying
// through the routes billing-ui calls (a checkout session for the catalog
// price) gets the signed video URL: a course for its buyers, every video for
// members.
func TestGatedContent(t *testing.T) {
	app := startApp(t)
	const css, tailwind, qa = "/api/courses/css-101", "/api/courses/tailwind-102", "/api/courses/live-qa"
	// The products on sale that unlock each: the course, the bundle, the membership.
	cssKeys, tailwindKeys, qaKeys := []string{"channel-membership", "course-101", "course-bundle"}, []string{"channel-membership", "course-102", "course-bundle"}, []string{"channel-membership"}

	t.Run("signed out", func(t *testing.T) {
		app.requireBuy(t, "", css, cssKeys, "/courses/css-101/buy")
		app.requireBuy(t, "", qa, qaKeys, "/courses/live-qa/buy")
	})
	t.Run("an unknown course", func(t *testing.T) {
		require.Equal(t, http.StatusNotFound, app.get(t, "", "/api/courses/no-such-course").StatusCode)
	})
	t.Run("a course bought alone", func(t *testing.T) {
		alice := app.signUp(t, "alice")
		app.requireBuy(t, alice, css, cssKeys, "/courses/css-101/buy")
		app.buy(t, alice, "course-101", "purchase")
		app.requireVideo(t, alice, css, "media/courses/css-101.mp4")
		app.requireBuy(t, alice, tailwind, tailwindKeys, "/courses/tailwind-102/buy")
		app.requireBuy(t, alice, qa, qaKeys, "/courses/live-qa/buy")
	})
	t.Run("a rental", func(t *testing.T) {
		dave := app.signUp(t, "dave")
		app.buy(t, dave, "course-102", "rent")
		app.requireVideo(t, dave, tailwind, "media/courses/tailwind-102.mp4")
	})
	t.Run("the bundle", func(t *testing.T) {
		bob := app.signUp(t, "bobby")
		app.buy(t, bob, "course-bundle", "purchase")
		app.requireVideo(t, bob, css, "media/courses/css-101.mp4")
		app.requireVideo(t, bob, tailwind, "media/courses/tailwind-102.mp4")
		app.requireBuy(t, bob, qa, qaKeys, "/courses/live-qa/buy")
	})
	t.Run("a member watches every video", func(t *testing.T) {
		carol := app.signUp(t, "carol")
		app.buy(t, carol, "channel-membership", "monthly")
		app.requireVideo(t, carol, css, "media/courses/css-101.mp4")
		app.requireVideo(t, carol, tailwind, "media/courses/tailwind-102.mp4")
		app.requireVideo(t, carol, qa, "media/courses/live-qa.mp4")
	})
}

// A media URL is good only as signed, and only until it expires.
func TestMediaURLs(t *testing.T) {
	app := startApp(t)
	signed := testMediaKey.url("courses/css-101.mp4")
	require.Equal(t, http.StatusOK, app.get(t, "", signed).StatusCode)

	tampered := strings.Replace(signed, "css-101", "tailwind-102", 1)
	require.Equal(t, http.StatusForbidden, app.get(t, "", tampered).StatusCode)
	past := strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
	expired := "/media/courses/css-101.mp4?" + url.Values{"expires": {past}, "sig": {testMediaKey.sign("courses/css-101.mp4", past)}}.Encode()
	require.Equal(t, http.StatusForbidden, app.get(t, "", expired).StatusCode)
	require.Equal(t, http.StatusForbidden, app.get(t, "", "/media/courses/css-101.mp4").StatusCode)
	other := mediaKey("another key").url("courses/css-101.mp4")
	require.Equal(t, http.StatusForbidden, app.get(t, "", other).StatusCode)
}

// The course list pages over the host's courses. Each page joins in its
// products' live prices and what the user owns with one product read and one
// entitlement read, never one per course; signed out, it reads no
// entitlements at all. A members-only video is sold with the membership.
func TestCourseList(t *testing.T) {
	app := startApp(t)
	type price struct{ Key, Amount, Currency, Terms string }
	type row struct {
		Slug, Title string
		MembersOnly bool `json:"members_only"`
		Owned       bool
		ProductKey  string `json:"product_key"`
		Prices      []price
	}
	var page struct {
		Data       []row
		NextCursor *string `json:"next_cursor"`
	}
	read := func(token, query string, entitlementReads int) {
		t.Helper()
		page.Data, page.NextCursor = nil, nil
		app.queries.reset()
		app.call(t, token, http.MethodGet, "/api/courses?"+query, nil, &page)
		require.Equal(t, 1, app.queries.count("ListProductsFiltered"), "one product read per page")
		require.Equal(t, 1, app.queries.count("ListCurrentPricesByProducts"), "its prices with it")
		require.Equal(t, entitlementReads, app.queries.count("ListValidEntitlementCaches"), "entitlement reads per page")
	}
	cssPrices := []price{{"rent", "1990000", "USD", "for 3 days"}, {"purchase", "4990000", "USD", "to keep"}}
	membershipPrices := []price{{"monthly", "10000000", "USD", "every 30 days"}, {"yearly", "99000000", "USD", "every 365 days"}}

	read("", "limit=1", 0)
	require.Equal(t, []row{{"css-101", "Intro to CSS", false, false, "course-101", cssPrices}}, page.Data)
	read("", "limit=1&cursor="+*page.NextCursor, 0)
	require.Equal(t, "tailwind-102", page.Data[0].Slug)
	read("", "limit=1&cursor="+*page.NextCursor, 0)
	require.Equal(t, []row{{"live-qa", "Live Q&A", true, false, "channel-membership", membershipPrices}}, page.Data)
	require.Nil(t, page.NextCursor)

	owned := func() []bool {
		var out []bool
		for _, r := range page.Data {
			out = append(out, r.Owned)
		}
		return out
	}
	alice := app.signUp(t, "alice")
	app.buy(t, alice, "course-101", "purchase")
	read(alice, "", 1)
	require.Equal(t, []bool{true, false, false}, owned(), "her course only")
	carol := app.signUp(t, "carol")
	app.buy(t, carol, "channel-membership", "yearly")
	read(carol, "", 1)
	require.Equal(t, []bool{true, true, true}, owned(), "a member owns every video")

	require.Equal(t, http.StatusBadRequest, app.get(t, "", "/api/courses?limit=0").StatusCode)
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
	users := []string{"reader1", "reader2", "reader3", "reader4"}
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

var testMediaKey = mediaKey("test media key")

type app struct {
	*httptest.Server
	auth    *authkit.Client
	nmi     *nmimock.Mock
	queries *queryCount
}

// queryCount counts the sqlc queries run on the app's pool, by name.
type queryCount struct {
	mu    sync.Mutex
	names map[string]int
}

func (q *queryCount) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if name, ok := strings.CutPrefix(data.SQL, "-- name: "); ok {
		name, _, _ = strings.Cut(name, " ")
		q.mu.Lock()
		q.names[name]++
		q.mu.Unlock()
	}
	return ctx
}

func (q *queryCount) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (q *queryCount) reset() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.names = map[string]int{}
}

func (q *queryCount) count(name string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.names[name]
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
	queries := &queryCount{names: map[string]int{}}
	db := freshDatabase(t, dsn, queries)

	rbac := authkit.NewRoles()
	billingRead := rbac.Root.Permission("billing", "read")
	billingManage := rbac.Root.Permission("billing", "manage")
	rbac.Root.Role("support", billingRead, billingManage)
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
		Permissions: openrails.Permissions{AdminRead: billingRead, AdminUpdate: billingManage},
	}))
	courseRoutes(r, ak, bill, testMediaKey)
	serveApp(r, "web/dist")
	server := httptest.NewServer(r)
	t.Cleanup(server.Close)
	return &app{Server: server, auth: ak, nmi: gateway, queries: queries}
}

// freshDatabase creates a database for this test on dsn's server and drops it
// afterwards.
func freshDatabase(t *testing.T, dsn string, tracer pgx.QueryTracer) *pgxpool.Pool {
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
	cfg.ConnConfig.Tracer = tracer
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

// requireBuy: path answers 402 naming the products that unlock it and the
// buy page.
func (a *app) requireBuy(t *testing.T, token, path string, products []string, buy string) {
	t.Helper()
	res := a.get(t, token, path)
	require.Equal(t, http.StatusPaymentRequired, res.StatusCode, path)
	var body struct {
		Error, Buy string
		Products   []string
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
	require.Equal(t, "access_required", body.Error)
	require.Equal(t, products, body.Products)
	require.Equal(t, buy, body.Buy)
}

// requireVideo: path answers a signed URL that serves file, ranges included.
func (a *app) requireVideo(t *testing.T, token, path, file string) {
	t.Helper()
	res := a.get(t, token, path)
	require.Equal(t, http.StatusOK, res.StatusCode, path)
	var body struct {
		VideoURL string `json:"video_url"`
	}
	require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
	want, err := os.ReadFile(file)
	require.NoError(t, err)

	video := a.get(t, "", body.VideoURL) // the URL is the credential
	require.Equal(t, http.StatusOK, video.StatusCode)
	require.Equal(t, "video/mp4", video.Header.Get("Content-Type"))
	got, err := io.ReadAll(video.Body)
	require.NoError(t, err)
	require.Equal(t, want, got)

	req, err := http.NewRequest(http.MethodGet, a.URL+body.VideoURL, nil)
	require.NoError(t, err)
	req.Header.Set("Range", "bytes=0-99")
	ranged, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = ranged.Body.Close() }()
	require.Equal(t, http.StatusPartialContent, ranged.StatusCode)
	part, err := io.ReadAll(ranged.Body)
	require.NoError(t, err)
	require.Equal(t, want[:100], part)
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
