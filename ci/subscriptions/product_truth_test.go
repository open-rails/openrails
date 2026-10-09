//go:build e2e && integration

package subscriptions_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// keysEvents is the product key-change host events of one product.
func (w *world) keysEvents(product billing.ProductID) []billing.ProductEntitlementsChangedEvent {
	w.t.Helper()
	page, err := w.client[embedded].ListHostEvents(w.t.Context(), billing.HostEventListParams{Type: billing.HostEventProductEntitlementsChanged, PageRequest: billing.PageRequest{Limit: 100}})
	require.NoError(w.t, err)
	var out []billing.ProductEntitlementsChangedEvent
	for _, event := range page.Items {
		if event.ProductEntitlements != nil && event.ProductEntitlements.ProductID == product {
			out = append(out, *event.ProductEntitlements)
		}
	}
	return out
}

// The owner's scenario: a customer buys the course bundle; a course added to
// the bundle reaches them at once, one removed leaves them unless another
// product they hold grants it.
func TestOwnerCourseBundleFollowsTheProduct(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	client := w.client[embedded]
	bundle, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "course-bundle", DisplayName: "Course bundle", Entitlements: []string{"course:101", "course:102"}})
	require.NoError(t, err)
	price, err := client.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: bundle.ID, Key: "buy", UnitAmount: 30_000_000, Currency: "USD"})
	require.NoError(t, err)
	buyer, single := w.newCustomer(), w.newCustomer()
	buyer.buy(price.ID.String(), "course:101", buyer.saveCard("stripe", visa))
	require.True(t, buyer.entitled("course:102"))
	single.buy(price.ID.String(), "course:101", single.saveCard("stripe", visa))
	single.grant(w.giftProduct("course:102"), nil, nil)

	_, err = client.UpdateProduct(t.Context(), bundle.ID, billing.UpdateProductParams{Entitlements: catalog.Value([]string{"course:101", "course:102", "course:103"})})
	require.NoError(t, err)
	require.True(t, buyer.entitled("course:103"), "a course added to the bundle reaches its buyer")

	_, err = client.UpdateProduct(t.Context(), bundle.ID, billing.UpdateProductParams{Entitlements: catalog.Value([]string{"course:101", "course:103"})})
	require.NoError(t, err)
	require.False(t, buyer.entitled("course:102"), "a course removed from the bundle leaves its buyer")
	require.True(t, single.entitled("course:102"), "another product the customer holds still grants it")
	require.True(t, buyer.entitled("course:101"))

	events := w.keysEvents(bundle.ID)
	require.Len(t, events, 2, "each edit tells the host")
	require.Equal(t, billing.ProductEntitlementsChangedEvent{ProductID: bundle.ID, ProductKey: "course-bundle", Added: []string{"course:103"}, Removed: []string{}, Holders: 2}, events[0])
	require.Equal(t, []string{"course:102"}, events[1].Removed)
	require.EqualValues(t, 2, events[1].Holders)

	page, err := client.ListCustomerEntitlements(t.Context(), buyer.customerID(), billing.CustomerEntitlementListParams{Prefix: "course:"})
	require.NoError(t, err)
	require.Equal(t, []billing.CustomerEntitlement{{Entitlement: "course:101"}, {Entitlement: "course:103"}}, page.Items)
	holders, err := client.ListEntitlementCustomers(t.Context(), "course:102", billing.EntitlementCustomerListParams{})
	require.NoError(t, err)
	require.Equal(t, []billing.CustomerID{single.customerID()}, holders.Items)
}

// The owner's scenario: content moves from key A to key B across every
// product in one edit; holders lose A and gain B together, and the receipt
// reports who was affected.
func TestOwnerReplaceContentMovesEveryHolder(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	client := w.client[embedded]
	first, second := w.giftProduct("content:a", "content:x"), w.giftProduct("content:a")
	c, other := w.newCustomer(), w.newCustomer()
	c.grant(first, nil, nil)
	other.grant(second, nil, nil)
	receipt, err := client.ReplaceEntitlements(t.Context(), billing.ReplaceEntitlementsParams{Pairs: []billing.EntitlementReplacement{{From: "content:a", To: "content:b"}}})
	require.NoError(t, err)
	require.Len(t, receipt.EntitlementChanges, 2)
	for _, change := range receipt.EntitlementChanges {
		require.Equal(t, []string{"content:b"}, change.Added)
		require.Equal(t, []string{"content:a"}, change.Removed)
		require.EqualValues(t, 1, change.Holders)
	}
	for _, customer := range []*customer{c, other} {
		require.True(t, customer.entitled("content:b"))
		require.False(t, customer.entitled("content:a"))
	}
	require.True(t, c.entitled("content:x"))
	require.Len(t, w.keysEvents(first), 1)
}

// The owner's scenario: staff grant the premium product free; editing the
// product reaches the comped customer like a buyer. Grants are batches, all or
// none, extend by hours, record who granted them and why, and replay.
func TestOwnerFreeGrantFollowsTheProduct(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	client := w.client[embedded]
	premium, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: "premium", DisplayName: "Premium", Entitlements: []string{"premium"}})
	require.NoError(t, err)
	comped, second := w.newCustomer(), w.newCustomer()
	day, note := 24, "support goodwill"
	granted, err := client.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{IdempotencyKey: "comp-" + uuid.NewString(), Items: []billing.CreateProductAccessParams{
		{CustomerID: comped.customerID(), ProductID: premium.ID, Hours: &day, Reason: billing.GrantReasonComp, Note: &note},
		{CustomerID: second.customerID(), ProductID: premium.ID, Hours: &day},
	}})
	require.NoError(t, err)
	require.Len(t, granted, 2)
	require.Equal(t, billing.ProductAccessSourceGrant, granted[0].SourceType)
	require.Equal(t, billing.GrantReasonComp, *granted[0].GrantReason)
	require.Equal(t, note, *granted[0].Note)
	require.NotNil(t, granted[0].GrantedBy)
	require.Equal(t, billing.GrantReasonStaff, *granted[1].GrantReason, "staff is the default reason")
	require.True(t, comped.entitled("premium"))

	_, err = client.UpdateProduct(t.Context(), premium.ID, billing.UpdateProductParams{Entitlements: catalog.Value([]string{"premium", "premium:hd"})})
	require.NoError(t, err)
	require.True(t, comped.entitled("premium:hd"), "a product edit reaches a comped holder")

	now := w.clock.Now()
	comped.grant(premium.ID, &day, nil)
	require.True(t, comped.entitledAt("premium", now.Add(36*time.Hour)), "hours extend after the live grant")
	require.False(t, comped.entitledAt("premium", now.Add(49*time.Hour)))

	// All or none: an unknown product refuses the whole batch.
	third := w.newCustomer()
	_, err = client.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{
		{CustomerID: third.customerID(), ProductID: premium.ID, Hours: &day},
		{CustomerID: third.customerID(), ProductID: billing.ProductID(uuid.New()), Hours: &day},
	}})
	require.Error(t, err)
	require.False(t, third.entitled("premium"), "a refused batch grants nothing")

	// A replay answers the first grant; another use of its key is refused.
	key := "replay-" + uuid.NewString()
	items := []billing.CreateProductAccessParams{{CustomerID: third.customerID(), ProductID: premium.ID, Hours: &day}}
	once, err := client.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{IdempotencyKey: key, Items: items})
	require.NoError(t, err)
	again, err := w.client[remote].CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{IdempotencyKey: key, Items: items})
	require.NoError(t, err)
	require.Equal(t, once[0].ID, again[0].ID)
	require.True(t, third.entitledAt("premium", now.Add(23*time.Hour)))
	require.False(t, third.entitledAt("premium", now.Add(25*time.Hour)), "the replay granted nothing more")
	other := w.giftProduct("other")
	_, err = client.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{IdempotencyKey: key, Items: []billing.CreateProductAccessParams{{CustomerID: third.customerID(), ProductID: other, Hours: &day}}})
	require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)

	// Revoking names the customer: another customer's window is not found.
	require.ErrorIs(t, client.DeleteProductAccess(t.Context(), comped.customerID(), once[0].ID), billing.ErrNotFound)
	require.True(t, third.entitled("premium"))
	require.NoError(t, client.DeleteProductAccess(t.Context(), third.customerID(), once[0].ID))
	require.False(t, third.entitled("premium"))
}

// Product access answers for every source: a subscription holds its product
// like a purchase, an archived product keeps granting its holders, and a past
// instant reads the product as it was then.
func TestProductAccessCoversSubscriptionsArchivesAndHistory(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	client := w.client[embedded]
	e := enroll(t, w, "stripe", embedded)
	sub := w.subscription(embedded, e.sub)
	access, err := client.CheckProductAccess(t.Context(), e.c.customerID(), billing.CheckProductAccessParams{ProductIDs: []billing.ProductID{sub.ProductID}})
	require.NoError(t, err)
	require.True(t, access[sub.ProductID.String()], "a subscription is product access")
	live, err := client.ListProductAccess(t.Context(), e.c.customerID(), billing.ProductAccessListParams{LiveOnly: true})
	require.NoError(t, err)
	require.Len(t, live.Items, 1)
	require.Equal(t, billing.ProductAccessSourceSubscription, live.Items[0].SourceType)
	require.Equal(t, sub.ProductID, live.Items[0].ProductID)

	c := w.newCustomer()
	product := w.giftProduct("history:a")
	c.grant(product, nil, nil)
	before := w.clock.Now()
	w.advance(time.Hour)
	_, err = client.UpdateProduct(t.Context(), product, billing.UpdateProductParams{Entitlements: catalog.Value([]string{"history:b"})})
	require.NoError(t, err)
	require.True(t, c.entitledAt("history:a", before.Add(time.Minute)), "a past instant reads the product then")
	require.False(t, c.entitledAt("history:b", before.Add(time.Minute)))
	require.True(t, c.entitled("history:b"))
	_, err = client.UpdateProduct(t.Context(), product, billing.UpdateProductParams{Archived: catalog.Value(true)})
	require.NoError(t, err)
	require.True(t, c.entitled("history:b"), "an archived product keeps granting its holders")
}
