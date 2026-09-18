//go:build integration

package money_test

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/money"
)

// Customer pay-now (#809) on the invoice_collection machine: the same
// durable operation as the merchant retry under a user origin, gated to
// rails OpenRails drives, with the customer's recovery projection.

func (e collectionEnv) payNow(t *testing.T, runner *intents.Runner, key string, method uuid.UUID) (*money.InvoiceCollectionRetryResult, error) {
	t.Helper()
	return e.svc.PayInvoiceNow(e.ctx, runner, e.payer, money.InvoiceCollectionRetryRequest{InvoiceID: e.invoice, IdempotencyKey: key, PaymentMethodID: method})
}

func (e collectionEnv) recovery(t *testing.T) *openrails.PaymentRecovery {
	t.Helper()
	out, err := e.svc.InvoiceRecovery(e.ctx, e.payer, []*models.Invoice{e.invoiceRow(t)})
	require.NoError(t, err)
	require.Contains(t, out, e.invoice)
	return out[e.invoice]
}

// A never-attempted open invoice is not merchant-retryable but is payable by
// its customer: one attempt, settled, invoice paid, one ledger transfer.
func TestInvoicePayNow_SettlesNeverAttemptedInvoice(t *testing.T) {
	e := newCollectionEnv(t, string(models.RailNMI))
	require.Equal(t, "open", e.invoiceRow(t).Status)
	before := e.recovery(t)
	require.True(t, before.Retryable, "%+v", before)
	require.Zero(t, before.AttemptCount)
	require.Equal(t, []openrails.PaymentMethodID{openrails.PaymentMethodID(e.method)}, before.CompatiblePaymentMethodIDs)

	silent := &fakeCharger{}
	_, err := e.retry(t, collectionRunner(e.db, silent, nil), "merchant-key", e.method)
	require.ErrorIs(t, err, money.ErrInvoiceNotRetryable, "the merchant retry needs a prior failure")
	require.Zero(t, silent.chargeCount())

	charger := &fakeCharger{}
	result, err := e.payNow(t, collectionRunner(e.db, charger, nil), "pay-1", e.method)
	require.NoError(t, err)
	require.False(t, result.Replayed)
	require.Equal(t, "settled", result.Attempt.Status)
	require.Equal(t, "paid", result.Invoice.Status)
	require.NotEqual(t, uuid.Nil, result.OperationID)
	require.Equal(t, 1, charger.chargeCount())
	require.Equal(t, e.method, charger.charges[0].PaymentMethodID)
	e.requireSettledOnce(t)
	op := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, result.OperationID, op.ID)
	require.Equal(t, string(intents.OriginUser), op.Origin)

	after := e.recovery(t)
	require.False(t, after.Retryable)
	require.Equal(t, openrails.RecoveryBlockedNotDue, after.BlockedReason)
	require.Equal(t, 1, after.AttemptCount)
	require.Nil(t, after.Operation)

	replayed, err := e.payNow(t, collectionRunner(e.db, &fakeCharger{}, nil), "pay-1", e.method)
	require.NoError(t, err)
	require.True(t, replayed.Replayed)
	require.Equal(t, result.Attempt.ID, replayed.Attempt.ID)
	_, err = e.payNow(t, collectionRunner(e.db, silent, nil), "pay-2", e.method)
	require.ErrorIs(t, err, money.ErrInvoiceNotRetryable, "a paid invoice is not payable")
	require.Zero(t, silent.chargeCount())
	e.requireSettledOnce(t)
}

// A provider-managed rail is refused before the operation exists: no
// attempt row, no operation, no provider call, invoice untouched. The
// merchant retry on the same method is allowed (Stripe charges saved
// methods); the customer surface is what the rail gate narrows.
func TestInvoicePayNow_RefusesProviderManagedRailWithoutProviderTraffic(t *testing.T) {
	e := newCollectionEnv(t, string(models.RailStripe))
	_, err := e.svc.ChargeOutstanding(e.ctx, collectionRunner(e.db, &fakeCharger{declineAll: true}, nil), 0)
	require.NoError(t, err)
	require.Equal(t, "past_due", e.invoiceRow(t).Status)
	before := e.invoiceRow(t)
	ops := len(collectionIntents(t, e.pool, e.ctx, e.invoice))

	charger := &fakeCharger{}
	_, err = e.payNow(t, collectionRunner(e.db, charger, nil), "stripe-pay", e.method)
	require.ErrorIs(t, err, money.ErrPaymentRecoveryRailUnsupported)
	require.Zero(t, charger.chargeCount(), "nothing reaches the provider")
	require.Len(t, collectionIntents(t, e.pool, e.ctx, e.invoice), ops, "no operation is created")
	require.Equal(t, []string{"failed"}, e.attemptStatuses(t), "no attempt is recorded")
	after := e.invoiceRow(t)
	require.Equal(t, before.Status, after.Status)
	require.Equal(t, before.AmountDue, after.AmountDue)
	require.Nil(t, after.CollectionIntentID)

	recovery := e.recovery(t)
	require.False(t, recovery.Retryable)
	require.Equal(t, openrails.RecoveryBlockedNoPaymentMethod, recovery.BlockedReason, "a Stripe method is not a recovery method")
	require.Empty(t, recovery.CompatiblePaymentMethodIDs)

	// The merchant's own retry still runs on Stripe.
	result, err := e.retry(t, collectionRunner(e.db, charger, nil), "merchant-stripe", e.method)
	require.NoError(t, err)
	require.Equal(t, "settled", result.Attempt.Status)
}

// A decline records the attempt, applies the invoice decline doctrine and
// leaves every earlier attempt intact; the same key replays the failed
// attempt, a new key starts a new one.
func TestInvoicePayNow_DeclineRecordsAttemptAndKeepsHistory(t *testing.T) {
	e := newCollectionEnv(t, string(models.RailNMI))
	_, err := e.svc.ChargeOutstanding(e.ctx, collectionRunner(e.db, &fakeCharger{declineAll: true}, nil), 0)
	require.NoError(t, err)
	require.Equal(t, []string{"failed"}, e.attemptStatuses(t))
	scheduled := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)

	declined := &fakeCharger{declineAll: true}
	result, err := e.payNow(t, collectionRunner(e.db, declined, nil), "pay-declined", e.method)
	require.NoError(t, err)
	require.Equal(t, "failed", result.Attempt.Status)
	require.NotNil(t, result.Attempt.FailureCode)
	require.Equal(t, 1, declined.chargeCount())
	require.Equal(t, []string{"failed", "failed"}, e.attemptStatuses(t))
	require.Equal(t, "past_due", result.Invoice.Status)
	require.Equal(t, int32(2), result.Invoice.CollectionFailureCount)
	require.Nil(t, result.Invoice.CollectionIntentID, "a decline releases the invoice")
	require.Equal(t, intents.StatusFailedTerminal, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status)
	require.Equal(t, intents.StatusFailedTerminal, mustIntent(t, e, scheduled.ID).Status, "history is not rewritten")

	recovery := e.recovery(t)
	require.True(t, recovery.Retryable, "%+v", recovery)
	require.Equal(t, 2, recovery.AttemptCount)
	require.NotEmpty(t, recovery.FailureCategory)
	require.NotNil(t, recovery.LastFailedAt)
	require.NotNil(t, recovery.NextAttemptAt, "bucket 1 keeps the invoice's own schedule")

	replayed, err := e.payNow(t, collectionRunner(e.db, &fakeCharger{}, nil), "pay-declined", e.method)
	require.NoError(t, err)
	require.True(t, replayed.Replayed)
	require.Equal(t, result.Attempt.ID, replayed.Attempt.ID)
	require.Equal(t, "failed", replayed.Attempt.Status)
	require.Equal(t, []string{"failed", "failed"}, e.attemptStatuses(t))

	ok := &fakeCharger{}
	again, err := e.payNow(t, collectionRunner(e.db, ok, nil), "pay-again", e.method)
	require.NoError(t, err)
	require.False(t, again.Replayed)
	require.Equal(t, "settled", again.Attempt.Status)
	require.Equal(t, []string{"settled", "failed", "failed"}, e.attemptStatuses(t))
	e.requireSettledOnce(t)
}

func mustIntent(t *testing.T, e collectionEnv, id uuid.UUID) gen.OpenrailsRailIntent {
	t.Helper()
	for _, op := range collectionIntents(t, e.pool, e.ctx, e.invoice) {
		if op.ID == id {
			return op
		}
	}
	t.Fatalf("operation %s not found", id)
	return gen.OpenrailsRailIntent{}
}

// A merchant-closed invoice is not the customer's to reopen.
func TestInvoicePayNow_RefusesUncollectibleInvoice(t *testing.T) {
	e := newCollectionEnv(t, string(models.RailNMI))
	_, err := e.pool.Exec(e.ctx, `UPDATE openrails.invoices SET status = 'uncollectible', uncollectible_at = now() WHERE id = $1`, e.invoice)
	require.NoError(t, err)
	charger := &fakeCharger{}
	_, err = e.payNow(t, collectionRunner(e.db, charger, nil), "pay-closed", e.method)
	require.ErrorIs(t, err, money.ErrInvoiceNotRetryable)
	require.Zero(t, charger.chargeCount())
	recovery := e.recovery(t)
	require.False(t, recovery.Retryable)
	require.Equal(t, openrails.RecoveryBlockedUncollectible, recovery.BlockedReason)
}

// Two equal pay-now requests that both wait on the invoice lock resolve to
// one operation (final review R2 applied to the customer surface).
func TestInvoicePayNow_ConcurrentEqualRequestsShareOneOperation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		decline bool
		status  string
	}{{"settles", false, "settled"}, {"declines", true, "failed"}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newCollectionEnv(t, string(models.RailNMI))
			charger := &fakeCharger{declineAll: tc.decline}
			runner := collectionRunner(e.db, charger, charger)
			lock, err := e.pool.Begin(e.ctx)
			require.NoError(t, err)
			_, err = lock.Exec(e.ctx, `SELECT id FROM openrails.invoices WHERE id = $1 FOR UPDATE`, e.invoice)
			require.NoError(t, err)

			results := make([]*money.InvoiceCollectionRetryResult, 2)
			errs := make([]error, 2)
			var wg sync.WaitGroup
			for i := range results {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					results[i], errs[i] = e.payNow(t, runner, "same-customer-key", e.method)
				}(i)
			}
			require.Eventually(t, func() bool { return lockWaiters(e.pool, e.ctx) >= 2 }, 15*time.Second, 20*time.Millisecond, "both requests wait on the invoice lock")
			require.NoError(t, lock.Rollback(e.ctx))
			wg.Wait()

			require.NoError(t, errs[0])
			require.NoError(t, errs[1])
			require.Equal(t, 1, charger.chargeCount(), "one provider submission")
			require.Equal(t, results[0].Attempt.ID, results[1].Attempt.ID)
			require.Equal(t, results[0].OperationID, results[1].OperationID)
			require.NotEqual(t, results[0].Replayed, results[1].Replayed)
			for _, r := range results {
				require.Contains(t, []string{tc.status, "attempted"}, r.Attempt.Status)
			}
			require.Len(t, collectionIntents(t, e.pool, e.ctx, e.invoice), 1)
			require.Equal(t, []string{tc.status}, e.attemptStatuses(t))
			if tc.decline {
				require.Nil(t, e.invoiceRow(t).CollectionIntentID)
				require.Zero(t, e.settledPayments(t))
			} else {
				e.requireSettledOnce(t)
			}
		})
	}
}

// A lost provider answer stays one unknown operation: the same key replays
// it, a new key is refused, the sweep skips the invoice, an empty search
// never resends, and a restarted verifier settles once from the receipt.
func TestInvoicePayNow_LostResponseStaysUnknownAndConvergesOnReceipt(t *testing.T) {
	e := newCollectionEnv(t, string(models.RailNMI))
	lost := &fakeCharger{lostResponse: true}
	result, err := e.payNow(t, collectionRunner(e.db, lost, lost), "pay-lost", e.method)
	require.NoError(t, err)
	require.Equal(t, "attempted", result.Attempt.Status)
	require.Equal(t, 1, lost.chargeCount())
	op := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, result.OperationID, op.ID)
	require.Equal(t, intents.StatusUnknownNeedsVerify, op.Status)
	require.Equal(t, op.ID, *e.invoiceRow(t).CollectionIntentID)

	recovery := e.recovery(t)
	require.False(t, recovery.Retryable)
	require.Equal(t, openrails.RecoveryBlockedOutcomeUnknown, recovery.BlockedReason)
	require.NotNil(t, recovery.Operation)
	require.Equal(t, op.ID, recovery.Operation.ID)
	require.Equal(t, intents.StatusUnknownNeedsVerify, recovery.Operation.Status)
	require.Equal(t, 1, recovery.AttemptCount)

	silent := &fakeCharger{}
	replayed, err := e.payNow(t, collectionRunner(e.db, silent, silent), "pay-lost", e.method)
	require.NoError(t, err)
	require.True(t, replayed.Replayed)
	require.Equal(t, "attempted", replayed.Attempt.Status)
	_, err = e.payNow(t, collectionRunner(e.db, silent, silent), "pay-lost-2", e.method)
	require.ErrorIs(t, err, money.ErrInvoiceRetryOutcomeUnknown)
	n, err := e.svc.ChargeOutstanding(e.ctx, collectionRunner(e.db, silent, silent), 0)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Zero(t, silent.chargeCount())

	hidden := &fakeCharger{}
	dueNow(t, e.pool, e.ctx, op.ID)
	_, err = collectionRunner(e.db, hidden, hidden).RunVerifyOnce(e.ctx)
	require.NoError(t, err)
	require.Equal(t, intents.StatusUnknownNeedsVerify, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status)
	require.Zero(t, hidden.chargeCount())

	restarted := collectionRunner(e.db, hidden, lost)
	dueNow(t, e.pool, e.ctx, op.ID)
	_, err = restarted.RunVerifyOnce(e.ctx)
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status)
	e.requireSettledOnce(t)
	require.Equal(t, []string{"settled"}, e.attemptStatuses(t))
	settled, err := e.payNow(t, collectionRunner(e.db, silent, silent), "pay-lost", e.method)
	require.NoError(t, err)
	require.True(t, settled.Replayed)
	require.Equal(t, "settled", settled.Attempt.Status)
	require.Zero(t, silent.chargeCount())
}

// Another payer's saved method, and another payer's invoice, are refused
// without provider traffic.
func TestInvoicePayNow_RefusesForeignMethodAndInvoice(t *testing.T) {
	e := newCollectionEnv(t, string(models.RailNMI))
	other := newCollectionEnv(t, string(models.RailNMI))
	charger := &fakeCharger{}
	_, err := e.payNow(t, collectionRunner(e.db, charger, nil), "foreign-method", other.method)
	require.ErrorIs(t, err, money.ErrCollectionPaymentMethodInvalid)
	_, err = e.svc.PayInvoiceNow(e.ctx, collectionRunner(e.db, charger, nil), e.payer, money.InvoiceCollectionRetryRequest{InvoiceID: other.invoice, IdempotencyKey: "foreign-invoice", PaymentMethodID: e.method})
	require.Error(t, err)
	require.ErrorContains(t, err, "lock invoice")
	require.Zero(t, charger.chargeCount())
	require.Equal(t, "open", other.invoiceRow(t).Status)
	require.Nil(t, other.invoiceRow(t).CollectionIntentID)
}
