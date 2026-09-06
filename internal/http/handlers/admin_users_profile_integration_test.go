//go:build integration

package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
)

func serveAdminUser(fx *findingsFixture, handler func(*httprequest.Request)) *httptest.ResponseRecorder {
	fx.t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/customers/"+fx.customer.String(), nil).WithContext(fx.ctx)
	req.SetPathValue("customer_id", fx.customer.String())
	rec := httptest.NewRecorder()
	hr := httprequest.NewHTTP(rec, req, fx.rt)
	hr.SetUserContext(billingauth.UserContext{UserID: fx.customer.String()})
	handler(hr)
	return rec
}

func seedSubscriptionWithStatus(fx *findingsFixture, productID, priceID uuid.UUID, status string) uuid.UUID {
	fx.t.Helper()
	subID := uuid.New()
	now := time.Now().UTC()
	var cancelType *string
	var cancelledAt *time.Time
	if status == "cancelled" {
		ct := "user"
		cancelType = &ct
		cancelledAt = &now
	}
	fx.exec(`INSERT INTO openrails.subscriptions
	          (id, price_id, product_id, status, rail, rail_subscription_id,
	           current_period_starts_at, current_period_ends_at, started_at,
	           cancel_type, cancelled_at, customer_id, merchant_id, psp_id)
	        VALUES ($1, $2, $3, $4, 'nmi', $5, $6, $7, $6, $8, $9, $10, $11, $12)`,
		subID, priceID, productID, status, "profile-"+uuid.NewString(),
		now.Add(-24*time.Hour), now.Add(29*24*time.Hour), cancelType, cancelledAt,
		fx.customer, fx.merchant, fx.pspFor("nmi"))
	return subID
}

// A failing collection-defaults loader must not 500 the composite admin
// profile: the other sections still arrive and the payment methods come back
// without collection_default_currencies. The dedicated payment-methods
// endpoint, where the defaults are the point, keeps its hard failure.
func TestAdminUserBillingProfile_DegradesWhenCollectionDefaultsFail(t *testing.T) {
	fx := newPaymentDefaultsFixture(t)
	payer := identity.CustomerID(fx.customer)
	pm := seedDefaultMethod(t, fx, fx.customer)
	require.NoError(t, fx.rt.MoneyService.SetInvoiceCollectionPaymentMethod(fx.ctx, payer, "USD", pm.ID))
	fx.seedActiveSubscription("profile-degrade-" + uuid.NewString())
	_, err := fx.rt.MoneyService.Deposit(fx.ctx, money.DepositParams{
		CustomerID: &payer,
		Invoker:    fx.customer.String(),
		Currency:   money.DefaultCurrency,
		Amount:     1200,
		Source:     "profile-degrade-test",
	})
	require.NoError(t, err)

	// Sanity: with a healthy loader the profile carries the default.
	healthy := readDefaultMethods(t, fx, fx.customer, GetAdminUserBillingProfile)
	require.Equal(t, []string{"USD"}, healthy[api.FormatPaymentMethodID(pm.ID)])

	original := loadCollectionPaymentMethodDefaults
	t.Cleanup(func() { loadCollectionPaymentMethodDefaults = original })
	loadCollectionPaymentMethodDefaults = func(*httprequest.Request, identity.CustomerID) (map[uuid.UUID][]string, error) {
		return nil, errors.New("relation openrails.money_settings does not exist")
	}

	dedicated := serveAdminUser(fx, GetAdminUserPaymentMethods)
	require.Equal(t, http.StatusInternalServerError, dedicated.Code, dedicated.Body.String())

	rec := serveAdminUser(fx, GetAdminUserBillingProfile)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var profile adminUserBillingProfile
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &profile), rec.Body.String())
	require.Equal(t, fx.customer.String(), profile.CustomerID)
	require.Len(t, profile.PaymentMethods, 1)
	require.Equal(t, api.FormatPaymentMethodID(pm.ID), profile.PaymentMethods[0].ID)
	require.Empty(t, profile.PaymentMethods[0].CollectionDefaultCurrencies, "defaults are dropped, not invented")
	require.Len(t, profile.Subscriptions, 1, "subscriptions section survives the defaults failure")
	require.Len(t, profile.CreditBalance, 1, "credit balance section survives the defaults failure")
	require.EqualValues(t, 1200, profile.CreditBalance[0].Balance)
}

// The profile is an admin 360: every non-deleted subscription appears with
// its status, not only the active ones.
func TestAdminUserBillingProfile_ListsSubscriptionsOfEveryStatus(t *testing.T) {
	fx := newPaymentDefaultsFixture(t)
	pastDue := seedSubscriptionWithStatus(fx, fx.product, fx.price, "past_due")
	product2, price2 := fx.seedSecondProduct()
	cancelled := seedSubscriptionWithStatus(fx, product2, price2, "cancelled")
	product3, price3 := fx.seedSecondProduct()
	pending := seedSubscriptionWithStatus(fx, product3, price3, "pending")
	product4, price4 := fx.seedSecondProduct()
	deleted := seedSubscriptionWithStatus(fx, product4, price4, "cancelled")
	fx.exec(`UPDATE openrails.subscriptions SET deleted_at = now() WHERE id = $1`, deleted)

	rec := serveAdminUser(fx, GetAdminUserBillingProfile)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var profile adminUserBillingProfile
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &profile), rec.Body.String())

	statusByID := map[uuid.UUID]models.SubscriptionStatus{}
	for _, sub := range profile.Subscriptions {
		statusByID[sub.ID] = sub.Status
	}
	require.Len(t, statusByID, 3, "three live rows, the soft-deleted one excluded: %s", rec.Body.String())
	require.Equal(t, models.StatusPastDue, statusByID[pastDue])
	require.Equal(t, models.StatusCancelled, statusByID[cancelled])
	require.Equal(t, models.StatusPending, statusByID[pending])
	require.NotContains(t, statusByID, deleted)
}
