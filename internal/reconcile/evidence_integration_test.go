//go:build integration

package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// StoreEvidence is intentionally tested through the RLS app role. The same
// provider event is pulled twice, then from a second PSP and merchant; only
// the matching merchant/PSP event set may be visible to each report query.
func TestPGEvidenceStore_IdempotentAndScoped(t *testing.T) {
	appDB := startReconcilePostgres(t)
	merchantA := newReconcileMerchant(t, appDB)
	merchantB := newReconcileMerchant(t, appDB)
	ctxA := merchant.WithID(context.Background(), merchantA)
	ctxB := merchant.WithID(context.Background(), merchantB)

	var pspA, pspA2, pspB uuid.UUID
	require.NoError(t, appDB.RunInMerchantConn(ctxA, func(ctx context.Context) error {
		pspA = dbtest.EnsureTestPSP(ctx, t, appDB.Qx(ctx), merchantA.UUID(), "nmi")
		// EnsureTestPSP reuses a merchant's existing fixture PSP. Create a second
		// account directly so the uniqueness boundary is exercised too.
		pspA2 = uuid.New()
		_, err := appDB.Qx(ctx).Exec(ctx, `INSERT INTO billing.psps
			(id, merchant_id, rail, environment, account_id, key, archived, created_at, first_seen_at)
			VALUES ($1,$2,'nmi','test',$3,'nmi',false,'epoch'::timestamptz,'epoch'::timestamptz)`,
			pspA2, merchantA.UUID(), "evidence-second-"+pspA2.String())
		return err
	}))
	require.NoError(t, appDB.RunInMerchantConn(ctxB, func(ctx context.Context) error {
		pspB = dbtest.EnsureTestPSP(ctx, t, appDB.Qx(ctx), merchantB.UUID(), "nmi")
		return nil
	}))

	when := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	since := when.Add(-24 * time.Hour)
	until := when.Add(48 * time.Hour)
	makeSnapshot := func(psp uuid.UUID, customer, order string) *RemoteSnapshot {
		return &RemoteSnapshot{
			Provider:  ProviderNMI,
			PspID:     psp.String(),
			FetchedAt: when,
			Capabilities: Capabilities{
				Transactions: true,
			},
			Coverage: SnapshotCoverage{
				TransactionsExhaustive:        true,
				TransactionsPaginatedComplete: true,
				TransactionWindowSince:        &since,
				TransactionWindowUntil:        &until,
			},
			Subscriptions: []RemoteSubscription{{
				RailSubscriptionID: "sub-" + customer,
				Status:             SubscriptionStatusActive,
				CustomerID:         customer,
				AmountCents:        999,
				// NMI's roster omits currency; the evidence contract records UNK.
			}},
			Transactions: []RemoteTransaction{
				{
					TransactionID: "tx-action",
					Source:        "api",
					CustomerID:    customer,
					OrderID:       order,
					Type:          TransactionTypeSale,
					Success:       true,
					AmountCents:   999,
					Currency:      " usd ",
					OccurredAt:    when,
					Raw:           rawJSON(map[string]any{"action": "initial"}),
				},
				// NMI can reuse a transaction id across action rows; source/time
				// keep the second action distinct while exact duplicates collapse.
				{
					TransactionID: "tx-action",
					Source:        "recurring",
					CustomerID:    customer,
					OrderID:       order,
					Type:          TransactionTypeSale,
					Success:       true,
					AmountCents:   999,
					Currency:      "USD",
					OccurredAt:    when.Add(24 * time.Hour),
					Raw:           rawJSON(map[string]any{"action": "renewal"}),
				},
				{
					TransactionID: "tx-action",
					Source:        "recurring",
					CustomerID:    customer,
					OrderID:       order,
					Type:          TransactionTypeSale,
					Success:       true,
					AmountCents:   999,
					Currency:      "USD",
					OccurredAt:    when.Add(24 * time.Hour),
					Raw:           rawJSON(map[string]any{"action": "renewal"}),
				},
			},
		}
	}

	storeA := &PGEvidenceStore{DB: appDB}
	run1, run2 := uuid.New(), uuid.New()
	require.NoError(t, appDB.RunInMerchantConn(ctxA, func(ctx context.Context) error {
		binding := PSPBinding{ID: pspA, Rail: "nmi", AccountID: "mobius-a"}
		if err := storeA.StoreEvidence(ctx, run1, binding, makeSnapshot(pspA, "customer-a", "order-a"), since, until); err != nil {
			return err
		}
		return storeA.StoreEvidence(ctx, run2, binding, makeSnapshot(pspA, "customer-a", "order-a"), since, until)
	}))

	// A second PSP on the same merchant must not collide with the first PSP's
	// event key, while an exact rerun on the first PSP still stores two snapshots
	// but only two canonical transaction/action rows.
	require.NoError(t, appDB.RunInMerchantConn(ctxA, func(ctx context.Context) error {
		binding := PSPBinding{ID: pspA2, Rail: "nmi", AccountID: "mobius-a2"}
		return storeA.StoreEvidence(ctx, uuid.New(), binding, makeSnapshot(pspA2, "customer-a2", "order-a2"), since, until)
	}))
	require.NoError(t, appDB.RunInMerchantConn(ctxB, func(ctx context.Context) error {
		binding := PSPBinding{ID: pspB, Rail: "nmi", AccountID: "mobius-b"}
		return storeA.StoreEvidence(ctx, uuid.New(), binding, makeSnapshot(pspB, "customer-b", "order-b"), since, until)
	}))

	count := func(ctx context.Context, sql string, args ...any) int {
		t.Helper()
		var n int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, sql, args...).Scan(&n))
		return n
	}
	var firstSnapshot, lastSnapshot uuid.UUID
	require.NoError(t, appDB.RunInMerchantConn(ctxA, func(ctx context.Context) error {
		require.Equal(t, 2, count(ctx, `SELECT count(*) FROM openrails.provider_evidence_transactions WHERE merchant_id=openrails.current_merchant_id() AND psp_id=$1`, pspA))
		require.Equal(t, 2, count(ctx, `SELECT count(*) FROM openrails.provider_evidence_snapshots WHERE merchant_id=openrails.current_merchant_id() AND psp_id=$1`, pspA))
		require.Equal(t, 2, count(ctx, `SELECT count(*) FROM openrails.provider_evidence_transactions WHERE merchant_id=openrails.current_merchant_id() AND psp_id=$1`, pspA2))
		require.Equal(t, 2, count(ctx, `SELECT count(*) FROM openrails.provider_evidence_subscriptions WHERE merchant_id=openrails.current_merchant_id() AND psp_id=$1 AND currency='UNK'`, pspA))
		return appDB.Qx(ctx).QueryRow(ctx, `SELECT first_snapshot_id,last_snapshot_id FROM openrails.provider_evidence_transactions WHERE merchant_id=openrails.current_merchant_id() AND psp_id=$1 ORDER BY occurred_at LIMIT 1`, pspA).Scan(&firstSnapshot, &lastSnapshot)
	}))
	require.NotEqual(t, uuid.Nil, firstSnapshot)
	require.NotEqual(t, uuid.Nil, lastSnapshot)
	require.NotEqual(t, firstSnapshot, lastSnapshot, "second pull advances last_snapshot while preserving first_snapshot")

	require.NoError(t, appDB.RunInMerchantConn(ctxB, func(ctx context.Context) error {
		require.Equal(t, 2, count(ctx, `SELECT count(*) FROM openrails.provider_evidence_transactions WHERE merchant_id=openrails.current_merchant_id() AND psp_id=$1`, pspB))
		require.Equal(t, 0, count(ctx, `SELECT count(*) FROM openrails.provider_evidence_transactions WHERE merchant_id=openrails.current_merchant_id() AND psp_id=$1`, pspA))
		return nil
	}))

	t.Cleanup(func() {
		for _, item := range []struct {
			ctx context.Context
			id  merchant.ID
		}{{ctxA, merchantA}, {ctxB, merchantB}} {
			_ = appDB.RunInMerchantConn(item.ctx, func(ctx context.Context) error {
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.provider_evidence_snapshots WHERE merchant_id=$1`, item.id.UUID())
				_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM billing.psps WHERE merchant_id=$1`, item.id.UUID())
				return nil
			})
		}
		_, _ = appDB.Pool().Exec(context.Background(), `DELETE FROM billing.merchants WHERE id = ANY($1)`, []uuid.UUID{merchantA.UUID(), merchantB.UUID()})
	})
}
