//go:build e2e && integration

package ci_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/server"
)

// customerOf is a host's Auth for a customer surface that serves the merchant
// each request selects: AuthKit's, admitting a user only as a customer of the
// merchant OpenRails bound. It answers the bound merchant in a header.
type customerOf struct {
	openrails.Auth
	customers map[billing.MerchantID]map[string]bool
}

const boundHeader = "X-Bound-Merchant"

func (a customerOf) Required() func(http.Handler) http.Handler {
	required := a.Auth.Required()
	return func(next http.Handler) http.Handler {
		return required(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			who, _ := a.Identity(r.Context())
			mid, ok := openrails.RequestMerchant(r.Context())
			if !ok || !a.customers[mid][who.Subject] {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.Header().Set(boundHeader, mid.String())
			next.ServeHTTP(w, r)
		}))
	}
}

// A server's customer surface without a merchant serves the merchant each
// request selects, by OpenRails-Merchant or the merchant's API host, and binds
// it before the host's Auth runs. The customer is the signed-in subject at
// that merchant: no selector reaches another subject's billing.
func TestServerCustomerProfileServesTheSelectedMerchant(t *testing.T) {
	f := newFixture(t)
	dns := newTXTServer(t)
	cp := f.newServer(t, func(_ *server.Config, deps *server.Deps) { deps.Engine.DNSResolver = dns.resolver() })
	ctx := t.Context()
	engine := cp.Client()

	// Each signs in: the profile's Auth is AuthKit's, which checks the session.
	alice, aliceToken := newOwner(t, cp)
	bob, bobToken := newOwner(t, cp)
	carol, carolToken := newOwner(t, cp)

	type shop struct {
		id    billing.MerchantID
		slug  string
		group string
	}
	provision := func(name string) shop {
		slug := uniqueName(name)
		m, err := cp.ProvisionMerchant(ctx, billing.ProvisionMerchantParams{Slug: slug})
		require.NoError(t, err)
		return shop{m.MerchantID, slug, m.GroupID}
	}
	first, second, retired := provision("first"), provision("second"), provision("retired")

	// Alice and Bob are customers of both merchants, Carol of the second only.
	// At each, Alice holds a pass and Bob another product.
	customers := map[billing.MerchantID]map[string]bool{
		first.id:   {alice: true, bob: true},
		second.id:  {alice: true, bob: true, carol: true},
		retired.id: {alice: true},
	}
	passes := map[billing.MerchantID]billing.ProductAccessID{}
	notes := map[billing.MerchantID]string{}
	aliceKeys, bobKeys := map[billing.MerchantID]string{}, map[billing.MerchantID]string{}
	for _, m := range []shop{first, second} {
		at := openrails.ForMerchantID(m.id)
		var ids []billing.EnsureCustomerParams
		for user := range customers[m.id] {
			ids = append(ids, billing.EnsureCustomerParams{ID: billing.CustomerID(uuid.MustParse(user))})
		}
		_, err := engine.EnsureCustomers(ctx, ids, at)
		require.NoError(t, err)
		aliceKeys[m.id], bobKeys[m.id] = m.slug+":alice", m.slug+":bob"
		pass, err := engine.CreateProduct(ctx, billing.CreateProductParams{Key: "pass", DisplayName: m.slug, Entitlements: []string{aliceKeys[m.id]}}, at)
		require.NoError(t, err)
		other, err := engine.CreateProduct(ctx, billing.CreateProductParams{Key: "other", DisplayName: m.slug, Entitlements: []string{bobKeys[m.id]}}, at)
		require.NoError(t, err)
		granted, err := engine.CreateProductAccess(ctx, billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{
			{CustomerID: billing.CustomerID(uuid.MustParse(alice)), ProductID: pass.ID},
			{CustomerID: billing.CustomerID(uuid.MustParse(bob)), ProductID: other.ID},
		}}, at)
		require.NoError(t, err)
		passes[m.id] = granted[0].ID
		var note uuid.UUID
		require.NoError(t, f.pool.QueryRow(ctx, `INSERT INTO `+f.schema+`.notifications (merchant_id, customer_id, event_type, data)
			VALUES ($1, $2, 'test.selected', '{}') RETURNING id`, m.id.UUID(), uuid.MustParse(alice)).Scan(&note))
		notes[m.id] = billing.NotificationID(note).String()
	}

	routes, err := cp.Routes(openrails.CustomerRoutes{Prefix: "/billing/v1/me", Scope: openrails.CustomerSelfService, Auth: customerOf{Auth: cp.AuthKit(), customers: customers}})
	require.NoError(t, err)
	mux := http.NewServeMux()
	for _, r := range routes {
		mux.Handle(r.Method+" "+r.Path, r.Handler)
	}
	// A host's own route: OpenRails bound nothing, whatever the header says.
	mux.HandleFunc("GET /host/merchant", func(w http.ResponseWriter, r *http.Request) {
		_, ok := openrails.RequestMerchant(r.Context())
		require.False(t, ok, "a host route has no merchant OpenRails bound")
		w.WriteHeader(http.StatusNoContent)
	})

	do := func(host, token, method, path, selector string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "https://"+host+path, strings.NewReader("{}"))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		if selector != "" {
			r.Header.Set("OpenRails-Merchant", selector)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	const api = "api.e2e.test"
	me := func(token, path, selector string) *httptest.ResponseRecorder {
		return do(api, token, http.MethodGet, "/billing/v1/me"+path, selector)
	}
	read := func(w *httptest.ResponseRecorder) string {
		t.Helper()
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		return w.Body.String()
	}
	// markRead marks notifications read and answers each requested id: the
	// notification, or nil when it is not the caller's.
	markRead := func(token, selector string, ids ...string) map[string]any {
		t.Helper()
		body, err := json.Marshal(map[string]any{"notification_ids": ids})
		require.NoError(t, err)
		r := httptest.NewRequest(http.MethodPost, "https://"+api+"/billing/v1/me/notifications/read", bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("OpenRails-Merchant", selector)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		var out struct {
			Notifications map[string]any `json:"notifications"`
		}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
		require.Len(t, out.Notifications, len(ids), "every requested id is answered")
		return out.Notifications
	}
	refused := func(w *httptest.ResponseRecorder, status int, code string) {
		t.Helper()
		require.Equal(t, status, w.Code, w.Body.String())
		require.Contains(t, w.Body.String(), code)
		require.Empty(t, w.Header().Get(boundHeader), "refused before the host's Auth")
	}
	type grant struct {
		ID billing.ProductAccessID `json:"id"`
	}
	held := func(w *httptest.ResponseRecorder) []billing.ProductAccessID {
		t.Helper()
		var page struct{ Data []grant }
		require.NoError(t, json.Unmarshal([]byte(read(w)), &page))
		var out []billing.ProductAccessID
		for _, g := range page.Data {
			out = append(out, g.ID)
		}
		return out
	}

	// Alice reads her own billing at each merchant she selects, by each
	// selector form, and the host's Auth sees that merchant.
	for _, m := range []shop{first, second} {
		for _, selector := range []string{m.slug, "id:" + m.id.String()} {
			w := me(aliceToken, "/product-access", selector)
			require.Equal(t, []billing.ProductAccessID{passes[m.id]}, held(w), selector)
			require.Equal(t, m.id.String(), w.Header().Get(boundHeader), selector)
			body := read(me(aliceToken, "/entitlements", selector))
			require.Contains(t, body, aliceKeys[m.id])
			require.NotContains(t, body, ":bob")
			require.NotContains(t, body, aliceKeys[other(m, first, second).id])
			body = read(me(aliceToken, "/notifications", selector))
			require.Contains(t, body, notes[m.id])
			require.NotContains(t, body, notes[other(m, first, second).id])
		}
	}
	// A former name selects the merchant it named.
	renamed := uniqueName("first")
	require.NoError(t, operatorRename(ctx, cp, first.id, renamed))
	w := me(aliceToken, "/product-access", first.slug)
	require.Equal(t, []billing.ProductAccessID{passes[first.id]}, held(w))
	require.Equal(t, first.id.String(), w.Header().Get(boundHeader))
	first.slug = renamed

	// So does the merchant's proven API host, without a selector; a selector
	// naming another merchant there is refused.
	const shopHost = "shop.first.e2e.test"
	claim, err := engine.SetAPIHost(ctx, billing.SetAPIHostParams{APIHost: shopHost}, openrails.ForMerchantID(first.id))
	require.NoError(t, err)
	dns.publish(claim.Claim.DNSRecord.Name, claim.Claim.DNSRecord.Value)
	_, err = engine.VerifyAPIHost(ctx, openrails.ForMerchantID(first.id))
	require.NoError(t, err)
	w = do(shopHost, aliceToken, http.MethodGet, "/billing/v1/me/product-access", "")
	require.Equal(t, []billing.ProductAccessID{passes[first.id]}, held(w))
	require.Equal(t, first.id.String(), w.Header().Get(boundHeader))
	require.Equal(t, []billing.ProductAccessID{passes[first.id]}, held(do(shopHost, aliceToken, http.MethodGet, "/billing/v1/me/product-access", first.slug)))
	refused(do(shopHost, aliceToken, http.MethodGet, "/billing/v1/me/product-access", second.slug), http.StatusForbidden, "host_merchant_mismatch")

	// No selection, an unknown merchant or a retired one is refused before
	// the host's Auth runs.
	refused(me(aliceToken, "/product-access", ""), http.StatusForbidden, "merchant_unresolved")
	refused(me(aliceToken, "/product-access", uniqueName("unknown")), http.StatusNotFound, "merchant_not_found")
	refused(me(aliceToken, "/product-access", "id:"+uuid.NewString()), http.StatusNotFound, "merchant_not_found")
	refused(me(aliceToken, "/product-access", "not a name!"), http.StatusBadRequest, "merchant_selector_invalid")
	require.Equal(t, http.StatusOK, me(aliceToken, "/product-access", retired.slug).Code)
	result, err := cp.RetireUnusedMerchant(ctx, retired.id, retired.group)
	require.NoError(t, err)
	require.True(t, result.Retired)
	refused(me(aliceToken, "/product-access", retired.slug), http.StatusNotFound, "merchant_not_found")
	refused(me(aliceToken, "/product-access", "id:"+retired.id.String()), http.StatusNotFound, "merchant_not_found")

	// The host's Auth checks the relationship against the bound merchant:
	// Carol is the second merchant's customer, not the first's.
	require.Equal(t, http.StatusOK, me(carolToken, "/product-access", second.slug).Code)
	w = me(carolToken, "/product-access", first.slug)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())

	// Bob, under every selector, never sees or touches Alice's billing.
	for _, m := range []shop{first, second} {
		for _, selector := range []string{m.slug, "id:" + m.id.String()} {
			require.NotContains(t, held(me(bobToken, "/product-access", selector)), passes[m.id])
			body := read(me(bobToken, "/entitlements", selector))
			require.Contains(t, body, bobKeys[m.id])
			require.NotContains(t, body, ":alice")
			body = read(me(bobToken, "/notifications", selector))
			for _, note := range notes {
				require.NotContains(t, body, note)
			}
			ids := make([]string, 0, len(notes))
			for _, note := range notes {
				ids = append(ids, note)
			}
			for id, marked := range markRead(bobToken, selector, ids...) {
				require.Nil(t, marked, "Bob marked Alice's %s", id)
			}
		}
		// Alice's own object at one merchant does not exist at another.
		require.Nil(t, markRead(aliceToken, other(m, first, second).slug, notes[m.id])[notes[m.id]])
	}
	for _, m := range []shop{first, second} {
		require.JSONEq(t, `{"unread_count":1}`, read(me(aliceToken, "/notifications/unread-count", m.slug)), "Alice's notifications are unread")
	}

	// The reader is false where OpenRails bound nothing.
	w = do(api, aliceToken, http.MethodGet, "/host/merchant", first.slug)
	require.Equal(t, http.StatusNoContent, w.Code)
}

func other[T comparable](m, a, b T) T {
	if m == a {
		return b
	}
	return a
}
