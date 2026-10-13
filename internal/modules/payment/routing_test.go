package payment

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/open-rails/openrails/internal/modules/solana/recurring"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/railresolve"
)

// pspCatalog is an in-memory PSP catalog: key lookup, the rail-kind list,
// archived keys and identity lookup.
type pspCatalog struct {
	scopes   []merchants.PSPScope
	archived []string
	err      error
}

func (c pspCatalog) ActivePSPSecretName(context.Context, billing.MerchantID, string, string, string) (string, bool, error) {
	return "", false, nil
}

func (c pspCatalog) PSPScopeByKey(_ context.Context, _ billing.MerchantID, key, _ string) (merchants.PSPScope, bool, error) {
	for _, s := range c.scopes {
		if strings.EqualFold(s.Key, key) {
			return s, true, c.err
		}
	}
	return merchants.PSPScope{}, false, c.err
}

func (c pspCatalog) ActivePSPScopesForRail(_ context.Context, _ billing.MerchantID, rail, _ string) ([]merchants.PSPScope, error) {
	var out []merchants.PSPScope
	for _, s := range c.scopes {
		if strings.EqualFold(s.Rail, rail) {
			out = append(out, s)
		}
	}
	return out, c.err
}

func (c pspCatalog) PSPKeyArchived(_ context.Context, _ billing.MerchantID, key, _ string) (bool, error) {
	for _, k := range c.archived {
		if k == key {
			return true, nil
		}
	}
	return false, nil
}

type keyOnlyCatalog struct{}

func (keyOnlyCatalog) ActivePSPSecretName(context.Context, billing.MerchantID, string, string, string) (string, bool, error) {
	return "", false, nil
}

func (keyOnlyCatalog) PSPScopeByKey(context.Context, billing.MerchantID, string, string) (merchants.PSPScope, bool, error) {
	return merchants.PSPScope{}, false, nil
}

var testMerchant = billing.MerchantID(uuid.MustParse("a5a5a5a5-0000-4000-8000-000000000001"))

func merchantCtx() context.Context { return merchant.WithID(context.Background(), testMerchant) }

func armedAll() railresolve.FixedSet {
	return railresolve.FixedSet{
		"stripe": {Rail: models.RailStripe, AccountID: "acct_stripe", Stripe: &config.StripeRailConfig{SecretKey: "sk_test_value"}},
		"nmi":    {Rail: models.RailNMI, AccountID: "acct_nmi", NMI: &config.NMIRailConfig{SecurityKey: "security_test"}},
		"ccbill": {Rail: models.RailCCBill, AccountID: "999999-0000", CCBill: &config.CCBillRailConfig{Salt: "salt_test"}},
	}
}

func scopesOf(armed railresolve.FixedSet) []merchants.PSPScope {
	out := make([]merchants.PSPScope, 0, len(armed))
	for key, p := range armed {
		out = append(out, merchants.PSPScope{ID: merchants.PspID(string(p.Rail), "live", p.AccountID), Rail: string(p.Rail), Environment: "live", AccountID: p.AccountID, Key: key})
	}
	return out
}

func routingService(armed railresolve.FixedSet, extra ...merchants.PSPScope) *OrderAttemptService {
	return routingServiceWith(armed, pspCatalog{scopes: append(scopesOf(armed), extra...)})
}

func routingServiceWith(armed railresolve.FixedSet, catalog pspCatalog) *OrderAttemptService {
	cfg := &config.Config{ProviderWriteMode: config.ProviderWriteModeFull}
	return &OrderAttemptService{config: cfg, purchaseService: &PurchaseService{Config: cfg, Rails: armed, ProviderSecrets: catalog}}
}

func recurringPrice() *models.Price {
	return &models.Price{ID: uuid.New(), Key: "pro-monthly", Currency: "usd", Amount: 1_000_000, BillingIntervalHours: intPtr(720), AccessDurationHours: intPtr(720),
		PSPLinks: map[string]map[string]string{
			"stripe": {models.RailKeyRail: "stripe", models.RailKeyStripePriceID: "price_test"},
			"nmi":    {models.RailKeyRail: "nmi", models.RailKeyPlanID: "plan_test"},
			"ccbill": {models.RailKeyRail: "ccbill", models.RailKeyCCBillFormName: "form_test", models.RailKeyCCBillFlexID: "flex_test"},
		}}
}

// A selector is a declared PSP key or, only when unambiguous, a rail kind.
// Everything else fails closed with no scope.
func TestResolveRailTarget(t *testing.T) {
	mobius := merchants.PSPScope{ID: uuid.New(), Rail: "nmi", AccountID: "gw-1", Key: "mobius"}
	paykings := merchants.PSPScope{ID: uuid.New(), Rail: "nmi", AccountID: "gw-2", Key: "paykings"}
	single := &PurchaseService{ProviderSecrets: pspCatalog{scopes: []merchants.PSPScope{mobius}}}
	double := &PurchaseService{ProviderSecrets: pspCatalog{scopes: []merchants.PSPScope{mobius, paykings}}}

	for _, tc := range []struct {
		name, selector string
		svc            *PurchaseService
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
		svc  *PurchaseService
		ctx  context.Context
		sel  string
		want string
	}{
		{"blank selector", single, merchantCtx(), "  ", "rail is required"},
		{"resolver not wired", &PurchaseService{}, merchantCtx(), "nmi", "resolution is not configured"},
		{"rail list capability missing", &PurchaseService{ProviderSecrets: keyOnlyCatalog{}}, merchantCtx(), "nmi", "rail resolution is not configured"},
		{"catalog fails", &PurchaseService{ProviderSecrets: pspCatalog{err: errors.New("catalog unavailable")}}, merchantCtx(), "nmi", "catalog unavailable"},
		{"no merchant on context", single, context.Background(), "nmi", "merchant"},
		{"scope without identity", &PurchaseService{ProviderSecrets: pspCatalog{scopes: []merchants.PSPScope{{Rail: "nmi", Key: "ghost"}}}}, merchantCtx(), "ghost", "identity is unavailable"},
		{"scope on unsupported rail", &PurchaseService{ProviderSecrets: pspCatalog{scopes: []merchants.PSPScope{{ID: uuid.New(), Rail: "paypal", Key: "pp"}}}}, merchantCtx(), "pp", "unsupported rail"},
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

	require.NoError(t, double.RailUsable(merchantCtx(), "mobius"))
	require.ErrorContains(t, double.RailUsable(merchantCtx(), "nmi"), "paykings")
	require.ErrorContains(t, double.RailUsable(merchantCtx(), "solana"), "has no armed PSP")
}

// The default order is part of the contract: a merchant that declares
// nothing gets stripe, nmi, ccbill, solana, and every skipped candidate is
// reported with its class rather than dropped.
func TestRouteDefaultPolicy(t *testing.T) {
	require.Equal(t, []string{"stripe", "nmi", "ccbill", "solana"}, defaultRoutingOrder)

	decision, err := routingService(armedAll()).Route(merchantCtx(), RoutingInput{Price: recurringPrice(), Mode: models.PurchaseSubscription})
	require.NoError(t, err)
	require.Equal(t, models.PSPRoutingPolicyDefault, decision.Policy)
	require.Equal(t, "stripe", decision.Selected())
	require.Equal(t, []string{"stripe", "nmi", "ccbill"}, decision.Eligible(), "CCBill sells a subscription on its FlexForm")
	reason := decision.Reason()
	require.Equal(t, []string{"nmi", "ccbill"}, reason.Fallbacks)
	require.Equal(t, []models.PSPRoutingSkip{
		{Selector: "solana", Reason: models.PSPRoutingSkipNotArmed},
	}, reason.Skipped)

	armed := armedAll()
	delete(armed, "stripe")
	oneOff := recurringPrice()
	oneOff.BillingIntervalHours = nil
	decision, err = routingService(armed).Route(merchantCtx(), RoutingInput{Price: oneOff, Mode: models.PurchaseOneOff})
	require.NoError(t, err)
	require.Equal(t, "nmi", decision.Selected())
	require.Empty(t, decision.Reason().Fallbacks)
	require.Equal(t, []models.PSPRoutingSkip{
		{Selector: "stripe", Reason: models.PSPRoutingSkipNotArmed},
		{Selector: "ccbill", Reason: models.PSPRoutingSkipModeUnsupported},
		{Selector: "solana", Reason: models.PSPRoutingSkipNotArmed},
	}, decision.Reason().Skipped)

	decision, err = routingService(railresolve.FixedSet{}).Route(merchantCtx(), RoutingInput{Price: recurringPrice(), Mode: models.PurchaseSubscription})
	require.ErrorIs(t, err, ErrNoRoutableProcessor, "routing never invents a processor")
	require.Len(t, decision.Candidates, 4, "the full trace survives the failure")
	require.Nil(t, decision.Reason())
}

// A named PSP is used as named with no fallback, but must still be armed.
func TestRouteExplicitSelector(t *testing.T) {
	decision, err := routingService(armedAll()).Route(merchantCtx(), RoutingInput{Price: recurringPrice(), Mode: models.PurchaseSubscription, Selector: " Stripe "})
	require.NoError(t, err)
	require.Equal(t, models.PSPRoutingPolicyExplicit, decision.Policy)
	require.Equal(t, "stripe", decision.Selected())
	require.Empty(t, decision.Reason().Fallbacks)
	require.Empty(t, decision.Reason().Skipped)

	// A named PSP still has to be able to make the sale.
	oneOff := recurringPrice()
	oneOff.BillingIntervalHours = nil
	decision, err = routingService(armedAll()).Route(merchantCtx(), RoutingInput{Price: oneOff, Mode: models.PurchaseOneOff, Selector: "ccbill"})
	require.ErrorIs(t, err, ErrNoRoutableProcessor)
	require.Empty(t, decision.Selected())
	require.Equal(t, models.PSPRoutingSkipModeUnsupported, decision.Candidates[0].Skip, "a FlexForm sells subscriptions only")

	declared := merchants.PSPScope{ID: uuid.New(), Rail: "stripe", Environment: "live", AccountID: "acct_declared", Key: "stripe-declared"}
	decision, err = routingService(armedAll(), declared).Route(merchantCtx(), RoutingInput{Price: recurringPrice(), Selector: "stripe-declared"})
	require.ErrorIs(t, err, ErrNoRoutableProcessor)
	require.Empty(t, decision.Selected())
	require.Equal(t, models.PSPRoutingSkipNotArmed, decision.Candidates[0].Skip)

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
		{"nmi", "nmi", models.PSPRoutingSkipAmbiguousSelector},
		{"retired", "", models.PSPRoutingSkipNotArmed},
		{"never-declared", "", models.PSPRoutingSkipUnknownSelector},
		{"mobius", "nmi", models.PSPRoutingSkipNotArmed},
		{"stripe", "stripe", ""},
	} {
		target, skip := svc.evaluateCandidate(merchantCtx(), svc.purchaseService, RoutingInput{Price: price, Mode: models.PurchaseSubscription}, tc.selector)
		require.Equal(t, tc.want, skip, tc.selector)
		require.Equal(t, tc.rail, target.Rail, "a bare rail kind names itself even when skipped: %s", tc.selector)
	}

	failing := routingServiceWith(armedAll(), pspCatalog{err: errors.New("catalog down")})
	_, skip := failing.evaluateCandidate(merchantCtx(), failing.purchaseService, RoutingInput{Price: price, Mode: models.PurchaseSubscription}, "stripe")
	require.Equal(t, models.PSPRoutingSkipResolveFailed, skip)
}

func TestRoutingRuleMatches(t *testing.T) {
	in := RoutingInput{Price: recurringPrice(), Product: &models.Product{Key: "pro"}, Mode: models.PurchaseSubscription, Country: "US"}
	for _, tc := range []struct {
		name  string
		match models.PSPRoutingMatch
		want  bool
	}{
		{"catch-all", models.PSPRoutingMatch{}, true},
		{"currency case-insensitive", models.PSPRoutingMatch{Currency: " USD "}, true},
		{"currency miss", models.PSPRoutingMatch{Currency: "eur"}, false},
		{"product", models.PSPRoutingMatch{Product: "pro"}, true},
		{"price key", models.PSPRoutingMatch{Price: "pro-monthly"}, true},
		{"mode miss", models.PSPRoutingMatch{Mode: "one_off"}, false},
		{"country", models.PSPRoutingMatch{Country: "us"}, true},
		{"all set conditions must hold", models.PSPRoutingMatch{Currency: "usd", Product: "enterprise"}, false},
	} {
		require.Equal(t, tc.want, routingRuleMatches(tc.match, in), tc.name)
	}
	require.False(t, routingRuleMatches(models.PSPRoutingMatch{Product: "pro"}, RoutingInput{}), "absent product never matches a product rule")
}

// One readiness verdict backs both the option list and routing.
func TestRailSkipReason(t *testing.T) {
	stripeCfg := &config.ResolvedPSP{Stripe: &config.StripeRailConfig{SecretKey: "sk_test"}}
	nmiCfg := &config.ResolvedPSP{NMI: &config.NMIRailConfig{SecurityKey: "key"}}
	solanaCfg := &config.ResolvedPSP{Solana: &config.SolanaRailConfig{Tokens: map[string]config.TokenConfig{"USDC": {Mint: "mint"}}}}
	sub := recurringPrice()
	trial := recurringPrice()
	trial.TrialUnitAmount = new(int64(0))
	noCycle := recurringPrice()
	noCycle.BillingIntervalHours = nil
	free := recurringPrice()
	free.Amount = 0
	noFlexForm := recurringPrice()
	delete(noFlexForm.PSPLinks["ccbill"], models.RailKeyCCBillFlexID)
	oneOff := &models.Price{ID: uuid.New(), Amount: 1_000_000, Currency: "USD"}
	solanaLinked := &models.Price{ID: uuid.New(), Amount: 1_000_000, Currency: "USD", PSPLinks: map[string]map[string]string{"solana": {models.RailKeyRail: "solana"}}}
	solanaPlan := recurringPrice()
	solanaPlan.PSPLinks = map[string]map[string]string{"solana": {models.RailKeyRail: "solana", "plan_id": "7", "amount_base_units": "1000000", "period_hours": "720", "mint_symbol": "USDC"}}
	recurringReady := &OrderAttemptService{solanaPrepareSubscribe: &recurring.PrepareSubscribeService{}, solanaEnroll: &recurring.EnrollService{}}
	const S, O = models.PurchaseSubscription, models.PurchaseOneOff

	for _, tc := range []struct {
		name  string
		rail  string
		price *models.Price
		cfg   *config.ResolvedPSP
		mode  models.PurchaseKind
		want  string
	}{
		{"stripe engine subscription needs no provider catalog", "stripe", sub, stripeCfg, S, ""},
		{"nmi engine subscription", "nmi", sub, nmiCfg, S, ""},
		{"stripe missing key", "stripe", sub, &config.ResolvedPSP{}, S, models.PSPRoutingSkipCredentialsMissing},
		{"nmi blank key", "nmi", sub, &config.ResolvedPSP{NMI: &config.NMIRailConfig{SecurityKey: "  "}}, S, models.PSPRoutingSkipCredentialsMissing},
		{"ccbill FlexForm enrollment", "ccbill", sub, &config.ResolvedPSP{CCBill: &config.CCBillRailConfig{Salt: "salt"}}, S, ""},
		{"ccbill without its salt", "ccbill", sub, &config.ResolvedPSP{CCBill: &config.CCBillRailConfig{}}, S, models.PSPRoutingSkipCredentialsMissing},
		{"ccbill without a FlexForm", "ccbill", noFlexForm, &config.ResolvedPSP{CCBill: &config.CCBillRailConfig{Salt: "salt"}}, S, models.PSPRoutingSkipLinkMissing},
		{"solana subscription without a published plan", "solana", sub, solanaCfg, S, models.PSPRoutingSkipLinkMissing},
		{"solana plan without recurring services", "solana", solanaPlan, solanaCfg, S, models.PSPRoutingSkipServiceUnavailable},
		{"trial terms", "stripe", trial, stripeCfg, S, models.PSPRoutingSkipModeUnsupported},
		{"no cycle", "stripe", noCycle, stripeCfg, S, models.PSPRoutingSkipModeUnsupported},
		{"zero amount", "nmi", free, nmiCfg, S, models.PSPRoutingSkipModeUnsupported},
		{"stripe one-off is inline, no link", "stripe", oneOff, stripeCfg, O, ""},
		{"nmi one-off needs no plan (#1055)", "nmi", oneOff, nmiCfg, O, ""},
		{"nmi one-off missing key", "nmi", oneOff, &config.ResolvedPSP{}, O, models.PSPRoutingSkipCredentialsMissing},
		{"ccbill one-off", "ccbill", oneOff, &config.ResolvedPSP{CCBill: &config.CCBillRailConfig{}}, O, models.PSPRoutingSkipModeUnsupported},
		{"solana without tokens", "solana", solanaLinked, &config.ResolvedPSP{Solana: &config.SolanaRailConfig{}}, O, models.PSPRoutingSkipCredentialsMissing},
		{"solana without link", "solana", oneOff, solanaCfg, O, models.PSPRoutingSkipLinkMissing},
		{"solana without pay services", "solana", solanaLinked, solanaCfg, O, models.PSPRoutingSkipServiceUnavailable},
		{"unknown rail", "paypal", oneOff, &config.ResolvedPSP{}, O, models.PSPRoutingSkipUnknownSelector},
		{"no provider config", "stripe", oneOff, nil, O, models.PSPRoutingSkipNotArmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := (&OrderAttemptService{}).railSkipReason(tc.price, railTarget{PSP: tc.rail, Rail: tc.rail}, tc.cfg, tc.mode)
			require.Equal(t, tc.want, got)
		})
	}
	// Solana subscribes new members to the price's on-chain plan.
	require.Empty(t, recurringReady.railSkipReason(solanaPlan, railTarget{PSP: "solana", Rail: "solana"}, solanaCfg, S))
	require.Equal(t, models.PSPRoutingSkipModeUnsupported, recurringReady.railSkipReason(trial, railTarget{PSP: "solana", Rail: "solana"}, solanaCfg, S))
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
	frozen := priceForPurchaseTarget(price, mobius)
	require.Equal(t, map[string]map[string]string{"mobius": price.PSPLinks["mobius"]}, frozen.PSPLinks)
	require.Len(t, price.PSPLinks, 4, "freezing does not mutate the catalog price")
}

func TestSavedMethodMustBelongToTargetPSP(t *testing.T) {
	id := uuid.New()
	target := railTarget{Scope: &merchants.PSPScope{ID: id}}
	other := uuid.New()
	require.NoError(t, paymentMethodMatchesTargetPSP(&models.PaymentMethod{PspID: &id}, target))
	require.ErrorIs(t, paymentMethodMatchesTargetPSP(&models.PaymentMethod{PspID: &other}, target), ErrPaymentMethodStale)
	require.ErrorIs(t, paymentMethodMatchesTargetPSP(&models.PaymentMethod{}, target), ErrPaymentMethodStale)
	require.NoError(t, paymentMethodMatchesTargetPSP(&models.PaymentMethod{Custodian: models.CustodianBasisTheory}, target), "a custodian's card is charged through the PSP routing picks")
	require.Error(t, paymentMethodMatchesTargetPSP(nil, target))
	require.Error(t, paymentMethodMatchesTargetPSP(&models.PaymentMethod{PspID: &id}, railTarget{}))
}

// Purchase refuses without a config and under readonly; a merchant's own
// posture is covered end to end.
func TestPurchaseRequiresProviderWrites(t *testing.T) {
	ctx := merchant.WithID(context.Background(), billing.MerchantID(uuid.New()))
	for _, cfg := range []*config.Config{nil, {ProviderWriteMode: config.ProviderWriteModeReadOnly}} {
		require.ErrorIs(t, (&OrderAttemptService{config: cfg}).requireProviderWrites(ctx), ErrOrderAttemptValidation)
	}
}
