//go:build integration

package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// or#903 — the wasted-spend report's once-only claim is the durable
// usage_events row, not a Redis SetNX.
//
// The defect these tests close: ClaimReport ran BEFORE any ledger call and its
// verdict was returned as the answer. It was a cache — it expired, and it did
// not survive a flush — so a replay after a flush was re-graded, re-consumed
// the payer's grace, and the engine's own changed-amount refusal could never
// fire inside the TTL. Hosts compensated with claim tables of their own
// (tensorhub th#1464's outbox fingerprint); this is what retires them.
//
// FLUSHING IS THE POINT. Every proof below flushes Redis between the two
// reports, because that is exactly the difference between "deduped" and
// "deduped for a while".

func or903WasteInput(payer identity.CustomerID, invokerType, requestID string, amount int64) billingservice.WastedSpendInput {
	return billingservice.WastedSpendInput{
		CustomerID:  payer,
		Invoker:     payer.UUID().String(),
		InvokerType: invokerType,
		Currency:    money.DefaultCurrency,
		Amount:      amount,
		Source:      "invoke",
		SourceID:    requestID,
		Reason:      "worker_failed",
	}
}

// or903WasteEventCount is the durable row count for this payer — the claim
// itself, read from the table rather than inferred from the verdict that is
// under test. Each test uses a freshly minted payer, so this counts only its
// own reports.
func or903WasteEventCount(t *testing.T, ctx context.Context, ms *money.MoneyService, payer identity.CustomerID) int64 {
	t.Helper()
	rows, err := ms.AggregateUsage(ctx, payer, money.DefaultCurrency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	for _, r := range rows {
		if r.EventType == "wasted_spend" {
			return r.EventCount
		}
	}
	return 0
}

// A FORGIVEN report — the common case, and the one that never touched Postgres
// before — is claimed durably. Flush Redis, replay it, and the engine still
// says duplicate.
func TestOr903_ForgivenWasteReportIsClaimedAcrossARedisFlush(t *testing.T) {
	svc, ms, payer, ctx, rdb := wastedSvcEnvWithRedis(t)
	requestID := uuid.NewString()
	// Grace far above the report, so nothing is chargeable and the whole
	// question is about ACCOUNTING, not about money.
	requireBoundBadSpendPolicy(t, svc, ctx, "free", 10_000_000)

	first, err := svc.ReportWastedSpend(ctx, or903WasteInput(payer, "payer", requestID, 1_000_000))
	require.NoError(t, err)
	require.Equal(t, "forgiven", first.Action)
	require.False(t, first.Duplicate)
	require.Equal(t, int64(1_000_000), first.ForgivenAmount)
	require.Equal(t, int64(1), or903WasteEventCount(t, ctx, ms, payer))

	// Everything the old claim lived in is gone.
	require.NoError(t, rdb.FlushAll(ctx).Err())

	second, err := svc.ReportWastedSpend(ctx, or903WasteInput(payer, "payer", requestID, 1_000_000))
	require.NoError(t, err)
	require.True(t, second.Duplicate,
		"a replayed waste report must be answered duplicate AFTER a Redis flush — before or#903 the claim was a SetNX and this replay was accepted as new")
	require.Equal(t, "duplicate", second.Action)
	require.Equal(t, int64(1), or903WasteEventCount(t, ctx, ms, payer),
		"the replay must not write a second durable row")
}

// The same report, replayed after a flush, must not consume the payer's grace
// twice. This is the harm the SetNX could not prevent: grace is a budget, and
// re-consuming it charges a payer sooner than their policy says.
func TestOr903_AReplayAfterAFlushDoesNotReconsumeGrace(t *testing.T) {
	svc, ms, payer, ctx, rdb := wastedSvcEnvWithRedis(t)
	// Grace sized so ONE report fits and one-and-a-half do not: 1_500_000 with
	// two 1_000_000 reports. Post-flush the counter is empty, so the new report
	// is inside grace — unless the replay put 1_000_000 back into it, which
	// leaves only 500_000 free and charges the difference.
	requireBoundBadSpendPolicy(t, svc, ctx, "free", 1_500_000)

	reqA := uuid.NewString()
	first, err := svc.ReportWastedSpend(ctx, or903WasteInput(payer, "payer", reqA, 1_000_000))
	require.NoError(t, err)
	require.Equal(t, int64(0), first.ChargedAmount)

	require.NoError(t, rdb.FlushAll(ctx).Err())

	// The replay: refused as a duplicate, so it adds nothing to the window.
	replay, err := svc.ReportWastedSpend(ctx, or903WasteInput(payer, "payer", reqA, 1_000_000))
	require.NoError(t, err)
	require.True(t, replay.Duplicate)

	// A genuinely NEW report of the same size. The flush wiped the window, so the
	// only spend the counter should have seen since is this one — inside grace,
	// rather than pushed over it by a replay that had already been counted.
	balanceBefore := or894Balance(t, ms, ctx, payer)
	reqB := uuid.NewString()
	other, err := svc.ReportWastedSpend(ctx, or903WasteInput(payer, "payer", reqB, 1_000_000))
	require.NoError(t, err)
	require.Equal(t, int64(0), other.ChargedAmount,
		"the replay consumed grace it had already consumed — a distinct report is now being charged for it")
	require.Equal(t, balanceBefore, or894Balance(t, ms, ctx, payer))
}

// A replay carrying a CHANGED chargeable amount is REFUSED, not answered
// duplicate with the change dropped. Under the SetNX this was unreachable
// inside the TTL, which is the whole reason a host kept a body fingerprint.
func TestOr903_AChangedWasteAmountIsRefusedNotDropped(t *testing.T) {
	svc, _, payer, ctx, rdb := wastedSvcEnvWithRedis(t)
	requestID := uuid.NewString()
	// No grace: every reported micro is chargeable, so a changed report is a
	// changed CHARGE and the ledger's own key must reject it.
	requireBoundBadSpendPolicy(t, svc, ctx, "free", 1)

	first, err := svc.ReportWastedSpend(ctx, or903WasteInput(payer, "payer", requestID, 500_000))
	require.NoError(t, err)
	require.Equal(t, "charged", first.Action)

	require.NoError(t, rdb.FlushAll(ctx).Err())

	_, err = svc.ReportWastedSpend(ctx, or903WasteInput(payer, "payer", requestID, 900_000))
	require.ErrorIs(t, err, money.ErrIdempotencyKeyReused,
		"a corrected waste amount under a claimed key must be refused, not silently dropped")

	// The identical replay is still an idempotent duplicate, not an error.
	same, err := svc.ReportWastedSpend(ctx, or903WasteInput(payer, "payer", requestID, 500_000))
	require.NoError(t, err)
	require.True(t, same.Duplicate)
}

// The delegated-invoker path is claimed the same way. It writes amount 0 and
// charges nothing, so before or#903 it had no durable trace at all and its flat
// cutoff counter was double-added by any retry that crossed the TTL or a flush.
func TestOr903_DelegatedInvokerReportIsClaimedAcrossARedisFlush(t *testing.T) {
	svc, ms, payer, ctx, rdb := wastedSvcEnvWithRedis(t)
	invoker := "user:" + uuid.NewString()
	grantDelegatedSpend(t, svc, ctx, payer, invoker)
	requestID := uuid.NewString()

	in := or903WasteInput(payer, "delegated_user", requestID, 750_000)
	in.Invoker = invoker

	first, err := svc.ReportWastedSpend(ctx, in)
	require.NoError(t, err)
	require.Equal(t, "invoker_cutoff_tracked", first.Action)
	require.False(t, first.Duplicate)
	require.Equal(t, int64(1), or903WasteEventCount(t, ctx, ms, payer))

	require.NoError(t, rdb.FlushAll(ctx).Err())

	second, err := svc.ReportWastedSpend(ctx, in)
	require.NoError(t, err)
	require.True(t, second.Duplicate,
		"a delegated invoker's report is claimed by the same durable row; a flush must not reopen it")
	require.Equal(t, int64(1), or903WasteEventCount(t, ctx, ms, payer))
}
