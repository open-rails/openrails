//go:build e2e && integration

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/helpers/auth/authtest"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/openrailstest"
)

// The shop's Authenticator passes OpenRails' conformance check against its
// real tables: staff hold the mounted groups' root: permissions in the root
// scope and nowhere else, a shopper none, a reader customer:read alone; a
// stale sign-in must sign in again; signed-out and unknown sessions are
// refused; staff switched to a merchant keep their root roles; and staff
// signing out ends their session.
func TestOwnAuthConforms(t *testing.T) {
	s := newTestShop(t)
	staff := s.user(t, "staff@shop.example", "admin")
	staffSession := s.signIn(t, staff, "")
	stale := s.signIn(t, s.user(t, "stale@shop.example", "admin"), "")
	s.age(t, stale, time.Hour)
	gone := s.signIn(t, s.user(t, "gone@shop.example", "support"), "")
	require.Equal(t, http.StatusNoContent, s.call(t, gone, http.MethodPost, "/sign-out", nil).status)

	openrailstest.CheckAuth(t, s.routes, authtest.Cases{
		Staff:      bearer(staffSession),
		User:       bearer(s.signIn(t, s.user(t, "shopper@shop.example"), "acme")),
		Holders:    map[string]func() *http.Request{string(billing.RootCustomerRead): bearer(s.signIn(t, s.user(t, "reader@shop.example", "reader"), ""))},
		Stale:      bearer(stale),
		Refused:    map[string]func() *http.Request{"signed out": bearer(gone), "unknown": bearer("not-a-session")},
		BoundStaff: bearer(s.signIn(t, staff, "acme")),
		Revoke: func() {
			require.Equal(t, http.StatusNoContent, s.call(t, staffSession, http.MethodPost, "/sign-out", nil).status)
		},
	})
}

// OpenRails' gates over the shop's sessions, through real HTTP: a session
// acts at the merchant it is bound to, and holds there what its user's roles
// grant.
func TestOwnAuthGates(t *testing.T) {
	s := newTestShop(t)
	const customers, entitlements = "/billing/v1/admin/customers", "/billing/v1/me/entitlements"
	acme, globex := s.merchants["acme"], s.merchants["globex"]

	// Shoppers: ann at Acme's store, sam at Globex's, pat at both. A first
	// request makes each the customer of the store's merchant.
	ann, sam, pat := s.user(t, "ann@shop.example"), s.user(t, "sam@shop.example"), s.user(t, "pat@shop.example")
	patAtAcme, patAtGlobex := s.signIn(t, pat, "acme"), s.signIn(t, pat, "globex")
	for _, token := range []string{s.signIn(t, ann, "acme"), s.signIn(t, sam, "globex"), patAtAcme, patAtGlobex} {
		got := s.call(t, token, http.MethodGet, entitlements, nil)
		require.Equal(t, http.StatusOK, got.status, got.body)
	}

	t.Run("a shopper sees only their store's merchant", func(t *testing.T) {
		g, err := s.bill.Merchant(globex)
		require.NoError(t, err)
		doc, err := catalog.ParseApplicationYAML([]byte("schema_version: 1\nproducts:\n  pro:\n    display_name: Globex Pro\n    entitlements: [\"globex:pro\"]\n"))
		require.NoError(t, err)
		_, err = g.ApplyCatalog(t.Context(), doc, billing.ApplyCatalogParams{})
		require.NoError(t, err)
		products, err := g.ListProducts(t.Context(), billing.ProductListParams{Keys: []string{"pro"}})
		require.NoError(t, err)
		require.Len(t, products.Items, 1)
		customer, err := billing.ParseCustomerID(pat)
		require.NoError(t, err)
		_, err = g.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{
			{CustomerID: customer, ProductID: products.Items[0].ID, Reason: billing.GrantReasonComp},
		}})
		require.NoError(t, err)

		require.Equal(t, []string{"globex:pro"}, s.entitlements(t, patAtGlobex, globex))
		require.Empty(t, s.entitlements(t, patAtAcme, acme), "Globex's grant is not Acme's")
	})

	t.Run("a root admin acts at the merchant they switched to", func(t *testing.T) {
		root := s.signIn(t, s.user(t, "root@shop.example", "admin"), "")
		requireCode(t, s.call(t, root, http.MethodGet, customers, nil), http.StatusForbidden, "merchant_unresolved")

		s.switchTo(t, root, acme, http.StatusNoContent)
		require.ElementsMatch(t, []string{"ann@shop.example", "pat@shop.example"}, s.customers(t, root, acme))
		s.switchTo(t, root, globex, http.StatusNoContent)
		require.ElementsMatch(t, []string{"sam@shop.example", "pat@shop.example"}, s.customers(t, root, globex))
	})

	t.Run("a merchant's support stays at that merchant", func(t *testing.T) {
		support := s.signIn(t, s.user(t, "support@shop.example", "support@acme"), "")
		s.switchTo(t, support, acme, http.StatusNoContent)
		require.ElementsMatch(t, []string{"ann@shop.example", "pat@shop.example"}, s.customers(t, support, acme))
		s.switchTo(t, support, globex, http.StatusForbidden)
		require.ElementsMatch(t, []string{"ann@shop.example", "pat@shop.example"}, s.customers(t, support, acme), "the refused switch changed nothing")
		requireCode(t, s.call(t, support, http.MethodPost, "/billing/v1/catalog/apply", map[string]any{}), http.StatusForbidden, "permission_required")
	})

	t.Run("a stale sign-in passes the refund permission gate", func(t *testing.T) {
		stale := s.signIn(t, s.user(t, "stale@shop.example", "admin"), "acme")
		s.age(t, stale, time.Hour)
		s.customers(t, stale, acme)
		refund := "/billing/v1/admin/payments/" + billing.PaymentID(uuid.New()).String() + "/refunds"
		got := s.call(t, stale, http.MethodPost, refund, map[string]any{})
		requireCode(t, got, http.StatusBadRequest, "invalid_param")
		require.Empty(t, got.header.Get("WWW-Authenticate"), "the refund handler validates the request without step-up")
	})

	t.Run("a signed-out session reaches nothing", func(t *testing.T) {
		admin := s.signIn(t, s.user(t, "leaving@shop.example", "admin"), "acme")
		s.customers(t, admin, acme)
		require.Equal(t, http.StatusNoContent, s.call(t, admin, http.MethodPost, "/sign-out", nil).status)
		requireCode(t, s.call(t, admin, http.MethodGet, customers, nil), http.StatusUnauthorized, "credential_revoked")
		s.switchTo(t, admin, globex, http.StatusUnauthorized)
		require.Equal(t, http.StatusUnauthorized, s.call(t, "", http.MethodGet, entitlements, nil).status)
	})
}

// testShop is the shop on a database of its own, served over HTTP.
type testShop struct {
	*shop
	db  *pgxpool.Pool
	url string
}

func newTestShop(t *testing.T) *testShop {
	t.Helper()
	ctx := context.Background()
	dsn := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_DSN"))
	require.NotEmpty(t, dsn, "OPENRAILS_E2E_DSN must name a disposable PostgreSQL 18 server")
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	name := "own_auth_" + hex.EncodeToString(suffix)
	_, err = admin.Exec(ctx, "CREATE DATABASE "+name)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = admin.Close(context.Background())
	})
	u, err := url.Parse(dsn)
	require.NoError(t, err)
	u.Path = "/" + name
	db, err := pgxpool.New(ctx, u.String())
	require.NoError(t, err)
	t.Cleanup(db.Close)
	s, err := newShop(ctx, db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.bill.Close(context.Background()) })
	server := httptest.NewServer(s.handler)
	t.Cleanup(server.Close)
	return &testShop{shop: s, db: db, url: server.URL}
}

// user adds a user holding roles: "admin" at every merchant, "admin@acme"
// at one.
func (s *testShop) user(t *testing.T, email string, roles ...string) string {
	t.Helper()
	var id string
	require.NoError(t, s.db.QueryRow(t.Context(), `INSERT INTO shop.users (email) VALUES ($1) RETURNING id`, email).Scan(&id))
	for _, role := range roles {
		name, slug, _ := strings.Cut(role, "@")
		var at *string
		if slug != "" {
			merchant := s.merchants[slug].String()
			at = &merchant
		}
		_, err := s.db.Exec(t.Context(), `INSERT INTO shop.roles (user_id, merchant_id, role) VALUES ($1, $2, $3)`, id, at, name)
		require.NoError(t, err)
	}
	return id
}

// signIn is a session of user at the merchant of slug ("" for none).
func (s *testShop) signIn(t *testing.T, user, slug string) string {
	t.Helper()
	merchant := ""
	if slug != "" {
		merchant = s.merchants[slug].String()
	}
	token, err := s.auth.signIn(t.Context(), user, merchant)
	require.NoError(t, err)
	return token
}

// age moves the sign-in behind token into the past.
func (s *testShop) age(t *testing.T, token string, by time.Duration) {
	t.Helper()
	sum := sha256.Sum256([]byte(token))
	_, err := s.db.Exec(t.Context(), `UPDATE shop.sessions SET signed_in_at = signed_in_at - make_interval(secs => $2) WHERE token_sha256 = $1`, sum[:], by.Seconds())
	require.NoError(t, err)
}

func (s *testShop) switchTo(t *testing.T, token string, merchant billing.MerchantID, status int) {
	t.Helper()
	got := s.call(t, token, http.MethodPost, "/switch", map[string]string{"merchant_id": merchant.String()})
	require.Equal(t, status, got.status, got.body)
}

// customers are the emails of the customers token reads, acting at merchant.
func (s *testShop) customers(t *testing.T, token string, merchant billing.MerchantID) []string {
	t.Helper()
	var page billing.ListPage[billing.Customer]
	s.read(t, token, "/billing/v1/admin/customers", merchant, &page)
	var out []string
	for _, c := range page.Items {
		require.NotNil(t, c.Contact, "the shop's directory names customer %s", c.ID)
		out = append(out, *c.Contact.Email)
	}
	return out
}

// entitlements are the keys token's customer holds, acting at merchant.
func (s *testShop) entitlements(t *testing.T, token string, merchant billing.MerchantID) []string {
	t.Helper()
	var page billing.ListPage[billing.CustomerEntitlement]
	s.read(t, token, "/billing/v1/me/entitlements", merchant, &page)
	var out []string
	for _, e := range page.Items {
		out = append(out, e.Entitlement)
	}
	return out
}

// read GETs path, which must answer 200 for merchant.
func (s *testShop) read(t *testing.T, token, path string, merchant billing.MerchantID, out any) {
	t.Helper()
	got := s.call(t, token, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, got.status, got.body)
	require.Equal(t, merchant.String(), got.header.Get("OpenRails-Bound-Merchant"))
	require.NoError(t, json.Unmarshal([]byte(got.body), out))
}

type reply struct {
	status int
	header http.Header
	body   string
}

func (s *testShop) call(t *testing.T, token, method, path string, body any) reply {
	t.Helper()
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, s.url+path, r)
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return reply{res.StatusCode, res.Header, string(raw)}
}

func requireCode(t *testing.T, got reply, status int, code string) {
	t.Helper()
	require.Equal(t, status, got.status, got.body)
	require.Contains(t, got.body, `"code":"`+code+`"`)
}

func bearer(token string) func() *http.Request {
	return func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/billing/v1/admin/customers", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		return r
	}
}
