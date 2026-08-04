//go:build integration

package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/delinquency"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// or#878: admission is the ONE enforcement lever OpenRails is authoritative
// over, and it is the TIME axis, not the amount axis. The payer below has a
// fully funded balance — it fails admission purely because a debt outlived the
// merchant's grace window, and it fails with its OWN deny code, because "you are
// over your limit" and "you have an unpaid invoice" ask different things of the
// customer.
func TestAdmitRefusesADelinquentPayerWithItsOwnDenyCode(t *testing.T) {
	svc, _, payer, ctx := wastedSvcEnv(t)
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()

	// grace 0 / floor 0: overdue is delinquent immediately.
	grace, floor := 0, int64(0)
	require.NoError(t, svc.SetMerchantConfiguration(ctx, billingservice.MerchantConfiguration{
		ArrearsGraceDays: &grace, ArrearsDelinquencyFloor: &floor,
	}))

	// A funded payer. If the amount cap were the only gate, this admit passes.
	admitted, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payer, Invoker: "user:" + payer.UUID().String(), InvokerType: "payer",
		Currency: money.DefaultCurrency, EstimatedAmount: 1_000_000,
		SourceID: uuid.NewString(), Source: "usage",
	})
	require.NoError(t, err)
	require.True(t, admitted.Allowed, "a funded payer with no overdue debt is admitted")

	// Now an overdue receivable — the debt the clock runs on.
	invoiceID := seedOverdueInvoice(t, ctx, pool, payer, money.DefaultCurrency, 5_000_000, 30*24*time.Hour)

	// Before the evaluator has recorded a transition, nothing changes:
	// enforcement follows a RECORDED state, never a read at admission time.
	admitted, err = svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payer, Invoker: "user:" + payer.UUID().String(), InvokerType: "payer",
		Currency: money.DefaultCurrency, EstimatedAmount: 1_000_000,
		SourceID: uuid.NewString(), Source: "usage",
	})
	require.NoError(t, err)
	require.True(t, admitted.Allowed, "an unevaluated payer is admitted; the gate does not improvise a cutoff")

	del := delinquency.NewService(dbi, nil)
	res, err := del.Evaluate(ctx, time.Now().UTC())
	require.NoError(t, err)
	require.Len(t, res.Transitions, 1)
	require.Equal(t, delinquency.StateDelinquent, res.Transitions[0].To)

	denied, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payer, Invoker: "user:" + payer.UUID().String(), InvokerType: "payer",
		Currency: money.DefaultCurrency, EstimatedAmount: 1_000_000,
		SourceID: uuid.NewString(), Source: "usage",
	})
	require.NoError(t, err)
	require.False(t, denied.Allowed, "past grace, new spend is refused — that is what stops the bill growing")
	require.Equal(t, admission.DenyDelinquent, denied.DenyCode)
	require.NotEqual(t, money.DenyInsufficientCredit, denied.DenyCode,
		"the two axes must be distinguishable: this payer is funded, it simply has not paid its bill")

	// Paying the bill restores admission on the next evaluation.
	_, err = pool.Exec(ctx, `
		UPDATE openrails.invoices
		   SET amount_paid = total_amount, amount_due = 0, status = 'paid', paid_at = now(), updated_at = now()
		 WHERE id = $1`, invoiceID)
	require.NoError(t, err)
	res, err = del.Evaluate(ctx, time.Now().UTC())
	require.NoError(t, err)
	require.Len(t, res.Transitions, 1)
	require.Equal(t, delinquency.StateCurrent, res.Transitions[0].To)

	admitted, err = svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payer, Invoker: "user:" + payer.UUID().String(), InvokerType: "payer",
		Currency: money.DefaultCurrency, EstimatedAmount: 1_000_000,
		SourceID: uuid.NewString(), Source: "usage",
	})
	require.NoError(t, err)
	require.True(t, admitted.Allowed, "a customer who has settled must be spending again")
}

// seedOverdueInvoice writes one open receivable whose due date has already
// passed — the exact row shape FinalizeInvoice produces for an arrears
// statement, which is what the delinquency derivation reads.
func seedOverdueInvoice(t *testing.T, ctx context.Context, pool *pgxpool.Pool, payer identity.CustomerID, currency string, amount int64, overdueBy time.Duration) uuid.UUID {
	t.Helper()
	id := uuid.New()
	now := time.Now().UTC()
	due := now.Add(-overdueBy)
	_, err := pool.Exec(ctx, `
		INSERT INTO openrails.invoices
			(id, merchant_id, customer_id, currency, period_from, period_to,
			 subtotal_amount, total_amount, amount_paid, amount_due, status, issued_at, due_at, finalized_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7, 0, $7, 'open', $6, $8, $6)`,
		id, dbtest.TestMerchantID.UUID(), payer.UUID(), currency,
		due.Add(-24*time.Hour), due, amount, due)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.host_lifecycle_events WHERE subject_id = $1", payer.UUID())
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.customer_delinquency WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.notification_queue WHERE customer_id = $1", payer.UUID())
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.invoices WHERE id = $1", id)
	})
	return id
}

// or#878 sequencing note: the amount cap had never demonstrably bitten in
// production — GetOutstandingOwed was an unpinned read returning zero exposure
// (fail-open) until or#824 scoped it, so `credit_limit_amount - exposure` could
// not refuse anything. Time-based delinquency must not ship on top of a control
// nobody has seen work, so this pins the OTHER axis through the same production
// entry point: an arrears payer whose overdue exposure has eaten its credit line
// is refused with `insufficient_credit`, under the RLS-enforcing harness.
func TestAdmitRefusesWhenOverdueExposureEatsTheCreditLine(t *testing.T) {
	// or#878 follow-up: two DELIBERATE exposure models disagree — AuthorizeAndHold
	// subtracts invoice-derived exposure; GetAdmissionCapacity documents that under
	// Phase-H used credit is already a negative customer_balance and deliberately
	// does not. Which view is authoritative is an owner ruling (money risk both
	// ways: double-count refuses legitimate spend, status quo admits unpaid-invoice
	// payers). Pinned aspiration, not behavior — skipped until the ruling lands.
	t.Skip("or#878: awaiting owner ruling on the authoritative exposure model (see PR #224 body)")
	svc, ms, _, ctx := wastedSvcEnv(t)
	dbi := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	pool := dbi.Pool()

	// A SECOND payer: unfunded, arrears, with a small credit line.
	payer := identity.CustomerIDFromString(uuid.NewString())
	dbtest.EnsureCustomerIDPgx(ctx, t, pool, payer.UUID().String())
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM openrails.money_settings WHERE customer_id = $1", payer.UUID())
	})
	arrears := money.BillingModeArrears
	_, err := ms.UpsertAccountSettings(ctx, payer, money.DefaultCurrency, money.AccountSettingsInput{BillingMode: &arrears})
	require.NoError(t, err)
	require.NoError(t, ms.SetCreditLimit(ctx, payer, money.DefaultCurrency, 2_000_000))

	// Inside the line: admitted.
	admitted, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payer, Invoker: "user:" + payer.UUID().String(), InvokerType: "payer",
		Currency: money.DefaultCurrency, EstimatedAmount: 1_000_000,
		SourceID: uuid.NewString(), Source: "usage",
	})
	require.NoError(t, err)
	require.True(t, admitted.Allowed)

	// An open receivable larger than the whole credit line. Not yet due, so the
	// TIME axis has nothing to say — only the amount does.
	id := uuid.New()
	now := time.Now().UTC()
	future := now.Add(30 * 24 * time.Hour)
	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.invoices
			(id, merchant_id, customer_id, currency, period_from, period_to,
			 subtotal_amount, total_amount, amount_paid, amount_due, status, issued_at, due_at, finalized_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7, 0, $7, 'open', $6, $8, $6)`,
		id, dbtest.TestMerchantID.UUID(), payer.UUID(), money.DefaultCurrency,
		now.Add(-24*time.Hour), now, int64(5_000_000), future)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DELETE FROM openrails.invoices WHERE id = $1", id) })

	owed, err := ms.GetOutstandingOwed(ctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	require.EqualValues(t, 5_000_000, owed, "the exposure read must see the receivable; a zero here is the fail-open bug")

	denied, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payer, Invoker: "user:" + payer.UUID().String(), InvokerType: "payer",
		Currency: money.DefaultCurrency, EstimatedAmount: 1_000_000,
		SourceID: uuid.NewString(), Source: "usage",
	})
	require.NoError(t, err)
	require.False(t, denied.Allowed, "the amount cap must actually refuse")
	require.Equal(t, money.DenyInsufficientCredit, denied.DenyCode,
		"an over-the-line payer is `insufficient_credit`, never the delinquency code — the debt is not even due yet")
}
