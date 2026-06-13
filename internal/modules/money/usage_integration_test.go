//go:build integration

package money_test

import (
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/stretchr/testify/require"
)

func TestRecordUsage_DebitsAndAggregates(t *testing.T) {
	svc, pool, payer, ct, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE tenant_subject_id = $1", payer.UUID())
	})

	_, err := svc.Deposit(ctx, credits.CreditDepositParams{
		TenantSubjectID: &payer, Actor: payer.UUID().String(), CreditType: ct, Amount: 100_000, Source: "seed",
	})
	require.NoError(t, err)

	_, err = svc.RecordUsage(ctx, credits.RecordUsageParams{
		Payer: &payer, Actor: "user:a", CreditType: ct, EventType: "gpt-4o",
		Dimensions: map[string]int64{"input_tokens": 100, "output_tokens": 50},
		Amount:     5_000, Source: "req", SourceID: "r1",
	})
	require.NoError(t, err)
	_, err = svc.RecordUsage(ctx, credits.RecordUsageParams{
		Payer: &payer, Actor: "user:a", CreditType: ct, EventType: "gpt-4o",
		Dimensions: map[string]int64{"input_tokens": 60, "output_tokens": 30},
		Amount:     3_000, Source: "req", SourceID: "r2",
	})
	require.NoError(t, err)

	// Balance debited by 8_000.
	bal, err := svc.GetBalanceForTenantSubject(ctx, payer, ct)
	require.NoError(t, err)
	require.Equal(t, int64(92_000), bal.Balance)

	// Rollup: one event_type, summed amount + dimensions.
	rows, err := svc.AggregateUsage(ctx, payer, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, rows, 1)
	r := rows[0]
	require.Equal(t, "gpt-4o", r.EventType)
	require.Equal(t, int64(8_000), r.TotalAmount)
	require.Equal(t, int64(2), r.EventCount)
	require.Equal(t, int64(160), r.Dimensions["input_tokens"])
	require.Equal(t, int64(80), r.Dimensions["output_tokens"])
}

func TestRecordUsage_Idempotent(t *testing.T) {
	svc, pool, payer, ct, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE tenant_subject_id = $1", payer.UUID())
	})

	_, err := svc.Deposit(ctx, credits.CreditDepositParams{
		TenantSubjectID: &payer, Actor: payer.UUID().String(), CreditType: ct, Amount: 10_000, Source: "seed",
	})
	require.NoError(t, err)

	p := credits.RecordUsageParams{
		Payer: &payer, Actor: "user:a", CreditType: ct, EventType: "gpt-4o",
		Amount: 2_000, Source: "req", SourceID: "dup",
	}
	ev1, err := svc.RecordUsage(ctx, p)
	require.NoError(t, err)
	ev2, err := svc.RecordUsage(ctx, p) // replay
	require.NoError(t, err)
	require.Equal(t, ev1.ID, ev2.ID, "replay must return the same event")

	// Only ONE debit happened.
	bal, err := svc.GetBalanceForTenantSubject(ctx, payer, ct)
	require.NoError(t, err)
	require.Equal(t, int64(8_000), bal.Balance)
}

func TestRecordUsage_ZeroCostNoDebit(t *testing.T) {
	svc, pool, payer, ct, ctx := moneyInEnv(t)
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM openrails.usage_events WHERE tenant_subject_id = $1", payer.UUID())
	})

	_, err := svc.Deposit(ctx, credits.CreditDepositParams{
		TenantSubjectID: &payer, Actor: payer.UUID().String(), CreditType: ct, Amount: 5_000, Source: "seed",
	})
	require.NoError(t, err)

	ev, err := svc.RecordUsage(ctx, credits.RecordUsageParams{
		Payer: &payer, Actor: "user:a", CreditType: ct, EventType: "cached-hit",
		Dimensions: map[string]int64{"cached_input_tokens": 200, "requests": 1},
		Amount:     0, Source: "req", SourceID: "z1",
	})
	require.NoError(t, err)
	require.Nil(t, ev.CreditTransactionID, "zero-cost event has no debit txn")

	bal, err := svc.GetBalanceForTenantSubject(ctx, payer, ct)
	require.NoError(t, err)
	require.Equal(t, int64(5_000), bal.Balance, "zero-cost event must not debit")
}
