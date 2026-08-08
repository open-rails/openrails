package checkout

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
)

// routingFixturePrice is a recurring price linked on all four rails.
func routingFixturePrice() *models.Price {
	return &models.Price{
		ID:        uuid.New(),
		Key:       "pro-monthly",
		Currency:  "usd",
		AutoRenew: true,
		PSPLinks: map[string]map[string]string{
			"stripe": {models.RailKeyRail: "stripe", models.RailKeyStripePriceID: "price_test"},
			"nmi":    {models.RailKeyRail: "nmi", models.RailKeyPlanID: "plan_test"},
			"ccbill": {
				models.RailKeyRail:           "ccbill",
				models.RailKeyCCBillFormName: "form_test",
				models.RailKeyCCBillFlexID:   "flex_test",
			},
		},
	}
}

func routingFixtureService(armed railresolve.FixedSet) *CheckoutSessionService {
	checkoutService := &CheckoutService{
		Config:          fullModeConfigUnit(),
		Rails:           armed,
		ProviderSecrets: fakePSPCatalog{scopes: routingScopes(armed)},
	}
	return &CheckoutSessionService{config: fullModeConfigUnit(), checkoutService: checkoutService}
}

func routingContext() context.Context {
	return merchant.WithID(context.Background(), dbtest.TestMerchantID)
}

func routingScopes(armed railresolve.FixedSet) []merchants.PSPScope {
	scopes := make([]merchants.PSPScope, 0, len(armed))
	for key, processor := range armed {
		scopes = append(scopes, merchants.PSPScope{
			ID:          merchants.PspID(string(processor.Rail), "live", processor.AccountID),
			Rail:        string(processor.Rail),
			Environment: "live",
			AccountID:   processor.AccountID,
			Key:         key,
		})
	}
	return scopes
}

func fullModeConfigUnit() *config.Config {
	return &config.Config{ProviderWriteMode: config.ProviderWriteModeFull}
}

func routingArmedAll() railresolve.FixedSet {
	return railresolve.FixedSet{
		"stripe": {Rail: models.RailStripe, AccountID: "acct_stripe", Stripe: &config.StripeRailConfig{SecretKey: "sk_test_value"}},
		"nmi":    {Rail: models.RailNMI, AccountID: "acct_nmi", NMI: &config.NMIRailConfig{SecurityKey: "security_test"}},
		"ccbill": {Rail: models.RailCCBill, AccountID: "945280-0000", CCBill: &config.CCBillRailConfig{}},
	}
}

// The default policy IS today's hardcoded order. Pin it: a merchant that
// declares nothing must keep getting stripe, then nmi, then ccbill, then
// solana — no reordering may sneak in through a refactor.
func TestRouteDefaultOrderIsTodaysHardcodedOrder(t *testing.T) {
	t.Parallel()

	require.Equal(t, []string{"stripe", "nmi", "ccbill", "solana"}, defaultRoutingOrder)

	svc := routingFixtureService(routingArmedAll())
	decision, err := svc.Route(routingContext(), RoutingInput{
		Price: routingFixturePrice(),
		Mode:  models.CheckoutSessionModeSubscription,
	})
	require.NoError(t, err)
	assert.Equal(t, models.CheckoutRoutingPolicyDefault, decision.Policy)
	assert.Nil(t, decision.Rule)
	assert.Equal(t, "stripe", decision.Selected())
	assert.Equal(t, "stripe", decision.Target.Rail)
	assert.Equal(t, []string{"stripe", "nmi", "ccbill"}, decision.Eligible())
}

// The first eligible candidate wins; the unavailable ones ahead of it are
// skipped WITH their class, not silently dropped.
func TestRouteFallsThroughUnavailableCandidates(t *testing.T) {
	t.Parallel()

	armed := routingArmedAll()
	delete(armed, "stripe") // stripe not armed at all

	svc := routingFixtureService(armed)
	decision, err := svc.Route(routingContext(), RoutingInput{
		Price: routingFixturePrice(),
		Mode:  models.CheckoutSessionModeSubscription,
	})
	require.NoError(t, err)
	assert.Equal(t, "nmi", decision.Selected())

	reason := decision.Reason()
	require.NotNil(t, reason)
	assert.Equal(t, models.CheckoutRoutingPolicyDefault, reason.Policy)
	assert.Equal(t, "nmi", reason.Selected)
	assert.Equal(t, []string{"ccbill"}, reason.Fallbacks)
	assert.Equal(t, []models.CheckoutRoutingSkip{
		{Selector: "stripe", Reason: models.CheckoutRoutingSkipNotArmed},
		{Selector: "solana", Reason: models.CheckoutRoutingSkipNotArmed},
	}, reason.Skipped)
}

// A one-off price cannot go to CCBill (subscription-only) — the skip class says
// so rather than reporting a generic unavailability.
func TestRouteSkipsModeUnsupported(t *testing.T) {
	t.Parallel()

	price := routingFixturePrice()
	price.AutoRenew = false

	armed := railresolve.FixedSet{
		"ccbill": {Rail: models.RailCCBill, AccountID: "945280-0000", CCBill: &config.CCBillRailConfig{}},
		"nmi":    {Rail: models.RailNMI, AccountID: "acct_nmi", NMI: &config.NMIRailConfig{SecurityKey: "security_test"}},
	}
	svc := routingFixtureService(armed)
	decision, err := svc.Route(routingContext(), RoutingInput{
		Price: price,
		Mode:  models.CheckoutSessionModeOneOff,
	})
	require.NoError(t, err)
	assert.Equal(t, "nmi", decision.Selected())
	assert.Equal(t, []models.CheckoutRoutingSkip{
		{Selector: "stripe", Reason: models.CheckoutRoutingSkipNotArmed},
		{Selector: "ccbill", Reason: models.CheckoutRoutingSkipModeUnsupported},
		{Selector: "solana", Reason: models.CheckoutRoutingSkipNotArmed},
	}, decision.Reason().Skipped)
}

// Nothing eligible must fail closed, and still hand back the full trace so the
// operator can see WHY nothing was routable.
func TestRouteFailsClosedWithNoEligibleProcessor(t *testing.T) {
	t.Parallel()

	svc := routingFixtureService(railresolve.FixedSet{})
	decision, err := svc.Route(routingContext(), RoutingInput{
		Price: routingFixturePrice(),
		Mode:  models.CheckoutSessionModeSubscription,
	})
	require.ErrorIs(t, err, ErrNoRoutableProcessor)
	require.NotNil(t, decision)
	assert.Empty(t, decision.Selected())
	assert.Len(t, decision.Candidates, 4)
	assert.Nil(t, decision.Reason(), "no winner means no decision to record")
}

// An explicitly named PSP is used as named, with no eligibility sweep and no
// fallback: the browser has already committed to that PSP's flow.
func TestRouteExplicitSelectorNeverFallsBack(t *testing.T) {
	t.Parallel()

	svc := routingFixtureService(routingArmedAll())
	decision, err := svc.Route(routingContext(), RoutingInput{
		Price:    routingFixturePrice(),
		Mode:     models.CheckoutSessionModeSubscription,
		Selector: "ccbill",
	})
	require.NoError(t, err)
	assert.Equal(t, models.CheckoutRoutingPolicyExplicit, decision.Policy)
	assert.Equal(t, "ccbill", decision.Selected())
	assert.Empty(t, decision.Reason().Fallbacks)
	assert.Empty(t, decision.Reason().Skipped)
}

func TestRouteExplicitSelectorMustBeArmed(t *testing.T) {
	t.Parallel()

	armed := routingArmedAll()
	delete(armed, "ccbill")
	svc := routingFixtureService(armed)

	_, err := svc.Route(routingContext(), RoutingInput{
		Price:    routingFixturePrice(),
		Mode:     models.CheckoutSessionModeSubscription,
		Selector: "ccbill",
	})
	require.ErrorContains(t, err, "has no armed PSP")
}

func TestRoutingRuleMatches(t *testing.T) {
	t.Parallel()

	price := routingFixturePrice()
	product := &models.Product{Key: "pro"}
	in := RoutingInput{
		Price:   price,
		Product: product,
		Mode:    models.CheckoutSessionModeSubscription,
		Country: "US",
	}

	tests := []struct {
		name  string
		match models.CheckoutRoutingMatch
		want  bool
	}{
		{name: "catch-all", match: models.CheckoutRoutingMatch{}, want: true},
		{name: "currency hit", match: models.CheckoutRoutingMatch{Currency: "usd"}, want: true},
		{name: "currency miss", match: models.CheckoutRoutingMatch{Currency: "eur"}},
		{name: "product hit", match: models.CheckoutRoutingMatch{Product: "pro"}, want: true},
		{name: "product miss", match: models.CheckoutRoutingMatch{Product: "enterprise"}},
		{name: "price hit", match: models.CheckoutRoutingMatch{Price: "pro-monthly"}, want: true},
		{name: "mode hit", match: models.CheckoutRoutingMatch{Mode: "subscription"}, want: true},
		{name: "mode miss", match: models.CheckoutRoutingMatch{Mode: "one_off"}},
		{name: "country hit", match: models.CheckoutRoutingMatch{Country: "US"}, want: true},
		{name: "country miss", match: models.CheckoutRoutingMatch{Country: "DE"}},
		{name: "all conditions AND", match: models.CheckoutRoutingMatch{Currency: "usd", Product: "pro", Mode: "subscription"}, want: true},
		{name: "one condition fails the whole rule", match: models.CheckoutRoutingMatch{Currency: "usd", Product: "enterprise"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, routingRuleMatches(tt.match, in))
		})
	}
}
