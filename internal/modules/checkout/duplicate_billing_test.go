package checkout

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

func tierProduct(group string, rank int) *models.Product {
	return &models.Product{ID: uuid.New(), DisplayName: "tier", TierGroup: &group, TierRank: rank}
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

// Stripe itself is asked, so a missed webhook cannot admit a parallel
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
		want string
	}{
		{models.Subscription{Status: models.StatusActive, Rail: models.RailStripe}, ""},
		{models.Subscription{Status: models.StatusPastDue, Rail: models.RailStripe}, ""},
		{models.Subscription{Status: models.StatusActive, Rail: models.RailNMI, CollectionPolicy: models.CollectionPolicyEngine}, ""},
		{models.Subscription{Status: models.StatusPending, Rail: models.RailStripe}, codeSubscriptionNotActive},
		{models.Subscription{Status: models.StatusCanceled, Rail: models.RailStripe}, codeSubscriptionNotActive},
	} {
		err := validateTierChangeSubscriptionStatus(&tc.sub)
		if tc.want == "" {
			require.NoError(t, err, "%+v", tc.sub)
			continue
		}
		var tierErr *TierChangeError
		require.ErrorAs(t, err, &tierErr)
		require.Equal(t, tc.want, tierErr.Code)
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
		require.ErrorContains(t, err, "saved-method checkout attempt and explicit agreement confirmation")
	}
}
