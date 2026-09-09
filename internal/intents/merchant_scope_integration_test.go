//go:build integration

package intents

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestMutationPredicatesRejectCrossMerchantIDsWithoutRLS(t *testing.T) {
	ctx := context.Background()
	super := dbtest.OpenAppDB(t, dbtest.SharedSuperuserDSN(t))
	tx, err := super.Pool().Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, tx.Rollback(context.Background())) })

	dbi := super.NewWithPgxTx(tx)
	qx := dbi.Qx(ctx)
	ownerID, otherID := uuid.New(), uuid.New()
	ownerCtx := merchant.WithID(ctx, merchant.ID(ownerID))
	otherCtx := merchant.WithID(ctx, merchant.ID(otherID))
	suffix := uuid.NewString()

	_, err = qx.Exec(ctx,
		`INSERT INTO openrails.merchants (id, slug, status)
		 VALUES ($1, $2, 'active'), ($3, $4, 'active')`,
		ownerID, "scope-owner-"+suffix, otherID, "scope-other-"+suffix)
	require.NoError(t, err)

	customerA, customerB := uuid.New(), uuid.New()
	productID, priceID, pspID := uuid.New(), uuid.New(), uuid.New()
	_, err = qx.Exec(ctx,
		`INSERT INTO openrails.customers (id, merchant_id)
		 VALUES ($1, $3), ($2, $3)`, customerA, customerB, ownerID)
	require.NoError(t, err)
	_, err = qx.Exec(ctx,
		`INSERT INTO openrails.products (id, key, display_name, merchant_id)
		 VALUES ($1, $2, 'Scope predicate product', $3)`, productID, "scope-product-"+suffix, ownerID)
	require.NoError(t, err)
	_, err = qx.Exec(ctx,
		`INSERT INTO openrails.prices (id, product_id, amount, currency, merchant_id, key)
		 VALUES ($1, $2, 1000000, 'USD', $3, $4)`, priceID, productID, ownerID, "scope-price-"+suffix)
	require.NoError(t, err)
	_, err = qx.Exec(ctx,
		`INSERT INTO openrails.psps (id, merchant_id, rail, account_id)
		 VALUES ($1, $2, 'nmi', $3)`, pspID, ownerID, "scope-account-"+suffix)
	require.NoError(t, err)

	t.Run("generated mutations", func(t *testing.T) {
		paymentMethodID := uuid.New()
		_, err := qx.Exec(ctx,
			`INSERT INTO openrails.payment_methods
			   (id, merchant_id, customer_id, psp_id, rail, initial_transaction_id, last_four, expiry_date)
			 VALUES ($1, $2, $3, $4, 'nmi', $5, '1111', '01/29')`,
			paymentMethodID, ownerID, customerA, pspID, "scope-txn-"+suffix)
		require.NoError(t, err)

		queries := dbi.Gen(ctx)
		adopt := gen.ReconcileAdoptPaymentMethodParams{
			ID: paymentMethodID, MerchantID: otherID, LastFour: "4242", ExpiryDate: "12/30",
		}
		n, err := queries.ReconcileAdoptPaymentMethod(ctx, adopt)
		require.NoError(t, err)
		require.Zero(t, n)
		var lastFour, expiry string
		require.NoError(t, qx.QueryRow(ctx,
			`SELECT last_four, expiry_date FROM openrails.payment_methods WHERE id = $1`, paymentMethodID,
		).Scan(&lastFour, &expiry))
		require.Equal(t, "1111", lastFour)
		require.Equal(t, "01/29", expiry)

		adopt.MerchantID = ownerID
		n, err = queries.ReconcileAdoptPaymentMethod(ctx, adopt)
		require.NoError(t, err)
		require.EqualValues(t, 1, n)
		require.NoError(t, qx.QueryRow(ctx,
			`SELECT last_four, expiry_date FROM openrails.payment_methods WHERE id = $1`, paymentMethodID,
		).Scan(&lastFour, &expiry))
		require.Equal(t, "4242", lastFour)
		require.Equal(t, "12/30", expiry)

		now := time.Now().UTC().Truncate(time.Microsecond)
		dueAt, leaseUntil := now.Add(-time.Hour), now.Add(time.Hour)
		claimSubID, scheduleSubID := uuid.New(), uuid.New()
		_, err = qx.Exec(ctx,
			`INSERT INTO openrails.subscriptions
			   (id, merchant_id, customer_id, product_id, price_id, status, rail, psp_id,
			    rail_subscription_id, current_period_ends_at, next_retry_at)
			 VALUES
			   ($1, $3, $4, $6, $7, 'past_due', 'nmi', $8, $9, $11, $12),
			   ($2, $3, $5, $6, $7, 'past_due', 'nmi', $8, $10, $11, NULL)`,
			claimSubID, scheduleSubID, ownerID, customerA, customerB, productID, priceID, pspID,
			"scope-claim-"+suffix, "scope-schedule-"+suffix, now.Add(-24*time.Hour), dueAt)
		require.NoError(t, err)

		claim := gen.ClaimDunningAttemptParams{
			ID: claimSubID, MerchantID: otherID, LeaseUntil: leaseUntil, ClaimedAt: now,
		}
		n, err = queries.ClaimDunningAttempt(ctx, claim)
		require.NoError(t, err)
		require.Zero(t, n)
		var nextRetry time.Time
		var lastRetry *time.Time
		require.NoError(t, qx.QueryRow(ctx,
			`SELECT next_retry_at, last_retry_at FROM openrails.subscriptions WHERE id = $1`, claimSubID,
		).Scan(&nextRetry, &lastRetry))
		require.True(t, dueAt.Equal(nextRetry))
		require.Nil(t, lastRetry)

		claim.MerchantID = ownerID
		n, err = queries.ClaimDunningAttempt(ctx, claim)
		require.NoError(t, err)
		require.EqualValues(t, 1, n)
		require.NoError(t, qx.QueryRow(ctx,
			`SELECT next_retry_at, last_retry_at FROM openrails.subscriptions WHERE id = $1`, claimSubID,
		).Scan(&nextRetry, &lastRetry))
		require.True(t, leaseUntil.Equal(nextRetry))
		require.NotNil(t, lastRetry)
		require.True(t, now.Equal(*lastRetry))

		scheduleAt := now.Add(30 * time.Minute)
		schedule := gen.SetSubscriptionNextRetryParams{
			ID: scheduleSubID, MerchantID: otherID, NextRetryAt: scheduleAt,
		}
		n, err = queries.SetSubscriptionNextRetry(ctx, schedule)
		require.NoError(t, err)
		require.Zero(t, n)
		var scheduled *time.Time
		require.NoError(t, qx.QueryRow(ctx,
			`SELECT next_retry_at FROM openrails.subscriptions WHERE id = $1`, scheduleSubID,
		).Scan(&scheduled))
		require.Nil(t, scheduled)

		schedule.MerchantID = ownerID
		n, err = queries.SetSubscriptionNextRetry(ctx, schedule)
		require.NoError(t, err)
		require.EqualValues(t, 1, n)
		require.NoError(t, qx.QueryRow(ctx,
			`SELECT next_retry_at FROM openrails.subscriptions WHERE id = $1`, scheduleSubID,
		).Scan(&scheduled))
		require.NotNil(t, scheduled)
		require.True(t, scheduleAt.Equal(*scheduled))
	})

	t.Run("raw intent mutations", func(t *testing.T) {
		store := NewStore(dbi)
		payload := `{"credential":"owner-only"}`
		evidence := `{"proof":"owner-only"}`
		ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()}
		statuses := []string{StatusSucceeded, StatusSucceeded, StatusSucceeded, StatusFailedTerminal, StatusPending}
		for i, id := range ids {
			_, err := qx.Exec(ctx,
				`INSERT INTO openrails.rail_intents
				   (id, merchant_id, rail, psp_id, intent_type, idempotency_key, status, origin,
				    payload, result_evidence, executed_at)
				 VALUES ($1, $2, 'nmi', $3, 'scope_test', $4, $5, 'system', $6, $7,
				         CASE WHEN $5 = 'succeeded' THEN now() ELSE NULL END)`,
				id, ownerID, pspID, "scope-intent-"+uuid.NewString(), statuses[i], payload, evidence)
			require.NoError(t, err)
		}

		assertDocuments := func(t *testing.T, id uuid.UUID, wantPayload, wantEvidence string) {
			t.Helper()
			var gotPayload, gotEvidence []byte
			require.NoError(t, qx.QueryRow(ctx,
				`SELECT payload, result_evidence FROM openrails.rail_intents WHERE id = $1`, id,
			).Scan(&gotPayload, &gotEvidence))
			if wantPayload == "" {
				require.Nil(t, gotPayload)
			} else {
				require.JSONEq(t, wantPayload, string(gotPayload))
			}
			if wantEvidence == "" {
				require.Nil(t, gotEvidence)
			} else {
				require.JSONEq(t, wantEvidence, string(gotEvidence))
			}
		}

		newEvidence := map[string]any{"transaction_id": "txn-owner"}
		require.NoError(t, store.PruneSucceeded(otherCtx, ids[0], newEvidence, false, true))
		assertDocuments(t, ids[0], payload, evidence)
		require.NoError(t, store.PruneSucceeded(ownerCtx, ids[0], newEvidence, false, true))
		assertDocuments(t, ids[0], "", evidence)

		require.NoError(t, store.PruneSucceeded(otherCtx, ids[1], newEvidence, true, false))
		assertDocuments(t, ids[1], payload, evidence)
		require.NoError(t, store.PruneSucceeded(ownerCtx, ids[1], newEvidence, true, false))
		assertDocuments(t, ids[1], payload, `{"transaction_id":"txn-owner"}`)

		require.NoError(t, store.PruneSucceeded(otherCtx, ids[2], newEvidence, false, false))
		assertDocuments(t, ids[2], payload, evidence)
		require.NoError(t, store.PruneSucceeded(ownerCtx, ids[2], newEvidence, false, false))
		assertDocuments(t, ids[2], "", `{"transaction_id":"txn-owner"}`)

		require.NoError(t, store.PruneTerminalPayload(otherCtx, ids[3]))
		assertDocuments(t, ids[3], payload, evidence)
		require.NoError(t, store.PruneTerminalPayload(ownerCtx, ids[3]))
		assertDocuments(t, ids[3], "", evidence)

		require.NoError(t, store.RecordProgress(otherCtx, ids[4], map[string]any{"signature": "owner-signature"}))
		assertDocuments(t, ids[4], payload, evidence)
		require.NoError(t, store.RecordProgress(ownerCtx, ids[4], map[string]any{"signature": "owner-signature"}))
		assertDocuments(t, ids[4], payload, `{"proof":"owner-only","signature":"owner-signature"}`)
	})
}
