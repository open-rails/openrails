package checkout

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// subscriptionRows answers the guard's queries the way the repo does:
// sql.ErrNoRows when nothing matches.
type subscriptionRows struct {
	byPrice            map[uuid.UUID]*models.Subscription
	byTierGroup        map[string]*models.Subscription
	unknownByProduct   map[uuid.UUID]*models.Subscription
	unknownByTierGroup map[string]*models.Subscription
}

func lookupRow[K comparable](m map[K]*models.Subscription, k K) (*models.Subscription, error) {
	if s, ok := m[k]; ok {
		return s, nil
	}
	return nil, sql.ErrNoRows
}

func (r subscriptionRows) GetActiveOrPendingByUserIDAndTierGroup(_ context.Context, _, g string) (*models.Subscription, error) {
	return lookupRow(r.byTierGroup, g)
}
func (r subscriptionRows) GetActiveOrPendingByUserIDAndProductID(context.Context, string, uuid.UUID) (*models.Subscription, error) {
	return nil, sql.ErrNoRows
}
func (r subscriptionRows) GetByUserIDAndPriceID(_ context.Context, _ string, id uuid.UUID) (*models.Subscription, error) {
	return lookupRow(r.byPrice, id)
}
func (r subscriptionRows) GetUnknownByUserIDAndProductID(_ context.Context, _ string, id uuid.UUID) (*models.Subscription, error) {
	return lookupRow(r.unknownByProduct, id)
}
func (r subscriptionRows) GetUnknownByUserIDAndTierGroup(_ context.Context, _, g string) (*models.Subscription, error) {
	return lookupRow(r.unknownByTierGroup, g)
}

func tierProduct(group string, rank int) *models.Product {
	return &models.Product{ID: uuid.New(), DisplayName: "tier", TierGroup: &group, TierRank: rank}
}

func heldSub(status models.SubscriptionStatus, p *models.Product) *models.Subscription {
	priceID := uuid.New()
	return &models.Subscription{ID: uuid.New(), Status: status, PriceID: priceID, Price: &models.Price{ID: priceID, Product: p}}
}

// #269/#691: a second subscribe in the same price or tier group is blocked
// before any charge; an `unknown` membership blocks with the verification
// code because it may still be billing at the provider.
func TestDuplicateBillingGuard(t *testing.T) {
	const group = "plans"
	held := tierProduct(group, 1)
	heldPrice := &models.Price{ID: uuid.New(), ProductID: held.ID, Product: held}
	higher := tierProduct(group, 2)
	higherPrice := &models.Price{ID: uuid.New(), ProductID: higher.ID, Product: higher}
	other := tierProduct("addons", 1)
	otherPrice := &models.Price{ID: uuid.New(), ProductID: other.ID, Product: other}

	for _, tc := range []struct {
		name      string
		rows      subscriptionRows
		price     *models.Price
		product   *models.Product
		blocked   bool
		code      string
		samePrice bool
	}{
		{"first subscribe", subscriptionRows{}, higherPrice, higher, false, "", false},
		{"same exact price", subscriptionRows{byPrice: map[uuid.UUID]*models.Subscription{heldPrice.ID: heldSub(models.StatusPastDue, held)}}, heldPrice, held, true, ConflictCodeDuplicateSubscription, true},
		{"cancelled exact price allows resubscribe", subscriptionRows{byPrice: map[uuid.UUID]*models.Subscription{heldPrice.ID: heldSub(models.StatusCancelled, held)}}, heldPrice, held, false, "", false},
		{"same product other price in group", subscriptionRows{byTierGroup: map[string]*models.Subscription{group: heldSub(models.StatusActive, held)}}, &models.Price{ID: uuid.New(), ProductID: held.ID}, held, true, ConflictCodeDuplicateSubscription, false},
		{"other tier in group", subscriptionRows{byTierGroup: map[string]*models.Subscription{group: heldSub(models.StatusActive, held)}}, higherPrice, higher, true, ConflictCodeChangeTierRequired, false},
		{"different group", subscriptionRows{byTierGroup: map[string]*models.Subscription{group: heldSub(models.StatusActive, held)}}, otherPrice, other, false, "", false},
		{"unknown sub on product", subscriptionRows{unknownByProduct: map[uuid.UUID]*models.Subscription{held.ID: heldSub(models.StatusUnknown, held)}}, heldPrice, held, true, ConflictCodeMembershipPendingVerification, false},
		{"unknown sub in group", subscriptionRows{unknownByTierGroup: map[string]*models.Subscription{group: heldSub(models.StatusUnknown, held)}}, higherPrice, higher, true, ConflictCodeMembershipPendingVerification, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (&CheckoutPurchaseService{SubscriptionService: tc.rows}).CheckSubscriptionConflict(context.Background(), "user-1", tc.price, tc.product)
			require.NoError(t, err)
			require.Equal(t, tc.blocked, got.Blocked)
			require.Equal(t, tc.code, got.Code)
			require.Equal(t, tc.samePrice, got.SamePrice)
		})
	}

	// Direction is relative rank, not sign or magnitude.
	for _, tc := range []struct {
		held, target int
		want         string
	}{{10, 20, "tier upgrades"}, {20, 10, "tier downgrades"}, {-1, 0, "tier upgrades"}, {0, -1, "tier downgrades"}} {
		h, target := tierProduct(group, tc.held), tierProduct(group, tc.target)
		got, err := (&CheckoutPurchaseService{SubscriptionService: subscriptionRows{byTierGroup: map[string]*models.Subscription{group: heldSub(models.StatusActive, h)}}}).
			CheckSubscriptionConflict(context.Background(), "u", &models.Price{ID: uuid.New(), ProductID: target.ID}, target)
		require.NoError(t, err)
		require.Contains(t, got.Message, tc.want)
	}

	_, err := (&CheckoutPurchaseService{SubscriptionService: subscriptionRows{}}).CheckSubscriptionConflict(context.Background(), "u", nil, held)
	require.Error(t, err)
}

type stripeCustomers struct {
	local, search string
	subs          []subscriptions.StripeSubscriptionSummary
	listErr       error
	created       int
}

func (s *stripeCustomers) GetCustomerID(context.Context, string, string) (string, error) {
	if s.local == "" {
		return "", sql.ErrNoRows
	}
	return s.local, nil
}
func (s *stripeCustomers) Upsert(context.Context, string, string, string) error { return nil }
func (s *stripeCustomers) FindCustomerIDByAppUserID(context.Context, string) (string, error) {
	return s.search, nil
}
func (s *stripeCustomers) CreateCustomer(context.Context, string, string) (string, error) {
	s.created++
	return "cus_new", nil
}
func (s *stripeCustomers) ListActiveSubscriptionsForCustomer(context.Context, string) ([]subscriptions.StripeSubscriptionSummary, error) {
	return s.subs, s.listErr
}

type stripeCatalog map[string]*models.Product

func (c stripeCatalog) GetByStripePriceID(_ context.Context, id string) (*models.Price, error) {
	if p, ok := c[id]; ok {
		return &models.Price{ProductID: p.ID}, nil
	}
	return nil, sql.ErrNoRows
}
func (c stripeCatalog) GetByID(_ context.Context, id uuid.UUID) (*models.Product, error) {
	for _, p := range c {
		if p.ID == id {
			return p, nil
		}
	}
	return nil, sql.ErrNoRows
}

// #213: Stripe itself is asked, so a missed webhook cannot admit a parallel
// subscription; the probe never creates a customer.
func TestStripeTierGroupConflict(t *testing.T) {
	catalog := stripeCatalog{"price_premium": tierProduct("premium", 1), "price_addon": tierProduct("addon", 1)}
	active := func(price string) []subscriptions.StripeSubscriptionSummary {
		return []subscriptions.StripeSubscriptionSummary{{ID: "sub_1", Status: "active", PriceID: price}}
	}
	for _, tc := range []struct {
		name    string
		c       *stripeCustomers
		blocked bool
	}{
		{"same group", &stripeCustomers{local: "cus_1", subs: active("price_premium")}, true},
		{"found by search", &stripeCustomers{search: "cus_1", subs: active("price_premium")}, true},
		{"other group", &stripeCustomers{local: "cus_1", subs: active("price_addon")}, false},
		{"unmapped price is skipped", &stripeCustomers{local: "cus_1", subs: active("price_manual")}, false},
		{"no customer anywhere", &stripeCustomers{subs: active("price_premium")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocked, err := stripeTierGroupConflictWith(context.Background(), tc.c, tc.c, catalog, catalog, &UserIdentity{ID: "u"}, " premium ")
			require.NoError(t, err)
			require.Equal(t, tc.blocked, blocked)
			require.Zero(t, tc.c.created)
		})
	}
	_, err := stripeTierGroupConflictWith(context.Background(), &stripeCustomers{local: "cus_1", listErr: errors.New("stripe down")}, &stripeCustomers{local: "cus_1", listErr: errors.New("stripe down")}, catalog, catalog, &UserIdentity{ID: "u"}, "premium")
	require.ErrorContains(t, err, "list stripe subscriptions", "an outage is not an absence")
}

func TestTierChangeAdmission(t *testing.T) {
	for _, tc := range []struct {
		sub  models.Subscription
		want int
	}{
		{models.Subscription{Status: models.StatusActive, Rail: models.RailStripe}, 0},
		{models.Subscription{Status: models.StatusPastDue, Rail: models.RailStripe}, 0},
		{models.Subscription{Status: models.StatusActive, Rail: models.RailNMI, CollectionPolicy: models.CollectionPolicyEngine}, 0},
		{models.Subscription{Status: models.StatusPending, Rail: models.RailStripe}, http.StatusConflict},
		{models.Subscription{Status: models.StatusCancelled, Rail: models.RailStripe}, http.StatusConflict},
	} {
		err := validateTierChangeSubscriptionStatus(&tc.sub)
		if tc.want == 0 {
			require.NoError(t, err, "%+v", tc.sub)
			continue
		}
		var tierErr *TierChangeError
		require.ErrorAs(t, err, &tierErr)
		require.Equal(t, tc.want, tierErr.HTTPStatus)
	}

	ctx := merchantCtx()
	configured, bare := &models.Price{}, &models.Price{}
	configured.SetStripeConfig("price_target")
	mobius := merchants.PSPScope{ID: uuid.New(), Rail: "nmi", AccountID: "gw-1", Key: "mobius"}
	nmiSvc := &CheckoutService{ProviderSecrets: pspCatalog{scopes: []merchants.PSPScope{mobius}}}
	otherPlan := &models.Price{ID: uuid.New(), PSPLinks: map[string]map[string]string{"paykings": {models.RailKeyRail: "nmi", models.RailKeyPlanID: "plan_paykings"}}}
	for _, tc := range []struct {
		name          string
		svc           *CheckoutService
		sub           *models.Subscription
		current, next *models.Price
		action, want  string
	}{
		{"stripe target configured", &CheckoutService{}, &models.Subscription{Rail: models.RailStripe}, configured, configured, "downgrade", ""},
		{"stripe target missing", &CheckoutService{}, &models.Subscription{Rail: models.RailStripe}, configured, bare, "upgrade", "target price not configured for Stripe"},
		{"ccbill downgrade", &CheckoutService{}, &models.Subscription{Rail: models.RailCCBill}, bare, bare, "downgrade", "downgrades are not supported"},
		{"nmi plan of another provider", nmiSvc, &models.Subscription{Rail: models.RailNMI, PspID: mobius.ID}, bare, otherPlan, "upgrade", "missing NMI plan configuration for payment provider mobius"},
		{"solana target without plan", &CheckoutService{}, &models.Subscription{Rail: models.RailSolana}, bare, bare, "upgrade", "Solana recurring"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.svc.validateTierChangePreviewTarget(ctx, tc.sub, tc.current, tc.next, &UserIdentity{}, tc.action)
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.want)
		})
	}
}

// New subscriptions exist only through a saved-method session with explicit
// agreement confirmation; no rail falls back to provider enrollment.
func TestLegacySubscriptionEnrollmentIsClosed(t *testing.T) {
	for _, rail := range []string{"stripe", "nmi", "ccbill", "solana"} {
		_, err := (&CheckoutService{}).processSubscription(context.Background(), nil, nil, nil, nil, nil, rail)
		require.ErrorContains(t, err, "saved-method checkout session and explicit agreement confirmation")
	}
}
