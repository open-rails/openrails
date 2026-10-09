//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// Customers are declared in batches, all or none, and read by the list's ids
// filter, unknown ones absent.
func TestCustomerBatches(t *testing.T) {
	w := newWorld(t)
	a, b, missing := billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New())
	email := "a@example.test"
	for _, tp := range []topology{embedded, remote} {
		client := w.client[tp]
		declared, err := client.EnsureCustomers(t.Context(), []billing.EnsureCustomerParams{{ID: a, Email: &email}, {ID: b}})
		require.NoError(t, err)
		require.Equal(t, []billing.CustomerID{a, b}, []billing.CustomerID{declared[0].ID, declared[1].ID}, "request order")
		require.Equal(t, &email, declared[0].Email)
		require.Nil(t, declared[1].Email)

		read, err := client.ListCustomers(t.Context(), billing.CustomerListParams{IDs: []billing.CustomerID{b, missing, a, b}})
		require.NoError(t, err)
		require.Len(t, read.Items, 2)
		require.Empty(t, read.Next)
		byID := map[billing.CustomerID]billing.Customer{}
		for _, c := range read.Items {
			byID[c.ID] = c
		}
		require.Equal(t, &email, byID[a].Email)
		require.Contains(t, byID, b)
	}

	late := billing.CustomerID(uuid.New())
	for _, body := range []struct {
		items any
		param string
	}{
		{[]any{map[string]any{"id": late}, map[string]any{"id": a}, map[string]any{"id": late}}, "items[2].id"},
		{[]any{map[string]any{"id": late}, map[string]any{"id": a, "email": "not-an-address"}}, "items[1].email"},
		{[]any{}, "items"},
	} {
		status, refused := w.staffJSON(http.MethodPost, "/v1/merchant/customers/ensure", map[string]any{"items": body.items})
		require.Equal(t, http.StatusBadRequest, status, "%v", refused)
		require.Equal(t, body.param, refused["error"].(map[string]any)["param"])
	}
	read, err := w.client[remote].ListCustomers(t.Context(), billing.CustomerListParams{IDs: []billing.CustomerID{late}})
	require.NoError(t, err)
	require.Empty(t, read.Items, "a refused declaration declares nothing")
}

// Product access is granted in batches across customers; a retry under the
// same idempotency key answers the existing grants.
func TestProductAccessGrantBatches(t *testing.T) {
	w := newWorld(t)
	a, b := w.newCustomer(), w.newCustomer()
	price := w.permanent("post:batch-access")
	until := w.clock.Now().Add(72 * time.Hour)
	batch := billing.CreateProductAccessBatchParams{IdempotencyKey: "batch-" + uuid.NewString(), Items: []billing.CreateProductAccessParams{
		{CustomerID: a.customerID(), ProductID: price.ProductID, EndsAt: &until},
		{CustomerID: b.customerID(), ProductID: price.ProductID},
	}}
	granted, err := w.client[embedded].CreateProductAccess(t.Context(), batch)
	require.NoError(t, err)
	require.Equal(t, []billing.CustomerID{a.customerID(), b.customerID()}, []billing.CustomerID{granted[0].CustomerID, granted[1].CustomerID})
	require.NotNil(t, granted[0].EndsAt)
	require.Nil(t, granted[1].EndsAt)
	again, err := w.client[remote].CreateProductAccess(t.Context(), batch)
	require.NoError(t, err)
	require.Equal(t, []billing.ProductAccessID{granted[0].ID, granted[1].ID}, []billing.ProductAccessID{again[0].ID, again[1].ID}, "a retry answers the existing grants")
	for _, c := range []*customer{a, b} {
		access, err := w.client[embedded].CheckProductAccess(t.Context(), c.customerID(), billing.CheckProductAccessParams{ProductIDs: []billing.ProductID{price.ProductID}})
		require.NoError(t, err)
		require.True(t, access[price.ProductID.String()])
	}
	_, err = w.client[embedded].CheckProductAccess(t.Context(), a.customerID(), billing.CheckProductAccessParams{ProductIDs: []billing.ProductID{}})
	require.ErrorIs(t, err, billing.ErrInvalid, "a check names at least one product")
}

// Usage is recorded in batches, each item on its own: a refused item does not
// refuse its neighbours, and each answers what recording it alone would.
func TestUsageBatchesAnswerPerItem(t *testing.T) {
	w := newWorld(t)
	c := w.newCustomer()
	_, err := createCreditGrant(t.Context(), w.client[embedded], c.customerID(), billing.CreateCreditGrantParams{Currency: "USD", Amount: 10_000_000, Source: "test", SourceID: uuid.NewString()})
	require.NoError(t, err)
	event := func(source string, amount int64) billing.RecordUsageParams {
		return billing.RecordUsageParams{CustomerID: c.cid(), Invoker: c.id, Currency: "usd", EventType: "batch", Amount: amount, Source: "test", SourceID: source}
	}
	zero := event("orphan", 1)
	zero.CustomerID = billing.CustomerID{}
	for _, tp := range []topology{embedded, remote} {
		source := "event-" + string(tp)
		results, err := w.client[tp].RecordUsage(t.Context(), []billing.RecordUsageParams{
			event(source, 1_000), event(source, 1_000), event(source, 2_000), event("negative", -1), zero, event(source+"-b", 3_000),
		})
		require.NoError(t, err)
		require.Len(t, results, 6)
		statuses := make([]int, len(results))
		for i, r := range results {
			statuses[i] = r.Status
		}
		require.Equal(t, []int{http.StatusCreated, http.StatusOK, http.StatusConflict, http.StatusBadRequest, http.StatusBadRequest, http.StatusCreated}, statuses)
		require.False(t, results[0].Event.Replayed)
		require.True(t, results[1].Event.Replayed)
		require.Equal(t, results[0].Event.ID, results[1].Event.ID)
		require.ErrorIs(t, results[2].Err(), billing.ErrIdempotencyKeyReused)
		require.Equal(t, "amount", *results[3].Error.Param)
		require.Equal(t, "customer_id", *results[4].Error.Param)
		require.NotEmpty(t, results[4].Error.RequestID)
		require.NoError(t, results[5].Err())
	}
	_, err = w.client[remote].RecordUsage(t.Context(), nil)
	require.ErrorIs(t, err, billing.ErrInvalid)
	status, body := w.staffJSON(http.MethodPost, "/v1/merchant/usage-events", map[string]any{"items": []any{}})
	require.Equal(t, http.StatusBadRequest, status, "%v", body)
}

// A drained page of host events is acknowledged in one call; an unknown id
// answers null and acknowledging again changes nothing.
func TestHostEventsAcknowledgeInBatches(t *testing.T) {
	w := newWorld(t)
	for range 2 {
		c := w.newCustomer()
		entitlement := "post:ack-" + uuidShort()
		c.buy(w.permanent(entitlement).ID.String(), entitlement, c.saveCard("stripe", visa))
	}
	client := w.client[embedded]
	page, err := client.ListHostEvents(t.Context(), billing.HostEventListParams{Type: billing.HostEventPaymentSettled})
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	unknown := billing.HostEventID(uuid.New())
	ids := []billing.HostEventID{page.Items[0].ID, unknown, page.Items[1].ID}
	acked, err := w.client[remote].AcknowledgeHostEvents(t.Context(), ids)
	require.NoError(t, err)
	require.Len(t, acked, 3)
	require.Nil(t, acked[unknown])
	for _, event := range page.Items {
		require.NotNil(t, acked[event.ID].AcknowledgedAt)
		require.NotNil(t, acked[event.ID].Payment, "the acknowledged event keeps its payload")
	}
	again, err := client.AcknowledgeHostEvents(t.Context(), ids[:1])
	require.NoError(t, err)
	require.Equal(t, acked[ids[0]].AcknowledgedAt, again[ids[0]].AcknowledgedAt, "acknowledging again changes nothing")
	pending, err := client.ListHostEvents(t.Context(), billing.HostEventListParams{Type: billing.HostEventPaymentSettled})
	require.NoError(t, err)
	require.Empty(t, pending.Items)
}
