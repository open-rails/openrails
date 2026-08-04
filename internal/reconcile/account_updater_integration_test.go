//go:build integration

package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// or#872 — DO NOT FIGHT THE ACCOUNT UPDATER.
//
// Card networks (VAU/ABU), Stripe and NMI's ACU add-on reissue a stored card
// server-side: the provider's expiry/last4 change without us doing anything.
// OpenRails builds none of that, but it must not undo it. This proves the
// convergence engine treats the provider's refreshed instrument as TRUTH:
//
//   - the local mirror ADOPTS the new expiry/last4 (never the reverse),
//   - nothing destructive fires — the subscription keeps billing, the
//     entitlement survives, the instrument row is not replaced,
//   - and the next pass does not re-raise it as a discrepancy needing repair.
func TestReconcileAdoptsAccountUpdaterRefreshedCard(t *testing.T) {
	appDB := startReconcilePostgres(t)
	baseCtx := merchant.WithID(context.Background(), dbtest.TestMerchantID)

	var (
		seeded   seededState
		vaultID  = "vault-au-" + uuid.NewString()[:8]
		methodID = uuid.New()
	)
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		seeded = seedReconcileFixtures(t, ctx, appDB)
		_, err := appDB.Qx(ctx).Exec(ctx, `
			INSERT INTO openrails.payment_methods
			  (id, merchant_id, customer_id, rail, rail_customer_ref, rail_method_ref,
			   initial_transaction_id, last_four, card_type, expiry_date)
			VALUES ($1, $2, $3, 'nmi', $4, '', '', '1111', 'visa', '1226')`,
			methodID, dbtest.TestMerchantID.UUID(), seeded.subjectID, vaultID)
		return err
	}))
	t.Cleanup(func() {
		_ = appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			_, _ = appDB.Qx(ctx).Exec(ctx, `DELETE FROM openrails.payment_methods WHERE id = $1`, methodID)
			return nil
		})
	})

	// The provider roster: BOTH local subscriptions still alive (so no PS-2
	// cancellation noise) and the vault entry carrying the REISSUED card —
	// exactly what an account-updater refresh looks like on the next pull.
	snapshot := func() *RemoteSnapshot {
		now := time.Now().UTC()
		end := now.Add(20 * 24 * time.Hour)
		var alive, dead string
		require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
				`SELECT rail_subscription_id FROM openrails.subscriptions WHERE id = $1`, seeded.subAlive).Scan(&alive))
			return appDB.Qx(ctx).QueryRow(ctx,
				`SELECT rail_subscription_id FROM openrails.subscriptions WHERE id = $1`, seeded.subDead).Scan(&dead)
		}))
		return &RemoteSnapshot{
			Provider:     ProviderNMI,
			FetchedAt:    now,
			Capabilities: Capabilities{Subscriptions: true, Vault: true},
			Coverage:     SnapshotCoverage{SubscriptionsExhaustive: true},
			Subscriptions: []RemoteSubscription{
				{RailSubscriptionID: alive, Status: SubscriptionStatusActive, NextBillingAt: &end},
				{RailSubscriptionID: dead, Status: SubscriptionStatusActive, NextBillingAt: &end},
			},
			PaymentMethods: []RemotePaymentMethod{
				{RailCustomerRef: vaultID, CardLast4: "4242", CardExpiry: "0330"},
			},
		}
	}

	newEngine := func(snap *RemoteSnapshot) *Engine {
		return &Engine{
			Fetchers:  map[Provider]RailFetcher{ProviderNMI: &fakeFetcher{provider: ProviderNMI, snap: snap}},
			Store:     &PGStore{DB: appDB},
			Local:     &PGLocalStateLoader{DB: appDB},
			Writer:    &PGLocalWriter{DB: appDB},
			Decisions: NewDecisionApplier(appDB, nil),
			Runs:      &PGDestructiveRunRecorder{DB: appDB},
		}
	}

	var enforce *RunResult
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := newEngine(snapshot()).Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
		enforce = res
		return err
	}))
	require.NotNil(t, enforce)
	require.Equal(t, "completed", enforce.Status)

	ps7 := findingsOfType(enforce.Findings, FindingPaymentMethodMismatch)
	require.Len(t, ps7, 1, "the reissued card is seen exactly once")
	assert.False(t, ps7[0].RequiresAdmin, "an updater refresh is never an admin-required discrepancy")
	assert.Equal(t, FindingStatusAutoFixed, ps7[0].Status, "the engine ADOPTS provider truth rather than parking it as drift")
	assert.Equal(t, methodID.String(), ps7[0].LocalEvidence["payment_method_id"])
	assert.GreaterOrEqual(t, enforce.Summary.Providers["nmi"].AutoFixed, 1, "the adopt applied")

	assertInstrument := func(t *testing.T, wantLast4, wantExpiry string) {
		t.Helper()
		require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
			var last4, expiry string
			require.NoError(t, appDB.Qx(ctx).QueryRow(ctx, `
				SELECT COALESCE(last_four,''), COALESCE(expiry_date,'')
				  FROM openrails.payment_methods WHERE id = $1`, methodID).
				Scan(&last4, &expiry))
			assert.Equal(t, wantLast4, last4)
			assert.Equal(t, wantExpiry, expiry)
			return nil
		}))
	}
	// PROVIDER TRUTH WINS: the mirror moved to the reissued card, and our stale
	// 1111/1226 was not pushed back over it.
	assertInstrument(t, "4242", "0330")

	// Nothing destructive fired: both subscriptions keep billing and the
	// entitlement window is intact.
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		for _, id := range []uuid.UUID{seeded.subAlive, seeded.subDead} {
			var status string
			var cancelledAt, deletedAt *time.Time
			require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
				`SELECT status::text, cancelled_at, deleted_at FROM openrails.subscriptions WHERE id = $1`, id).
				Scan(&status, &cancelledAt, &deletedAt))
			assert.Equal(t, "active", status, "a refreshed card must never cost a subscription")
			assert.Nil(t, cancelledAt, "no cancellation")
			assert.Nil(t, deletedAt, "no soft delete")
		}
		var live int
		require.NoError(t, appDB.Qx(ctx).QueryRow(ctx,
			`SELECT count(*) FROM openrails.entitlements WHERE id = $1 AND revoked_at IS NULL`, seeded.entDeadID).Scan(&live))
		assert.Equal(t, 1, live, "entitlements survive an instrument refresh")
		return nil
	}))

	// The SECOND pass sees agreement and raises nothing — no repair pass tries
	// to fix the provider's card back to our old copy.
	var second *RunResult
	require.NoError(t, appDB.RunInMerchantConn(baseCtx, func(ctx context.Context) error {
		res, err := newEngine(snapshot()).Run(ctx, RunParams{Mode: ModeEnforce, Providers: []Provider{ProviderNMI}})
		second = res
		return err
	}))
	require.NotNil(t, second)
	assert.Empty(t, findingsOfType(second.Findings, FindingPaymentMethodMismatch),
		"an adopted card is agreement, not standing drift")
	assertInstrument(t, "4242", "0330")
}

func findingsOfType(findings []FindingRecord, t FindingType) []FindingRecord {
	var out []FindingRecord
	for _, f := range findings {
		if f.Type == t {
			out = append(out, f)
		}
	}
	return out
}

