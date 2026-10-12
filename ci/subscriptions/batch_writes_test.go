//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// A settings write creates a customer OpenRails has not seen; the list's ids
// filter reads them, unknown ones absent. A refused write creates nothing.
func TestCustomerBatches(t *testing.T) {
	w := newWorld(t)
	a, b, missing := billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New())
	for _, tp := range []topology{embedded, remote} {
		client := w.client[tp]
		for _, id := range []billing.CustomerID{a, b} {
			created, err := client.UpdateCustomer(t.Context(), id, billing.UpdateCustomerParams{})
			require.NoError(t, err)
			require.Equal(t, id, created.ID)
		}

		read, err := client.ListCustomers(t.Context(), billing.CustomerListParams{IDs: []billing.CustomerID{b, missing, a, b}})
		require.NoError(t, err)
		require.Len(t, read.Items, 2)
		require.Empty(t, read.Next)
		require.ElementsMatch(t, []billing.CustomerID{a, b}, []billing.CustomerID{read.Items[0].ID, read.Items[1].ID})
	}

	late := billing.CustomerID(uuid.New())
	status, refused := w.staffJSON(http.MethodPatch, "/v1/admin/customers/"+late.String(), map[string]any{"credit_limits": []any{map[string]any{"currency": "USD", "amount": "-1"}}})
	require.Equal(t, http.StatusBadRequest, status, "%v", refused)
	require.Equal(t, "credit_limits[0].amount", refused["error"].(map[string]any)["param"])
	read, err := w.client[remote].ListCustomers(t.Context(), billing.CustomerListParams{IDs: []billing.CustomerID{late}})
	require.NoError(t, err)
	require.Empty(t, read.Items, "a refused write creates nothing")
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
		access, err := heldProducts(t.Context(), w.client[embedded], c.customerID(), price.ProductID)
		require.NoError(t, err)
		require.True(t, access[price.ProductID])
	}
	both, err := w.client[remote].ListProductAccess(t.Context(), billing.ProductAccessListParams{CustomerIDs: []billing.CustomerID{a.customerID(), b.customerID()}, ProductIDs: []billing.ProductID{price.ProductID}, LiveOnly: true})
	require.NoError(t, err)
	require.Len(t, both.Items, 2, "one read covers both customers")
	_, err = w.client[embedded].ListProductAccess(t.Context(), billing.ProductAccessListParams{ProductIDs: []billing.ProductID{}})
	require.ErrorIs(t, err, billing.ErrInvalid, "a product filter names at least one product")
}

// Usage is recorded in batches, each item on its own: a refused item does not
// refuse its neighbours, and each answers what recording it alone would.
func TestUsageBatchesAnswerPerItem(t *testing.T) {
	w := newWorld(t)
	declareDynamicMeter(t, w.client[embedded], "batch", "units")
	c := w.newCustomer()
	_, err := createCreditGrant(t.Context(), w.client[embedded], c.customerID(), billing.CreateCreditGrantParams{Currency: "USD", Amount: 10_000_000, Source: "test", SourceID: uuid.NewString()})
	require.NoError(t, err)
	event := func(source string, amount int64) billing.RecordUsageParams {
		return billing.RecordUsageParams{Dimensions: map[string]int64{"units": 1}, CustomerID: c.cid(), Invoker: c.id, Currency: "usd", EventType: "batch", Amount: new(int64(amount)), Source: "test", SourceID: source}
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
		require.Equal(t, []int{http.StatusCreated, http.StatusOK, http.StatusUnprocessableEntity, http.StatusBadRequest, http.StatusBadRequest, http.StatusCreated}, statuses)
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
	status, body := w.hostJSON(http.MethodPost, "/v1/app/usage-events", map[string]any{"items": []any{}})
	require.Equal(t, http.StatusBadRequest, status, "%v", body)
	status, body = w.staffJSON(http.MethodPost, "/v1/app/usage-events", map[string]any{"items": []any{}})
	require.Equal(t, http.StatusForbidden, status, "a person never writes usage: %v", body)
	require.Equal(t, billing.CodeApplicationRequired, body["error"].(map[string]any)["code"])
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

// A usage source identity owns its receipt. Equivalent JSON retries and a
// missing HTTP key both recover it; changed monetary terms fail per item.
func TestUsageRetriesUseSourceIdentity(t *testing.T) {
	w := newWorld(t)
	declareDynamicMeter(t, w.client[embedded], "replay", "units")
	c := w.newCustomer()
	_, err := createCreditGrant(t.Context(), w.client[embedded], c.cid(), billing.CreateCreditGrantParams{Currency: "USD", Amount: 10_000_000, Source: "test", SourceID: uuid.NewString()})
	require.NoError(t, err)
	send := func(token, key string, amount int64, formatted bool) (int, billing.RecordUsageBatchResult) {
		t.Helper()
		input := map[string]any{"items": []any{map[string]any{
			"customer_id": c.id, "invoker": c.id, "currency": "USD", "event_type": "replay",
			"dimensions": map[string]int64{"units": 1}, "amount": strconv.FormatInt(amount, 10), "source": "test", "source_id": "replay-1",
		}}}
		raw, err := json.Marshal(input)
		require.NoError(t, err)
		if formatted {
			var indented bytes.Buffer
			require.NoError(t, json.Indent(&indented, raw, "", "  "))
			raw = indented.Bytes()
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, w.server.URL+mountPrefix+"/v1/app/usage-events", bytes.NewReader(raw))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		res, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer res.Body.Close()
		var body billing.RecordUsageBatchResult
		require.NoError(t, json.NewDecoder(res.Body).Decode(&body))
		return res.StatusCode, body
	}
	host, key := w.auth.hostToken(t), uuid.NewString()
	status, first := send(host, key, 1_000, false)
	require.Equal(t, http.StatusOK, status)
	require.NoError(t, first.Items[0].Err())
	require.False(t, first.Items[0].Event.Replayed)
	for _, retryKey := range []string{key, "", uuid.NewString()} {
		status, again := send(host, retryKey, 1_000, true)
		require.Equal(t, http.StatusOK, status)
		require.NoError(t, again.Items[0].Err())
		require.True(t, again.Items[0].Event.Replayed)
		require.Equal(t, first.Items[0].Event.ID, again.Items[0].Event.ID)
	}
	status, changed := send(host, key, 2_000, false)
	require.Equal(t, http.StatusOK, status)
	require.ErrorIs(t, changed.Items[0].Err(), billing.ErrIdempotencyKeyReused)
	require.Equal(t, http.StatusUnprocessableEntity, changed.Items[0].Status)
}
