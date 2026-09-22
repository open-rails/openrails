//go:build integration

package checkout

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/stretchr/testify/require"
)

func TestInitialEnrollmentRefusesExistingAndUncertainTierObligations(t *testing.T) {
	for _, state := range []string{"active", "past_due", "pending", "unknown", "unresolved_enrollment"} {
		t.Run(state, func(t *testing.T) {
			fx := newSubIntentFixture(t)
			fx.prepare(t)
			mid := dbtest.TestMerchantID.UUID()
			terms := fx.payload.Terms
			group := "exclusive-" + uuid.NewString()
			other, otherPrice := uuid.New(), uuid.New()
			_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.products SET tier_group=$2 WHERE id=$1`, terms.ProductID, group)
			require.NoError(t, err)
			_, err = fx.db.Pool().Exec(fx.ctx, `INSERT INTO billing.products(merchant_id,id,key,display_name,tier_group) VALUES($1,$2,$2::uuid::text,'Other tier',$3)`, mid, other, group)
			require.NoError(t, err)
			_, err = fx.db.Pool().Exec(fx.ctx, `INSERT INTO billing.prices(merchant_id,id,product_id,amount,currency,auto_renew,access_duration_hours) VALUES($1,$2,$3,9990000,'USD',true,720)`, mid, otherPrice, other)
			require.NoError(t, err)
			params := intents.EnqueueParams{MerchantID: mid, Provider: "nmi", IntentType: TypeInitialMembership, PriceID: &fx.priceID, PspID: terms.PSPID, Payload: fx.payload, IdempotencyKey: InitialMembershipIdempotencyKey(fx.payload.CheckoutIdempotencyKey), NextAttemptAt: time.Now(), Origin: intents.OriginUser}
			if state == "unresolved_enrollment" {
				otherPayload := fx.payload
				otherPayload.Terms.ProductID = other
				otherPayload.Terms.PriceID = otherPrice
				otherPayload.Terms.SubscriptionID = uuid.New()
				otherPayload.Terms.PaymentID = uuid.New()
				otherPayload.CheckoutIdempotencyKey = "other-" + uuid.NewString()
				otherParams := params
				otherParams.PriceID = &otherPrice
				otherParams.Payload = otherPayload
				otherParams.IdempotencyKey = InitialMembershipIdempotencyKey(otherPayload.CheckoutIdempotencyKey)
				accepted, err := intents.NewStore(fx.db).Enqueue(fx.ctx, otherParams)
				require.NoError(t, err)
				t.Cleanup(func() {
					_, err := fx.db.Pool().Exec(fx.ctx, `DELETE FROM billing.rail_intents WHERE id=$1`, accepted.ID)
					require.NoError(t, err)
					_, err = fx.db.Pool().Exec(fx.ctx, `DELETE FROM billing.prices WHERE id=$1`, otherPrice)
					require.NoError(t, err)
					_, err = fx.db.Pool().Exec(fx.ctx, `DELETE FROM billing.products WHERE id=$1`, other)
					require.NoError(t, err)
				})
				_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE billing.rail_intents SET status='unknown_needs_verify' WHERE id=$1`, accepted.ID)
				require.NoError(t, err)
			} else {
				_, err = fx.db.Pool().Exec(fx.ctx, `INSERT INTO billing.subscriptions(merchant_id,customer_id,product_id,price_id,psp_id,rail,rail_subscription_id,status,current_period_starts_at,current_period_ends_at) VALUES($1,$2,$3,$4,$5,'nmi',$7,$6,now(),now()+interval '30 days')`, mid, terms.CustomerID, other, otherPrice, terms.PSPID, state, "untouched-"+other.String())
				require.NoError(t, err)
			}
			_, err = intents.NewStore(fx.db).Enqueue(fx.ctx, params)
			require.ErrorContains(t, err, "tier group")
			var accepted int
			require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.rail_intents WHERE price_id=$1`, fx.priceID).Scan(&accepted))
			require.Zero(t, accepted, "refusal precedes any accepted payment/provider work")
		})
	}
}
