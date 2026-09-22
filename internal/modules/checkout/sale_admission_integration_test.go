//go:build integration

package checkout

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/stretchr/testify/require"
)

func saleAdmissionParams(fx *saleIntentFixture, key string) intents.EnqueueParams {
	return intents.EnqueueParams{MerchantID: fx.merchantID.UUID(), Provider: "nmi", PspID: fx.payload.Instrument.PSPID, IntentType: payments.TypeNMISale, PriceID: &fx.priceID, Payload: fx.payload, IdempotencyKey: NMISaleIdempotencyKey(key), Origin: intents.OriginUser}
}

func TestSaleCrossCustomerKeyRaceKeepsOnlyCanonicalOwner(t *testing.T) {
	a := newSaleIntentFixture(t)
	b := newSaleIntentFixtureForMerchant(t, a.merchantID) // Same merchant, different customers competing for one key.
	gate, err := a.db.Pool().Begin(a.ctx)
	require.NoError(t, err)
	defer func() { _ = gate.Rollback(a.ctx) }()
	for _, fx := range []*saleIntentFixture{a, b} {
		var id uuid.UUID
		require.NoError(t, gate.QueryRow(a.ctx, `SELECT id FROM billing.payment_methods WHERE id=$1 FOR UPDATE`, fx.payload.PaymentMethodID).Scan(&id))
	}
	var pid int
	require.NoError(t, gate.QueryRow(a.ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	type result struct {
		row gen.OpenrailsRailIntent
		err error
	}
	results := make(chan result, 2)
	key := uuid.NewString()
	for _, fx := range []*saleIntentFixture{a, b} {
		go func() {
			row, err := intents.NewStore(fx.db).Enqueue(fx.ctx, saleAdmissionParams(fx, key))
			results <- result{row, err}
		}()
	}
	require.Eventually(t, func() bool {
		var n int
		err := a.db.Pool().QueryRow(a.ctx, `SELECT count(*) FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))`, pid).Scan(&n)
		return err == nil && n == 2
	}, 10*time.Second, 10*time.Millisecond, "both callers must miss the key before their inserts race")
	require.NoError(t, gate.Commit(a.ctx))
	first, second := <-results, <-results
	if first.err != nil {
		first, second = second, first
	}
	require.NoError(t, first.err)
	require.Error(t, second.err, "a cross-customer conflict winner is never overwritten or returned as the caller's purchase")
	canonical, err := intents.NewStore(a.db).GetByIdempotencyKey(a.ctx, NMISaleIdempotencyKey(key))
	require.NoError(t, err)
	require.Equal(t, first.row.ID, canonical.ID)
	require.JSONEq(t, string(first.row.Payload), string(canonical.Payload))
	var count int
	require.NoError(t, a.db.Pool().QueryRow(a.ctx, `SELECT count(*) FROM billing.rail_intents WHERE idempotency_key=$1`, NMISaleIdempotencyKey(key)).Scan(&count))
	require.Equal(t, 1, count)
	require.Zero(t, a.gateway.saleCalls.Load()+b.gateway.saleCalls.Load())
}

func TestSaleInstrumentRemapAndAdmissionHaveOneOrder(t *testing.T) {
	for _, remapFirst := range []bool{false, true} {
		name := "accepted_operation_pins_method"
		if remapFirst {
			name = "remap_wins_before_admission"
		}
		t.Run(name, func(t *testing.T) {
			fx := newSaleIntentFixture(t)
			store := intents.NewStore(fx.db)
			params := saleAdmissionParams(fx, uuid.NewString())
			if !remapFirst {
				row, err := store.Enqueue(fx.ctx, params)
				require.NoError(t, err)
				require.Equal(t, intents.StatusPending, row.Status)
				// This is the production custody migration predicate, including
				// pending accepted work that has made no provider request yet.
				pinned, err := fx.db.Gen(fx.ctx).CountUnresolvedOperationsNamingPaymentMethod(fx.ctx, gen.CountUnresolvedOperationsNamingPaymentMethodParams{MerchantID: params.MerchantID, PaymentMethodID: fx.payload.PaymentMethodID})
				require.NoError(t, err)
				require.EqualValues(t, 1, pinned)
			} else {
				gate, err := fx.db.Pool().Begin(fx.ctx)
				require.NoError(t, err)
				defer func() { _ = gate.Rollback(fx.ctx) }()
				var id uuid.UUID
				require.NoError(t, gate.QueryRow(fx.ctx, `SELECT id FROM billing.payment_methods WHERE id=$1 FOR UPDATE`, fx.payload.PaymentMethodID).Scan(&id))
				var pid int
				require.NoError(t, gate.QueryRow(fx.ctx, `SELECT pg_backend_pid()`).Scan(&pid))
				done := make(chan error, 1)
				go func() { _, err := store.Enqueue(fx.ctx, params); done <- err }()
				require.Eventually(t, func() bool {
					var waiting bool
					err := fx.db.Pool().QueryRow(fx.ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting)
					return err == nil && waiting
				}, 10*time.Second, 10*time.Millisecond)
				_, err = gate.Exec(fx.ctx, `UPDATE billing.payment_methods SET rail_customer_ref='remapped-before-admission' WHERE id=$1`, id)
				require.NoError(t, err)
				require.NoError(t, gate.Commit(fx.ctx))
				require.Error(t, <-done)
				var count int
				require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.rail_intents WHERE idempotency_key=$1`, params.IdempotencyKey).Scan(&count))
				require.Zero(t, count)
			}
			require.Zero(t, fx.gateway.saleCalls.Load())
		})
	}
}
