package checkout

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
)

// pspCatalog is an in-memory Layer-B PSP catalog: key lookup, the #848
// rail-kind list, archived keys (or#288) and identity lookup.
type pspCatalog struct {
	scopes   []merchants.PSPScope
	archived []string
	err      error
}

func (c pspCatalog) ActivePSPSecretName(context.Context, merchant.ID, string, string, string) (string, bool, error) {
	return "", false, nil
}

func (c pspCatalog) PSPScopeByKey(_ context.Context, _ merchant.ID, key, _ string) (merchants.PSPScope, bool, error) {
	for _, s := range c.scopes {
		if strings.EqualFold(s.Key, key) {
			return s, true, c.err
		}
	}
	return merchants.PSPScope{}, false, c.err
}

func (c pspCatalog) ActivePSPScopesForRail(_ context.Context, _ merchant.ID, rail, _ string) ([]merchants.PSPScope, error) {
	var out []merchants.PSPScope
	for _, s := range c.scopes {
		if strings.EqualFold(s.Rail, rail) {
			out = append(out, s)
		}
	}
	return out, c.err
}

func (c pspCatalog) PSPKeyArchived(_ context.Context, _ merchant.ID, key, _ string) (bool, error) {
	for _, k := range c.archived {
		if k == key {
			return true, nil
		}
	}
	return false, nil
}

type keyOnlyCatalog struct{}

func (keyOnlyCatalog) ActivePSPSecretName(context.Context, merchant.ID, string, string, string) (string, bool, error) {
	return "", false, nil
}

func (keyOnlyCatalog) PSPScopeByKey(context.Context, merchant.ID, string, string) (merchants.PSPScope, bool, error) {
	return merchants.PSPScope{}, false, nil
}

var testMerchant = merchant.ID(uuid.MustParse("a5a5a5a5-0000-4000-8000-000000000001"))

func merchantCtx() context.Context { return merchant.WithID(context.Background(), testMerchant) }

func armedAll() railresolve.FixedSet {
	return railresolve.FixedSet{
		"stripe": {Rail: models.RailStripe, AccountID: "acct_stripe", Stripe: &config.StripeRailConfig{SecretKey: "sk_test_value"}},
		"nmi":    {Rail: models.RailNMI, AccountID: "acct_nmi", NMI: &config.NMIRailConfig{SecurityKey: "security_test"}},
		"ccbill": {Rail: models.RailCCBill, AccountID: "945280-0000", CCBill: &config.CCBillRailConfig{}},
	}
}

func scopesOf(armed railresolve.FixedSet) []merchants.PSPScope {
	out := make([]merchants.PSPScope, 0, len(armed))
	for key, p := range armed {
		out = append(out, merchants.PSPScope{ID: merchants.PspID(string(p.Rail), "live", p.AccountID), Rail: string(p.Rail), Environment: "live", AccountID: p.AccountID, Key: key})
	}
	return out
}

func routingService(armed railresolve.FixedSet, extra ...merchants.PSPScope) *CheckoutSessionService {
	return routingServiceWith(armed, pspCatalog{scopes: append(scopesOf(armed), extra...)})
}

func routingServiceWith(armed railresolve.FixedSet, catalog pspCatalog) *CheckoutSessionService {
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeFull}
	return &CheckoutSessionService{config: cfg, checkoutService: &CheckoutService{Config: cfg, Rails: armed, ProviderSecrets: catalog}}
}

func recurringPrice() *models.Price {
	return &models.Price{ID: uuid.New(), Key: "pro-monthly", Currency: "usd", Amount: 1_000_000, AutoRenew: true, AccessDurationHours: intPtr(720),
		PSPLinks: map[string]map[string]string{
			"stripe": {models.RailKeyRail: "stripe", models.RailKeyStripePriceID: "price_test"},
			"nmi":    {models.RailKeyRail: "nmi", models.RailKeyPlanID: "plan_test"},
			"ccbill": {models.RailKeyRail: "ccbill", models.RailKeyCCBillFormName: "form_test", models.RailKeyCCBillFlexID: "flex_test"},
		}}
}

// A selector is a declared PSP key or, only when unambiguous, a rail kind.
// Everything else fails closed with no scope (#848, or#893).
func TestResolveRailTarget(t *testing.T) {
	mobius := merchants.PSPScope{ID: uuid.New(), Rail: "nmi", AccountID: "gw-1", Key: "mobius"}
	paykings := merchants.PSPScope{ID: uuid.New(), Rail: "nmi", AccountID: "gw-2", Key: "paykings"}
	single := &CheckoutService{ProviderSecrets: pspCatalog{scopes: []merchants.PSPScope{mobius}}}
	double := &CheckoutService{ProviderSecrets: pspCatalog{scopes: []merchants.PSPScope{mobius, paykings}}}

	for _, tc := range []struct {
		name, selector string
		svc            *CheckoutService
		wantPSP        string
		wantID         uuid.UUID
	}{
		{"psp key", " MOBIUS ", single, "mobius", mobius.ID},
		{"single armed rail kind adopts its key", "nmi", single, "mobius", mobius.ID},
		{"psp key stays exact when its kind is ambiguous", "paykings", double, "paykings", paykings.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.svc.resolveRailTarget(merchantCtx(), tc.selector)
			require.NoError(t, err)
			require.Equal(t, tc.wantPSP, got.PSP)
			require.Equal(t, "nmi", got.Rail)
			require.Equal(t, tc.wantID, got.Scope.ID)
		})
	}

	_, err := double.resolveRailTarget(merchantCtx(), "nmi")
	var ambiguous *AmbiguousRailError
	require.ErrorAs(t, err, &ambiguous)
	require.ElementsMatch(t, []string{"mobius", "paykings"}, ambiguous.Keys)

	_, err = single.resolveRailTarget(merchantCtx(), "bogus")
	var unknown *UnknownRailError
	require.ErrorAs(t, err, &unknown)

	_, err = single.resolveRailTarget(merchantCtx(), "stripe")
	var unarmed *UnarmedRailError
	require.ErrorAs(t, err, &unarmed)

	for _, tc := range []struct {
		name string
		svc  *CheckoutService
		ctx  context.Context
		sel  string
		want string
	}{
		{"blank selector", single, merchantCtx(), "  ", "rail is required"},
		{"resolver not wired", &CheckoutService{}, merchantCtx(), "nmi", "resolution is not configured"},
		{"rail list capability missing", &CheckoutService{ProviderSecrets: keyOnlyCatalog{}}, merchantCtx(), "nmi", "rail resolution is not configured"},
		{"catalog fails", &CheckoutService{ProviderSecrets: pspCatalog{err: errors.New("catalog unavailable")}}, merchantCtx(), "nmi", "catalog unavailable"},
		{"no merchant on context", single, context.Background(), "nmi", "merchant"},
		{"scope without identity", &CheckoutService{ProviderSecrets: pspCatalog{scopes: []merchants.PSPScope{{Rail: "nmi", Key: "ghost"}}}}, merchantCtx(), "ghost", "identity is unavailable"},
		{"scope on unsupported rail", &CheckoutService{ProviderSecrets: pspCatalog{scopes: []merchants.PSPScope{{ID: uuid.New(), Rail: "paypal", Key: "pp"}}}}, merchantCtx(), "pp", "unsupported rail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.svc.resolveRailTarget(tc.ctx, tc.sel)
			require.ErrorContains(t, err, tc.want)
			require.Nil(t, got.Scope)
		})
	}

	// A resource-owning account is never swapped for a sibling on the same rail.
	got, err := double.resolveRailTargetForPSP(merchantCtx(), "nmi", paykings.ID)
	require.NoError(t, err)
	require.Equal(t, "paykings", got.PSP)
	_, err = double.resolveRailTargetForPSP(merchantCtx(), "nmi", uuid.New())
	require.ErrorContains(t, err, "is not armed")
	_, err = double.resolveRailTargetForPSP(merchantCtx(), "nmi", uuid.Nil)
	require.ErrorContains(t, err, "identity is unavailable")

	require.NoError(t, double.CheckoutRailUsable(merchantCtx(), "mobius"))
	require.ErrorContains(t, double.CheckoutRailUsable(merchantCtx(), "nmi"), "paykings")
	require.ErrorContains(t, double.CheckoutRailUsable(merchantCtx(), "solana"), "has no armed PSP")
}

// The default order is part of the contract: a merchant that declares
// nothing gets stripe, nmi, ccbill, solana, and every skipped candidate is
// reported with its class rather than dropped.
func TestRouteDefaultPolicy(t *testing.T) {
	require.Equal(t, []string{"stripe", "nmi", "ccbill", "solana"}, defaultRoutingOrder)

	decision, err := routingService(armedAll()).Route(merchantCtx(), RoutingInput{Price: recurringPrice(), Mode: models.CheckoutSessionModeSubscription})
	require.NoError(t, err)
	require.Equal(t, models.CheckoutRoutingPolicyDefault, decision.Policy)
	require.Equal(t, "stripe", decision.Selected())
	require.Equal(t, []string{"stripe", "nmi"}, decision.Eligible())
	reason := decision.Reason()
	require.Equal(t, []string{"nmi"}, reason.Fallbacks)
	require.Equal(t, []models.CheckoutRoutingSkip{
		{Selector: "ccbill", Reason: models.CheckoutRoutingSkipModeUnsupported},
		{Selector: "solana", Reason: models.CheckoutRoutingSkipNotArmed},
	}, reason.Skipped)

	armed := armedAll()
	delete(armed, "stripe")
	oneOff := recurringPrice()
	oneOff.AutoRenew = false
	decision, err = routingService(armed).Route(merchantCtx(), RoutingInput{Price: oneOff, Mode: models.CheckoutSessionModeOneOff})
	require.NoError(t, err)
	require.Equal(t, "nmi", decision.Selected())
	require.Empty(t, decision.Reason().Fallbacks)
	require.Equal(t, []models.CheckoutRoutingSkip{
		{Selector: "stripe", Reason: models.CheckoutRoutingSkipNotArmed},
		{Selector: "ccbill", Reason: models.CheckoutRoutingSkipModeUnsupported},
		{Selector: "solana", Reason: models.CheckoutRoutingSkipNotArmed},
	}, decision.Reason().Skipped)

	decision, err = routingService(railresolve.FixedSet{}).Route(merchantCtx(), RoutingInput{Price: recurringPrice(), Mode: models.CheckoutSessionModeSubscription})
	require.ErrorIs(t, err, ErrNoRoutableProcessor, "routing never invents a processor")
	require.Len(t, decision.Candidates, 4, "the full trace survives the failure")
	require.Nil(t, decision.Reason())
}

// A named PSP is used as named with no fallback, but must still be armed.
func TestRouteExplicitSelector(t *testing.T) {
	decision, err := routingService(armedAll()).Route(merchantCtx(), RoutingInput{Price: recurringPrice(), Mode: models.CheckoutSessionModeSubscription, Selector: " CCBill "})
	require.NoError(t, err)
	require.Equal(t, models.CheckoutRoutingPolicyExplicit, decision.Policy)
	require.Equal(t, "ccbill", decision.Selected())
	require.Empty(t, decision.Reason().Fallbacks)
	require.Empty(t, decision.Reason().Skipped)

	declared := merchants.PSPScope{ID: uuid.New(), Rail: "stripe", Environment: "live", AccountID: "acct_declared", Key: "stripe-declared"}
	decision, err = routingService(armedAll(), declared).Route(merchantCtx(), RoutingInput{Price: recurringPrice(), Selector: "stripe-declared"})
	require.ErrorIs(t, err, ErrNoRoutableProcessor)
	require.Empty(t, decision.Selected())
	require.Equal(t, models.CheckoutRoutingSkipNotArmed, decision.Candidates[0].Skip)

	armed := armedAll()
	delete(armed, "ccbill")
	_, err = routingService(armed).Route(merchantCtx(), RoutingInput{Price: recurringPrice(), Selector: "ccbill"})
	require.ErrorContains(t, err, "has no armed PSP")
}

// Resolution failures classify distinctly: support reads these classes.
func TestRouteCandidateSkipClasses(t *testing.T) {
	armed := armedAll()
	delete(armed, "nmi")
	svc := routingServiceWith(armed, pspCatalog{archived: []string{"retired"}, scopes: append(scopesOf(armed),
		merchants.PSPScope{ID: uuid.New(), Rail: "nmi", AccountID: "gw-1", Key: "mobius"},
		merchants.PSPScope{ID: uuid.New(), Rail: "nmi", AccountID: "gw-2", Key: "paykings"})})
	price := recurringPrice()
	for _, tc := range []struct{ selector, rail, want string }{
		{"nmi", "nmi", models.CheckoutRoutingSkipAmbiguousSelector},
		{"retired", "", models.CheckoutRoutingSkipNotArmed},
		{"never-declared", "", models.CheckoutRoutingSkipUnknownSelector},
		{"mobius", "nmi", models.CheckoutRoutingSkipNotArmed},
		{"stripe", "stripe", ""},
	} {
		target, skip := svc.evaluateCandidate(merchantCtx(), svc.checkoutService, RoutingInput{Price: price, Mode: models.CheckoutSessionModeSubscription}, tc.selector)
		require.Equal(t, tc.want, skip, tc.selector)
		require.Equal(t, tc.rail, target.Rail, "a bare rail kind names itself even when skipped: %s", tc.selector)
	}

	failing := routingServiceWith(armedAll(), pspCatalog{err: errors.New("catalog down")})
	_, skip := failing.evaluateCandidate(merchantCtx(), failing.checkoutService, RoutingInput{Price: price, Mode: models.CheckoutSessionModeSubscription}, "stripe")
	require.Equal(t, models.CheckoutRoutingSkipResolveFailed, skip)
	_, err := failing.listCheckoutRailOptionsForPrice(merchantCtx(), price, &models.Product{})
	require.ErrorContains(t, err, "resolution failed", "an outage is not an empty option list")
}

func TestRoutingRuleMatches(t *testing.T) {
	in := RoutingInput{Price: recurringPrice(), Product: &models.Product{Key: "pro"}, Mode: models.CheckoutSessionModeSubscription, Country: "US"}
	for _, tc := range []struct {
		name  string
		match models.CheckoutRoutingMatch
		want  bool
	}{
		{"catch-all", models.CheckoutRoutingMatch{}, true},
		{"currency case-insensitive", models.CheckoutRoutingMatch{Currency: " USD "}, true},
		{"currency miss", models.CheckoutRoutingMatch{Currency: "eur"}, false},
		{"product", models.CheckoutRoutingMatch{Product: "pro"}, true},
		{"price key", models.CheckoutRoutingMatch{Price: "pro-monthly"}, true},
		{"mode miss", models.CheckoutRoutingMatch{Mode: "one_off"}, false},
		{"country", models.CheckoutRoutingMatch{Country: "us"}, true},
		{"all set conditions must hold", models.CheckoutRoutingMatch{Currency: "usd", Product: "enterprise"}, false},
	} {
		require.Equal(t, tc.want, routingRuleMatches(tc.match, in), tc.name)
	}
	require.False(t, routingRuleMatches(models.CheckoutRoutingMatch{Product: "pro"}, RoutingInput{}), "absent product never matches a product rule")
}

// The option list is a projection of routing: exactly the eligible
// candidates, each executable through mode resolution and payment validation.
func TestListCheckoutRailOptions(t *testing.T) {
	svc := routingService(armedAll())
	price := recurringPrice()
	options, err := svc.listCheckoutRailOptionsForPrice(merchantCtx(), price, &models.Product{})
	require.NoError(t, err)
	require.Equal(t, []CheckoutRailOption{
		{Selector: "stripe", PSPID: merchants.PspID("stripe", "live", "acct_stripe"), Rail: "stripe", Mode: "subscription"},
		{Selector: "nmi", PSPID: merchants.PspID("nmi", "live", "acct_nmi"), Rail: "nmi", Mode: "subscription"},
	}, options)
	for _, o := range options {
		mode, err := svc.resolveMode(o.Mode, o.Selector, price)
		require.NoError(t, err)
		require.Equal(t, models.CheckoutSessionModeSubscription, mode)
		payment := &CheckoutSessionPaymentRequest{Rail: o.Selector}
		if o.Rail == "nmi" {
			payment.PaymentToken = "token_test"
		}
		require.NoError(t, svc.validatePayment(merchantCtx(), o.Selector, payment, &UserIdentity{ID: uuid.NewString()}))
	}

	empty, err := routingService(railresolve.FixedSet{}).listCheckoutRailOptionsForPrice(merchantCtx(), price, &models.Product{})
	require.NoError(t, err)
	require.Empty(t, empty)
}

// One readiness verdict backs both the option list and routing (or#288).
func TestCheckoutRailSkipReason(t *testing.T) {
	stripeCfg := &config.PSPConfig{Stripe: &config.StripeRailConfig{SecretKey: "sk_test"}}
	nmiCfg := &config.PSPConfig{NMI: &config.NMIRailConfig{SecurityKey: "key"}}
	solanaCfg := &config.PSPConfig{Solana: &config.SolanaRailConfig{Tokens: map[string]config.TokenConfig{"USDC": {Mint: "mint"}}}}
	sub := recurringPrice()
	trial := recurringPrice()
	trial.TrialUnitAmount = new(int64(0))
	noCycle := recurringPrice()
	noCycle.AccessDurationHours = nil
	free := recurringPrice()
	free.Amount = 0
	oneOff := &models.Price{ID: uuid.New(), Amount: 1_000_000, Currency: "USD"}
	solanaLinked := &models.Price{ID: uuid.New(), Amount: 1_000_000, Currency: "USD", PSPLinks: map[string]map[string]string{"solana": {models.RailKeyRail: "solana"}}}
	const S, O = models.CheckoutSessionModeSubscription, models.CheckoutSessionModeOneOff

	for _, tc := range []struct {
		name  string
		rail  string
		price *models.Price
		cfg   *config.PSPConfig
		mode  models.CheckoutSessionMode
		want  string
	}{
		{"stripe engine subscription needs no provider catalog", "stripe", sub, stripeCfg, S, ""},
		{"nmi engine subscription", "nmi", sub, nmiCfg, S, ""},
		{"stripe missing key", "stripe", sub, &config.PSPConfig{}, S, models.CheckoutRoutingSkipCredentialsMissing},
		{"nmi blank key", "nmi", sub, &config.PSPConfig{NMI: &config.NMIRailConfig{SecurityKey: "  "}}, S, models.CheckoutRoutingSkipCredentialsMissing},
		{"ccbill new enrollment", "ccbill", sub, &config.PSPConfig{CCBill: &config.CCBillRailConfig{}}, S, models.CheckoutRoutingSkipModeUnsupported},
		{"solana new enrollment", "solana", sub, solanaCfg, S, models.CheckoutRoutingSkipModeUnsupported},
		{"trial terms", "stripe", trial, stripeCfg, S, models.CheckoutRoutingSkipModeUnsupported},
		{"no cycle", "stripe", noCycle, stripeCfg, S, models.CheckoutRoutingSkipModeUnsupported},
		{"zero amount", "nmi", free, nmiCfg, S, models.CheckoutRoutingSkipModeUnsupported},
		{"stripe one-off is inline, no link", "stripe", oneOff, stripeCfg, O, ""},
		{"nmi one-off needs no plan (#1055)", "nmi", oneOff, nmiCfg, O, ""},
		{"nmi one-off missing key", "nmi", oneOff, &config.PSPConfig{}, O, models.CheckoutRoutingSkipCredentialsMissing},
		{"ccbill one-off", "ccbill", oneOff, &config.PSPConfig{CCBill: &config.CCBillRailConfig{}}, O, models.CheckoutRoutingSkipModeUnsupported},
		{"solana without tokens", "solana", solanaLinked, &config.PSPConfig{Solana: &config.SolanaRailConfig{}}, O, models.CheckoutRoutingSkipCredentialsMissing},
		{"solana without link", "solana", oneOff, solanaCfg, O, models.CheckoutRoutingSkipLinkMissing},
		{"solana without pay services", "solana", solanaLinked, solanaCfg, O, models.CheckoutRoutingSkipServiceUnavailable},
		{"unknown rail", "paypal", oneOff, &config.PSPConfig{}, O, models.CheckoutRoutingSkipUnknownSelector},
		{"no provider config", "stripe", oneOff, nil, O, models.CheckoutRoutingSkipNotArmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := (&CheckoutSessionService{}).checkoutRailSkipReason(tc.price, railTarget{PSP: tc.rail, Rail: tc.rail}, tc.cfg, tc.mode)
			require.Equal(t, tc.want, got)
		})
	}
}

// A price link belongs to one provider account: never substituted from a
// sibling account on the same rail, and a psp_id-pinned link wins.
func TestPriceLinkIsProviderScoped(t *testing.T) {
	mobiusID, paykingsID := uuid.New(), uuid.New()
	mobius := railTarget{PSP: "mobius", Rail: "nmi", Scope: &merchants.PSPScope{ID: mobiusID}}
	price := &models.Price{ID: uuid.New(), PSPLinks: map[string]map[string]string{
		"mobius":   {models.RailKeyRail: "nmi", models.RailKeyPlanID: "plan_mobius"},
		"paykings": {models.RailKeyRail: "nmi", models.RailKeyPlanID: "plan_paykings"},
		"legacy":   {models.RailKeyRail: "nmi", models.RailKeyPlanID: "plan_pinned", models.RailKeyPSPID: paykingsID.String()},
		"wrong":    {models.RailKeyRail: "ccbill", models.RailKeyPlanID: "plan_ccbill"},
	}}
	for _, tc := range []struct {
		name   string
		target railTarget
		want   string
	}{
		{"own key", mobius, "plan_mobius"},
		{"psp_id pin wins over the key", railTarget{PSP: "paykings", Rail: "nmi", Scope: &merchants.PSPScope{ID: paykingsID}}, "plan_pinned"},
		{"key naming a link pinned to another account", railTarget{PSP: "legacy", Rail: "nmi", Scope: &merchants.PSPScope{ID: mobiusID}}, ""},
		{"no sibling fallback", railTarget{PSP: "other", Rail: "nmi", Scope: &merchants.PSPScope{ID: uuid.New()}}, ""},
		{"link on another rail", railTarget{PSP: "wrong", Rail: "nmi"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := requireNMIPlanForTarget(price, tc.target)
			if tc.want == "" {
				require.ErrorContains(t, err, "missing NMI plan configuration")
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, plan)
		})
	}
	frozen := priceForCheckoutTarget(price, mobius)
	require.Equal(t, map[string]map[string]string{"mobius": price.PSPLinks["mobius"]}, frozen.PSPLinks)
	require.Len(t, price.PSPLinks, 4, "freezing does not mutate the catalog price")
}

func TestSavedMethodMustBelongToTargetPSP(t *testing.T) {
	id := uuid.New()
	target := railTarget{Scope: &merchants.PSPScope{ID: id}}
	require.NoError(t, paymentMethodMatchesTargetPSP(&models.PaymentMethod{PspID: id}, target))
	require.ErrorIs(t, paymentMethodMatchesTargetPSP(&models.PaymentMethod{PspID: uuid.New()}, target), ErrPaymentMethodStale)
	require.ErrorIs(t, paymentMethodMatchesTargetPSP(&models.PaymentMethod{}, target), ErrPaymentMethodStale)
	require.Error(t, paymentMethodMatchesTargetPSP(nil, target))
	require.Error(t, paymentMethodMatchesTargetPSP(&models.PaymentMethod{PspID: id}, railTarget{}))
}

func TestCheckoutRequiresProviderWrites(t *testing.T) {
	for _, tc := range []struct {
		cfg *config.Config
		ok  bool
	}{
		{nil, false},
		{&config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly}, false},
		{&config.Config{ProviderWriteMode: config.ProviderWriteModeLimited}, true},
		{&config.Config{ProviderWriteMode: config.ProviderWriteModeFull}, true},
	} {
		err := (&CheckoutSessionService{config: tc.cfg}).requireProviderWrites()
		if tc.ok {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, ErrCheckoutSessionValidation)
		}
	}
}
