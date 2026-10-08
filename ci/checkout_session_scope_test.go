//go:build e2e && integration

package ci_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/checkoutsession"
	"github.com/stretchr/testify/require"
)

type refuseCheckoutProvider struct{ calls atomic.Int32 }

func (p *refuseCheckoutProvider) RoundTrip(*http.Request) (*http.Response, error) {
	p.calls.Add(1)
	return nil, context.Canceled
}

// A shared server has no configured merchant. Its opaque checkout credential
// must select the stored book before tenant connection setup, including on pay.
func TestCheckoutSessionResolvesStoredMerchant(t *testing.T) {
	f := newFixture(t)
	provider := &refuseCheckoutProvider{}
	cp := f.attachControlPlane(t, func(_ *openrails.Config, deps *openrails.Deps) { deps.StripeTransport = provider })
	handler, err := standaloneHandler(cp)
	require.NoError(t, err)
	rt := engine.Graph(cp).Runtime
	var mids []billing.MerchantID
	var prices []billing.PriceID
	var slugs []string
	buyer := billing.CustomerID(uuid.New())
	for _, name := range []string{"private-first", "private-second"} {
		slug := uniqueName(name)
		entry, err := cp.ProvisionMerchant(t.Context(), billing.ProvisionMerchantParams{Slug: slug, DisplayName: name})
		require.NoError(t, err)
		mid := entry.MerchantID
		product, err := cp.CreateProduct(t.Context(), billing.CreateProductParams{Key: "scope", DisplayName: name}, openrails.ForMerchantID(mid))
		require.NoError(t, err)
		price, err := cp.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "scope", Currency: "USD", UnitAmount: 123_000_000}, openrails.ForMerchantID(mid))
		require.NoError(t, err)
		_, err = cp.EnsureCustomer(t.Context(), buyer, billing.EnsureCustomerParams{}, openrails.ForMerchantID(mid))
		require.NoError(t, err)
		slugs = append(slugs, slug)
		mids = append(mids, mid)
		prices = append(prices, price.ID)
	}
	id, err := checkoutsession.NewID()
	require.NoError(t, err)
	plan, err := checkoutsession.NewPlan("private-offer", 123_000_000, "USD", nil, nil)
	require.NoError(t, err)
	// This is the capability store boundary, not a fabricated payment. Actual
	// provider-backed native checkout is exercised by the SaaS adoption journey.
	mint := func(mid billing.MerchantID, price billing.PriceID, expires time.Time) {
		require.NoError(t, rt.CheckoutSessions.Create(merchant.WithID(t.Context(), mid), id, checkoutsession.Session{CustomerID: buyer.UUID(), PriceID: price.UUID(), ExpiresAt: expires, Offer: checkoutsession.Offer{MerchantDisplayName: "private-first", Plan: plan, DueToday: 123_000_000}}, rt.Clock.Now()))
	}
	mint(mids[0], prices[0], rt.Clock.Now().Add(time.Hour))
	requestCount := 0
	request := func(method, capability, selector string, ctx context.Context) *httptest.ResponseRecorder {
		t.Helper()
		path := "/v1/checkout-sessions/" + capability
		if method == http.MethodPost {
			path += "/pay"
		}
		req := httptest.NewRequest(method, path, strings.NewReader(`{"option_id":"missing"}`)).WithContext(ctx)
		requestCount++
		req.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", requestCount)
		req.Header.Set("Content-Type", "application/json")
		if selector != "" {
			req.Header.Set(merchant.SelectorHeader, selector)
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	good := request(http.MethodGet, id, "", t.Context())
	require.Equal(t, http.StatusOK, good.Code, good.Body.String())
	var document checkoutsession.CheckoutSession
	require.NoError(t, json.Unmarshal(good.Body.Bytes(), &document))
	require.Equal(t, "private-first", document.Merchant.DisplayName)
	require.Equal(t, id, document.ID)
	require.Equal(t, 123_000_000, int(*document.DueToday))
	good = request(http.MethodGet, id, "id:"+mids[0].String(), t.Context())
	require.Equal(t, http.StatusOK, good.Code, good.Body.String())
	good = request(http.MethodGet, id, slugs[0], t.Context())
	require.Equal(t, http.StatusOK, good.Code, good.Body.String())
	paid := request(http.MethodPost, id, "", t.Context())
	require.Equal(t, http.StatusUnprocessableEntity, paid.Code, paid.Body.String())
	require.Contains(t, paid.Body.String(), "checkout_request_invalid", "valid capability reached the payment request validator")
	unknown, err := checkoutsession.NewID()
	require.NoError(t, err)
	changed := id[:len(id)-1] + "0"
	if changed == id {
		changed = id[:len(id)-1] + "1"
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, tc := range []struct {
			name, id, selector string
			ctx                context.Context
		}{
			{"malformed", "ocs_short", "", t.Context()},
			{"unknown", unknown, "", t.Context()},
			{"wrong secret", changed, "", t.Context()},
			{"other selector", id, "id:" + mids[1].String(), t.Context()},
			{"other name", id, slugs[1], t.Context()},
			{"malformed selector", id, "invalid:selector", t.Context()},
			{"other host", id, "", merchant.WithHostMerchant(t.Context(), mids[1])},
			{"other pin", id, "", merchant.WithID(t.Context(), mids[1])},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				res := request(method, tc.id, tc.selector, tc.ctx)
				require.Equal(t, http.StatusNotFound, res.Code, res.Body.String())
				require.Contains(t, res.Body.String(), "checkout_session_not_found")
				require.Equal(t, "no-store", res.Header().Get("Cache-Control"))
				for _, secret := range []string{id, buyer.String(), mids[0].String(), "private-first", "123000000"} {
					require.NotContains(t, res.Body.String(), secret)
				}
			})
		}
	}
	// Retained rows cease to authenticate when their merchant is deleted.
	_, err = f.pool.Exec(t.Context(), `UPDATE `+f.schema+`.merchants SET status='deleted' WHERE id=$1`, mids[0].UUID())
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, request(http.MethodGet, id, "", t.Context()).Code)
	_, err = f.pool.Exec(t.Context(), `UPDATE `+f.schema+`.merchants SET status='active' WHERE id=$1`, mids[0].UUID())
	require.NoError(t, err)
	rt.SetConfiguredMerchant(mids[1])
	require.Equal(t, http.StatusNotFound, request(http.MethodGet, id, "", t.Context()).Code)
	rt.SetConfiguredMerchant(billing.MerchantID{})
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	unavailable := request(http.MethodGet, id, "", canceled)
	require.Equal(t, http.StatusServiceUnavailable, unavailable.Code, unavailable.Body.String())
	require.NotContains(t, unavailable.Body.String(), id)
	// A hash collision/imported duplicate never chooses a book, even when the
	// caller supplies a matching selector for one of the candidate merchants.
	mint(mids[1], prices[1], rt.Clock.Now().Add(time.Hour))
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		res := request(method, id, "id:"+mids[0].String(), t.Context())
		require.Equal(t, http.StatusNotFound, res.Code, res.Body.String())
	}
	id, err = checkoutsession.NewID()
	require.NoError(t, err)
	mint(mids[0], prices[0], rt.Clock.Now().Add(-checkoutsession.ReconciliationWindow-time.Hour))
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		require.Equal(t, http.StatusNotFound, request(method, id, "", t.Context()).Code, "past-retention capability no longer resolves")
	}
	require.Zero(t, provider.calls.Load(), "refused capabilities never reach a provider")
	var count int
	require.NoError(t, f.pool.QueryRow(t.Context(), `SELECT count(*) FROM `+f.schema+`.payments`).Scan(&count))
	require.Zero(t, count)
	require.False(t, bytes.Contains(good.Body.Bytes(), []byte(mids[0].String())), "the capability document need not reveal an internal merchant ID")
}
