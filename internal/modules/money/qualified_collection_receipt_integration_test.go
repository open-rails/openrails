//go:build integration

package money_test

import (
	"context"
	"encoding/json"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/money"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/stretchr/testify/require"
)

func TestQualifiedCollectionReceiptCustody(t *testing.T) {
	e := nmiReceiptScenario(t)
	store := intents.NewStore(e.db)
	operation := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	candidate := intents.CollectionCandidate{TransactionID: "sale-receipt"}
	require.NoError(t, store.RetainCollectionCandidate(e.ctx, operation, candidate))
	dueNow(t, e.pool, e.ctx, e.op)
	_, err := e.runner.RunVerifyOnce(e.ctx)
	require.NoError(t, err)
	operation = latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusUnknownNeedsVerify, operation.Status, "a candidate without provider evidence cannot settle")
	require.Error(t, (intents.CollectedReceipt{}).Validate(operation))

	e.gateway.orderSale(e.op.String(), candidate.TransactionID)
	e.gateway.payment(candidate.TransactionID, e.vault, "0.05", e.currency)
	// Hold the real provider query open across the runner's lease renewal.
	_, err = e.pool.Exec(e.ctx, `UPDATE billing.rail_intents SET claimed_until=now()+interval '1 hour' WHERE id=$1`, e.op)
	require.NoError(t, err)
	started, gate := make(chan struct{}), make(chan struct{})
	e.gateway.queryStarted = started
	e.gateway.queryGate = gate
	type readResult struct {
		receipt intents.CollectedReceipt
		found   bool
		err     error
	}
	answer := make(chan readResult, 1)
	go func() {
		receipt, found, err := e.plane.ReadCollectionReceipt(e.ctx, operation, candidate.TransactionID)
		answer <- readResult{receipt, found, err}
	}()
	<-started
	renewed, err := store.RenewClaim(e.ctx, e.op, time.Now(), time.Now().Add(2*time.Hour))
	require.NoError(t, err)
	require.True(t, renewed)
	close(gate)
	result := <-answer
	receipt, found, err := result.receipt, result.found, result.err
	require.NoError(t, err)
	require.True(t, found)

	for _, mutate := range []func(*gen.OpenrailsRailIntent){
		func(in *gen.OpenrailsRailIntent) { in.ID = uuid.New() },
		func(in *gen.OpenrailsRailIntent) { in.MerchantID = uuid.New() },
		func(in *gen.OpenrailsRailIntent) { id := uuid.New(); in.PspID = &id },
		func(in *gen.OpenrailsRailIntent) {
			var p intents.InvoiceCollectionPayload
			require.NoError(t, json.Unmarshal(in.Payload, &p))
			p.Amount += 10_000
			p.AmountMinor++
			in.Payload, _ = json.Marshal(p)
		},
	} {
		changed := operation
		mutate(&changed)
		require.Error(t, receipt.Validate(changed), "a different accepted operation cannot reuse the receipt")
	}

	require.NoError(t, e.db.MerchantTx(e.ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := intents.NewStore(e.db.NewWithPgxTx(tx)).RetainCollectedReceipt(ctx, operation, receipt)
		require.ErrorContains(t, err, "outside a transaction")
		return nil
	}))

	retained, err := store.RetainCollectedReceipt(e.ctx, operation, receipt)
	require.NoError(t, err)
	require.Equal(t, candidate.TransactionID, retained.TransactionID())
	custodyOnly, err := store.Get(e.ctx, e.op)
	require.NoError(t, err)
	require.Empty(t, intents.EvidenceString(custodyOnly, "transaction_id"))
	e.gateway.mu.Lock()
	beforeReads := e.gateway.queryCalls
	delete(e.gateway.saleForOrder, e.op.String())
	e.gateway.mu.Unlock()
	_, err = e.pool.Exec(e.ctx, `UPDATE billing.rail_intents SET claimed_until=NULL WHERE id=$1`, e.op)
	require.NoError(t, err)
	_, err = e.runner.Resolve(e.ctx, e.op, intents.Resolution{NotExecuted: true, Actor: "ops", Reason: "provider search later disappeared"})
	require.ErrorIs(t, err, intents.ErrResolutionRejected)
	require.Contains(t, err.Error(), "qualified receipt")
	e.gateway.mu.Lock()
	afterReads := e.gateway.queryCalls
	e.gateway.mu.Unlock()
	require.Equal(t, beforeReads, afterReads, "receipt custody must reject nonexecution before any new provider read")
	e.gateway.orderSale(e.op.String(), candidate.TransactionID)

	for _, write := range []func() error{
		func() error {
			return store.RecordProgress(e.ctx, e.op, map[string]any{"qualified_receipt": map[string]any{"transaction_id": "forged"}})
		},
		func() error {
			_, err := store.RecordProgressIfAbsent(e.ctx, e.op, " qualified_receipt ", nil)
			return err
		},
		func() error {
			return store.MarkSucceeded(e.ctx, e.op, time.Now(), map[string]any{"qualified_receipt": nil})
		},
		func() error {
			return store.MarkUnknown(e.ctx, e.op, time.Now(), "changed", map[string]any{"qualified_receipt": nil})
		},
		func() error {
			return store.MarkFailedTerminal(e.ctx, e.op, "changed", map[string]any{"qualified_receipt": nil})
		},
	} {
		require.ErrorContains(t, write(), "reserved")
	}
	// A reader actually armed for another account cannot be relabelled by passing
	// the expected PSP next to it, even when that server returns matching facts.
	armed, ok, err := e.plane.ResolveNMIClient(e.ctx, operation.MerchantID, operation.PspID)
	require.NoError(t, err)
	require.True(t, ok)
	wrong, err := nmi.NewAccountClient(operation.MerchantID, uuid.New(), "wrong-account", &config.NMIProviderSettings{SecurityKey: "synthetic-key"}, true)
	require.NoError(t, err)
	wrong.QueryURL = armed.QueryURL
	wrong.V5BaseURL = armed.V5BaseURL
	resolver := receiptFixtureNMI{client: wrong, request: money.ChargeRequest{MerchantID: operation.MerchantID, Instrument: e.frozenInstrument(t)}}
	_, _, err = intents.ReadNMICollectionReceipt(e.ctx, operation, resolver, candidate.TransactionID)
	require.ErrorContains(t, err, "another provider account")
	// Release the test lease before driving the ordinary verifier.
	_, err = e.pool.Exec(e.ctx, `UPDATE billing.rail_intents SET claimed_until=NULL WHERE id=$1`, e.op)
	require.NoError(t, err)

	// A second internally valid provider object still cannot replace custody.
	e.gateway.orderSale(e.op.String(), "different-sale")
	e.gateway.payment("different-sale", e.vault, "0.05", e.currency)
	different, found, err := e.plane.ReadCollectionReceipt(e.ctx, operation, "different-sale")
	require.NoError(t, err)
	require.True(t, found)
	_, err = store.RetainCollectedReceipt(e.ctx, operation, different)
	require.ErrorContains(t, err, "conflicting")

	// Local failure cannot roll back the already committed provider receipt.
	_, err = e.pool.Exec(e.ctx, `UPDATE billing.invoices SET status='voided', amount_due=0 WHERE id=$1`, e.invoice)
	require.NoError(t, err)
	dueNow(t, e.pool, e.ctx, e.op)
	_, err = e.runner.RunVerifyOnce(e.ctx)
	require.NoError(t, err)
	operation = latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusUnknownNeedsVerify, operation.Status)
	retained, found, err = intents.LoadCollectedReceipt(operation)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, candidate.TransactionID, retained.TransactionID())
	_, err = e.pool.Exec(e.ctx, `UPDATE billing.invoices SET status='open', amount_due=50000 WHERE id=$1`, e.invoice)
	require.NoError(t, err)
	// The changed provider answer must never be consulted after custody.
	dueNow(t, e.pool, e.ctx, e.op)
	_, err = e.runner.RunVerifyOnce(e.ctx)
	require.NoError(t, err)
	operation = latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusSucceeded, operation.Status)
	retained, found, err = intents.LoadCollectedReceipt(operation)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, candidate.TransactionID, retained.TransactionID())
	require.NoError(t, store.PruneSucceeded(e.ctx, e.op, nil, false, false))
	operation = latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	_, found, err = intents.LoadCollectedReceipt(operation)
	require.NoError(t, err)
	require.True(t, found, "generic pruning preserves the receipt and its binding payload")
	same, err := store.RetainCollectedReceipt(e.ctx, operation, retained)
	require.NoError(t, err)
	require.Equal(t, retained.TransactionID(), same.TransactionID(), "terminal equal receipt replays")
	require.Len(t, e.gateway.sentOrderIDs(), 1)
}

// This drives custody through the real verifier rather than calling the store:
// deleting the handler's custody write must strand the second pass offline.
func TestInvoiceCollectionRetainsReceiptBeforeLocalFailure(t *testing.T) {
	e := nmiReceiptScenario(t)
	e.gateway.orderSale(e.op.String(), "before-local-write")
	e.gateway.payment("before-local-write", e.vault, "0.05", e.currency)
	_, err := e.pool.Exec(e.ctx, `UPDATE billing.invoices SET status='voided',amount_due=0 WHERE id=$1`, e.invoice)
	require.NoError(t, err)
	dueNow(t, e.pool, e.ctx, e.op)
	_, err = e.runner.RunVerifyOnce(e.ctx)
	require.NoError(t, err)
	operation := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusUnknownNeedsVerify, operation.Status)
	receipt, found, err := intents.LoadCollectedReceipt(operation)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "before-local-write", receipt.TransactionID())
	e.gateway.mu.Lock()
	e.gateway.saleForOrder = map[string]string{}
	e.gateway.payments = map[string]map[string]any{}
	e.gateway.mu.Unlock()
	_, err = e.pool.Exec(e.ctx, `UPDATE billing.invoices SET status='open',amount_due=50000 WHERE id=$1`, e.invoice)
	require.NoError(t, err)
	dueNow(t, e.pool, e.ctx, e.op)
	_, err = e.runner.RunVerifyOnce(e.ctx)
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status)
	e.requireSettledOnce(t)
	require.Len(t, e.gateway.sentOrderIDs(), 1)
}
