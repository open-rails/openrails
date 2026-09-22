//go:build integration

package money_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/money"
)

// The invoice_collection operation machine against real Postgres and a
// loopback charger: one durable operation per attempt, one provider identity
// per operation, no release/resend after a possible submission, exactly-once
// local effects across restart, and operator resolution through `intents
// resolve`.

type collectionEnv struct {
	svc      *money.MoneyService
	db       *db.DB
	pool     *pgxpool.Pool
	payer    identity.CustomerID
	currency string
	method   uuid.UUID
	invoice  uuid.UUID
	ctx      context.Context
}

func newCollectionEnv(t *testing.T, rail string) collectionEnv {
	t.Helper()
	svc, dbi, pool, payer, currency, ctx := moneyInEnvWithDB(t)
	cleanupCollection(t, pool, ctx, payer)
	method := seedPaymentMethod(t, pool, ctx, payer, rail)
	_, err := svc.UpsertAccountSettings(ctx, payer, currency, money.AccountSettingsInput{BillingMode: strptr(money.BillingModeArrears)})
	require.NoError(t, err)
	require.NoError(t, svc.SetInvoiceCollectionPaymentMethod(ctx, payer, currency, method))
	_, err = svc.AccrueOwed(ctx, payer, currency, "usage", "collection-"+uuid.NewString(), 5_000_000)
	require.NoError(t, err)
	invoice, err := svc.FinalizeInvoice(ctx, payer, currency, time.Now().Add(-time.Hour), time.Now())
	require.NoError(t, err)
	return collectionEnv{svc: svc, db: dbi, pool: pool, payer: payer, currency: currency, method: method, invoice: invoice.ID, ctx: ctx}
}

func (e collectionEnv) invoiceRow(t *testing.T) *models.Invoice {
	t.Helper()
	row, err := e.svc.GetInvoiceByID(e.ctx, e.payer, e.invoice)
	require.NoError(t, err)
	return row
}

func (e collectionEnv) attemptStatuses(t *testing.T) []string {
	t.Helper()
	attempts, _, err := e.svc.ListInvoicePaymentAttempts(e.ctx, e.payer, e.invoice, 20, 0)
	require.NoError(t, err)
	out := make([]string, 0, len(attempts))
	for _, a := range attempts {
		out = append(out, a.Status)
	}
	return out
}

func (e collectionEnv) settledPayments(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT count(*) FROM billing.invoice_payments WHERE invoice_id = $1 AND status = 'settled'`, e.invoice).Scan(&n))
	return n
}

func (e collectionEnv) owedPaymentTransfers(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT count(*) FROM billing.ledger_transfers WHERE customer_id = $1 AND transfer_type = 'owed_payment'`, e.payer.UUID()).Scan(&n))
	return n
}

func (e collectionEnv) requireSettledOnce(t *testing.T) {
	t.Helper()
	inv := e.invoiceRow(t)
	require.Equal(t, "paid", inv.Status)
	require.Equal(t, int64(0), inv.AmountDue)
	require.Nil(t, inv.CollectionIntentID)
	require.Equal(t, 1, e.settledPayments(t))
	require.Equal(t, 1, e.owedPaymentTransfers(t))
	owed, err := e.svc.GetOutstandingOwed(e.ctx, e.payer, e.currency)
	require.NoError(t, err)
	require.Zero(t, owed)
}

func (e collectionEnv) retry(t *testing.T, runner *intents.Runner, key string, method uuid.UUID) (*money.InvoiceCollectionRetryResult, error) {
	t.Helper()
	return e.svc.RetryInvoiceCollection(e.ctx, runner, e.payer, money.InvoiceCollectionRetryRequest{InvoiceID: e.invoice, IdempotencyKey: key, PaymentMethodID: method})
}

func TestInvoiceCollection_ClientKeyReplaysDurableOutcomeWithoutRecharging(t *testing.T) {
	e := newCollectionEnv(t, string(models.RailStripe))
	_, err := e.svc.ChargeOutstanding(e.ctx, collectionRunner(e.db, &fakeCharger{declineAll: true}, nil), 0)
	require.NoError(t, err)
	require.Equal(t, "past_due", e.invoiceRow(t).Status)
	bound := seedPaymentMethod(t, e.pool, e.ctx, e.payer, string(models.RailStripe))

	charger := &fakeCharger{}
	result, err := e.retry(t, collectionRunner(e.db, charger, nil), "client-retry-key", bound)
	require.NoError(t, err)
	require.False(t, result.Replayed)
	require.Equal(t, "paid", result.Invoice.Status)
	require.Nil(t, result.Invoice.LastCollectionFailureCode)
	require.Equal(t, "settled", result.Attempt.Status)
	require.Equal(t, bound, *result.Attempt.PaymentMethodID)
	require.Equal(t, bound, charger.charges[0].PaymentMethodID)
	require.Equal(t, []string{"settled", "failed"}, e.attemptStatuses(t))
	require.Equal(t, 1, charger.chargeCount())

	replayCharger := &fakeCharger{}
	replayed, err := e.retry(t, collectionRunner(e.db, replayCharger, nil), "client-retry-key", bound)
	require.NoError(t, err)
	require.True(t, replayed.Replayed)
	require.Equal(t, result.Attempt.ID, replayed.Attempt.ID)
	require.Zero(t, replayCharger.chargeCount())

	_, err = e.retry(t, collectionRunner(e.db, replayCharger, nil), "client-retry-key", e.method)
	require.ErrorIs(t, err, money.ErrInvoiceRetryIdempotencyConflict)
	_, err = e.pool.Exec(e.ctx, "DELETE FROM billing.payment_methods WHERE id = $1", bound)
	require.NoError(t, err)
	_, err = e.retry(t, collectionRunner(e.db, replayCharger, nil), "client-retry-key", e.method)
	require.ErrorIs(t, err, money.ErrInvoiceRetryIdempotencyConflict)
	e.requireSettledOnce(t)
}

// lockWaiters counts sessions of this database blocked on a lock (0 when the
// read itself fails; the caller polls).
func lockWaiters(pool *pgxpool.Pool, ctx context.Context) int {
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
		return 0
	}
	return n
}

// TestInvoiceCollection_ConcurrentEqualRetriesShareOneOperation (final review
// R2): two identical retries that both wait on the invoice lock resolve to
// ONE operation — the retry key is looked up under the lock, so the second
// replays the first's attempt instead of seeing its in-flight state or the
// attempt-key constraint. One provider submission, one attempt identity in
// both answers, whether the charge settles or declines.
func TestInvoiceCollection_ConcurrentEqualRetriesShareOneOperation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		decline bool
		status  string
	}{{"settles", false, "settled"}, {"declines", true, "failed"}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newCollectionEnv(t, string(models.RailNMI))
			_, err := e.svc.ChargeOutstanding(e.ctx, collectionRunner(e.db, &fakeCharger{declineAll: true}, nil), 0)
			require.NoError(t, err)
			require.Equal(t, "past_due", e.invoiceRow(t).Status)

			charger := &fakeCharger{declineAll: tc.decline}
			runner := collectionRunner(e.db, charger, charger)
			lock, err := e.pool.Begin(e.ctx)
			require.NoError(t, err)
			_, err = lock.Exec(e.ctx, `SELECT id FROM billing.invoices WHERE id = $1 FOR UPDATE`, e.invoice)
			require.NoError(t, err)

			results := make([]*money.InvoiceCollectionRetryResult, 2)
			errs := make([]error, 2)
			var wg sync.WaitGroup
			for i := range results {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					results[i], errs[i] = e.retry(t, runner, "same-client-key", e.method)
				}(i)
			}
			require.Eventually(t, func() bool { return lockWaiters(e.pool, e.ctx) >= 2 }, 15*time.Second, 20*time.Millisecond, "both retries wait on the invoice lock")
			require.NoError(t, lock.Rollback(e.ctx))
			wg.Wait()

			require.NoError(t, errs[0])
			require.NoError(t, errs[1])
			require.Equal(t, 1, charger.chargeCount(), "one provider submission")
			require.Equal(t, results[0].Attempt.ID, results[1].Attempt.ID, "both answers name the same attempt")
			require.NotEqual(t, results[0].Replayed, results[1].Replayed, "one caller enqueued, the other replayed")
			for _, r := range results {
				require.Equal(t, e.method, *r.Attempt.PaymentMethodID)
				require.Contains(t, []string{tc.status, "attempted"}, r.Attempt.Status, "a caller may answer while the other's execution is in flight")
			}
			ops := collectionIntents(t, e.pool, e.ctx, e.invoice)
			require.Len(t, ops, 2, "the scheduled decline and ONE retry operation")
			require.Equal(t, []string{tc.status, "failed"}, e.attemptStatuses(t))
			if tc.decline {
				inv := e.invoiceRow(t)
				require.Nil(t, inv.CollectionIntentID)
				require.Zero(t, e.settledPayments(t))
			} else {
				e.requireSettledOnce(t)
			}
		})
	}
}

// TestInvoiceCollection_LostResponseNeverResendsAndConvergesOnReceipt is the
// #990 core: a charge whose response is lost stays unknown under its one
// provider identity — no scheduled resend, no resend under a new client key,
// no resend from an empty provider search — and settles exactly once when the
// receipt becomes visible, after a restart.
func TestInvoiceCollection_LostResponseNeverResendsAndConvergesOnReceipt(t *testing.T) {
	e := newCollectionEnv(t, string(models.RailNMI))
	_, err := e.svc.ChargeOutstanding(e.ctx, collectionRunner(e.db, &fakeCharger{declineAll: true}, nil), 0)
	require.NoError(t, err)

	lost := &fakeCharger{lostResponse: true}
	result, err := e.retry(t, collectionRunner(e.db, lost, lost), "lost-response", e.method)
	require.NoError(t, err)
	require.Equal(t, "attempted", result.Attempt.Status, "an unresolved operation answers with its live attempt")
	require.Equal(t, 1, lost.chargeCount())
	op := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusUnknownNeedsVerify, op.Status)
	require.Equal(t, op.ID.String(), lost.charges[0].IdempotencyKey)
	blocked := e.invoiceRow(t)
	require.Equal(t, op.ID, *blocked.CollectionIntentID)
	require.Equal(t, "past_due", blocked.Status)
	require.Equal(t, int32(1), blocked.CollectionFailureCount, "an unresolved attempt is not a failure")

	// Same key: the durable state, no charge. New key: refused. Scheduled
	// sweep: skipped. Nothing below reaches the provider.
	silent := &fakeCharger{}
	replayed, err := e.retry(t, collectionRunner(e.db, silent, silent), "lost-response", e.method)
	require.NoError(t, err)
	require.True(t, replayed.Replayed)
	require.Equal(t, "attempted", replayed.Attempt.Status)
	_, err = e.retry(t, collectionRunner(e.db, silent, silent), "different-key", e.method)
	require.ErrorIs(t, err, money.ErrInvoiceRetryOutcomeUnknown)
	n, err := e.svc.ChargeOutstanding(e.ctx, collectionRunner(e.db, silent, silent), 0)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Zero(t, silent.chargeCount())
	require.Len(t, collectionIntents(t, e.pool, e.ctx, e.invoice), 2)

	// Delayed visibility: the provider search is empty. Still unknown.
	hidden := &fakeCharger{}
	dueNow(t, e.pool, e.ctx, op.ID)
	_, err = collectionRunner(e.db, hidden, hidden).RunVerifyOnce(e.ctx)
	require.NoError(t, err)
	require.Equal(t, intents.StatusUnknownNeedsVerify, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status)
	require.Zero(t, hidden.chargeCount(), "an empty search never re-arms the send")

	// Restart: a fresh process finds the receipt by the operation's order
	// reference and settles once; a second look is a no-op.
	restarted := collectionRunner(e.db, hidden, lost)
	dueNow(t, e.pool, e.ctx, op.ID)
	_, err = restarted.RunVerifyOnce(e.ctx)
	require.NoError(t, err)
	final := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusSucceeded, final.Status)
	require.Contains(t, string(final.ResultEvidence), "tx_"+op.ID.String())
	e.requireSettledOnce(t)
	require.Equal(t, []string{"settled", "failed"}, e.attemptStatuses(t))
	require.Equal(t, intents.OutcomeSucceeded, restarted.Registry.Lookup(money.TypeInvoiceCollection).Verify(e.ctx, final).Class)
	e.requireSettledOnce(t)
	require.Zero(t, hidden.chargeCount())
}

// TestInvoiceCollection_UntypedErrorAfterSendIsUnknownNotReleased: an adapter
// error that is not typed as transport-ambiguous (a Stripe-style plain error
// after the request went out) must not release the claim and resend under a
// new identity.
func TestInvoiceCollection_UntypedErrorAfterSendIsUnknownNotReleased(t *testing.T) {
	e := newCollectionEnv(t, string(models.RailNMI))
	flaky := &fakeCharger{submitErrors: 1}
	runner := collectionRunner(e.db, flaky, flaky)
	n, err := e.svc.ChargeOutstanding(e.ctx, runner, 0)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Equal(t, 1, flaky.chargeCount())
	op := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusUnknownNeedsVerify, op.Status)
	require.Equal(t, []string{"attempted"}, e.attemptStatuses(t), "the attempt is retained, never deleted")

	n, err = e.svc.ChargeOutstanding(e.ctx, runner, 0)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Equal(t, 1, flaky.chargeCount(), "no resend while the outcome is unknown")
	_, err = e.retry(t, runner, "new-key", e.method)
	require.ErrorIs(t, err, money.ErrInvoiceRetryOutcomeUnknown)

	// The charge did land; a restarted verifier finds it and settles once.
	dueNow(t, e.pool, e.ctx, op.ID)
	_, err = collectionRunner(e.db, &fakeCharger{}, flaky).RunVerifyOnce(e.ctx)
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status)
	require.Equal(t, 1, flaky.chargeCount())
	e.requireSettledOnce(t)
}

// crashAfterSubmitCharger settles the charge at the "provider" and then
// breaks the local settlement once (the attempt row is no longer claimable
// under it), the window between a confirmed charge and its record.
type crashAfterSubmitCharger struct {
	fakeCharger
	pool    *pgxpool.Pool
	attempt uuid.UUID
	broke   sync.Once
}

func (c *crashAfterSubmitCharger) Prepare(ctx context.Context, req money.ChargeRequest) (money.PreparedCharge, error) {
	inner, err := c.fakeCharger.Prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	return money.PreparedChargeFunc(func(ctx context.Context) (money.ChargeResult, error) {
		res, err := inner.Submit(ctx)
		c.broke.Do(func() {
			_, _ = c.pool.Exec(ctx, `UPDATE billing.invoice_payments SET status = 'failed' WHERE id = $1`, c.attempt)
		})
		return res, err
	}), nil
}

// TestInvoiceCollection_SettleFailureRetainsReceiptAndConvergesAfterRestart:
// the charge is confirmed but the local settlement fails; the transaction id
// stays on the operation and a restarted verifier settles from it without any
// provider read or resend.
func TestInvoiceCollection_SettleFailureRetainsReceiptAndConvergesAfterRestart(t *testing.T) {
	e := newCollectionEnv(t, string(models.RailNMI))
	// Pre-claim to learn the attempt id the handler will settle.
	parked := &fakeCharger{prepareFailures: 1}
	_, err := e.svc.ChargeOutstanding(e.ctx, collectionRunner(e.db, parked, nil), 0)
	require.NoError(t, err)
	op := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusPending, op.Status)
	attempts, _, err := e.svc.ListInvoicePaymentAttempts(e.ctx, e.payer, e.invoice, 20, 0)
	require.NoError(t, err)
	require.Len(t, attempts, 1)

	crash := &crashAfterSubmitCharger{pool: e.pool, attempt: attempts[0].ID}
	runner := collectionRunner(e.db, crash, crash)
	dueNow(t, e.pool, e.ctx, op.ID)
	_, err = runner.RunExecuteOnce(e.ctx)
	require.NoError(t, err)
	unknown := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusUnknownNeedsVerify, unknown.Status)
	require.Contains(t, string(unknown.ResultEvidence), "tx_"+op.ID.String(), "the receipt survives the failed settlement")
	require.Equal(t, 1, crash.chargeCount())
	require.Zero(t, e.settledPayments(t))
	require.Equal(t, op.ID, *e.invoiceRow(t).CollectionIntentID)

	// Repair the local row, restart, converge from the retained receipt.
	_, err = e.pool.Exec(e.ctx, `UPDATE billing.invoice_payments SET status = 'attempted' WHERE id = $1`, attempts[0].ID)
	require.NoError(t, err)
	reader := &fakeCharger{}
	dueNow(t, e.pool, e.ctx, op.ID)
	_, err = collectionRunner(e.db, reader, reader).RunVerifyOnce(e.ctx)
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status)
	require.Zero(t, reader.chargeCount(), "converging from a retained receipt never resends")
	e.requireSettledOnce(t)
}

// TestInvoiceCollection_CrashBetweenFenceAndSendStaysUnknown: a durable claim
// that may or may not have been sent is conservatively uncertain; reclaiming
// it after the lease lapses verifies instead of sending.
func TestInvoiceCollection_CrashBetweenFenceAndSendStaysUnknown(t *testing.T) {
	e := newCollectionEnv(t, string(models.RailNMI))
	parked := &fakeCharger{prepareFailures: 1}
	_, err := e.svc.ChargeOutstanding(e.ctx, collectionRunner(e.db, parked, nil), 0)
	require.NoError(t, err)
	op := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	// Simulate the executor dying right after the write-ahead fence.
	_, err = e.pool.Exec(e.ctx, `UPDATE billing.rail_intents SET status = 'in_flight', claimed_until = now() - interval '1 minute', attempts = 1, result_evidence = jsonb_build_object('submitted_at', $2::text) WHERE id = $1`, op.ID, time.Now().UTC().Format(time.RFC3339Nano))
	require.NoError(t, err)

	fresh := &fakeCharger{}
	_, err = collectionRunner(e.db, fresh, fresh).RunExecuteOnce(e.ctx)
	require.NoError(t, err)
	require.Zero(t, fresh.chargeCount(), "a resumed operation may only read")
	require.Equal(t, intents.StatusUnknownNeedsVerify, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status)
	require.Equal(t, op.ID, *e.invoiceRow(t).CollectionIntentID)
}

func TestInvoiceCollection_OperatorResolutionFromExactReceipt(t *testing.T) {
	e := newCollectionEnv(t, string(models.RailNMI))
	lost := &fakeCharger{lostResponse: true}
	n, err := e.svc.ChargeOutstanding(e.ctx, collectionRunner(e.db, lost, lost), 0)
	require.NoError(t, err)
	require.Zero(t, n)
	op := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusUnknownNeedsVerify, op.Status)

	hidden := &fakeCharger{} // the search stays empty; only the exact read confirms
	runner := collectionRunner(e.db, hidden, lost)
	_, err = runner.Resolve(e.ctx, op.ID, intents.Resolution{ProviderReference: "tx_unrelated", Actor: "ops@example", Reason: "gateway portal"})
	require.ErrorIs(t, err, intents.ErrResolutionRejected)
	require.Equal(t, intents.StatusUnknownNeedsVerify, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status)

	resolved, err := runner.Resolve(e.ctx, op.ID, intents.Resolution{ProviderReference: "tx_" + op.ID.String(), Actor: "ops@example", Reason: "gateway portal shows the sale"})
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, resolved.Status)
	require.Zero(t, hidden.chargeCount())
	e.requireSettledOnce(t)

	var logged int
	require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT count(*) FROM billing.rail_mutation_logs WHERE rail_intent_id = $1 AND evidence->'operator_resolution'->>'actor' = 'ops@example'`, op.ID).Scan(&logged))
	require.Equal(t, 1, logged, "the resolution is recorded with its actor")
}

func TestInvoiceCollection_EmptyNMISearchCannotReleasePossibleCharge(t *testing.T) {
	e := nmiReceiptScenario(t)
	// An authenticated empty order search still cannot exclude a charge that
	// has not reached the provider's query view. An operator reason is not proof.
	_, err := e.runner.Resolve(e.ctx, e.op, intents.Resolution{NotExecuted: true, Actor: "ops", Reason: "gateway search is empty"})
	require.ErrorIs(t, err, intents.ErrResolutionRejected)
	require.Contains(t, err.Error(), "cannot prove nonexecution")
	operation := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusUnknownNeedsVerify, operation.Status)
	invoice := e.invoiceRow(t)
	require.NotNil(t, invoice.CollectionIntentID)
	require.Equal(t, e.op, *invoice.CollectionIntentID)
	require.Equal(t, []string{"attempted"}, e.attemptStatuses(t))
	count, err := e.svc.ChargeOutstanding(e.ctx, e.runner, 0)
	require.NoError(t, err)
	require.Zero(t, count)
	require.Len(t, e.gateway.sentOrderIDs(), 1, "absence never authorizes a second provider submission")
}

// TestInvoiceCollection_OperatorReleasesNeverSubmittedOperation: an operation
// parked before any submission (its account never armed) holds the invoice
// pointer with no verifier to close it. The absent write-ahead fence proves
// nothing reached the provider, so --not-executed releases it; a receipt does
// not apply, and an operation that crossed the fence is refused.
func TestInvoiceCollection_OperatorReleasesNeverSubmittedOperation(t *testing.T) {
	e := newCollectionEnv(t, string(models.RailNMI))
	unarmed := &fakeCharger{prepareFailures: 100}
	runner := collectionRunner(e.db, unarmed, unarmed)
	_, err := e.svc.ChargeOutstanding(e.ctx, runner, 0)
	require.NoError(t, err)
	op := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusPending, op.Status)
	require.Equal(t, op.ID, *e.invoiceRow(t).CollectionIntentID)

	_, err = runner.Resolve(e.ctx, op.ID, intents.Resolution{ProviderReference: "tx_x", Actor: "ops", Reason: "portal"})
	require.ErrorIs(t, err, intents.ErrResolutionNotUnknown, "a never-submitted operation has no receipt to accept")
	require.Equal(t, intents.StatusPending, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status)

	resolved, err := runner.Resolve(e.ctx, op.ID, intents.Resolution{NotExecuted: true, Actor: "ops", Reason: "account never armed; releasing the invoice"})
	require.NoError(t, err)
	require.Equal(t, intents.StatusFailedTerminal, resolved.Status)
	require.Contains(t, string(resolved.ResultEvidence), `"actor": "ops"`)
	inv := e.invoiceRow(t)
	require.Nil(t, inv.CollectionIntentID)
	require.Equal(t, int32(0), inv.CollectionFailureCount)
	require.NotNil(t, inv.NextCollectionAttemptAt)
	require.Equal(t, []string{"failed"}, e.attemptStatuses(t))
	require.Zero(t, unarmed.chargeCount())
	// The released invoice is the operator's again.
	_, err = e.svc.VoidInvoice(e.ctx, e.payer, e.invoice)
	require.NoError(t, err)
}

func TestInvoiceCollection_OperatorReleaseRefusedPastTheSubmissionFence(t *testing.T) {
	e := newCollectionEnv(t, string(models.RailNMI))
	unarmed := &fakeCharger{prepareFailures: 100}
	runner := collectionRunner(e.db, unarmed, unarmed)
	_, err := e.svc.ChargeOutstanding(e.ctx, runner, 0)
	require.NoError(t, err)
	op := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusPending, op.Status)
	// A fenced pending row must remain protected even when its earlier
	// executor disappeared before recording the final outcome.
	_, err = e.pool.Exec(e.ctx, `UPDATE billing.rail_intents SET result_evidence = jsonb_build_object('submitted_at', $2::text) WHERE id = $1`, op.ID, time.Now().UTC().Format(time.RFC3339Nano))
	require.NoError(t, err)

	_, err = runner.Resolve(e.ctx, op.ID, intents.Resolution{NotExecuted: true, Actor: "ops", Reason: "looks unsent"})
	require.ErrorIs(t, err, intents.ErrResolutionRejected)
	after := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	require.Equal(t, intents.StatusUnknownNeedsVerify, after.Status, "blocked submitted work remains owned by read-only verification")
	require.Nil(t, after.ClaimedUntil)
	require.NotEmpty(t, intents.EvidenceString(after, "submitted_at"))
	require.Greater(t, after.Attempts, op.Attempts)
	require.Equal(t, op.ID, *e.invoiceRow(t).CollectionIntentID)
	require.Equal(t, []string{"attempted"}, e.attemptStatuses(t))
}

func TestInvoiceCollection_LiveOperationRejectsCompetingInvoiceMutations(t *testing.T) {
	e := newCollectionEnv(t, string(models.RailStripe))
	lost := &fakeCharger{lostResponse: true}
	_, err := e.svc.ChargeOutstanding(e.ctx, collectionRunner(e.db, lost, nil), 0)
	require.NoError(t, err)
	require.NotNil(t, e.invoiceRow(t).CollectionIntentID)

	_, err = e.svc.VoidInvoice(e.ctx, e.payer, e.invoice)
	require.Error(t, err)
	_, err = e.svc.MarkInvoiceUncollectible(e.ctx, e.payer, e.invoice)
	require.Error(t, err)
	_, err = e.svc.RecordOutOfBandInvoicePayment(e.ctx, e.payer, e.invoice, 1, "wire-during-live-operation")
	require.ErrorIs(t, err, money.ErrInvoiceRetryInProgress)
	_, err = e.svc.ApplyInvoiceAdminMutation(e.ctx, e.payer, e.invoice, money.InvoiceAdminMutation{Action: money.InvoiceAdminVoid})
	require.ErrorIs(t, err, money.ErrInvoiceActionNotAllowed)
	require.Empty(t, money.InvoiceAdminActions(e.invoiceRow(t)))
}

func TestInvoiceCollection_RejectsAnotherPayersPaymentMethod(t *testing.T) {
	svc, _, payer, currency, ctx := moneyInEnv(t)
	_, otherPool, otherPayer, _, otherCtx := moneyInEnv(t)
	otherMethod := seedPaymentMethod(t, otherPool, otherCtx, otherPayer, string(models.RailStripe))

	err := svc.SetInvoiceCollectionPaymentMethod(ctx, payer, currency, otherMethod)
	require.ErrorIs(t, err, money.ErrCollectionPaymentMethodInvalid)
}

func TestInvoiceCollection_RejectsUnknownRail(t *testing.T) {
	svc, pool, payer, currency, ctx := moneyInEnv(t)
	method := seedPaymentMethod(t, pool, ctx, payer, "unknown")

	err := svc.SetInvoiceCollectionPaymentMethod(ctx, payer, currency, method)
	require.ErrorIs(t, err, money.ErrCollectionPaymentMethodInvalid)
}
