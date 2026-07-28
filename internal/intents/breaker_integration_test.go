//go:build integration

package intents

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

// breakerMerchant seeds a DEDICATED merchant with n cancelled NMI subscriptions
// (marker set) and one pending nmi_delete intent each — isolated from the
// shared test merchant so rolling-window counts are deterministic.
type breakerMerchant struct {
	id      uuid.UUID
	intents []uuid.UUID
	subs    []uuid.UUID
}

func seedBreakerMerchant(t *testing.T, dbi *db.DB, n int) breakerMerchant {
	t.Helper()
	ctx := context.Background()
	pool := dbi.Pool()
	store := NewStore(dbi)
	sfx := uuid.NewString()[:8]

	m := breakerMerchant{id: uuid.New()}
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	exec(`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`, m.id, "breaker-"+sfx)
	productID, priceID := uuid.New(), uuid.New()
	exec(`INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, $2, $3)`,
		productID, "breaker-prod-"+sfx, m.id)
	exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, access_duration_hours, auto_renew, merchant_id)
	      VALUES ($1, $2, 999, 'USD', 720, true, $3)`, priceID, productID, m.id)

	now := time.Now().UTC()
	for i := 0; i < n; i++ {
		subID := uuid.New()
		custID, err := gen.New(pool).EnsureCustomer(ctx, gen.EnsureCustomerParams{
			ID: uuid.New(), MerchantID: m.id, Subject: nil,
		})
		require.NoError(t, err)
		psid := fmt.Sprintf("psid-%s-%d", sfx, i)
		exec(`INSERT INTO openrails.subscriptions
		        (id, price_id, product_id, status, rail, rail_subscription_id,
		         current_period_starts_at, current_period_ends_at, started_at,
		         cancelled_at, cancel_type, deletion_scheduled_at, customer_id, merchant_id)
		      VALUES ($1, $2, $3, 'cancelled', 'mobius', $4, $5, $6, $5, $7, 'expired', $7, $8, $9)`,
			subID, priceID, productID, psid,
			now.Add(-40*24*time.Hour), now.Add(-10*24*time.Hour), now, custID, m.id)
		row, err := store.Enqueue(ctx, EnqueueParams{
			MerchantID:     m.id,
			Provider:       "mobius",
			IntentType:     TypeNMIDeleteSubscription,
			SubscriptionID: &subID,
			Payload:        NMIDeletePayload{UserID: custID.String(), RailSubscriptionID: psid},
			IdempotencyKey: NMIDeleteIdempotencyKey(subID),
			NextAttemptAt:  now.Add(-time.Minute),
			Origin:         OriginSystem,
			OriginReason:   "breaker integration test",
		})
		require.NoError(t, err)
		m.subs = append(m.subs, subID)
		m.intents = append(m.intents, row.ID)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.rail_mutation_logs WHERE merchant_id = $1`, m.id)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.rail_intents WHERE merchant_id = $1`, m.id)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id = $1`, m.id)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.subscriptions WHERE merchant_id = $1`, m.id)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.prices WHERE merchant_id = $1`, m.id)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.products WHERE merchant_id = $1`, m.id)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.customers WHERE merchant_id = $1`, m.id)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.merchants WHERE id = $1`, m.id)
	})
	return m
}

func (m breakerMerchant) statusCounts(t *testing.T, dbi *db.DB) map[string]int {
	t.Helper()
	rows, err := dbi.Pool().Query(context.Background(),
		`SELECT status, count(*) FROM openrails.rail_intents WHERE merchant_id = $1 GROUP BY status`, m.id)
	require.NoError(t, err)
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s string
		var n int
		require.NoError(t, rows.Scan(&s, &n))
		out[s] = n
	}
	require.NoError(t, rows.Err())
	return out
}

func breakerRunner(dbi *db.DB, client *nmi.NMIClient) *Runner {
	return &Runner{
		Store: NewStore(dbi),
		Registry: NewRegistry(
			NewNMIDeleteHandler(dbi, fullModeConfig(), fakeNMIResolver{client: client}, nil),
		),
		Config:  fullModeConfig(),
		Breaker: NewVolumeBreaker(dbi),
	}
}

// #679: N destructive intents under budget execute; the N+1th halts with ONE
// requires_review held_bulk finding; while the finding is open destructive
// execution stays halted (and held intents cannot expire); a different
// merchant is unaffected; operator ack resumes.
func TestBreakerHaltsBulkDestructiveExecution(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	pool := dbi.Pool()

	const over = 2 // intents beyond the budget floor
	bulk := seedBreakerMerchant(t, dbi, DestructiveBudgetFloor+over)
	other := seedBreakerMerchant(t, dbi, 1)

	_, client := newFakeNMI(t, "any", true)
	runner := breakerRunner(dbi, client)

	_, err := runner.RunExecuteOnce(ctx)
	require.NoError(t, err)

	// Bulk merchant: exactly the budget executed, the rest held pending.
	counts := bulk.statusCounts(t, dbi)
	assert.Equal(t, DestructiveBudgetFloor, counts[StatusSucceeded], "exactly the budget executes")
	assert.Equal(t, over, counts[StatusPending], "over-budget intents stay pending (held)")

	// The other merchant's single delete executed — breakers are per merchant.
	assert.Equal(t, map[string]int{StatusSucceeded: 1}, other.statusCounts(t, dbi))

	// ONE operator finding, requires_review, evidence carries count/budget/window.
	var findingID uuid.UUID
	var status string
	var evidence []byte
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT id, status, evidence FROM openrails.reconciliation_findings
		 WHERE merchant_id = $1 AND finding_type = $2`, bulk.id, HeldBulkFindingType).
		Scan(&findingID, &status, &evidence))
	assert.Equal(t, "requires_review", status)
	assert.Contains(t, string(evidence), `"budget": 25`)
	assert.Contains(t, string(evidence), `"window_hours": 24`)
	assert.Contains(t, string(evidence), `"executed_count"`)
	var findingCount int
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT count(*) FROM openrails.reconciliation_findings WHERE merchant_id = $1`, bulk.id).Scan(&findingCount))
	assert.Equal(t, 1, findingCount, "one standing finding per merchant")

	// Held intents never expire out of the ledger while the finding is open,
	// even past their relevance window.
	heldID := heldIntent(t, dbi, bulk)
	_, err = pool.Exec(ctx, `UPDATE openrails.rail_intents SET expires_at = now() - interval '1 minute' WHERE id = $1`, heldID)
	require.NoError(t, err)
	expired, err := NewStore(dbi).ExpireOverdue(ctx, time.Now().UTC())
	require.NoError(t, err)
	_ = expired // other tests' rows may expire; ours must not
	var heldStatus string
	require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM openrails.rail_intents WHERE id = $1`, heldID).Scan(&heldStatus))
	assert.Equal(t, StatusPending, heldStatus, "breaker-held intent must not expire while the finding is open")
	_, err = pool.Exec(ctx, `UPDATE openrails.rail_intents SET expires_at = NULL WHERE id = $1`, heldID)
	require.NoError(t, err)

	// A second pass with the finding still open executes nothing more.
	_, err = pool.Exec(ctx,
		`UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE merchant_id = $1 AND status = $2`,
		bulk.id, StatusPending)
	require.NoError(t, err)
	_, err = runner.RunExecuteOnce(ctx)
	require.NoError(t, err)
	counts = bulk.statusCounts(t, dbi)
	assert.Equal(t, DestructiveBudgetFloor, counts[StatusSucceeded], "open finding keeps destructive execution halted")
	assert.Equal(t, over, counts[StatusPending])

	// Operator resolution (ack → fixed) resumes: the window restarts at the
	// resolution instant, so the held intents drain.
	n, err := gen.New(pool).AckReconciliationFinding(ctx, gen.AckReconciliationFindingParams{ID: findingID})
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	_, err = pool.Exec(ctx,
		`UPDATE openrails.rail_intents SET next_attempt_at = now() WHERE merchant_id = $1 AND status = $2`,
		bulk.id, StatusPending)
	require.NoError(t, err)
	_, err = runner.RunExecuteOnce(ctx)
	require.NoError(t, err)
	counts = bulk.statusCounts(t, dbi)
	assert.Equal(t, DestructiveBudgetFloor+over, counts[StatusSucceeded], "resolution resumes destructive execution")
	assert.Zero(t, counts[StatusPending])
}

func heldIntent(t *testing.T, dbi *db.DB, m breakerMerchant) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, dbi.Pool().QueryRow(context.Background(),
		`SELECT id FROM openrails.rail_intents WHERE merchant_id = $1 AND status = $2 LIMIT 1`,
		m.id, StatusPending).Scan(&id))
	return id
}
