//go:build integration

package integrationharness

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func TestHostEventsReplayAcrossEmbeddedAndHTTPClients(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	server := h.StartStandalone("USD")
	a := server.ProvisionOwnedMerchant("events-a-" + uuid.NewString())
	b := server.ProvisionOwnedMerchant("events-b-" + uuid.NewString())
	token := server.MintAPIKey(a.MerchantSlug, "host-events", []string{controlplane.PermMerchantHostEventsRead, controlplane.PermMerchantHostEventsAcknowledge, controlplane.PermMerchantPaymentsRead})
	remote := server.Client(openrails.WithTokenProvider(func(context.Context) (string, error) { return token, nil }))
	host := h.StartEmbeddedMerchant("USD", a.MerchantID, a.MerchantSlug)
	local, err := host.Runtime().Client()
	require.NoError(t, err)
	pool := h.MerchantPool(a.MerchantID.UUID())
	payer, product, price, payment := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO openrails.customers (id,merchant_id) VALUES ($1,$2)`, payer, a.MerchantID.UUID())
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO openrails.products (id,merchant_id,key,display_name) VALUES ($1,$2,$3,'Host event test')`, product, a.MerchantID.UUID(), uuid.NewString())
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO openrails.prices (id,merchant_id,product_id,amount,currency) VALUES ($1,$2,$3,7000000,'USD')`, price, a.MerchantID.UUID(), product)
	require.NoError(t, err)
	psp := dbtest.EnsureTestPSP(ctx, t, pool, a.MerchantID.UUID(), "nmi")
	for _, client := range []*openrails.Client{remote, local} {
		settled, err := client.HasSettledPayment(ctx, openrails.CustomerID(payer), openrails.PriceID(price))
		require.NoError(t, err)
		require.False(t, settled)
	}
	// This earlier event commits after the later payment has been consumed.
	// A persisted UUID high-water mark would be unsafe for this stream.
	lifecycleID := uuid.Must(uuid.NewV7())
	lifecycleTx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = lifecycleTx.Rollback(ctx) }()
	_, err = lifecycleTx.Exec(ctx, `INSERT INTO openrails.host_outbox
		(id,merchant_id,event_type,subject_type,subject_id,currency,data,dedupe_key)
		VALUES ($1,$2,'delinquency.entered','customer',$3,'USD','{"from_state":"grace","to_state":"delinquent","overdue_amount":12000000}', $4)`, lifecycleID, a.MerchantID.UUID(), payer, uuid.NewString())
	require.NoError(t, err)
	settledAt := time.Now().UTC().Truncate(time.Microsecond)
	_, err = pool.Exec(ctx, `INSERT INTO openrails.payments
		(id,merchant_id,customer_id,price_id,rail,transaction_id,amount,list_amount,currency,status,money_movement,psp_id,purchased_at)
		VALUES ($1,$2,$3,$4,'nmi',$5,7000000,7000000,'USD','completed','rail',$6,$7)`, payment, a.MerchantID.UUID(), payer, price, uuid.NewString(), psp, settledAt)
	require.NoError(t, err)
	// A failed consumer retains the same event for replay through either transport.
	options := openrails.HostEventListOptions{Type: openrails.HostEventPaymentSettled, Limit: 1}
	first, err := remote.ListHostEvents(ctx, options)
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.Equal(t, openrails.PaymentID(payment), first[0].Payment.PaymentID)
	require.EqualValues(t, 7000000, first[0].Payment.Amount)
	require.Equal(t, openrails.CustomerID(payer), first[0].Payment.CustomerID, "the settled event names its payer")
	require.Equal(t, openrails.PriceID(price), first[0].Payment.PriceID, "the settled event names its price")
	require.Nil(t, first[0].Payment.SubscriptionID)
	status, body := requestJSON(t, http.MethodGet, server.BaseURL+"/v1/merchant/host-events?type=payment.settled", token, nil)
	require.Equal(t, http.StatusOK, status, string(body))
	require.Contains(t, string(body), `"amount":"7000000"`)
	require.Contains(t, string(body), `"customer_id":"`+payer.String()+`"`)
	require.Contains(t, string(body), `"price_id":"`+openrails.PriceID(price).String()+`"`)
	require.Equal(t, "USD", first[0].Payment.Currency)
	require.True(t, settledAt.Equal(first[0].OccurredAt))
	require.Nil(t, first[0].Delinquency)
	replay, err := local.ListHostEvents(ctx, options)
	require.NoError(t, err)
	require.Equal(t, first, replay)
	otherToken := server.MintAPIKey(b.MerchantSlug, "other-host-events", []string{controlplane.PermMerchantHostEventsRead, controlplane.PermMerchantHostEventsAcknowledge, controlplane.PermMerchantPaymentsRead})
	other := server.Client(openrails.WithTokenProvider(func(context.Context) (string, error) { return otherToken, nil }))
	require.ErrorIs(t, other.AcknowledgeHostEvent(ctx, first[0].ID), openrails.ErrNotFound)
	missing, err := other.ListHostEvents(ctx, options)
	require.NoError(t, err)
	require.Empty(t, missing)
	readToken := server.MintAPIKey(a.MerchantSlug, "read-host-events", []string{controlplane.PermMerchantHostEventsRead})
	reader := server.Client(openrails.WithTokenProvider(func(context.Context) (string, error) { return readToken, nil }))
	require.Error(t, reader.AcknowledgeHostEvent(ctx, first[0].ID))
	for _, client := range []*openrails.Client{remote, local} {
		_, err = client.ListHostEvents(ctx, openrails.HostEventListOptions{Limit: openrails.MaxHostEventPageSize + 1})
		require.Error(t, err)
		_, err = client.ListHostEvents(ctx, openrails.HostEventListOptions{Type: "unknown"})
		require.Error(t, err)
	}
	require.NoError(t, local.AcknowledgeHostEvent(ctx, first[0].ID))
	require.NoError(t, remote.AcknowledgeHostEvent(ctx, first[0].ID))
	pending, err := remote.ListHostEvents(ctx, options)
	require.NoError(t, err)
	require.Empty(t, pending)
	beforeCommit, err := local.ListHostEvents(ctx, openrails.HostEventListOptions{})
	require.NoError(t, err)
	require.Empty(t, beforeCommit)
	require.NoError(t, lifecycleTx.Commit(ctx))
	options.IncludeAcknowledged, options.PaymentID = true, openrails.PaymentID(payment)
	history, err := local.ListHostEvents(ctx, options)
	require.NoError(t, err)
	require.Len(t, history, 1)
	require.NotNil(t, history[0].AcknowledgedAt)
	lifecycle, err := remote.ListHostEvents(ctx, openrails.HostEventListOptions{})
	require.NoError(t, err)
	require.Len(t, lifecycle, 1, "payment acknowledgment must not consume lifecycle events")
	require.Equal(t, lifecycleID, lifecycle[0].ID)
	require.Nil(t, lifecycle[0].Payment)
	require.Equal(t, openrails.CustomerID(payer), lifecycle[0].Delinquency.CustomerID)
	require.Equal(t, "delinquent", lifecycle[0].Delinquency.ToState)
	require.EqualValues(t, 12000000, lifecycle[0].Delinquency.OverdueAmount)
	// RLS remains fail-closed even when the SQL has no merchant predicate.
	bPool := h.MerchantPool(b.MerchantID.UUID())
	var visible int
	require.NoError(t, bPool.QueryRow(ctx, `SELECT count(*) FROM openrails.host_outbox WHERE id=$1 OR id=$2`, first[0].ID, lifecycleID).Scan(&visible))
	require.Zero(t, visible)
	tag, err := bPool.Exec(ctx, `UPDATE openrails.host_outbox SET delivered_at=now() WHERE id=$1 OR id=$2`, first[0].ID, lifecycleID)
	require.NoError(t, err)
	require.Zero(t, tag.RowsAffected())
	// A bounded retention pass may delete acknowledged events only.
	q := gen.New(pool)
	deleted, err := q.DeleteDeliveredPaymentSettlementsBefore(ctx, gen.DeleteDeliveredPaymentSettlementsBeforeParams{MerchantID: a.MerchantID.UUID(), Cutoff: time.Now().Add(time.Hour), RowLimit: 10})
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
	// Eligibility comes from durable payments, never acknowledged event history.
	for _, client := range []*openrails.Client{remote, local} {
		settled, err := client.HasSettledPayment(ctx, openrails.CustomerID(payer), openrails.PriceID(price))
		require.NoError(t, err)
		require.True(t, settled, "settlement proof must survive event retention")
		for _, pair := range [][2]uuid.UUID{{uuid.New(), price}, {payer, uuid.New()}} {
			settled, err = client.HasSettledPayment(ctx, openrails.CustomerID(pair[0]), openrails.PriceID(pair[1]))
			require.NoError(t, err)
			require.False(t, settled)
		}
		_, err = client.HasSettledPayment(ctx, openrails.CustomerID(payer), openrails.PriceID{})
		require.Error(t, err)
	}
	settled, err := other.HasSettledPayment(ctx, openrails.CustomerID(payer), openrails.PriceID(price))
	require.NoError(t, err)
	require.False(t, settled, "another merchant must not observe the payment")
	issuer := server.RegisterDelegatedIssuer("settlement-reader-"+uuid.NewString(), a.MerchantSlug)
	narrow := issuer.Mint(uuid.NewString(), "", "", []string{controlplane.PermMerchantHostEventsRead})
	status, body = requestJSON(t, http.MethodGet, server.BaseURL+"/v1/merchant/customers/"+payer.String()+"/payment-settlement-status?price_id="+openrails.PriceID(price).String(), narrow, nil)
	require.Equal(t, http.StatusForbidden, status, "host-event permission alone must not grant payment reads: %s", body)
	_, err = pool.Exec(ctx, `UPDATE openrails.payments SET status='refunded' WHERE id=$1`, payment)
	require.NoError(t, err)
	settled, err = remote.HasSettledPayment(ctx, openrails.CustomerID(payer), openrails.PriceID(price))
	require.NoError(t, err)
	require.True(t, settled, "a refund cannot recreate first-payment eligibility")
	_, err = pool.Exec(ctx, `UPDATE openrails.payments SET deleted_at=now() WHERE id=$1`, payment)
	require.NoError(t, err)
	settled, err = local.HasSettledPayment(ctx, openrails.CustomerID(payer), openrails.PriceID(price))
	require.NoError(t, err)
	require.True(t, settled, "archiving a real payment must not grant another first-payment trial")
	for _, test := range []struct {
		status, movement string
		amount           int64
	}{
		{"pending", "rail", 7000000}, {"failed", "rail", 7000000},
		{"completed", "none", 7000000}, {"completed", "rail", 0},
	} {
		_, err = pool.Exec(ctx, `UPDATE openrails.payments SET status=$2,money_movement=$3,amount=$4 WHERE id=$1`, payment, test.status, test.movement, test.amount)
		require.NoError(t, err)
		settled, err = local.HasSettledPayment(ctx, openrails.CustomerID(payer), openrails.PriceID(price))
		require.NoError(t, err)
		require.False(t, settled, "only a positive completed rail payment establishes eligibility")
	}

	deleted, err = q.DeleteDeliveredHostLifecycleEventsBefore(ctx, gen.DeleteDeliveredHostLifecycleEventsBeforeParams{MerchantID: a.MerchantID.UUID(), Cutoff: time.Now().Add(time.Hour), RowLimit: 10})
	require.NoError(t, err)
	require.Zero(t, deleted, "unacknowledged lifecycle instruction must survive retention")
	require.NoError(t, local.AcknowledgeHostEvent(ctx, lifecycleID))
	require.NoError(t, remote.AcknowledgeHostEvent(ctx, lifecycleID))
	deleted, err = q.DeleteDeliveredHostLifecycleEventsBefore(ctx, gen.DeleteDeliveredHostLifecycleEventsBeforeParams{MerchantID: a.MerchantID.UUID(), Cutoff: time.Now().Add(time.Hour), RowLimit: 10})
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
	status, body = requestJSON(t, http.MethodGet, server.BaseURL+"/v1/merchant/host-events?limit=bad", token, nil)
	require.Equal(t, http.StatusBadRequest, status, string(body))
}
