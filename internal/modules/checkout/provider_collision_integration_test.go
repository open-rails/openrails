//go:build integration

package checkout

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/stretchr/testify/require"
)

func TestCheckoutSubscriptionAdoptionIgnoresSiblingPSPCollision(t *testing.T) {
	fixture := newSubIntentFixture(t)
	siblingID, siblingSubID := uuid.New(), uuid.New()
	_, err := fixture.db.Qx(fixture.ctx).Exec(fixture.ctx, `INSERT INTO billing.psps (id,merchant_id,rail,environment,account_id,key) VALUES ($1,$2,'nmi','test',$3,$3)`, siblingID, dbtest.TestMerchantID.UUID(), uuid.NewString())
	require.NoError(t, err)
	siblingCustomer := dbtest.EnsureCustomerIDPgx(fixture.ctx, t, fixture.db.Pool(), uuid.NewString())
	price, err := fixture.svc.PriceService.GetByID(fixture.ctx, fixture.priceID)
	require.NoError(t, err)
	// This row deliberately owns the exact identifier returned by the selected
	// account's real HTTP test gateway. It must never be adopted or activated.
	_, err = fixture.db.Qx(fixture.ctx).Exec(fixture.ctx, `INSERT INTO billing.subscriptions (id,merchant_id,customer_id,product_id,price_id,rail,psp_id,rail_subscription_id,status) VALUES ($1,$2,$3,$4,$5,'nmi',$6,$7,'pending')`, siblingSubID, dbtest.TestMerchantID.UUID(), siblingCustomer, price.ProductID, price.ID, siblingID, fixture.gateway.subID)
	require.NoError(t, err)
	intent := fixture.enqueueAndExecute(t)
	require.Equal(t, intents.StatusSucceeded, intent.Status)
	require.EqualValues(t, 1, fixture.gateway.createCalls.Load())
	selected, err := fixture.svc.SubscriptionService.GetByPSPSubscriptionID(db.WithPSPID(fixture.ctx, *intent.PspID), "nmi", fixture.gateway.subID)
	require.NoError(t, err)
	require.Equal(t, models.StatusActive, selected.Status)
	require.NotEqual(t, siblingSubID, selected.ID)
	require.Equal(t, *intent.PspID, selected.PspID)
	sibling, err := fixture.svc.SubscriptionService.GetByPSPSubscriptionID(db.WithPSPID(fixture.ctx, siblingID), "nmi", fixture.gateway.subID)
	require.NoError(t, err)
	require.Equal(t, models.StatusPending, sibling.Status)
	var siblingPayments int
	require.NoError(t, fixture.db.Qx(fixture.ctx).QueryRow(fixture.ctx, `SELECT count(*) FROM billing.payments WHERE psp_id=$1`, siblingID).Scan(&siblingPayments))
	require.Zero(t, siblingPayments)
}
