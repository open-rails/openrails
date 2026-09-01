//go:build integration

package service_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// or#894: two DIFFERENT money operations that share one (source, source_id)
// used to collide at the ledger, because the spend coordinate carried no
// discriminator for WHICH KIND of write posted the leg. usage_events has one
// (uq_usage_events_idem includes event_type); the ledger leg did not.

func or894Balance(t *testing.T, ms *money.MoneyService, ctx context.Context, payer identity.CustomerID) int64 {
	t.Helper()
	bal, err := ms.GetBalanceForCustomer(ctx, payer, money.DefaultCurrency)
	require.NoError(t, err)
	return bal.Balance
}

// The induced defect: a wasted-spend overage charge posts through
// RecordUsage(event_type="wasted_spend") and lands a ledger spend at
// ("invoke", request_id). The SAME request then renders and is captured at the
// same coordinates — and the capture moved 0 micros and returned the WASTE
// transfer. A delivered service went uncharged and the caller was told the
// capture succeeded.
func TestOr894_WasteOverageThenCaptureOfTheSameRequestChargesBoth(t *testing.T) {
	svc, ms, payer, ctx := wastedSvcEnv(t)
	pid := payer.UUID()
	requestID := uuid.NewString()

	// A REAL bad-spend policy at the payer's resolved trust level, with grace
	// >= 1 — abuse.RecordPayerGrace skips a window whose Limit <= 0, which makes
	// the overage CHARGE branch unreachable and every assertion under it vacuous.
	requireBoundBadSpendPolicy(t, svc, ctx, "free", 1)

	before := or894Balance(t, ms, ctx, payer)

	res, err := svc.ReportWastedSpend(ctx, billingservice.WastedSpendInput{
		CustomerID: payer, Invoker: pid.String(), InvokerType: string(identity.InvokerTypePayer),
		Currency: money.DefaultCurrency, Amount: 11,
		Source: "invoke", SourceID: requestID, Reason: "failed render",
	})
	require.NoError(t, err)
	require.Equal(t, "charged", res.Action)
	require.Equal(t, int64(10), res.ChargedAmount)
	require.Equal(t, int64(10), before-or894Balance(t, ms, ctx, payer),
		"the overage must actually move money, else the collision below is untested")

	// The same request then renders and is captured.
	admit, err := svc.Admit(ctx, billingservice.AdmitInput{
		CustomerID: payer, Invoker: pid.String(), InvokerType: string(identity.InvokerTypePayer),
		Currency: money.DefaultCurrency, EstimatedAmount: 900_000,
		ExpiresAtUnix: time.Now().Add(time.Hour).Unix(),
		Source:        "invoke", SourceID: requestID,
	})
	require.NoError(t, err)
	require.True(t, admit.Allowed)

	trx, err := svc.CaptureHold(ctx, billingservice.CaptureHoldRequest{
		RequestID: requestID, Amount: 900_000, CustomerID: pid.String(),
		Currency: money.DefaultCurrency, Invoker: pid.String(),
	})
	require.NoError(t, err)
	require.Equal(t, int64(-900_000), trx.Amount,
		"the capture must return ITS OWN transfer, never the waste transfer at the same (source, source_id)")

	require.Equal(t, int64(900_010), before-or894Balance(t, ms, ctx, payer),
		"a rendered service must be charged even when a waste overage already posted at the same request id")

	// The capture is still idempotent at its own coordinate.
	_, err = svc.CaptureHold(ctx, billingservice.CaptureHoldRequest{
		RequestID: requestID, Amount: 900_000, CustomerID: pid.String(),
		Currency: money.DefaultCurrency, Invoker: pid.String(),
	})
	require.NoError(t, err)
	require.Equal(t, int64(900_010), before-or894Balance(t, ms, ctx, payer))
}

// The reciprocal direction: usage dedupe keys on event_type, the ledger dedupe
// did not. Two genuinely different usage events at one request id must both
// post — before or#894 the second either silently absorbed into the first or
// (after or#891's unique index) blew up as a constraint violation.
func TestOr894_TwoUsageEventTypesAtOneSourceIDPostTwoDebits(t *testing.T) {
	svc, ms, payer, ctx := wastedSvcEnv(t)
	// ledger_transfers is FORCE RLS: a read without app.merchant_id pinned sees
	// nothing, even as the owner. The assertion below counts real rows, so it
	// needs the merchant-scoped pool, not the facade's unpinned one.
	pool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	requestID := uuid.NewString()

	before := or894Balance(t, ms, ctx, payer)

	require.NoError(t, svc.RecordUsage(ctx, billingservice.RecordUsageInput{
		CustomerID: payer, Currency: money.DefaultCurrency, EventType: "gpt-4o",
		Amount: 700, Key: money.MustIdempotencyKey(money.UsageOperation("gpt-4o"), "invoke", requestID),
	}))
	require.NoError(t, svc.RecordUsage(ctx, billingservice.RecordUsageInput{
		CustomerID: payer, Currency: money.DefaultCurrency, EventType: "image-gen",
		Amount: 900, Key: money.MustIdempotencyKey(money.UsageOperation("image-gen"), "invoke", requestID),
	}))

	require.Equal(t, int64(1_600), before-or894Balance(t, ms, ctx, payer),
		"two different metered events at one request id are two charges, not one")

	var debits int64
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM openrails.ledger_transfers
		 WHERE customer_id = $1 AND source = 'invoke' AND source_id = $2
		   AND transfer_type = 'credit_spend'
	`, payer.UUID(), requestID).Scan(&debits))
	require.EqualValues(t, 2, debits)

	// Each event type is still idempotent on its own coordinate.
	require.NoError(t, svc.RecordUsage(ctx, billingservice.RecordUsageInput{
		CustomerID: payer, Currency: money.DefaultCurrency, EventType: "gpt-4o",
		Amount: 700, Key: money.MustIdempotencyKey(money.UsageOperation("gpt-4o"), "invoke", requestID),
	}))
	require.Equal(t, int64(1_600), before-or894Balance(t, ms, ctx, payer))
}
