//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

// trailWorld is an import world whose providers also send their webhooks:
// Stripe through the fake's own signed delivery, NMI as signed notices.
type trailWorld struct {
	*importWorld
}

const (
	kindConverge = "openrails.subscription_converge"
	kindObserve  = "openrails.provider_object_observe"
)

// newTrailWorld is a trail world whose PSPs' onboarding pulls ran: what they
// hold from then on is not their backlog.
func newTrailWorld(t *testing.T, psps ...string) *trailWorld {
	t.Helper()
	return trail(newImportWorld(t, psps...))
}

// bootTrailWorld is a trail world whose fleet has not started: its PSPs'
// onboarding pulls are queued.
func bootTrailWorld(t *testing.T, psps ...string) *trailWorld {
	t.Helper()
	return trail(bootImportWorld(t, psps...))
}

func trail(iw *importWorld) *trailWorld {
	tw := &trailWorld{importWorld: iw}
	if tw.stripe != nil {
		tw.stripe.SendWebhooksTo(tw.server.URL+"/v1/webhooks/stripe/acct_import", "whsec_import")
	}
	return tw
}

// settle runs every queued job of kinds now, instead of after its debounce,
// and waits until none is left to run.
func settle(t *testing.T, fleet *river.Client[pgx.Tx], kinds ...string) []*rivertype.JobRow {
	t.Helper()
	var jobs []*rivertype.JobRow
	require.Eventually(t, func() bool {
		res, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds(kinds...).First(200))
		if err != nil {
			return false
		}
		jobs = res.Jobs
		for _, j := range jobs {
			switch j.State {
			case rivertype.JobStateScheduled, rivertype.JobStateRetryable:
				_, _ = fleet.JobRetry(t.Context(), j.ID)
				return false
			case rivertype.JobStateAvailable, rivertype.JobStateRunning, rivertype.JobStatePending:
				return false
			}
		}
		return true
	}, 60*time.Second, 100*time.Millisecond)
	return jobs
}

// completedOnce asserts every job of kinds completed on its first attempt:
// nothing burned River retries.
func completedOnce(t *testing.T, fleet *river.Client[pgx.Tx], kinds ...string) []*rivertype.JobRow {
	t.Helper()
	jobs := settle(t, fleet, kinds...)
	require.NotEmpty(t, jobs)
	for _, j := range jobs {
		require.Equal(t, rivertype.JobStateCompleted, j.State, "%s %d: %v", j.Kind, j.ID, j.Errors)
		require.Equal(t, 1, j.Attempt, "%s %d ran more than once: %v", j.Kind, j.ID, j.Errors)
	}
	return jobs
}

func (tw *trailWorld) send(t *testing.T, kind string, object stripemock.Object) string {
	t.Helper()
	id, err := tw.stripe.SendEvent(t.Context(), kind, object)
	require.NoError(t, err)
	return id
}

// invoicePaid is Stripe announcing the subscription's latest invoice paid.
func (tw *trailWorld) invoicePaid(t *testing.T, sub string) string {
	t.Helper()
	return tw.send(t, "invoice.paid", tw.stripe.Subscription(sub)["latest_invoice"].(stripemock.Object))
}

func (tw *trailWorld) parks(t *testing.T, params billing.ParkedSubscriptionListParams) []billing.ParkedSubscription {
	t.Helper()
	params.Limit = 100
	page, err := tw.alphaRemote.ListParkedSubscriptions(t.Context(), params)
	require.NoError(t, err)
	return page.Items
}

// findings are the open and auto-resolved findings of one type, by subject.
func (tw *trailWorld) findings(t *testing.T, kind string) map[string]billing.Finding {
	t.Helper()
	out := map[string]billing.Finding{}
	for _, status := range []billing.FindingStatus{"fixed", ""} {
		page, err := tw.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: kind, Status: status, PageRequest: billing.PageRequest{Limit: 100}})
		require.NoError(t, err)
		for _, f := range page.Items {
			out[f.SubjectKey] = f
		}
	}
	return out
}

// pulled is the PSP's onboarding pull of what it holds now: parks wait for
// one (the backlog rule).
func (tw *trailWorld) pulled(t *testing.T, key string) *billing.PSPImport {
	t.Helper()
	since := tw.clock.Now().Add(-24 * time.Hour)
	return tw.pull(t, billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: key}, HistorySince: &since})
}

func (tw *trailWorld) stripeProduct(t *testing.T, key, entitlement string) billing.ProductID {
	t.Helper()
	product, err := tw.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, TierGroup: new("membership"), Entitlements: []string{entitlement}})
	require.NoError(t, err)
	return product.ID
}

func (tw *trailWorld) mapStripe(t *testing.T, product billing.ProductID, key, ref string) *billing.Price {
	t.Helper()
	price, err := tw.alphaRemote.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product, Key: key, PSP: "stripe", PriceID: ref})
	require.NoError(t, err)
	return price
}

// nmiNotice posts a signed NMI webhook to server.
func nmiNotice(t *testing.T, server string, id, kind string, body map[string]any) {
	t.Helper()
	body["merchant"] = map[string]string{"id": "critical-import-gateway"}
	raw, err := json.Marshal(map[string]any{"event_id": id, "event_type": kind, "event_body": body})
	require.NoError(t, err)
	at := fmt.Sprint(time.Now().Unix())
	h := hmac.New(sha256.New, []byte("critical-import-webhook"))
	_, _ = h.Write([]byte(at + "."))
	_, _ = h.Write(raw)
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server+"/v1/webhooks/nmi/critical-import-gateway", bytes.NewReader(raw))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Webhook-Signature", "t="+at+",s="+hex.EncodeToString(h.Sum(nil)))
	res, err := http.DefaultClient.Do(r)
	require.NoError(t, err)
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	require.Equal(t, http.StatusOK, res.StatusCode, string(out))
}

// ownVault makes customer the owner of an NMI vault's card in OpenRails.
func (tw *trailWorld) ownVault(t *testing.T, customer billing.CustomerID, vault string) {
	t.Helper()
	_, err := tw.alpha.ImportBilling(t.Context(), billing.DeclaredBilling{AsOf: tw.clock.Now(), DefaultPSP: billing.PSPRef{Key: "nmi"},
		Customers:      []billing.DeclaredCustomer{{Customer: customer}},
		PaymentMethods: []billing.DeclaredPaymentMethod{{Customer: customer, Rail: "nmi", PSP: billing.PSPRef{Key: "nmi"}, RailCustomerRef: vault, RailMethodRef: tw.nmi.Vault(vault).BillingID}}})
	require.NoError(t, err)
}

// nmiSchedule is a live NMI schedule on plan billing vault, paid once.
func (tw *trailWorld) nmiSchedule(t *testing.T, vault, plan, amount string) string {
	t.Helper()
	start := tw.clock.Now().Add(-2 * 24 * time.Hour).Truncate(24 * time.Hour)
	id := tw.nmi.AddSchedule(nmimock.Schedule{Vault: vault, Plan: plan, Amount: amount, Days: 30, Created: start, NextBilling: start.AddDate(0, 0, 30), Order: "legacy-" + vault})
	tw.nmi.AddScheduleSale(id, start)
	return id
}

// paidAmounts are the customer's payment amounts in micros, ascending.
func paidAmounts(t *testing.T, tw *trailWorld, customer billing.CustomerID) []int64 {
	t.Helper()
	page, err := tw.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: customer})
	require.NoError(t, err)
	out := make([]int64, 0, len(page.Items))
	for _, p := range page.Items {
		out = append(out, p.Amount)
	}
	slices.Sort(out)
	return out
}

func openPark(t *testing.T, parks []billing.ParkedSubscription, ref string) billing.ParkedSubscription {
	t.Helper()
	for _, p := range parks {
		if p.SubscriptionRef == ref {
			return p
		}
	}
	t.Fatalf("no park for %s in %+v", ref, parks)
	return billing.ParkedSubscription{}
}

func TestProviderTrailing(t *testing.T) {
	t.Run("OR-J15-S08/stripe-price-event-raises-one-finding", func(t *testing.T) {
		tw := newTrailWorld(t, "stripe")
		tw.stripe.AddProduct("prod_pulled", "Pulled")
		tw.stripe.AddPrice(stripemock.Price{ID: "price_pulled", Product: "prod_pulled", Amount: 900, Interval: "month"})
		tw.pulled(t, "stripe")

		tw.stripe.AddProduct("prod_new", "New")
		tw.stripe.AddPrice(stripemock.Price{ID: "price_new", Product: "prod_new", Amount: 1200, Interval: "month"})
		created := tw.send(t, "price.created", stripemock.Object{"id": "price_new", "object": "price"})
		completedOnce(t, tw.fleet, kindObserve)
		catalog := tw.catalog(t, billing.ProviderCatalogListParams{PSP: billing.PSPRef{Key: "stripe"}})
		require.Equal(t, "webhook", catalog["price:price_new"].ObservedVia)
		require.Equal(t, "12.00", *catalog["price:price_new"].Terms.StatedAmount)
		unmapped := tw.findings(t, "catalog.price.unmapped")
		require.Len(t, unmapped, 1)
		finding := unmapped["price_new"]
		require.Equal(t, "medium", finding.Severity)
		require.Nil(t, finding.ResolvedAt)
		require.Equal(t, finding.ID, *catalog["price:price_new"].Finding)

		require.NoError(t, tw.stripe.Redeliver(t.Context(), created))
		for range 3 {
			tw.send(t, "price.updated", stripemock.Object{"id": "price_new", "object": "price"})
		}
		jobs := completedOnce(t, tw.fleet, kindObserve)
		require.Len(t, jobs, 2, "the redelivery is deduplicated and the burst coalesces into one read")
		unmapped = tw.findings(t, "catalog.price.unmapped")
		require.Len(t, unmapped, 1, "a redelivery and a burst add nothing")
		require.Equal(t, finding.ID, unmapped["price_new"].ID)

		tw.stripe.SetPriceActive("price_new", false)
		tw.send(t, "price.updated", stripemock.Object{"id": "price_new", "object": "price"})
		completedOnce(t, tw.fleet, kindObserve)
		require.NotNil(t, tw.findings(t, "catalog.price.unmapped")["price_new"].ResolvedAt, "a retired price with no parks is resolved")

		tw.stripe.SetPriceActive("price_pulled", false)
		tw.send(t, "price.updated", stripemock.Object{"id": "price_pulled", "object": "price"})
		tw.stripe.SetPriceActive("price_pulled", true)
		tw.send(t, "price.updated", stripemock.Object{"id": "price_pulled", "object": "price"})
		completedOnce(t, tw.fleet, kindObserve)
		require.NotContains(t, tw.findings(t, "catalog.price.unmapped"), "price_pulled", "the pull's backlog is the import's to list")
		require.Equal(t, "webhook", tw.catalog(t, billing.ProviderCatalogListParams{PSP: billing.PSPRef{Key: "stripe"}})["price:price_pulled"].ObservedVia)
	})

	t.Run("OR-J15-S09/stripe-unmapped-signup-parks-then-lands-once", func(t *testing.T) {
		tw := newTrailWorld(t, "stripe")
		tw.stripe.AddProduct("prod_gold", "Gold")
		tw.stripe.AddPrice(stripemock.Price{ID: "price_gold", Product: "prod_gold", Amount: 1500, Interval: "month"})
		start := tw.clock.Now().Add(-time.Hour)
		customer := tw.stripe.AddCustomer(tw.alice.Email, "Alice", nil)
		sub := tw.stripe.AddSubscription(customer, "pm_alice", "price_gold", 1500, start, start.AddDate(0, 1, 0))
		tw.stripe.TagSubscription(sub, "user_id", tw.alice.ID, start)
		event := tw.invoicePaid(t, sub)
		completedOnce(t, tw.fleet, kindConverge)

		parks := tw.parks(t, billing.ParkedSubscriptionListParams{})
		require.Len(t, parks, 1)
		park := openPark(t, parks, sub)
		require.Equal(t, billing.ParkPriceUnmapped, park.Reason)
		require.Equal(t, "price_gold", *park.PriceRef)
		require.Nil(t, park.Subscription)
		finding := tw.findings(t, "catalog.price.unmapped")["price_gold"]
		require.Equal(t, "high", finding.Severity)
		require.EqualValues(t, 1, finding.Evidence["parked"])
		require.Empty(t, tw.subscriptions(t, customerOf(tw.alice)))
		require.Equal(t, "read", tw.catalog(t, billing.ProviderCatalogListParams{PSP: billing.PSPRef{Key: "stripe"}})["price:price_gold"].ObservedVia, "the parked price is observed")

		product := tw.stripeProduct(t, "gold", "member:gold")
		require.False(t, tw.entitled(t, customerOf(tw.alice), "member:gold"))
		price := tw.mapStripe(t, product, "stripe-gold", "price_gold")
		completedOnce(t, tw.fleet, kindConverge)
		subs := tw.subscriptions(t, customerOf(tw.alice))
		require.Len(t, subs, 1)
		require.Equal(t, price.ID, subs[0].PriceID)
		require.Equal(t, billing.SubscriptionActive, subs[0].Status)
		require.True(t, tw.entitled(t, customerOf(tw.alice), "member:gold"))
		payments, err := tw.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: customerOf(tw.alice)})
		require.NoError(t, err)
		require.Len(t, payments.Items, 1, "its bill is recorded once")
		closed := openPark(t, tw.parks(t, billing.ParkedSubscriptionListParams{Released: true}), sub)
		require.Equal(t, billing.ParkLanded, *closed.ReleasedBy)
		require.Equal(t, subs[0].ID, *closed.Subscription)
		require.Empty(t, tw.parks(t, billing.ParkedSubscriptionListParams{}))
		require.NotNil(t, tw.findings(t, "catalog.price.unmapped")["price_gold"].ResolvedAt)

		require.NoError(t, tw.stripe.Redeliver(t.Context(), event))
		tw.invoicePaid(t, sub)
		completedOnce(t, tw.fleet, kindConverge)
		require.Len(t, tw.subscriptions(t, customerOf(tw.alice)), 1)
		payments, err = tw.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: customerOf(tw.alice)})
		require.NoError(t, err)
		require.Len(t, payments.Items, 1)
		require.Len(t, tw.parks(t, billing.ParkedSubscriptionListParams{Released: true}), 1)

		// A second subscription for the product she holds parks as held.
		second := tw.stripe.AddSubscription(customer, "pm_alice", "price_gold", 1500, start, start.AddDate(0, 1, 0))
		tw.stripe.TagSubscription(second, "user_id", tw.alice.ID, start)
		tw.invoicePaid(t, second)
		completedOnce(t, tw.fleet, kindConverge)
		held := openPark(t, tw.parks(t, billing.ParkedSubscriptionListParams{}), second)
		require.Equal(t, billing.ParkProductHeld, held.Reason)
		require.Contains(t, string(held.Evidence), subs[0].ID.UUID().String(), "it names the subscription held")
		require.Equal(t, "high", tw.findings(t, "life.subscription.unattributed")[second].Severity)
		require.Len(t, tw.subscriptions(t, customerOf(tw.alice)), 1)
	})

	t.Run("OR-J15-S10/stripe-unattributed-customer-parks-and-lands-on-link", func(t *testing.T) {
		tw := newTrailWorld(t, "stripe")
		tw.stripe.AddProduct("prod_gold", "Gold")
		tw.stripe.AddPrice(stripemock.Price{ID: "price_gold", Product: "prod_gold", Amount: 1500, Interval: "month"})
		product := tw.stripeProduct(t, "gold", "member:gold")
		tw.mapStripe(t, product, "stripe-gold", "price_gold")
		start := tw.clock.Now().Add(-time.Hour)
		customer := tw.stripe.AddCustomer(tw.bob.Email, "Bob", nil)
		sub := tw.stripe.AddSubscription(customer, "pm_bob", "price_gold", 1500, start, start.AddDate(0, 1, 0))
		tw.invoicePaid(t, sub)
		completedOnce(t, tw.fleet, kindConverge)

		park := openPark(t, tw.parks(t, billing.ParkedSubscriptionListParams{}), sub)
		require.Equal(t, billing.ParkCustomerUnlinked, park.Reason)
		require.Equal(t, customer, *park.CustomerRef)
		finding := tw.findings(t, "life.subscription.unattributed")[sub]
		require.Equal(t, "high", finding.Severity)
		require.Nil(t, finding.ResolvedAt)
		require.Empty(t, tw.subscriptions(t, customerOf(tw.bob)))

		tw.send(t, "checkout.session.completed", stripemock.Object{"id": "cs_link_bob", "object": "checkout.session", "mode": "setup", "status": "complete",
			"customer": customer, "metadata": map[string]string{"user_id": tw.bob.ID}})
		completedOnce(t, tw.fleet, kindConverge)
		subs := tw.subscriptions(t, customerOf(tw.bob))
		require.Len(t, subs, 1)
		require.True(t, tw.entitled(t, customerOf(tw.bob), "member:gold"))
		require.Empty(t, tw.parks(t, billing.ParkedSubscriptionListParams{}))
		require.Equal(t, billing.ParkLanded, *openPark(t, tw.parks(t, billing.ParkedSubscriptionListParams{Released: true}), sub).ReleasedBy)
		require.NotNil(t, tw.findings(t, "life.subscription.unattributed")[sub].ResolvedAt)
	})

	t.Run("OR-J15-S11/stripe-move-to-unmapped-price-keeps-tier", func(t *testing.T) {
		tw := newTrailWorld(t, "stripe")
		for _, p := range []struct {
			product, price string
			amount         int64
		}{{"prod_basic", "price_basic", 900}, {"prod_pro", "price_pro", 1900}} {
			tw.stripe.AddProduct(p.product, p.product)
			tw.stripe.AddPrice(stripemock.Price{ID: p.price, Product: p.product, Amount: p.amount, Interval: "month"})
		}
		basic := tw.mapStripe(t, tw.stripeProduct(t, "basic", "member:basic"), "stripe-basic", "price_basic")
		start := tw.clock.Now().Add(-24 * time.Hour)
		customer := tw.stripe.AddCustomer(tw.alice.Email, "Alice", nil)
		sub := tw.stripe.AddSubscription(customer, "pm_alice", "price_basic", 900, start, start.AddDate(0, 1, 0))
		tw.stripe.TagSubscription(sub, "user_id", tw.alice.ID, start)
		tw.invoicePaid(t, sub)
		completedOnce(t, tw.fleet, kindConverge)
		held := tw.subscriptions(t, customerOf(tw.alice))
		require.Len(t, held, 1)
		require.Equal(t, basic.ID, held[0].PriceID)

		tw.stripe.PortalPriceChangePaid(sub, "price_pro", 1900, 1000)
		tw.send(t, "customer.subscription.updated", tw.stripe.Subscription(sub))
		completedOnce(t, tw.fleet, kindConverge)
		kept, err := tw.alpha.GetSubscription(t.Context(), held[0].ID)
		require.NoError(t, err)
		require.Equal(t, basic.ID, kept.PriceID, "the paid tier stays")
		require.Equal(t, billing.SubscriptionActive, kept.Status)
		require.True(t, tw.entitled(t, customerOf(tw.alice), "member:basic"))
		park := openPark(t, tw.parks(t, billing.ParkedSubscriptionListParams{}), sub)
		require.Equal(t, billing.ParkPriceUnmapped, park.Reason)
		require.Equal(t, "price_pro", *park.PriceRef)
		require.Equal(t, held[0].ID, *park.Subscription)
		require.Equal(t, "high", tw.findings(t, "catalog.price.unmapped")["price_pro"].Severity)

		pro := tw.mapStripe(t, tw.stripeProduct(t, "pro", "member:pro"), "stripe-pro", "price_pro")
		completedOnce(t, tw.fleet, kindConverge)
		moved, err := tw.alpha.GetSubscription(t.Context(), held[0].ID)
		require.NoError(t, err)
		require.Equal(t, pro.ID, moved.PriceID, "one tier move")
		require.Len(t, tw.subscriptions(t, customerOf(tw.alice)), 1)
		require.True(t, tw.entitled(t, customerOf(tw.alice), "member:pro"))
		require.Equal(t, billing.ParkConverged, *openPark(t, tw.parks(t, billing.ParkedSubscriptionListParams{Released: true}), sub).ReleasedBy)
		require.NotNil(t, tw.findings(t, "catalog.price.unmapped")["price_pro"].ResolvedAt)
		require.Equal(t, []int64{9_000_000, 10_000_000}, paidAmounts(t, tw, customerOf(tw.alice)), "the deferred proration bill lands once its price is mapped")

		// A one-off item on an unmapped price defers its bill and parks the
		// held subscription on that price; mapping it lands the bill.
		tw.stripe.AddProduct("prod_addon", "Add-on")
		tw.stripe.AddPrice(stripemock.Price{ID: "price_addon", Product: "prod_addon", Amount: 500, Interval: "month"})
		tw.send(t, "invoice.paid", tw.stripe.ChargeInvoiceItem(sub, "price_addon", 500))
		completedOnce(t, tw.fleet, kindConverge)
		addon := openPark(t, tw.parks(t, billing.ParkedSubscriptionListParams{}), sub)
		require.Equal(t, billing.ParkPriceUnmapped, addon.Reason)
		require.Equal(t, "price_addon", *addon.PriceRef)
		require.Equal(t, "high", tw.findings(t, "catalog.price.unmapped")["price_addon"].Severity)
		require.Equal(t, []int64{9_000_000, 10_000_000}, paidAmounts(t, tw, customerOf(tw.alice)), "the add-on bill waits")
		tw.mapStripe(t, tw.stripeProduct(t, "addon", "member:addon"), "stripe-addon", "price_addon")
		completedOnce(t, tw.fleet, kindConverge)
		require.Equal(t, []int64{5_000_000, 9_000_000, 10_000_000}, paidAmounts(t, tw, customerOf(tw.alice)))
		require.Empty(t, tw.parks(t, billing.ParkedSubscriptionListParams{}))
		moved, err = tw.alpha.GetSubscription(t.Context(), held[0].ID)
		require.NoError(t, err)
		require.Equal(t, pro.ID, moved.PriceID, "an add-on never moves the tier")
	})

	t.Run("OR-J15-S12/mapping-during-converge-strands-no-park", func(t *testing.T) {
		tw := newTrailWorld(t, "stripe")
		tw.stripe.AddProduct("prod_gold", "Gold")
		tw.stripe.AddPrice(stripemock.Price{ID: "price_gold", Product: "prod_gold", Amount: 1500, Interval: "month"})
		product := tw.stripeProduct(t, "gold", "member:gold")
		var reads atomic.Int32
		gate := tw.stripe.Hold(func(r *http.Request) bool {
			return r.Method == http.MethodGet && r.URL.Path == "/v1/prices/price_gold" && reads.Add(1) == 1
		}, stripemock.HoldRequest)
		defer gate.Release()
		start := tw.clock.Now().Add(-time.Hour)
		customer := tw.stripe.AddCustomer(tw.alice.Email, "Alice", nil)
		sub := tw.stripe.AddSubscription(customer, "pm_alice", "price_gold", 1500, start, start.AddDate(0, 1, 0))
		tw.stripe.TagSubscription(sub, "user_id", tw.alice.ID, start)
		tw.invoicePaid(t, sub)
		jobs, err := tw.fleet.JobList(t.Context(), river.NewJobListParams().Kinds(kindConverge))
		require.NoError(t, err)
		require.Len(t, jobs.Jobs, 1)
		_, err = tw.fleet.JobRetry(t.Context(), jobs.Jobs[0].ID)
		require.NoError(t, err)
		select {
		case <-gate.Arrived():
		case <-time.After(30 * time.Second):
			t.Fatal("converge never read the unmapped price")
		}
		price := tw.mapStripe(t, product, "stripe-gold", "price_gold")
		gate.Release()
		completedOnce(t, tw.fleet, kindConverge)
		subs := tw.subscriptions(t, customerOf(tw.alice))
		require.Len(t, subs, 1)
		require.Equal(t, price.ID, subs[0].PriceID)
		require.Empty(t, tw.parks(t, billing.ParkedSubscriptionListParams{}), "no park is stranded")
		require.Empty(t, tw.parks(t, billing.ParkedSubscriptionListParams{Released: true}), "the park re-read the mapping under its lock")

		// Park waits on the cause's lock (held here as a mapping in flight
		// would) and the mapping waits behind it: whichever goes first, the
		// subscription lands once and no park stays open.
		tw.stripe.AddProduct("prod_silver", "Silver")
		tw.stripe.AddPrice(stripemock.Price{ID: "price_silver", Product: "prod_silver", Amount: 900, Interval: "month"})
		silver := tw.stripeProduct(t, "silver", "member:silver")
		lock, err := tw.pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = lock.Rollback(context.Background()) }()
		cause := "openrails.park:" + tw.alpha.MerchantID().UUID().String() + ":" + tw.psp(t, "stripe").UUID().String() + ":price_unmapped:price_silver"
		_, err = lock.Exec(t.Context(), "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", cause)
		require.NoError(t, err)
		waiting := func(n int) {
			require.Eventually(t, func() bool {
				var got int
				err := tw.pool.QueryRow(t.Context(), "SELECT count(DISTINCT l.pid) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid WHERE NOT l.granted AND a.datname = current_database()").Scan(&got)
				return err == nil && got >= n
			}, 30*time.Second, 20*time.Millisecond)
		}
		bobSub := tw.stripe.AddSubscription(tw.stripe.AddCustomer(tw.bob.Email, "Bob", nil), "pm_bob", "price_silver", 900, start, start.AddDate(0, 1, 0))
		tw.stripe.TagSubscription(bobSub, "user_id", tw.bob.ID, start)
		tw.invoicePaid(t, bobSub)
		queued, err := tw.fleet.JobList(t.Context(), river.NewJobListParams().Kinds(kindConverge).States(rivertype.JobStateScheduled))
		require.NoError(t, err)
		require.Len(t, queued.Jobs, 1)
		_, err = tw.fleet.JobRetry(t.Context(), queued.Jobs[0].ID)
		require.NoError(t, err)
		waiting(1)
		mapped := make(chan error, 1)
		go func() {
			_, err := tw.alphaRemote.CreateProviderPrice(context.Background(), billing.CreateProviderPriceParams{ProductID: silver, Key: "stripe-silver", PSP: "stripe", PriceID: "price_silver"})
			mapped <- err
		}()
		waiting(2)
		require.NoError(t, lock.Rollback(t.Context()))
		select {
		case err := <-mapped:
			require.NoError(t, err)
		case <-time.After(30 * time.Second):
			t.Fatal("the mapping never committed")
		}
		completedOnce(t, tw.fleet, kindConverge)
		require.Len(t, tw.subscriptions(t, customerOf(tw.bob)), 1)
		require.Empty(t, tw.parks(t, billing.ParkedSubscriptionListParams{}), "no park is stranded")
	})

	t.Run("OR-J15-S13/nmi-unheld-subscription-adopts-or-parks", func(t *testing.T) {
		tw := bootTrailWorld(t, "nmi")
		tw.nmi.AddPlan(nmimock.Plan{ID: "gold", Name: "Gold", Amount: "20.00", Days: 30})
		tw.nmi.AddPlan(nmimock.Plan{ID: "silver", Name: "Silver", Amount: "10.00", Days: 30})
		product, err := tw.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "gold", DisplayName: "Gold", Entitlements: []string{"member:gold"}})
		require.NoError(t, err)
		gold, err := tw.alphaRemote.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "nmi-gold", PSP: "nmi", PlanID: "gold"})
		require.NoError(t, err)

		// (d) While the PSP's onboarding pull has not run, its backlog is the
		// import's.
		cancelOnboarding(t, tw.fleet, 1)
		tw.start(t)
		early := tw.nmi.AddVault(nmimock.Card{Brand: "visa", Last4: "1111"})
		earlySub := tw.nmiSchedule(t, early, "silver", "10.00")
		nmiNotice(t, tw.server.URL, "evt_early", "recurring.subscription.add", map[string]any{"subscription_id": earlySub})
		completedOnce(t, tw.fleet, kindConverge)
		onboarding := tw.onboarding(t, "nmi")
		require.Equal(t, billing.PSPImportPulling, onboarding.Status)
		early1 := openPark(t, tw.parks(t, billing.ParkedSubscriptionListParams{}), earlySub)
		require.True(t, early1.ImportOwned, "the pending onboarding pull owns it")
		require.Empty(t, tw.findings(t, "catalog.price.unmapped"), "an import-owned park raises no finding")
		pending, err := tw.fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.psp_import"))
		require.NoError(t, err)
		for _, job := range pending.Jobs {
			_, err := tw.fleet.JobRetry(t.Context(), job.ID)
			require.NoError(t, err)
		}
		imp := tw.await(t, onboarding.ID, billing.PSPImportPulled)
		require.Equal(t, billing.PSPImportUnmatched, tw.records(t, imp.ID)[earlySub].State)
		nmiNotice(t, tw.server.URL, "evt_early_again", "recurring.subscription.update", map[string]any{"subscription_id": earlySub})
		completedOnce(t, tw.fleet, kindConverge)
		require.True(t, openPark(t, tw.parks(t, billing.ParkedSubscriptionListParams{}), earlySub).ImportOwned, "the import stages it unlanded")
		require.Empty(t, tw.findings(t, "catalog.price.unmapped"))

		// (a) One owner, mapped plan: adopted once.
		aliceVault := tw.nmi.AddVault(nmimock.Card{Brand: "visa", Last4: "4242"})
		tw.ownVault(t, customerOf(tw.alice), aliceVault)
		adopted := tw.nmiSchedule(t, aliceVault, "gold", "20.00")
		reads := tw.nmi.Reads()["v5:plans/{id}"]
		nmiNotice(t, tw.server.URL, "evt_adopt", "recurring.subscription.add", map[string]any{"subscription_id": adopted})
		nmiNotice(t, tw.server.URL, "evt_adopt_sale", "transaction.sale.success", map[string]any{"transaction_id": "t1", "subscription": map[string]any{"subscription_id": adopted}})
		completedOnce(t, tw.fleet, kindConverge)
		subs := tw.subscriptions(t, customerOf(tw.alice))
		require.Len(t, subs, 1)
		require.Equal(t, gold.ID, subs[0].PriceID)
		require.Equal(t, adopted, *subs[0].RailSubscriptionID)
		require.Equal(t, billing.SubscriptionActive, subs[0].Status)
		require.True(t, tw.entitled(t, customerOf(tw.alice), "member:gold"))
		payments, err := tw.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: customerOf(tw.alice)})
		require.NoError(t, err)
		require.Len(t, payments.Items, 1, "the provider's sale lands once")
		require.Equal(t, reads, tw.nmi.Reads()["v5:plans/{id}"], "a plan the pull observed is not read again")
		require.Len(t, tw.parks(t, billing.ParkedSubscriptionListParams{}), 1, "only the import-owned park")

		// A second schedule for a product the owner already holds parks.
		twice := tw.nmiSchedule(t, aliceVault, "gold", "20.00")
		nmiNotice(t, tw.server.URL, "evt_twice", "recurring.subscription.add", map[string]any{"subscription_id": twice})
		completedOnce(t, tw.fleet, kindConverge)
		held := openPark(t, tw.parks(t, billing.ParkedSubscriptionListParams{}), twice)
		require.Equal(t, billing.ParkProductHeld, held.Reason)
		require.Contains(t, string(held.Evidence), subs[0].ID.UUID().String(), "it names the subscription held")
		require.Equal(t, "high", tw.findings(t, "life.subscription.unattributed")[twice].Severity)
		require.Len(t, tw.subscriptions(t, customerOf(tw.alice)), 1)

		// (b) A vault no one owns parks unlinked.
		stranger := tw.nmi.AddVault(nmimock.Card{Brand: "visa", Last4: "2222"})
		unowned := tw.nmiSchedule(t, stranger, "gold", "20.00")
		nmiNotice(t, tw.server.URL, "evt_unowned", "recurring.subscription.add", map[string]any{"subscription_id": unowned})
		completedOnce(t, tw.fleet, kindConverge)
		park := openPark(t, tw.parks(t, billing.ParkedSubscriptionListParams{}), unowned)
		require.Equal(t, billing.ParkCustomerUnlinked, park.Reason)
		require.Equal(t, stranger, *park.CustomerRef)
		require.Equal(t, "high", tw.findings(t, "life.subscription.unattributed")[unowned].Severity)

		// (c) An unmapped plan parks; mapping it lands the subscription.
		tw.nmi.AddPlan(nmimock.Plan{ID: "bronze", Name: "Bronze", Amount: "5.00", Days: 30})
		bobVault := tw.nmi.AddVault(nmimock.Card{Brand: "visa", Last4: "3333"})
		tw.ownVault(t, customerOf(tw.bob), bobVault)
		bronzeSub := tw.nmiSchedule(t, bobVault, "bronze", "5.00")
		reads = tw.nmi.Reads()["v5:plans/{id}"]
		nmiNotice(t, tw.server.URL, "evt_bronze", "recurring.subscription.add", map[string]any{"subscription_id": bronzeSub})
		completedOnce(t, tw.fleet, kindConverge)
		require.Equal(t, reads+1, tw.nmi.Reads()["v5:plans/{id}"], "an unseen plan is read once by id")
		park = openPark(t, tw.parks(t, billing.ParkedSubscriptionListParams{Reason: billing.ParkPriceUnmapped}), bronzeSub)
		require.Equal(t, "bronze", *park.PriceRef)
		require.Equal(t, "high", tw.findings(t, "catalog.price.unmapped")["bronze"].Severity)
		bronzeProduct, err := tw.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "bronze", DisplayName: "Bronze", Entitlements: []string{"member:bronze"}})
		require.NoError(t, err)
		_, err = tw.alphaRemote.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: bronzeProduct.ID, Key: "nmi-bronze", PSP: "nmi", PlanID: "bronze"})
		require.NoError(t, err)
		require.Equal(t, reads+1, tw.nmi.Reads()["v5:plans/{id}"], "mapping reads the observed plan")
		completedOnce(t, tw.fleet, kindConverge)
		require.Len(t, tw.subscriptions(t, customerOf(tw.bob)), 1)
		require.True(t, tw.entitled(t, customerOf(tw.bob), "member:bronze"))
		require.NotNil(t, tw.findings(t, "catalog.price.unmapped")["bronze"].ResolvedAt)

		// (e) An order id is no subscription: dropped.
		nmiNotice(t, tw.server.URL, "evt_order", "transaction.sale.success", map[string]any{"transaction_id": "t9", "order_id": "one-off-order-9"})
		completedOnce(t, tw.fleet, kindConverge)
		require.Len(t, tw.parks(t, billing.ParkedSubscriptionListParams{}), 3, "the import-owned, the unowned vault and the held product stay parked")
	})

	t.Run("OR-J15-S14/park-closes-when-import-lands-it", func(t *testing.T) {
		tw := newTrailWorld(t, "nmi")
		tw.nmi.AddPlan(nmimock.Plan{ID: "gold", Name: "Gold", Amount: "20.00", Days: 30})
		product, err := tw.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "gold", DisplayName: "Gold", Entitlements: []string{"member:gold"}})
		require.NoError(t, err)
		_, err = tw.alphaRemote.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "nmi-gold", PSP: "nmi", PlanID: "gold"})
		require.NoError(t, err)
		vault := tw.nmi.AddVault(nmimock.Card{Brand: "visa", Last4: "4242"})
		sub := tw.nmiSchedule(t, vault, "gold", "20.00")
		nmiNotice(t, tw.server.URL, "evt_unlinked", "recurring.subscription.add", map[string]any{"subscription_id": sub})
		completedOnce(t, tw.fleet, kindConverge)
		require.Equal(t, billing.ParkCustomerUnlinked, openPark(t, tw.parks(t, billing.ParkedSubscriptionListParams{}), sub).Reason)

		imp := tw.pulled(t, "nmi")
		recs := tw.records(t, imp.ID)
		require.Equal(t, billing.PSPImportUnmatched, recs[sub].State)
		require.NoError(t, tw.link(t, imp.ID, billing.LinkPSPImportParams{Links: []billing.PSPImportLink{{Record: recs[vault].ID, Customer: new(customerOf(tw.alice))}}}))
		recs = tw.apply(t, imp.ID)
		require.Equal(t, billing.PSPImportImported, recs[sub].State, "%v", recs[sub].Reason)
		subs := tw.subscriptions(t, customerOf(tw.alice))
		require.Len(t, subs, 1)
		require.Empty(t, tw.parks(t, billing.ParkedSubscriptionListParams{}))
		closed := openPark(t, tw.parks(t, billing.ParkedSubscriptionListParams{Released: true}), sub)
		require.Equal(t, billing.ParkLanded, *closed.ReleasedBy)
		require.Equal(t, subs[0].ID, *closed.Subscription)
		require.NotNil(t, tw.findings(t, "life.subscription.unattributed")[sub].ResolvedAt)
		settle(t, tw.fleet, kindConverge)
		require.Len(t, tw.subscriptions(t, customerOf(tw.alice)), 1)
	})

	t.Run("OR-J15-S15/shared-merchant-two-hosts-land-once", func(t *testing.T) {
		tw := newTrailWorld(t, "nmi")
		tw.nmi.AddPlan(nmimock.Plan{ID: "gold", Name: "Gold", Amount: "20.00", Days: 30})
		// A second host on the same schema and merchant, with its own workers.
		replica, err := openrails.New(t.Context(), tw.cfg, tw.deps)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, replica.Close(context.Background())) })
		mux := http.NewServeMux()
		require.NoError(t, openrailshttp.Mount(mux, replica, openrails.Routes{Auth: tw.auth.Authenticator(), RouteGroups: openrails.RouteGroups{Admin: true, Catalog: true, MerchantConfig: true, Metrics: true, Programmatic: true}}))
		second := httptest.NewServer(mux)
		t.Cleanup(second.Close)
		fleet, err := riverhelpers.New(t.Context(), tw.pool, &river.Config{Schema: tw.schema, Queues: map[string]river.QueueConfig{openrails.QueueBilling: {MaxWorkers: 2}}}, replica.RiverJobs())
		require.NoError(t, err)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			require.NoError(t, fleet.StopAndCancel(ctx))
		})
		require.NoError(t, replica.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))

		vault := tw.nmi.AddVault(nmimock.Card{Brand: "visa", Last4: "4242"})
		tw.ownVault(t, customerOf(tw.alice), vault)
		sub := tw.nmiSchedule(t, vault, "gold", "20.00")
		for _, host := range []string{tw.server.URL, second.URL} {
			nmiNotice(t, host, "evt_shared", "recurring.subscription.add", map[string]any{"subscription_id": sub})
		}
		nmiNotice(t, second.URL, "evt_shared_sale", "transaction.sale.success", map[string]any{"transaction_id": "t1", "subscription": map[string]any{"subscription_id": sub}})
		completedOnce(t, tw.fleet, kindConverge)
		require.Len(t, tw.parks(t, billing.ParkedSubscriptionListParams{}), 1, "one park")

		product, err := tw.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "gold", DisplayName: "Gold", Entitlements: []string{"member:gold"}})
		require.NoError(t, err)
		_, err = tw.alphaRemote.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "nmi-gold", PSP: "nmi", PlanID: "gold"})
		require.NoError(t, err)
		nmiNotice(t, second.URL, "evt_shared_update", "recurring.subscription.update", map[string]any{"subscription_id": sub})
		completedOnce(t, tw.fleet, kindConverge)
		require.Len(t, tw.subscriptions(t, customerOf(tw.alice)), 1, "one subscription")
		payments, err := tw.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: customerOf(tw.alice)})
		require.NoError(t, err)
		require.Len(t, payments.Items, 1, "one payment")
		require.Empty(t, tw.parks(t, billing.ParkedSubscriptionListParams{}))
	})

	t.Run("OR-J15-S16/list-parked-subscriptions", func(t *testing.T) {
		tw := newTrailWorld(t, "nmi")
		tw.nmi.AddPlan(nmimock.Plan{ID: "gold", Name: "Gold", Amount: "20.00", Days: 30})
		var refs []string
		for i := range 3 {
			vault := tw.nmi.AddVault(nmimock.Card{Brand: "visa", Last4: fmt.Sprintf("%04d", 5000+i)})
			refs = append(refs, tw.nmiSchedule(t, vault, "gold", "20.00"))
		}
		product, err := tw.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "gold", DisplayName: "Gold", Entitlements: []string{"member:gold"}})
		require.NoError(t, err)
		for i, ref := range refs {
			if i == 2 {
				_, err = tw.alphaRemote.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "nmi-gold", PSP: "nmi", PlanID: "gold"})
				require.NoError(t, err)
			}
			nmiNotice(t, tw.server.URL, "evt_list_"+ref, "recurring.subscription.add", map[string]any{"subscription_id": ref})
			completedOnce(t, tw.fleet, kindConverge)
		}
		parks := tw.parks(t, billing.ParkedSubscriptionListParams{})
		require.Len(t, parks, 3)
		require.Equal(t, refs[2], parks[0].SubscriptionRef, "newest first")
		require.Len(t, tw.parks(t, billing.ParkedSubscriptionListParams{Reason: billing.ParkCustomerUnlinked}), 3, "mapping re-parked the first two as unlinked")
		require.Empty(t, tw.parks(t, billing.ParkedSubscriptionListParams{Reason: billing.ParkPriceUnmapped}))
		require.Len(t, tw.parks(t, billing.ParkedSubscriptionListParams{PSP: "nmi"}), 3)
		require.Len(t, tw.parks(t, billing.ParkedSubscriptionListParams{PSP: tw.psp(t, "nmi").String()}), 3)
		require.Empty(t, tw.parks(t, billing.ParkedSubscriptionListParams{Released: true}))

		first, err := tw.alphaRemote.ListParkedSubscriptions(t.Context(), billing.ParkedSubscriptionListParams{PageRequest: billing.PageRequest{Limit: 2}})
		require.NoError(t, err)
		require.Len(t, first.Items, 2)
		require.NotEmpty(t, first.Next)
		rest, err := tw.alphaRemote.ListParkedSubscriptions(t.Context(), billing.ParkedSubscriptionListParams{PageRequest: billing.PageRequest{Limit: 2, Cursor: first.Next}})
		require.NoError(t, err)
		require.Len(t, rest.Items, 1)
		require.Empty(t, rest.Next)
		require.Equal(t, refs[0], rest.Items[0].SubscriptionRef)

		_, err = tw.alphaRemote.ListParkedSubscriptions(t.Context(), billing.ParkedSubscriptionListParams{PSP: "nope"})
		status, code := refusalCode(t, err)
		require.Equal(t, http.StatusNotFound, status)
		require.Equal(t, "psp_not_found", code)
		_, err = tw.alphaRemote.ListParkedSubscriptions(t.Context(), billing.ParkedSubscriptionListParams{Reason: "lost"})
		status, _ = refusalCode(t, err)
		require.Equal(t, http.StatusBadRequest, status)
		status, raw := tw.request(t, http.MethodGet, "/v1/merchant/parked-subscriptions", tw.bobToken, nil)
		require.Contains(t, []int{http.StatusForbidden, http.StatusUnauthorized, http.StatusNotFound}, status, string(raw))
		require.False(t, strings.Contains(string(raw), refs[0]), "a customer token reads no park")
	})

	t.Run("OR-J15-S17/stripe-link-before-the-first-pull-lands-at-once", func(t *testing.T) {
		tw := bootTrailWorld(t, "stripe")
		tw.stripe.AddProduct("prod_gold", "Gold")
		tw.stripe.AddPrice(stripemock.Price{ID: "price_gold", Product: "prod_gold", Amount: 1500, Interval: "month"})
		tw.mapStripe(t, tw.stripeProduct(t, "gold", "member:gold"), "stripe-gold", "price_gold")
		cancelOnboarding(t, tw.fleet, 1)
		tw.start(t)
		start := tw.clock.Now().Add(-time.Hour)
		customer := tw.stripe.AddCustomer(tw.bob.Email, "Bob", nil)
		sub := tw.stripe.AddSubscription(customer, "pm_bob", "price_gold", 1500, start, start.AddDate(0, 1, 0))
		tw.invoicePaid(t, sub)
		completedOnce(t, tw.fleet, kindConverge)
		park := openPark(t, tw.parks(t, billing.ParkedSubscriptionListParams{}), sub)
		require.Equal(t, billing.ParkCustomerUnlinked, park.Reason)
		require.True(t, park.ImportOwned, "no pull has run yet")
		require.Empty(t, tw.findings(t, "life.subscription.unattributed"), "an import-owned park raises no finding")

		tw.send(t, "checkout.session.completed", stripemock.Object{"id": "cs_link_bob", "object": "checkout.session", "mode": "subscription", "status": "complete",
			"customer": customer, "subscription": sub, "metadata": map[string]string{"user_id": tw.bob.ID}})
		completedOnce(t, tw.fleet, kindConverge)
		subs := tw.subscriptions(t, customerOf(tw.bob))
		require.Len(t, subs, 1, "the link lands it at once")
		require.True(t, tw.entitled(t, customerOf(tw.bob), "member:gold"))
		closed := openPark(t, tw.parks(t, billing.ParkedSubscriptionListParams{Released: true}), sub)
		require.Equal(t, billing.ParkLanded, *closed.ReleasedBy)
		require.Equal(t, subs[0].ID, *closed.Subscription)
	})
}
