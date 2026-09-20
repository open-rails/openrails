//go:build integration

package money_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
)

// hookCharger succeeds like a real rail and runs a hook BETWEEN the provider
// charge and OpenRails' recording transaction — the crash/interleave window the
// #674 arrears hardening closes.
type hookCharger struct {
	charges []money.ChargeRequest
	hook    func()
}

func (h *hookCharger) Prepare(_ context.Context, req money.ChargeRequest) (money.PreparedCharge, error) {
	return money.PreparedChargeFunc(func(context.Context) (money.ChargeResult, error) {
		h.charges = append(h.charges, req)
		if h.hook != nil {
			h.hook()
		}
		return money.ChargeResult{TransactionID: "tx_" + req.IdempotencyKey}, nil
	}), nil
}

// TestChargeOutstanding_MutatedInvoiceUnderLiveOperationFailsClosed: the live
// operation pointer refuses every API mutation, so only raw surgery can change
// an invoice between the successful provider charge and its settlement. When
// that happens the charge is NEVER dropped and NEVER applied blindly: nothing
// is written, the receipt stays on the operation, the invoice keeps pointing
// at it (no further collection) and an operator must repair it.
func TestChargeOutstanding_MutatedInvoiceUnderLiveOperationFailsClosed(t *testing.T) {
	svc, dbi, pool, payer, cur, ctx := moneyInEnvWithDB(t)
	cleanupCollection(t, pool, ctx, payer)
	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailStripe))
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears),
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetInvoiceCollectionPaymentMethod(ctx, payer, money.DefaultCurrency, pm))
	_, err = svc.AccrueOwed(ctx, payer, cur, "usage", "cas-miss", 5_000_000)
	require.NoError(t, err)
	inv, err := svc.FinalizeInvoice(ctx, payer, cur, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, "open", inv.Status)

	ch := &hookCharger{hook: func() {
		_, err := pool.Exec(ctx,
			`UPDATE billing.invoices SET status = 'voided', amount_due = 0, voided_at = now() WHERE id = $1`, inv.ID)
		require.NoError(t, err)
	}}
	runner := collectionRunner(dbi, ch, nil)
	n, err := svc.ChargeOutstanding(ctx, runner, 0)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Len(t, ch.charges, 1)
	op := latestCollectionIntent(t, pool, ctx, inv.ID)
	require.Equal(t, op.ID.String(), ch.charges[0].IdempotencyKey, "provider identity = the durable operation, not the mutable amount snapshot")
	require.Equal(t, "invoice_collection:"+inv.ID.String()+":attempt:0", op.IdempotencyKey)
	require.Equal(t, intents.StatusUnknownNeedsVerify, op.Status)
	require.Contains(t, string(op.ResultEvidence), "tx_"+op.ID.String(), "the receipt is retained")
	require.Contains(t, *op.LastFailureReason, "needs repair")

	var settled int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM billing.invoice_payments WHERE invoice_id = $1 AND status = 'settled'`, inv.ID).Scan(&settled))
	require.Zero(t, settled, "nothing is recorded against an invoice that no longer matches the frozen snapshot")
	var transfers int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM billing.ledger_transfers WHERE customer_id = $1 AND transfer_type = 'owed_payment'`, payer.UUID()).Scan(&transfers))
	require.Zero(t, transfers)
	var pointer *uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `SELECT collection_intent_id FROM billing.invoices WHERE id = $1`, inv.ID).Scan(&pointer))
	require.NotNil(t, pointer, "the invoice stays claimed until an operator repairs it")
	require.Equal(t, op.ID, *pointer)

	// Neither the verifier nor the sweep moves money: the verifier re-derives
	// the same repair need, the sweep sees a claimed invoice.
	dueNow(t, pool, ctx, op.ID)
	_, err = runner.RunVerifyOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, intents.StatusUnknownNeedsVerify, latestCollectionIntent(t, pool, ctx, inv.ID).Status)
	n, err = svc.ChargeOutstanding(ctx, runner, 0)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Len(t, ch.charges, 1)
	_, err = runner.Resolve(ctx, op.ID, intents.Resolution{ProviderReference: "tx_" + op.ID.String(), Actor: "ops", Reason: "portal"})
	require.ErrorIs(t, err, intents.ErrResolutionRejected, "a retained receipt is the verifier's, not re-resolvable")
}

// TestChargeOutstanding_AttemptKeyAdvancesAfterRecordedAttempt proves the durable
// attempt identity: a recorded failed attempt ends its operation; the explicit
// retry is a NEW operation with its own provider identity, and its ledger key is
// the client's retry key bound to the invoice.
func TestChargeOutstanding_AttemptKeyAdvancesAfterRecordedAttempt(t *testing.T) {
	svc, dbi, pool, payer, cur, ctx := moneyInEnvWithDB(t)
	cleanupCollection(t, pool, ctx, payer)
	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailStripe))
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears),
	})
	require.NoError(t, err)
	require.NoError(t, svc.SetInvoiceCollectionPaymentMethod(ctx, payer, money.DefaultCurrency, pm))
	_, err = svc.AccrueOwed(ctx, payer, cur, "usage", "attempt-key", 5_000_000)
	require.NoError(t, err)
	inv, err := svc.FinalizeInvoice(ctx, payer, cur, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)

	decl := &fakeCharger{declineAll: true}
	n, err := svc.ChargeOutstanding(ctx, collectionRunner(dbi, decl, nil), 0)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Equal(t, 1, decl.chargeCount())
	first := latestCollectionIntent(t, pool, ctx, inv.ID)
	require.Equal(t, intents.StatusFailedTerminal, first.Status)
	require.Equal(t, first.ID.String(), decl.charges[0].IdempotencyKey)

	ok := &fakeCharger{}
	result, err := svc.RetryInvoiceCollection(ctx, collectionRunner(dbi, ok, nil), payer, money.InvoiceCollectionRetryRequest{
		InvoiceID: inv.ID, IdempotencyKey: "retry-after-decline", PaymentMethodID: pm,
	})
	require.NoError(t, err)
	require.False(t, result.Replayed)
	require.Equal(t, 1, ok.chargeCount())
	second := latestCollectionIntent(t, pool, ctx, inv.ID)
	require.NotEqual(t, first.ID, second.ID)
	require.Equal(t, second.ID.String(), ok.charges[0].IdempotencyKey, "a new operation carries a new provider identity")
	require.Contains(t, second.IdempotencyKey, "invoice_collection:"+inv.ID.String()+":retry:")
	require.Equal(t, "settled", result.Attempt.Status)
	require.Equal(t, "paid", result.Invoice.Status)
}
