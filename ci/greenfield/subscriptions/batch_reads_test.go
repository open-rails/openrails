//go:build greenfield && integration

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

	"github.com/open-rails/openrails"
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
	var prices []*openrails.Price
	for i := range 3 {
		price := w.membership(fmt.Sprintf("content:batch-%d", i), int64(1_000_000*(i+1)))
		prices = append(prices, price)
		c.subscribeAgain(embedded, "stripe", price.ID, fmt.Sprintf("content:batch-%d", i), method)
	}

	for _, tp := range []topology{embedded, remote} {
		var subs *openrails.Page[openrails.Subscription]
		got := w.count(func() {
			var err error
			subs, err = w.client[tp].ListSubscriptions(t.Context(), openrails.SubscriptionFilter{CustomerID: c.id})
			require.NoError(t, err)
		})
		requireBatched(t, got, "ListPricesWithProductByIDs")
		require.Len(t, subs.Data, 3)
		requireEnriched(t, prices, subs.Data)
	}

	var mine []openrails.Subscription
	got := w.count(func() { mine = decodeSubs(t, c.must(http.MethodGet, "/subscriptions", "", nil)) })
	requireBatched(t, got, "ListPricesWithProductByIDs")
	require.Len(t, mine, 3)
	requireEnriched(t, prices, mine)

	var owned []*openrails.Price
	for i := range 3 {
		owned = append(owned, w.permanent(fmt.Sprintf("post:batch-%d", i)))
		c.buy(owned[i].ID, fmt.Sprintf("post:batch-%d", i), method)
	}
	var access *openrails.ProductAccessList
	got = w.count(func() {
		var err error
		access, err = w.client[remote].ProductAccess.List(t.Context(), &openrails.ProductAccessListParams{CustomerID: c.id})
		require.NoError(t, err)
	})
	requireBatched(t, got, "ListProductsByIDs")
	require.Len(t, access.Data, 3)
	names := map[string]string{}
	for _, grant := range access.Data {
		names[grant.ProductID] = grant.ProductName
		require.NotEmpty(t, grant.ProductKey)
	}
	for _, price := range owned {
		require.Equal(t, "Post", names[price.ProductID])
	}
}

func (w *world) permanent(entitlement string) *openrails.Price {
	w.t.Helper()
	client := w.client[embedded]
	product, err := client.Products.Create(w.t.Context(), &openrails.ProductCreateParams{Key: "post-" + uuidShort(), DisplayName: "Post", EntitlementsSpec: map[string]*int{entitlement: nil}})
	require.NoError(w.t, err)
	price, err := client.Prices.Create(w.t.Context(), &openrails.PriceCreateParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 2_000_000, Currency: "USD"})
	require.NoError(w.t, err)
	return price
}

// buy completes a permanent purchase through checkout, as enrollment does.
func (c *customer) buy(priceID, entitlement, method string) {
	c.w.t.Helper()
	c.purchase(openrails.OfferPermanent, priceID, entitlement, method)
	require.True(c.w.t, c.entitled(entitlement))
}

func (c *customer) purchase(kind openrails.OfferKind, priceID, entitlement, method string) {
	c.w.t.Helper()
	session, err := c.w.client[embedded].CreateCheckoutSession(c.w.t.Context(), openrails.CreateCheckoutSessionRequest{
		OfferKind: kind, Customer: openrails.CheckoutCustomerIdentity{ID: c.id}, Entitlement: entitlement, PriceID: priceID,
		IdempotencyKey: "buy-" + uuid.NewString(), PaymentOptions: openrails.CheckoutPaymentOptions{PSPID: c.w.psp["stripe"], Rail: "stripe", PaymentMethodID: method},
		SuccessURL: "https://greenfield.test/return", CancelURL: "https://greenfield.test/return?canceled=1",
	})
	require.NoError(c.w.t, err)
	done := unwrap(c.must(http.MethodPost, fmt.Sprintf("/checkout/%s/confirm", session.ID), "", map[string]any{"payment": map[string]string{"rail": "stripe"}}))
	require.Equal(c.w.t, "succeeded", done["status"], "%v", done)
	c.w.settle()
}

func uuidShort() string { return uuid.NewString()[:8] }

func decodeSubs(t *testing.T, body map[string]any) []openrails.Subscription {
	t.Helper()
	raw, err := json.Marshal(body["data"])
	require.NoError(t, err)
	var out []openrails.Subscription
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func requireEnriched(t *testing.T, prices []*openrails.Price, subs []openrails.Subscription) {
	t.Helper()
	byID := map[string]*openrails.Price{}
	for _, price := range prices {
		byID[price.ID] = price
	}
	for _, sub := range subs {
		price := byID[sub.PriceID]
		require.NotNil(t, price, "subscription %s price %s", sub.ID, sub.PriceID)
		require.NotNil(t, sub.Price, "price enriched")
		require.NotNil(t, sub.Product, "product enriched")
		require.Equal(t, price.ProductID, sub.Product.ID)
		require.Equal(t, "Membership", sub.Product.DisplayName)
	}
}

func TestCheckoutCoverageIsOneQuery(t *testing.T) {
	w := newWorld(t)
	client := w.client[embedded]
	hours := 48
	keys := map[string]*int{"content:cov-a": nil, "content:cov-b": nil, "content:cov-c": nil}
	product, err := client.Products.Create(t.Context(), &openrails.ProductCreateParams{Key: "bundle-" + uuidShort(), DisplayName: "Bundle", EntitlementsSpec: keys})
	require.NoError(t, err)
	price, err := client.Prices.Create(t.Context(), &openrails.PriceCreateParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 5_000_000, Currency: "USD", AccessDurationHours: &hours})
	require.NoError(t, err)
	c := w.newCustomer()
	now := w.clock.Now()
	for key, days := range map[string]int{"content:cov-a": 5, "content:cov-c": 10} {
		end := now.Add(time.Duration(days) * 24 * time.Hour)
		_, err := client.GrantEntitlement(t.Context(), c.id, openrails.GrantEntitlementRequest{Entitlement: key, EndAt: &end})
		require.NoError(t, err)
	}
	method := c.saveCard("stripe", visa)
	got := w.count(func() { c.purchase(openrails.OfferFinite, price.ID, "content:cov-b", method) })
	require.Equal(t, 1, got["EntitlementCoverage"], "one coverage query for every product key: %v", got)
	require.Zero(t, got["EntitlementHasActiveIndefinite"], "no per-key coverage reads: %v", got)
	// The rental stacks after the latest finite window across all product keys.
	start := now.Add(10 * 24 * time.Hour)
	records, err := client.ListEntitlements(t.Context(), c.id, start.Add(time.Hour))
	require.NoError(t, err)
	rented := map[string]bool{}
	for _, r := range records {
		if r.SourceType == "one_off" {
			rented[r.Entitlement] = true
			require.True(t, start.Equal(r.StartAt), "%s starts %s", r.Entitlement, r.StartAt)
			require.True(t, start.Add(48*time.Hour).Equal(*r.EndAt), "%s ends %v", r.Entitlement, r.EndAt)
		}
	}
	require.Len(t, rented, 3)
}

func TestListOffersForEntitlementsIsOneRequest(t *testing.T) {
	w := newWorld(t)
	client := w.client[embedded]
	hours := 48
	create := func(key string, spec map[string]*int, currency string, amount int64, duration *int) *openrails.Price {
		product, err := client.Products.Create(t.Context(), &openrails.ProductCreateParams{Key: key, DisplayName: key, EntitlementsSpec: spec})
		require.NoError(t, err)
		price, err := client.Prices.Create(t.Context(), &openrails.PriceCreateParams{ProductID: product.ID, Key: key + "-" + strings.ToLower(currency), UnitAmount: amount, Currency: currency, AccessDurationHours: duration})
		require.NoError(t, err)
		return price
	}
	postA := create("post-a-"+uuidShort(), map[string]*int{"post:a": nil}, "USD", 1_000_000, nil)
	bundleUSD := create("bundle-"+uuidShort(), map[string]*int{"post:a": nil, "post:b": nil}, "USD", 3_000_000, nil)
	bundleEUR := create("bundle-eur-"+uuidShort(), map[string]*int{"post:a": nil, "post:b": nil}, "EUR", 2_500_000, nil)
	create("rental-"+uuidShort(), map[string]*int{"post:b": nil}, "USD", 500_000, &hours)

	for _, tp := range []topology{embedded, remote} {
		var offers map[string]openrails.OfferList
		got := w.count(func() {
			var err error
			offers, err = w.client[tp].ListOffersForEntitlements(t.Context(), []string{"post:a", "post:b", "post:none", "post:a"}, openrails.OfferListParams{Kind: openrails.OfferPermanent, PreferredCurrency: "EUR", Limit: 2})
			require.NoError(t, err)
		})
		require.Equal(t, 1, got["ListOffersForEntitlements"], "one query for every key: %v", got)
		require.Len(t, offers, 3)
		require.Empty(t, offers["post:none"].Data)
		require.False(t, offers["post:none"].HasMore)

		a := offers["post:a"]
		require.Len(t, a.Data, 2)
		require.True(t, a.HasMore)
		require.Equal(t, bundleEUR.ID, a.Data[0].PriceID, "preferred currency first")
		require.Equal(t, "EUR", a.Data[0].Currency)
		require.Equal(t, openrails.OfferPermanent, a.Data[0].Kind)
		b := offers["post:b"]
		require.Len(t, b.Data, 2, "finite rental excluded from permanent offers")
		require.False(t, b.HasMore)
		require.Equal(t, bundleEUR.ID, b.Data[0].PriceID)
		require.Equal(t, bundleUSD.ID, b.Data[1].PriceID)

		next, err := w.client[tp].ListOffersForEntitlements(t.Context(), []string{"post:a", "post:b"}, openrails.OfferListParams{Kind: openrails.OfferPermanent, PreferredCurrency: "EUR", Limit: 2, Cursors: map[string]string{"post:a": a.NextCursor}})
		require.NoError(t, err)
		seen := map[string]bool{a.Data[0].PriceID: true, a.Data[1].PriceID: true}
		require.Len(t, next["post:a"].Data, 1)
		require.False(t, next["post:a"].HasMore)
		seen[next["post:a"].Data[0].PriceID] = true
		require.Equal(t, map[string]bool{postA.ID: true, bundleUSD.ID: true, bundleEUR.ID: true}, seen)
		require.Len(t, next["post:b"].Data, 2, "keys without a cursor start at their first page")

		_, err = w.client[tp].ListOffersForEntitlements(t.Context(), []string{"post:b"}, openrails.OfferListParams{Kind: openrails.OfferPermanent, PreferredCurrency: "EUR", Cursors: map[string]string{"post:b": a.NextCursor}})
		require.ErrorIs(t, err, openrails.ErrInvalid, "a cursor is bound to its key")

		finite, err := w.client[tp].ListOffersForEntitlements(t.Context(), []string{"post:b"}, openrails.OfferListParams{Kind: openrails.OfferFinite})
		require.NoError(t, err)
		require.Len(t, finite["post:b"].Data, 1)
		require.Equal(t, 48, *finite["post:b"].Data[0].AccessDurationHours)
	}

	tooMany := make([]string, openrails.MaxEntitlementChecks+1)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("post:%d", i)
	}
	_, err := client.ListOffersForEntitlements(t.Context(), tooMany, openrails.OfferListParams{Kind: openrails.OfferPermanent})
	require.Error(t, err)
}
