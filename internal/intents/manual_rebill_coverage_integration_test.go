//go:build integration

package intents

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/collection"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
)

func TestLateRebillCompletionDoesNotPayANewerMissedPeriod(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(fmt.Sprintf("corrupt_payment_%t", corrupt), func(t *testing.T) {
			clock := clockwork.NewFakeClockAt(time.Now().UTC().Add(-90 * 24 * time.Hour).Truncate(time.Second))
			fx := seedPastDueSubscriptionAt(t, uuid.New(), clock.Now())
			gateway, client := newFakeNMIRebillGateway(t, fx)
			ctx := fx.handlerCtx()
			h := NewManualRebillHandler(fx.db, fullModeConfig(), fakeNMIResolver{client: client}, clock)
			runner := &Runner{Store: fx.store, Config: fullModeConfig(), Registry: NewRegistry(h), Clock: clock}
			accepted, err := h.EnqueueScheduled(ctx, fx.subID)
			require.NoError(t, err)
			admin := dbtest.SharedSuperuserPGXPool(t)
			trigger := "late_rebill_" + uuid.NewString()[:8]
			_, err = admin.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION openrails.%s() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE EXCEPTION 'injected terminal failure'; END$$; CREATE TRIGGER %s BEFORE UPDATE ON openrails.rail_intents FOR EACH ROW WHEN (NEW.id='%s'::uuid AND NEW.status='succeeded') EXECUTE FUNCTION openrails.%s()`, trigger, trigger, accepted.ID, trigger))
			require.NoError(t, err)
			t.Cleanup(func() {
				_, _ = admin.Exec(context.WithoutCancel(ctx), "DROP FUNCTION IF EXISTS openrails."+trigger+"() CASCADE")
			})
			row, err := runner.ExecuteByID(ctx, accepted.ID)
			require.NoError(t, err)
			require.Equal(t, StatusUnknownNeedsVerify, row.Status)
			_, found, err := LoadCollectedReceipt(row)
			require.NoError(t, err)
			require.True(t, found)
			_, err = admin.Exec(ctx, "DROP FUNCTION openrails."+trigger+"() CASCADE")
			require.NoError(t, err)
			p, err := DecodeManualRebillPayload(row)
			require.NoError(t, err)
			laterStart, laterEnd := p.Renewal.PeriodEnd, p.Renewal.PeriodEnd.Add(30*24*time.Hour)
			clock.Advance(laterStart.Add(time.Minute).Sub(clock.Now()))
			require.NoError(t, h.lifecycle(fx.db).RenewMembership(ctx, &subscriptions.RenewMembershipParams{Rail: "nmi", RailSubscriptionID: p.RailSubscriptionID, TransactionID: "later-" + uuid.NewString(), Amount: p.Renewal.Amount, AmountProvided: true, Currency: p.Renewal.Currency, CurrentPeriodStartsAt: &laterStart, CurrentPeriodEndsAt: &laterEnd}))
			clock.Advance(laterEnd.Add(time.Minute).Sub(clock.Now()))
			require.Equal(t, OutcomeSucceeded, h.Verify(ctx, row).Class)
			require.True(t, fx.subscription(t).CurrentPeriodEndsAt.Equal(laterEnd))
			require.Equal(t, 1, fx.paymentsFor(t, gateway.txnID))
			// The old timestamp heuristic misidentifies this late local write as a
			// payment of the now-missed later period. No fabricated provider time fixes it.
			legacy, err := fx.db.Gen(ctx).HasCompletedPaymentAtOrAfterPeriodEnd(ctx, gen.HasCompletedPaymentAtOrAfterPeriodEndParams{MerchantID: fx.merchantID, SubscriptionID: fx.subID, PeriodEnd: laterEnd})
			require.NoError(t, err)
			require.True(t, legacy)
			reason := "declined"
			require.NoError(t, h.lifecycle(fx.db).FailMembership(ctx, &subscriptions.FailMembershipParams{Rail: "nmi", SubscriptionID: &fx.subID, FailureReason: &reason, AttemptRecorded: true}))
			clock.Advance(collection.NextRetryIn(720, 1) + time.Second)
			next, err := h.EnqueueScheduled(ctx, fx.subID)
			require.NoError(t, err)
			require.NotEqual(t, row.ID, next.ID)
			nextTerms, err := DecodeManualRebillPayload(next)
			require.NoError(t, err)
			require.True(t, nextTerms.Renewal.PeriodStart.Equal(laterEnd))
			nextFixture := fx
			nextFixture.payload = nextTerms
			nextGateway, nextClient := newFakeNMIRebillGateway(t, nextFixture)
			nextHandler := NewManualRebillHandler(fx.db, fullModeConfig(), fakeNMIResolver{client: nextClient}, clock)
			nextRunner := &Runner{Store: fx.store, Config: fullModeConfig(), Registry: NewRegistry(nextHandler), Clock: clock}
			if corrupt {
				// A local payment contradicting its qualified historical receipt must
				// refuse before the next provider write, even though its interval is old.
				_, err = fx.db.Pool().Exec(ctx, `UPDATE openrails.payments SET amount=amount+1 WHERE subscription_id=$1 AND transaction_id=$2`, fx.subID, gateway.txnID)
				require.NoError(t, err)
				blocked, err := nextRunner.ExecuteByID(ctx, next.ID)
				require.NoError(t, err)
				require.Equal(t, StatusPending, blocked.Status)
				require.Zero(t, nextGateway.saleCalls.Load())
				require.Contains(t, *blocked.LastFailureReason, "contradicts its accepted receipt terms")
				_, err = fx.db.Pool().Exec(ctx, `UPDATE openrails.payments SET amount=amount-1 WHERE subscription_id=$1 AND transaction_id=$2`, fx.subID, gateway.txnID)
				require.NoError(t, err)
			}
			completed, err := nextRunner.ExecuteByID(ctx, next.ID)
			require.NoError(t, err)
			require.Equal(t, StatusSucceeded, completed.Status)
			require.EqualValues(t, 1, nextGateway.saleCalls.Load())
			require.EqualValues(t, 1, gateway.saleCalls.Load())
			require.Equal(t, 1, fx.paymentsFor(t, nextGateway.txnID))
		})
	}
}

func TestQualifiedRebillCoverageUsesHalfOpenIntervalsAndExactPayment(t *testing.T) {
	fx := seedPastDueSubscription(t)
	_, client := newFakeNMIRebillGateway(t, fx)
	ctx := fx.handlerCtx()
	h := NewManualRebillHandler(fx.db, fullModeConfig(), fakeNMIResolver{client: client}, nil)
	accepted, err := h.EnqueueScheduled(ctx, fx.subID)
	require.NoError(t, err)
	done, err := fx.rebillRunner(client, fullModeConfig()).ExecuteByID(ctx, accepted.ID)
	require.NoError(t, err)
	require.Equal(t, StatusSucceeded, done.Status)
	rows, err := fx.db.Gen(ctx).ListCompletedManualRebillPaymentCoverage(ctx, gen.ListCompletedManualRebillPaymentCoverageParams{MerchantID: fx.merchantID, SubscriptionID: fx.subID, PspID: fx.pspID})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	p, err := DecodeManualRebillPayload(done)
	require.NoError(t, err)
	for _, tc := range []struct {
		name       string
		start, end time.Time
		covered    bool
	}{
		{"equal", p.Renewal.PeriodStart, p.Renewal.PeriodEnd, true},
		{"left edge", p.Renewal.PeriodStart.Add(-time.Hour), p.Renewal.PeriodStart, false},
		{"right edge", p.Renewal.PeriodEnd, p.Renewal.PeriodEnd.Add(time.Hour), false},
		{"overlap", p.Renewal.PeriodEnd.Add(-time.Hour), p.Renewal.PeriodEnd.Add(time.Hour), true},
		{"future disjoint", p.Renewal.PeriodEnd.Add(time.Hour), p.Renewal.PeriodEnd.Add(2 * time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := p
			target.Renewal.PeriodStart, target.Renewal.PeriodEnd = tc.start, tc.end
			covered, err := acceptedRebillPaymentOverlaps(rows[0], target)
			require.NoError(t, err)
			require.Equal(t, tc.covered, covered)
		})
	}
	for name, mutate := range map[string]func(*gen.ListCompletedManualRebillPaymentCoverageRow){
		"amount":       func(r *gen.ListCompletedManualRebillPaymentCoverageRow) { r.PaidAmount++ },
		"currency":     func(r *gen.ListCompletedManualRebillPaymentCoverageRow) { r.PaidCurrency = "EUR" },
		"customer":     func(r *gen.ListCompletedManualRebillPaymentCoverageRow) { r.PaidCustomerID = uuid.New() },
		"price":        func(r *gen.ListCompletedManualRebillPaymentCoverageRow) { r.PaidPriceID = uuid.New() },
		"account":      func(r *gen.ListCompletedManualRebillPaymentCoverageRow) { id := uuid.New(); r.PaidPspID = &id },
		"subscription": func(r *gen.ListCompletedManualRebillPaymentCoverageRow) { id := uuid.New(); r.PaidSubscriptionID = &id },
		"transaction":  func(r *gen.ListCompletedManualRebillPaymentCoverageRow) { r.PaidTransactionID = "other" },
		"receipt": func(r *gen.ListCompletedManualRebillPaymentCoverageRow) {
			r.OpenrailsRailIntent.ResultEvidence = []byte(`{"qualified_receipt":null}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			row := rows[0]
			mutate(&row)
			_, err := acceptedRebillPaymentOverlaps(row, p)
			require.Error(t, err)
		})
	}
}
