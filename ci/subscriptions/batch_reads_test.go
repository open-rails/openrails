//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// queryLog is a pgx tracer counting sqlc statements by their "-- name:".
type queryLog struct {
	mu    sync.Mutex
	on    bool
	names map[string]int
}

func (l *queryLog) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	_, rest, ok := strings.Cut(data.SQL, "-- name: ")
	if !ok {
		return ctx
	}
	name, _, _ := strings.Cut(rest, " ")
	l.mu.Lock()
	if l.on {
		l.names[name]++
	}
	l.mu.Unlock()
	return ctx
}

func (*queryLog) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// count runs fn and returns how often each named statement ran meanwhile.
func (w *world) count(fn func()) map[string]int {
	w.t.Helper()
	w.queries.mu.Lock()
	w.queries.on, w.queries.names = true, map[string]int{}
	w.queries.mu.Unlock()
	fn()
	w.queries.mu.Lock()
	defer w.queries.mu.Unlock()
	w.queries.on = false
	return w.queries.names
}

var perRowReads = []string{"GetPriceByID", "GetProductByID", "EntitlementHasActiveIndefinite"}

func requireBatched(t *testing.T, got map[string]int, batch string) {
	t.Helper()
	require.Equal(t, 1, got[batch], "one %s per page: %v", batch, got)
	for _, name := range perRowReads {
		require.Zero(t, got[name], "no per-row %s: %v", name, got)
	}
}

func TestListReadsAreBatched(t *testing.T) {
	w := newWorld(t)
	c := w.newCustomer()
	method := c.saveCard("stripe", visa)
	var prices []*billing.Price
	for i := range 3 {
		price := w.membership(fmt.Sprintf("content:batch-%d", i), int64(1_000_000*(i+1)))
		prices = append(prices, price)
		c.subscribeAgain(embedded, "stripe", price.ID.String(), fmt.Sprintf("content:batch-%d", i), method)
	}

	for _, tp := range []topology{embedded, remote} {
		var subs *billing.ListPage[billing.Subscription]
		got := w.count(func() {
			var err error
			subs, err = w.client[tp].ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: c.customerID()})
			require.NoError(t, err)
		})
		requireBatched(t, got, "ListPricesWithProductByIDs")
		require.Len(t, subs.Items, 3)
		requireEnriched(t, prices, subs.Items)
	}

	var mine []billing.Subscription
	got := w.count(func() { mine = decodeSubs(t, c.must(http.MethodGet, "/subscriptions", "", nil)) })
	requireBatched(t, got, "ListPricesWithProductByIDs")
	require.Len(t, mine, 3)
	requireEnriched(t, prices, mine)

	var owned []*billing.Price
	for i := range 3 {
		owned = append(owned, w.permanent(fmt.Sprintf("post:batch-%d", i)))
		c.buy(owned[i].ID.String(), fmt.Sprintf("post:batch-%d", i), method)
	}
	var access *billing.ListPage[billing.ProductAccessGrant]
	got = w.count(func() {
		var err error
		access, err = w.client[remote].ListProductAccess(t.Context(), c.customerID(), billing.ProductAccessListParams{})
		require.NoError(t, err)
	})
	requireBatched(t, got, "ListProductAccessPage")
	names := map[string]string{}
	purchases := 0
	for _, grant := range access.Items {
		names[grant.ProductID.String()] = grant.ProductName
		require.NotEmpty(t, grant.ProductKey)
		if grant.SourceType == billing.ProductAccessSourcePurchase {
			purchases++
		}
	}
	require.Equal(t, 3, purchases, "the page holds the purchases beside the memberships' windows")
	for _, price := range owned {
		require.Equal(t, "Post", names[price.ProductID.String()])
	}
}

func (w *world) permanent(entitlement string) *billing.Price {
	w.t.Helper()
	client := w.client[embedded]
	product, err := client.CreateProduct(w.t.Context(), billing.CreateProductParams{Key: "post-" + uuidShort(), DisplayName: "Post", Entitlements: []string{entitlement}})
	require.NoError(w.t, err)
	price, err := client.CreatePrice(w.t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 2_000_000, Currency: "USD"})
	require.NoError(w.t, err)
	return price
}

// buy completes a permanent purchase through checkout, as enrollment does.
func (c *customer) buy(priceID, entitlement, method string) {
	c.w.t.Helper()
	c.purchase(priceID, method)
	require.True(c.w.t, c.entitled(entitlement))
}

// purchase pays a session for the price with a saved Stripe card.
func (c *customer) purchase(priceID, method string) {
	c.w.t.Helper()
	c.mustCheckout(embedded, order{price: pid(priceID), rail: "stripe", method: method, successURL: "https://e2e.test/return"})
}

func uuidShort() string { return uuid.NewString()[:8] }

func decodeSubs(t *testing.T, body map[string]any) []billing.Subscription {
	t.Helper()
	raw, err := json.Marshal(body["data"])
	require.NoError(t, err)
	var out []billing.Subscription
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func requireEnriched(t *testing.T, prices []*billing.Price, subs []billing.Subscription) {
	t.Helper()
	byID := map[billing.PriceID]*billing.Price{}
	for _, price := range prices {
		byID[price.ID] = price
	}
	for _, sub := range subs {
		price := byID[sub.PriceID]
		require.NotNil(t, price, "subscription %s price %s", sub.ID, sub.PriceID)
		require.NotNil(t, sub.Price, "price enriched")
		require.Equal(t, price.ID, sub.Price.ID)
		require.NotNil(t, sub.Product, "product enriched")
		require.Equal(t, price.ProductID, sub.Product.ID)
		require.Equal(t, "Membership", sub.Product.DisplayName)
	}
}

func TestCheckoutCoverageIsOneQuery(t *testing.T) {
	w := newWorld(t)
	client := w.client[embedded]
	hours := 48
	keys := []string{"content:cov-a", "content:cov-b", "content:cov-c"}
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "bundle-" + uuidShort(), DisplayName: "Bundle", Entitlements: keys})
	require.NoError(t, err)
	price, err := client.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 5_000_000, Currency: "USD", AccessDurationHours: &hours})
	require.NoError(t, err)
	c := w.newCustomer()
	now := w.clock.Now()
	for _, days := range []int{5, 10} {
		end := now.Add(time.Duration(days) * 24 * time.Hour)
		_, err := client.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{{CustomerID: c.customerID(), ProductID: product.ID, EndsAt: &end}}})
		require.NoError(t, err)
	}
	method := c.saveCard("stripe", visa)
	got := w.count(func() { c.purchase(price.ID.String(), method) })
	require.Equal(t, 1, got["ProductAccessCoverage"], "one coverage query for the product: %v", got)
	// The rental stacks after the customer's latest window of the product.
	start := now.Add(10 * 24 * time.Hour)
	access, err := client.ListProductAccess(t.Context(), c.customerID(), billing.ProductAccessListParams{})
	require.NoError(t, err)
	var rented []billing.ProductAccessGrant
	for _, a := range access.Items {
		if a.SourceType == billing.ProductAccessSourcePurchase {
			rented = append(rented, a)
		}
	}
	require.Len(t, rented, 1)
	require.True(t, start.Equal(rented[0].StartsAt), "starts %s", rented[0].StartsAt)
	require.True(t, start.Add(48*time.Hour).Equal(*rented[0].EndsAt), "ends %v", rented[0].EndsAt)
	held, err := client.CheckEntitlements(t.Context(), c.customerID(), billing.CheckEntitlementsParams{Entitlements: keys, At: start.Add(time.Hour)})
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"content:cov-a": true, "content:cov-b": true, "content:cov-c": true}, held.Entitlements)
}

func TestListOffersIsOneRequest(t *testing.T) {
	w := newWorld(t)
	client := w.client[embedded]
	hours := 48
	create := func(key string, spec []string, currency string, amount int64, duration *int) *billing.Price {
		product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, Entitlements: spec})
		require.NoError(t, err)
		price, err := client.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: key + "-" + strings.ToLower(currency), UnitAmount: amount, Currency: currency, AccessDurationHours: duration})
		require.NoError(t, err)
		return price
	}
	postA := create("post-a-"+uuidShort(), []string{"post:a"}, "USD", 1_000_000, nil)
	bundleUSD := create("bundle-"+uuidShort(), []string{"post:a", "post:b"}, "USD", 3_000_000, nil)
	bundleEUR := create("bundle-eur-"+uuidShort(), []string{"post:a", "post:b"}, "EUR", 2_500_000, nil)
	create("rental-"+uuidShort(), []string{"post:b"}, "USD", 500_000, &hours)

	for _, tp := range []topology{embedded, remote} {
		var offers billing.OfferPages
		got := w.count(func() {
			var err error
			offers, err = w.client[tp].ListOffers(t.Context(), billing.OfferListParams{Entitlements: []string{"post:a", "post:b", "post:none", "post:a"}, Kind: billing.OfferPermanent, PreferredCurrency: "EUR", Limit: 2})
			require.NoError(t, err)
		})
		require.Equal(t, 1, got["ListOffersForEntitlements"], "one query for every key: %v", got)
		require.Len(t, offers, 3)
		require.Empty(t, offers["post:none"].Items)
		require.Empty(t, offers["post:none"].Next)

		a := offers["post:a"]
		require.Len(t, a.Items, 2)
		require.NotEmpty(t, a.Next)
		require.Equal(t, bundleEUR.ID, a.Items[0].PriceID, "preferred currency first")
		require.Equal(t, "EUR", a.Items[0].Currency)
		require.Equal(t, billing.OfferPermanent, a.Items[0].Kind)
		b := offers["post:b"]
		require.Len(t, b.Items, 2, "finite rental excluded from permanent offers")
		require.Empty(t, b.Next)
		require.Equal(t, bundleEUR.ID, b.Items[0].PriceID)
		require.Equal(t, bundleUSD.ID, b.Items[1].PriceID)

		next, err := w.client[tp].ListOffers(t.Context(), billing.OfferListParams{Entitlements: []string{"post:a", "post:b"}, Kind: billing.OfferPermanent, PreferredCurrency: "EUR", Limit: 2, Cursors: map[string]string{"post:a": a.Next}})
		require.NoError(t, err)
		seen := map[billing.PriceID]bool{a.Items[0].PriceID: true, a.Items[1].PriceID: true}
		require.Len(t, next["post:a"].Items, 1)
		require.Empty(t, next["post:a"].Next)
		seen[next["post:a"].Items[0].PriceID] = true
		require.Equal(t, map[billing.PriceID]bool{postA.ID: true, bundleUSD.ID: true, bundleEUR.ID: true}, seen)
		require.Len(t, next["post:b"].Items, 2, "keys without a cursor start at their first page")

		_, err = w.client[tp].ListOffers(t.Context(), billing.OfferListParams{Entitlements: []string{"post:b"}, Kind: billing.OfferPermanent, PreferredCurrency: "EUR", Cursors: map[string]string{"post:b": a.Next}})
		require.ErrorIs(t, err, billing.ErrInvalid, "a cursor is bound to its key")

		finite, err := w.client[tp].ListOffers(t.Context(), billing.OfferListParams{Entitlements: []string{"post:b"}, Kind: billing.OfferFinite})
		require.NoError(t, err)
		require.Len(t, finite["post:b"].Items, 1)
		require.Equal(t, 48, *finite["post:b"].Items[0].AccessDurationHours)
	}

	tooMany := make([]string, billing.MaxEntitlementChecks+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("post:%d", i)
	}
	_, err := client.ListOffers(t.Context(), billing.OfferListParams{Entitlements: tooMany, Kind: billing.OfferPermanent})
	require.Error(t, err)
}
