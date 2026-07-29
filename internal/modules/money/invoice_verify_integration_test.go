//go:build integration

package money_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
)

// #828 invoice-consumer verifier suite: the collection_outcome_unknown park
// is resolvable — settled charges apply, unexecuted ones release, residue has
// an admin unpark — and the schedule/terminal semantics ride the ONE
// collection core.

type fakeInvoiceVerifier struct {
	supported bool
	settled   bool
	txnID     string
	err       error
	orderRefs []string
}

func (f *fakeInvoiceVerifier) VerifyCollectionCharge(_ context.Context, _ gen.OpenrailsPaymentMethod, wireOrderRef string) (money.CollectionVerifyResult, error) {
	f.orderRefs = append(f.orderRefs, wireOrderRef)
	if f.err != nil {
		return money.CollectionVerifyResult{}, f.err
	}
	return money.CollectionVerifyResult{Supported: f.supported, Settled: f.settled, TransactionID: f.txnID}, nil
}

// softDeclineCharger declines every charge with a retryable failure code so
// the schedule (not the needs-payment-method pause) drives.
type softDeclineCharger struct{ charges []money.ChargeRequest }

func (c *softDeclineCharger) ChargeSavedMethod(_ context.Context, req money.ChargeRequest) (money.ChargeResult, error) {
	c.charges = append(c.charges, req)
	code := "202" // NMI insufficient funds — normalizes retryable
	msg := "insufficient funds"
	return money.ChargeResult{Rail: "nmi", Declined: true, FailureCode: &code, FailureMessage: &msg}, nil
}

func seedUnknownParkedInvoice(t *testing.T, svc *money.MoneyService, pool *pgxpool.Pool, payer identity.CustomerID, currency string, ctx context.Context, rail string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	method := seedPaymentMethod(t, pool, ctx, payer, rail)
	_, err := svc.UpsertAccountSettings(ctx, payer, currency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &method,
	})
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, currency, "usage", "verify-"+uuid.NewString()[:8], 5_000_000)
	require.NoError(t, err)
	invoice, err := svc.FinalizeInvoice(ctx, payer, currency, time.Now().Add(-time.Hour), time.Now())
	require.NoError(t, err)

	ambiguous := &fakeCharger{ambiguous: true}
	_, err = svc.ChargeOutstanding(ctx, ambiguous, 0)
	require.Error(t, err)
	require.ErrorIs(t, err, money.ErrInvoiceRetryOutcomeUnknown)
	require.Len(t, ambiguous.charges, 1)

	parked, err := svc.GetInvoiceByID(ctx, payer, invoice.ID)
	require.NoError(t, err)
	require.Equal(t, "collection_outcome_unknown", *parked.LastCollectionFailureCode)

	// The resolver's min-age gate lets the provider settle before the read;
	// age the park so this pass may resolve it.
	_, err = pool.Exec(ctx, "UPDATE openrails.invoices SET updated_at = updated_at - interval '30 minutes' WHERE id = $1", invoice.ID)
	require.NoError(t, err)
	return invoice.ID, method
}

func cleanupInvoices(t *testing.T, pool *pgxpool.Pool, ctx context.Context, payer identity.CustomerID) {
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_payments WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoice_items WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.invoices WHERE customer_id = $1", payer.UUID())
	})
}

func owedPaymentTransfers(t *testing.T, pool *pgxpool.Pool, ctx context.Context, payer identity.CustomerID) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT count(*) FROM openrails.ledger_transfers WHERE customer_id = $1 AND transfer_type = 'owed_payment'",
		payer.UUID()).Scan(&n))
	return n
}

// TestInvoiceVerify_ConfirmedChargeSettlesWithoutDoubleCharge: an ambiguous
// transport outcome parks unknown; the verifier's provider read confirms the
// charge landed; the payment applies EXACTLY ONCE and no second provider
// charge is ever sent — invoice-consumer crash-window convergence (#828).
func TestInvoiceVerify_ConfirmedChargeSettlesWithoutDoubleCharge(t *testing.T) {
	svc, pool, payer, currency, ctx := moneyInEnv(t)
	cleanupInvoices(t, pool, ctx, payer)
	invoiceID, _ := seedUnknownParkedInvoice(t, svc, pool, payer, currency, ctx, string(models.RailNMI))

	verifier := &fakeInvoiceVerifier{supported: true, settled: true, txnID: "txn-confirmed-1"}
	stats, err := svc.ResolveUnknownInvoiceCollections(ctx, verifier)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Examined)
	require.Equal(t, 1, stats.Settled)
	require.Len(t, verifier.orderRefs, 1)
	require.Contains(t, verifier.orderRefs[0], "inv:", "wire order ref uses the compacted NMI budget")

	paid, err := svc.GetInvoiceByID(ctx, payer, invoiceID)
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)
	require.Nil(t, paid.LastCollectionFailureCode)
	require.Equal(t, 1, owedPaymentTransfers(t, pool, ctx, payer), "exactly one ledger settlement")

	attempts, _, err := svc.ListInvoicePaymentAttempts(ctx, payer, invoiceID, 20, 0)
	require.NoError(t, err)
	require.Equal(t, "settled", attempts[0].Status)
	require.Equal(t, "txn-confirmed-1", *attempts[0].RailPaymentID)

	// Idempotent: a second pass finds nothing; a second sweep charges nothing.
	stats, err = svc.ResolveUnknownInvoiceCollections(ctx, verifier)
	require.NoError(t, err)
	require.Zero(t, stats.Examined)
	after := &fakeCharger{}
	n, err := svc.ChargeOutstanding(ctx, after, 0)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Empty(t, after.charges)
	require.Equal(t, 1, owedPaymentTransfers(t, pool, ctx, payer))
}

// TestInvoiceVerify_NotExecutedReleasesAndCollectsOnce: the provider read
// finds no successful sale — no money moved — so the claim releases and the
// schedule resumes; the next sweep collects exactly once.
func TestInvoiceVerify_NotExecutedReleasesAndCollectsOnce(t *testing.T) {
	svc, pool, payer, currency, ctx := moneyInEnv(t)
	cleanupInvoices(t, pool, ctx, payer)
	invoiceID, _ := seedUnknownParkedInvoice(t, svc, pool, payer, currency, ctx, string(models.RailNMI))

	verifier := &fakeInvoiceVerifier{supported: true, settled: false}
	stats, err := svc.ResolveUnknownInvoiceCollections(ctx, verifier)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Released)

	released, err := svc.GetInvoiceByID(ctx, payer, invoiceID)
	require.NoError(t, err)
	// or#828: no charge ever landed and nothing declined, so the invoice is
	// exactly where the clock left it. Claiming and parking an attempt never
	// age an invoice — only a terminal outcome and MarkInvoicesPastDue do.
	require.Equal(t, "open", released.Status)
	require.Nil(t, released.LastCollectionFailureCode)
	attempts, total, err := svc.ListInvoicePaymentAttempts(ctx, payer, invoiceID, 20, 0)
	require.NoError(t, err)
	require.Zero(t, total, "the unexecuted claimed attempt is released")
	require.Empty(t, attempts)

	charger := &fakeCharger{}
	n, err := svc.ChargeOutstanding(ctx, charger, 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Len(t, charger.charges, 1)
	paid, err := svc.GetInvoiceByID(ctx, payer, invoiceID)
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)
	require.Equal(t, 1, owedPaymentTransfers(t, pool, ctx, payer))
}

// TestInvoiceVerify_CrashMidClaimConverges: a worker crash AFTER dispatching
// the charge leaves a live claim + attempted row and no recorded outcome. The
// stale-claim sweep takes the claim over into the unknown park (no longer a
// dead end) and the verifier converges it — without ever re-charging.
func TestInvoiceVerify_CrashMidClaimConverges(t *testing.T) {
	svc, pool, payer, currency, ctx := moneyInEnv(t)
	cleanupInvoices(t, pool, ctx, payer)
	method := seedPaymentMethod(t, pool, ctx, payer, string(models.RailNMI))
	_, err := svc.UpsertAccountSettings(ctx, payer, currency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &method,
	})
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, currency, "usage", "crash-claim", 5_000_000)
	require.NoError(t, err)
	invoice, err := svc.FinalizeInvoice(ctx, payer, currency, time.Now().Add(-time.Hour), time.Now())
	require.NoError(t, err)

	// Crash forensics, verbatim: claim marker on the invoice + the claimed
	// attempt row, dispatched 20 minutes ago, outcome never recorded.
	attemptID := uuid.New()
	key := "invoice:" + invoice.ID.String() + ":attempt:0"
	_, err = pool.Exec(ctx, `UPDATE openrails.invoices
		SET status = 'past_due', last_collection_failure_code = 'collection_attempt_in_progress',
		    next_collection_attempt_at = NULL, updated_at = now() - interval '20 minutes'
		WHERE id = $1`, invoice.ID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO openrails.invoice_payments
		(id, merchant_id, customer_id, invoice_id, currency, amount, status, attempted_at, created_at, updated_at, payment_method_id, idempotency_key)
		VALUES ($1, $2, $3, $4, $5, $6, 'attempted', now() - interval '20 minutes', now() - interval '20 minutes', now() - interval '20 minutes', $7, $8)`,
		attemptID, invoice.MerchantID, payer.UUID(), invoice.ID, invoice.Currency, invoice.AmountDue, method, key)
	require.NoError(t, err)

	// The next sweep's stale-claim takeover parks it unknown instead of
	// leaving it frozen; no new charge is dispatched around the live claim.
	sweep := &fakeCharger{}
	n, err := svc.ChargeOutstanding(ctx, sweep, 0)
	require.NoError(t, err)
	require.Zero(t, n)
	require.Empty(t, sweep.charges)
	parked, err := svc.GetInvoiceByID(ctx, payer, invoice.ID)
	require.NoError(t, err)
	require.Equal(t, "collection_outcome_unknown", *parked.LastCollectionFailureCode)

	_, err = pool.Exec(ctx, "UPDATE openrails.invoices SET updated_at = updated_at - interval '30 minutes' WHERE id = $1", invoice.ID)
	require.NoError(t, err)

	// The crashed charge DID land at the provider: the verifier converges it.
	verifier := &fakeInvoiceVerifier{supported: true, settled: true, txnID: "txn-crash-recovered"}
	stats, err := svc.ResolveUnknownInvoiceCollections(ctx, verifier)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Settled)
	require.Equal(t, []string{"inv:" + invoice.ID.String() + ":a0"}, verifier.orderRefs,
		"provider read keys off the recovered attempt order reference")

	paid, err := svc.GetInvoiceByID(ctx, payer, invoice.ID)
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)
	require.Equal(t, 1, owedPaymentTransfers(t, pool, ctx, payer))
	again := &fakeCharger{}
	_, err = svc.ChargeOutstanding(ctx, again, 0)
	require.NoError(t, err)
	require.Empty(t, again.charges, "converged invoice is never re-charged")
}

// TestInvoiceVerify_UnsupportedRailParksUntilAdminUnpark: rails without a
// provider read stay parked (never auto-released blind); the admin unpark
// surface releases residue by operator judgment.
func TestInvoiceVerify_UnsupportedRailParksUntilAdminUnpark(t *testing.T) {
	svc, pool, payer, currency, ctx := moneyInEnv(t)
	cleanupInvoices(t, pool, ctx, payer)
	invoiceID, _ := seedUnknownParkedInvoice(t, svc, pool, payer, currency, ctx, string(models.RailStripe))

	verifier := &fakeInvoiceVerifier{supported: false}
	stats, err := svc.ResolveUnknownInvoiceCollections(ctx, verifier)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Skipped)
	still, err := svc.GetInvoiceByID(ctx, payer, invoiceID)
	require.NoError(t, err)
	require.Equal(t, "collection_outcome_unknown", *still.LastCollectionFailureCode)

	// Transient verifier errors also leave the park intact.
	verifier = &fakeInvoiceVerifier{err: errors.New("gateway read down")}
	stats, err = svc.ResolveUnknownInvoiceCollections(ctx, verifier)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Skipped)

	require.Error(t, svc.UnparkInvoiceCollection(ctx, payer, uuid.New()), "unknown invoice id")
	require.NoError(t, svc.UnparkInvoiceCollection(ctx, payer, invoiceID))
	require.ErrorIs(t, svc.UnparkInvoiceCollection(ctx, payer, invoiceID), money.ErrInvoiceNotParkedUnknown)

	charger := &fakeCharger{}
	n, err := svc.ChargeOutstanding(ctx, charger, 0)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	paid, err := svc.GetInvoiceByID(ctx, payer, invoiceID)
	require.NoError(t, err)
	require.Equal(t, "paid", paid.Status)
}

// TestInvoiceVerify_AmbiguityDoesNotAbortBatch: one ambiguous outcome no
// longer takes the merchant's whole ChargeOutstanding run down with it — the
// other invoice in the batch still collects (#828 review fix).
func TestInvoiceVerify_AmbiguityDoesNotAbortBatch(t *testing.T) {
	svc, pool, payerA, currency, ctx := moneyInEnv(t)
	cleanupInvoices(t, pool, ctx, payerA)
	payerB := identity.CustomerIDFromString(uuid.NewString())
	cleanupInvoices(t, pool, ctx, payerB)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.money_settings WHERE customer_id = $1", payerB.UUID())
	})

	for _, payer := range []identity.CustomerID{payerA, payerB} {
		method := seedPaymentMethod(t, pool, ctx, payer, string(models.RailNMI))
		_, err := svc.UpsertAccountSettings(ctx, payer, currency, money.AccountSettingsInput{
			BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &method,
		})
		require.NoError(t, err)
		_, err = svc.AccrueOwed(ctx, payer, currency, "usage", "batch-isolation", 5_000_000)
		require.NoError(t, err)
		_, err = svc.FinalizeInvoice(ctx, payer, currency, time.Now().Add(-time.Hour), time.Now())
		require.NoError(t, err)
	}

	charger := &ambiguousOnceCharger{}
	n, err := svc.ChargeOutstanding(ctx, charger, 0)
	require.Error(t, err, "the ambiguous outcome still surfaces")
	require.ErrorIs(t, err, money.ErrInvoiceRetryOutcomeUnknown)
	require.Equal(t, 1, n, "the OTHER invoice in the batch still collected")
	require.Len(t, charger.charges, 2)
}

type ambiguousOnceCharger struct {
	charges []money.ChargeRequest
	fired   bool
}

func (c *ambiguousOnceCharger) ChargeSavedMethod(_ context.Context, req money.ChargeRequest) (money.ChargeResult, error) {
	c.charges = append(c.charges, req)
	if !c.fired {
		c.fired = true
		return money.ChargeResult{}, &nmi.TransportAmbiguousError{Err: errors.New("timeout after send")}
	}
	return money.ChargeResult{TransactionID: "tx_" + req.IdempotencyKey}, nil
}

// TestInvoiceVerify_ScheduleParityAndTerminal: the invoice schedule persisted
// through the DB path IS the subscription monthly table — same offsets from
// the ONE collection core — and the 5th failure is terminal with the
// INVOICE consumer's policy (uncollectible; the subscription consumer's
// terminal policy — cancel + entitlement revoke — is pinned by
// subscription_lifecycle_simulation_integration_test.go).
func TestInvoiceVerify_ScheduleParityAndTerminal(t *testing.T) {
	_, dbi, pool, payer, currency, ctx := moneyInEnvWithDB(t)
	cleanupInvoices(t, pool, ctx, payer)
	clock := clockwork.NewFakeClockAt(time.Now().UTC().Truncate(time.Second))
	svc := money.NewMoneyService(dbi, clock)

	method := seedPaymentMethod(t, pool, ctx, payer, string(models.RailNMI))
	_, err := svc.UpsertAccountSettings(ctx, payer, currency, money.AccountSettingsInput{
		BillingMode: strptr(money.BillingModeArrears), AutoTopupPaymentMethod: &method,
	})
	require.NoError(t, err)
	_, err = svc.AccrueOwed(ctx, payer, currency, "usage", "parity-terminal", 5_000_000)
	require.NoError(t, err)
	// The accrual lands AT clock-now; the finalize window is [from, to) so the
	// frozen clock must advance past it.
	clock.Advance(time.Minute)
	// A MONTHLY statement period — parity is "same cycle ⇒ same offsets", and
	// since or#828 the invoice consumer reads its cycle off this window rather
	// than assuming a month for every invoice ever issued.
	invoice, err := svc.FinalizeInvoice(ctx, payer, currency, clock.Now().AddDate(0, -1, 0), clock.Now())
	require.NoError(t, err)

	offsets := collection.RetryOffsets(collection.MonthlyCycleHours)
	require.Equal(t, collection.RetryOffsets(30*24), offsets,
		"invoice arrears cadence and a 30d subscription cycle share ONE schedule")

	charger := &softDeclineCharger{}
	var firstFailure time.Time
	for attempt := 0; attempt < collection.MaxFailures(collection.MonthlyCycleHours); attempt++ {
		n, err := svc.ChargeOutstanding(ctx, charger, 0)
		require.NoError(t, err)
		require.Zero(t, n)
		row, err := svc.GetInvoiceByID(ctx, payer, invoice.ID)
		require.NoError(t, err)
		if attempt == 0 {
			require.NotNil(t, row.CollectionFailedAt)
			firstFailure = *row.CollectionFailedAt
		}
		if attempt < len(offsets) {
			require.Equal(t, "past_due", row.Status)
			require.NotNil(t, row.NextCollectionAttemptAt)
			require.True(t, row.NextCollectionAttemptAt.Equal(firstFailure.Add(offsets[attempt])),
				"failure %d: next attempt must sit at first-failure + subscription offset %s", attempt+1, offsets[attempt])
			clock.Advance(row.NextCollectionAttemptAt.Sub(clock.Now()) + time.Minute)
		} else {
			require.Equal(t, "uncollectible", row.Status, "invoice terminal policy is uncollectible")
			require.NotNil(t, row.UncollectibleAt)
			require.Nil(t, row.NextCollectionAttemptAt)
		}
	}
	require.Len(t, charger.charges, collection.MaxFailures(collection.MonthlyCycleHours))

	// Terminal means terminal for the SCHEDULE only: no further scheduled
	// charges fire.
	n, err := svc.ChargeOutstanding(ctx, &fakeCharger{}, 0)
	require.NoError(t, err)
	require.Zero(t, n)
}
