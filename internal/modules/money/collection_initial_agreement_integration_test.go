//go:build integration

package money_test

import (
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestInitialInvoiceAgreementsSettleWithoutReplacingFirstAnchor(t *testing.T) {
	e := newNMIReceiptEnv(t)
	_, err := e.pool.Exec(e.ctx, `UPDATE billing.payment_methods SET stored_credential_unscheduled_ref='' WHERE id=$1`, e.method)
	require.NoError(t, err)
	require.NoError(t, e.svc.SetCreditLimit(e.ctx, e.payer, e.currency, 1_000_000))
	_, err = e.svc.AccrueOwed(e.ctx, e.payer, e.currency, "review", uuid.NewString(), 50_000)
	require.NoError(t, err)
	secondInvoice, err := e.svc.FinalizeInvoice(e.ctx, e.payer, e.currency, time.Now().Add(-time.Hour), time.Now().Add(2*time.Hour))
	require.NoError(t, err)
	require.NotEqual(t, e.invoice, secondInvoice.ID)
	require.EqualValues(t, 50_000, secondInvoice.AmountDue)
	invoices := []uuid.UUID{e.invoice, secondInvoice.ID}
	operations := make([]uuid.UUID, 2)
	// Both accepted charges are dispatched before either positive receipt is visible.
	for i, invoice := range invoices {
		result, err := e.svc.PayInvoiceNow(e.ctx, e.runner, e.payer, money.InvoiceCollectionRetryRequest{InvoiceID: invoice, PaymentMethodID: e.method, IdempotencyKey: uuid.NewString()})
		require.NoError(t, err)
		require.Equal(t, intents.StatusUnknownNeedsVerify, result.Operation.Status)
		operations[i] = result.Operation.ID
	}
	require.Len(t, e.gateway.sentOrderIDs(), 2)
	handler := money.NewInvoiceCollectionHandler(e.db, nil, e.plane, fullModeConfig(), nil)
	for i, op := range operations {
		txn := "review-initial-" + op.String()
		e.gateway.orderSale(op.String(), txn)
		e.gateway.payment(txn, e.vault, "0.05", e.currency)
		row, err := intents.NewStore(e.db).Get(e.ctx, op)
		require.NoError(t, err)
		outcome := handler.Verify(e.ctx, row)
		require.Equal(t, intents.OutcomeSucceeded, outcome.Class, "invoice %d receipt failed: %s", i, outcome.Reason)
		invoice, err := e.svc.GetInvoiceByID(e.ctx, e.payer, invoices[i])
		require.NoError(t, err)
		require.Equal(t, "paid", invoice.Status)
	}
	require.Equal(t, 2, e.owedPaymentTransfers(t))
	owed, err := e.svc.GetOutstandingOwed(e.ctx, e.payer, e.currency)
	require.NoError(t, err)
	require.Zero(t, owed)
	require.Equal(t, "review-initial-"+operations[0].String(), e.methodRow(t).StoredCredentialUnscheduledRef)
	require.Len(t, e.gateway.sentOrderIDs(), 2)
}
