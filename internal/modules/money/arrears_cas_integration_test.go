//go:build integration

package money_test

import (
	"context"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
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

func (h *hookCharger) ChargeSavedMethod(_ context.Context, req money.ChargeRequest) (money.ChargeResult, error) {
	h.charges = append(h.charges, req)
	if h.hook != nil {
		h.hook()
	}
	return money.ChargeResult{TransactionID: "tx_" + req.IdempotencyKey}, nil
}

// TestChargeOutstanding_CASMissAfterSuccessfulCharge_RecordsPayment proves the
// #674 minimal hardening: when the invoice mutates between the successful
// provider charge and the snapshot CAS (ApplyInvoicePaymentSnapshot returns 0
// rows), the charge is NEVER silently dropped — a settled invoice_payments row
// and the owed_payment ledger transfer are still recorded (unapplied to
// amount_due) for repair.
func TestChargeOutstanding_CASMissAfterSuccessfulCharge_RecordsPayment(t *testing.T) {
	svc, pool, payer, cur, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailStripe))
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, cur, "usage", "cas-miss", 5_000_000)
	require.NoError(t, err)
	inv, err := svc.FinalizeInvoice(ctx, payer, cur, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, "open", inv.Status)

	// Between provider success and the recording tx, the invoice is voided out
	// from under the collector: the snapshot CAS will match 0 rows.
	ch := &hookCharger{hook: func() {
		_, err := pool.Exec(ctx,
			`UPDATE openrails.invoices SET status = 'voided', amount_due = 0, voided_at = now() WHERE id = $1`, inv.ID)
		require.NoError(t, err)
	}}
	n, err := svc.ChargeOutstanding(ctx, ch, 0)
	require.NoError(t, err)
	require.Equal(t, 1, n, "the provider charge happened and must be counted")
	require.Len(t, ch.charges, 1)
	key := "invoice:" + inv.ID.String() + ":attempt:0"
	require.Equal(t, key, ch.charges[0].IdempotencyKey, "key = invoice + durable attempt identity, not the mutable amount snapshot")

	// The successful charge is recorded: settled invoice_payments row + the
	// owed_payment transfer at the attempt key.
	var settled int
	var recorded int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(SUM(amount), 0)::bigint
		FROM openrails.invoice_payments
		WHERE invoice_id = $1 AND status = 'settled'`, inv.ID).Scan(&settled, &recorded))
	require.Equal(t, 1, settled, "CAS miss after a successful charge must still record the payment")
	require.Equal(t, int64(5_000_000), recorded)

	var transfers int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*)
		FROM openrails.ledger_transfers
		WHERE customer_id = $1 AND transfer_type = 'owed_payment'
		  AND source = 'invoice_charge' AND source_id = $2`, payer.UUID(), key).Scan(&transfers))
	require.Equal(t, 1, transfers)

	// Voided invoice is no longer chargeable: the next sweep must not re-charge.
	n, err = svc.ChargeOutstanding(ctx, ch, 0)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Len(t, ch.charges, 1)
}

// TestChargeOutstanding_AttemptKeyAdvancesAfterRecordedAttempt proves the durable
// attempt identity: a recorded failed attempt advances the key; an unrecorded
// outcome (transient error before recording) replays the SAME key so the
// provider's idempotency guard can dedupe instead of double-charging.
func TestChargeOutstanding_AttemptKeyAdvancesAfterRecordedAttempt(t *testing.T) {
	svc, pool, payer, cur, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
	pm := seedPaymentMethod(t, pool, ctx, payer, string(models.RailStripe))
	_, err := svc.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &pm,
	})
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, cur, "usage", "attempt-key", 5_000_000)
	require.NoError(t, err)
	inv, err := svc.FinalizeInvoice(ctx, payer, cur, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)

	// Recorded decline -> attempt 0 recorded as failed.
	decl := &fakeCharger{declineAll: true}
	n, err := svc.ChargeOutstanding(ctx, decl, 0)
	require.NoError(t, err)
	require.Equal(t, 0, n)
	require.Len(t, decl.charges, 1)
	require.Equal(t, "invoice:"+inv.ID.String()+":attempt:0", decl.charges[0].IdempotencyKey)

	// Explicit retry: the recorded failure advances the durable attempt count -> new key.
	ok := &fakeCharger{}
	_, err = svc.RetryInvoiceCollection(ctx, ok, payer, inv.ID)
	require.NoError(t, err)
	require.Len(t, ok.charges, 1)
	require.Equal(t, "invoice:"+inv.ID.String()+":attempt:1", ok.charges[0].IdempotencyKey)

	paid, err := svc.GetInvoiceByID(ctx, payer, inv.ID)
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)
}
