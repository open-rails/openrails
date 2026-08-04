//go:build integration

package checkout

import (
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
)

// #297 checkout proof: the initial cardholder-present sale is the unscheduled
// sequence's initial CIT (indicator=stored, no reference) and finalize
// persists the returned transaction id as the instrument's anchor; a checkout
// on an already-anchored instrument sends indicator=used + the anchor.

// seedSaleInstrument stores a payment_methods row matching the fixture
// payload's rail handles, with the given unscheduled anchor ("" = legacy/new).
func (fx *saleIntentFixture) seedSaleInstrument(t *testing.T, unscheduledRef string) uuid.UUID {
	t.Helper()
	pool := fx.db.Pool()
	customerID := dbtest.EnsureCustomerIDPgx(fx.ctx, t, pool, fx.userID)
	pmID := uuid.New()
	_, err := pool.Exec(fx.ctx,
		`INSERT INTO openrails.payment_methods
		   (id, merchant_id, customer_id, rail, rail_customer_ref, rail_method_ref,
		    initial_transaction_id, stored_credential_unscheduled_ref)
		 VALUES ($1, $2, $3, 'mobius', $4, '', '', $5)`,
		pmID, dbtest.TestMerchantID.UUID(), customerID, fx.payload.CustomerVaultID, unscheduledRef)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(fx.ctx, "DELETE FROM openrails.payment_methods WHERE id = $1", pmID)
	})
	return pmID
}

func (fx *saleIntentFixture) unscheduledRef(t *testing.T, pmID uuid.UUID) string {
	t.Helper()
	var ref string
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx,
		"SELECT stored_credential_unscheduled_ref FROM openrails.payment_methods WHERE id = $1", pmID).Scan(&ref))
	return ref
}

func TestNMISaleIntent_InitialCITAnchorsInstrument(t *testing.T) {
	fx := newSaleIntentFixture(t)
	pmID := fx.seedSaleInstrument(t, "")

	intent := fx.enqueueAndExecute(t, "sale-297-"+uuid.NewString()[:8])
	require.Equal(t, intents.StatusSucceeded, intent.Status)

	form := fx.gateway.saleForm.Load().(url.Values)
	assert.Equal(t, "customer", form.Get("initiated_by"))
	assert.Equal(t, "stored", form.Get("stored_credential_indicator"), "first charge on an unanchored card is the sequence's initial CIT")
	_, hasInitial := form["initial_transaction_id"]
	assert.False(t, hasInitial)
	_, hasBillingMethod := form["billing_method"]
	assert.False(t, hasBillingMethod, "one-time checkout is unscheduled CoF: no billing_method")

	assert.Equal(t, fx.gateway.txnID, fx.unscheduledRef(t, pmID), "finalize persists the CIT transaction id as the anchor")
}

func TestNMISaleIntent_AnchoredInstrumentSendsUsedWithReference(t *testing.T) {
	fx := newSaleIntentFixture(t)
	pmID := fx.seedSaleInstrument(t, "anchor-297-uns")
	fx.payload.StoredCredentialRef = "anchor-297-uns"

	intent := fx.enqueueAndExecute(t, "sale-297-"+uuid.NewString()[:8])
	require.Equal(t, intents.StatusSucceeded, intent.Status)

	form := fx.gateway.saleForm.Load().(url.Values)
	assert.Equal(t, "customer", form.Get("initiated_by"))
	assert.Equal(t, "used", form.Get("stored_credential_indicator"))
	assert.Equal(t, "anchor-297-uns", form.Get("initial_transaction_id"))

	assert.Equal(t, "anchor-297-uns", fx.unscheduledRef(t, pmID), "existing anchor is never overwritten")
}

// #297 enrollment proof: the subscription create is the RECURRING sequence's
// initial CIT (billing_method=recurring + initiated_by=customer +
// stored_credential_indicator=stored — the portal's MUST-INCLUDE trio) and
// finalize persists the first-charge transaction id as the recurring anchor.

func (fx *subIntentFixture) seedSubInstrument(t *testing.T, recurringRef string) uuid.UUID {
	t.Helper()
	pool := fx.db.Pool()
	customerID := dbtest.EnsureCustomerIDPgx(fx.ctx, t, pool, fx.payload.UserID)
	pmID := uuid.New()
	_, err := pool.Exec(fx.ctx,
		`INSERT INTO openrails.payment_methods
		   (id, merchant_id, customer_id, rail, rail_customer_ref, rail_method_ref,
		    initial_transaction_id, stored_credential_recurring_ref)
		 VALUES ($1, $2, $3, 'mobius', $4, '', '', $5)`,
		pmID, dbtest.TestMerchantID.UUID(), customerID, fx.payload.CustomerVaultID, recurringRef)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pool.Exec(fx.ctx, "DELETE FROM openrails.payment_methods WHERE id = $1", pmID)
	})
	return pmID
}

func TestNMISubscriptionIntent_InitialRecurringCITAnchorsInstrument(t *testing.T) {
	fx := newSubIntentFixture(t)
	// Wire the DB handle finalize persists the anchor through.
	fx.svc.RailPaymentMethodService = &paymentmethods.RailPaymentMethodService{DB: fx.db}
	pmID := fx.seedSubInstrument(t, "")

	intent := fx.enqueueAndExecute(t)
	require.Equal(t, intents.StatusSucceeded, intent.Status)

	form := fx.gateway.createForm.Load().(url.Values)
	assert.Equal(t, "recurring", form.Get("billing_method"))
	assert.Equal(t, "customer", form.Get("initiated_by"))
	assert.Equal(t, "stored", form.Get("stored_credential_indicator"))
	_, hasInitial := form["initial_transaction_id"]
	assert.False(t, hasInitial)

	var ref string
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx,
		"SELECT stored_credential_recurring_ref FROM openrails.payment_methods WHERE id = $1", pmID).Scan(&ref))
	assert.Equal(t, fx.gateway.txnID, ref, "finalize persists the enrollment charge as the recurring anchor")
}

func TestNMISubscriptionIntent_AnchoredInstrumentSendsUsedWithReference(t *testing.T) {
	fx := newSubIntentFixture(t)
	fx.svc.RailPaymentMethodService = &paymentmethods.RailPaymentMethodService{DB: fx.db}
	fx.seedSubInstrument(t, "anchor-297-sub")
	fx.payload.StoredCredentialRef = "anchor-297-sub"

	intent := fx.enqueueAndExecute(t)
	require.Equal(t, intents.StatusSucceeded, intent.Status)

	form := fx.gateway.createForm.Load().(url.Values)
	assert.Equal(t, "recurring", form.Get("billing_method"))
	assert.Equal(t, "customer", form.Get("initiated_by"))
	assert.Equal(t, "used", form.Get("stored_credential_indicator"))
	assert.Equal(t, "anchor-297-sub", form.Get("initial_transaction_id"))
}
