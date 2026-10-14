package payment

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/railresolve"
)

// Deterministic processor routing: one function decides which PSP a purchase
// lands on and backs the option list, session creation and the dry-run trace.
// It ranks declared candidates, drops those that cannot serve this price, and
// takes the first survivor. It never retries after a charge: a decline is a
// per-charge outcome, not a routing failure.

// defaultRoutingOrder is the built-in preference order used when the merchant
// declares no policy. Rail kinds, not PSP keys: it must hold for merchants
// whose PSPs it has never seen.
var defaultRoutingOrder = []string{
	string(models.RailStripe),
	string(models.RailNMI),
	string(models.RailCCBill),
	string(models.RailSolana),
}

// RoutingInput is everything routing is allowed to look at. Price and Product
// are already resolved and ownership-checked by the caller.
type RoutingInput struct {
	Price   *models.Price
	Product *models.Product
	// Mode is the purchase mode this price implies (one_off | subscription).
	Mode models.PurchaseKind
	// Country is the payer's country when the request carries one, else "".
	Country string
	// Selector is an EXPLICIT client preference (the wire payment.rail). When
	// set it wins outright and no fallback applies: the browser has already
	// committed to that rail's flow, so silently switching would break it.
	Selector string
}

// RoutingCandidate is one evaluated candidate in preference order.
type RoutingCandidate struct {
	Selector string    `json:"selector"`
	Rail     string    `json:"rail,omitempty"`
	PSPID    uuid.UUID `json:"-"`
	// Skip is "" when the candidate is eligible, else its skip class.
	Skip string `json:"skip,omitempty"`
}

// RoutingDecision is the full trace: the winner, the ranked remainder, and
// every candidate with its verdict.
type RoutingDecision struct {
	Target     railTarget
	Policy     string
	Rule       *int
	Candidates []RoutingCandidate
}

// Selected is the winning selector, or "" when nothing was eligible.
func (d *RoutingDecision) Selected() string {
	if d == nil {
		return ""
	}
	return d.Target.PSP
}

// Eligible returns the eligible selectors in preference order.
func (d *RoutingDecision) Eligible() []string {
	if d == nil {
		return nil
	}
	out := make([]string, 0, len(d.Candidates))
	for _, c := range d.Candidates {
		if c.Skip == "" {
			out = append(out, c.Selector)
		}
	}
	return out
}

// Reason projects the decision onto the persisted trace.
func (d *RoutingDecision) Reason() *models.PSPRoutingReason {
	if d == nil || d.Target.PSP == "" {
		return nil
	}
	reason := &models.PSPRoutingReason{
		Policy:   d.Policy,
		Rule:     d.Rule,
		Selected: d.Target.PSP,
		Rail:     d.Target.Rail,
	}
	for _, c := range d.Candidates {
		switch {
		case c.Skip != "":
			reason.Skipped = append(reason.Skipped, models.PSPRoutingSkip{Selector: c.Selector, Reason: c.Skip})
		case c.Selector != reason.Selected:
			reason.Fallbacks = append(reason.Fallbacks, c.Selector)
		}
	}
	return reason
}

// ErrNoRoutableProcessor is returned when no declared candidate can serve the
// price. Fail closed: routing never invents a processor.
var ErrNoRoutableProcessor = errors.New("no payment provider is available for this price")

// Route picks the PSP for a purchase. The returned decision always carries the
// full candidate trace, including on the no-winner error path.
func (s *OrderAttemptService) Route(ctx context.Context, in RoutingInput) (*RoutingDecision, error) {
	targets := s.purchaseService
	if targets == nil || targets.railSource() == nil {
		return nil, fmt.Errorf("PSP routing unavailable")
	}
	if in.Price == nil {
		return nil, fmt.Errorf("PSP routing requires a price")
	}

	// An explicitly named PSP is resolved and used as named — no eligibility
	// sweep, no fallback. The caller asked for this processor. It must still
	// be armed: a declared account without credentials is an identity for
	// attribution, and naming it is refused here rather than failing at the
	// provider call.
	if selector := strings.ToLower(strings.TrimSpace(in.Selector)); selector != "" {
		target, err := targets.resolveRailTarget(ctx, selector)
		if err != nil {
			return nil, err
		}
		decision := &RoutingDecision{
			Target:     target,
			Policy:     models.PSPRoutingPolicyExplicit,
			Candidates: []RoutingCandidate{{Selector: target.PSP, Rail: target.Rail, PSPID: targetPSPID(target)}},
		}
		providerConfig, err := s.routingConfig(ctx, target)
		if err != nil {
			decision.Target = railTarget{}
			if errors.Is(err, railresolve.ErrRailNotArmed) {
				decision.Candidates[0].Skip = models.PSPRoutingSkipNotArmed
				return decision, fmt.Errorf("%w: payment provider %q is not armed", ErrNoRoutableProcessor, selector)
			}
			decision.Candidates[0].Skip = models.PSPRoutingSkipResolveFailed
			return decision, fmt.Errorf("resolve payment provider %q: %w", selector, err)
		}
		// Named, not swept: no fallback, but the PSP must still be able to
		// make this sale (a named CCBill never enrolls a new subscription), and
		// a card PSP must be one the merchant charges new cards through.
		skip := s.railSkipReason(in.Price, target, providerConfig, in.Mode)
		if skip == "" {
			if skip, err = s.chargePSPSkip(ctx, target); err != nil {
				return decision, err
			}
		}
		if skip != "" {
			decision.Target = railTarget{}
			decision.Candidates[0].Skip = skip
			return decision, fmt.Errorf("%w: payment provider %q cannot sell this price (%s)", ErrNoRoutableProcessor, selector, skip)
		}
		return decision, nil
	}

	order, policy, rule, err := s.routingOrder(ctx, in)
	if err != nil {
		return nil, err
	}
	decision := &RoutingDecision{Policy: policy, Rule: rule, Candidates: make([]RoutingCandidate, 0, len(order))}
	for _, selector := range order {
		target, skip := s.evaluateCandidate(ctx, targets, in, selector)
		candidate := RoutingCandidate{Selector: selector, Rail: target.Rail, PSPID: targetPSPID(target), Skip: skip}
		if skip == "" {
			// The resolved PSP key is the answer, not the requested selector: a
			// rail kind resolves to the armed account's key.
			candidate.Selector = target.PSP
			if decision.Target.PSP == "" {
				decision.Target = target
			}
		}
		decision.Candidates = append(decision.Candidates, candidate)
	}
	if decision.Target.PSP == "" {
		skipped := make([]string, 0, len(decision.Candidates))
		for _, c := range decision.Candidates {
			skipped = append(skipped, c.Selector+":"+c.Skip)
		}
		return decision, fmt.Errorf("%w (%s)", ErrNoRoutableProcessor, strings.Join(skipped, ", "))
	}
	return decision, nil
}

func targetAccountID(target railTarget) string {
	if target.Scope == nil {
		return ""
	}
	return target.Scope.AccountID
}

// A gateway can offer an existing merchant-held card or a new platform card.
// This is only an availability check: submission and execution resolve the
// chosen instrument's exact custody, never fall back to another vault.
func (s *OrderAttemptService) routingConfig(ctx context.Context, target railTarget) (*config.ResolvedPSP, error) {
	source := s.purchaseService.railSource()
	cfg, err := source.RailConfig(ctx, target.Rail, targetAccountID(target))
	if err == nil || target.Rail != string(models.RailNMI) {
		return cfg, err
	}
	svc := merchants.Of(s.db)
	if svc == nil {
		return nil, err
	}
	mid, scopeErr := merchant.Require(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	platform, platformErr := svc.PlatformCardCustodian(ctx, mid)
	if platformErr != nil {
		return nil, platformErr
	}
	if platform == nil {
		return nil, err
	}
	return source.RailConfig(db.WithCustodianID(ctx, platform.ID), target.Rail, targetAccountID(target))
}

func targetPSPID(target railTarget) uuid.UUID {
	if target.Scope == nil {
		return uuid.Nil
	}
	return target.Scope.ID
}

// unresolvedRailTarget is what a candidate knows about itself when resolution
// failed. A bare rail kind ("stripe") is its own rail, so the trace can still
// name it; a PSP key ("mobius") that did not resolve has no discoverable rail,
// and an empty one is honest rather than guessed.
func unresolvedRailTarget(selector string) railTarget {
	name := strings.ToLower(strings.TrimSpace(selector))
	if _, ok := knownRails[name]; !ok {
		return railTarget{}
	}
	return railTarget{PSP: name, Rail: name}
}

// routingOrder resolves the candidate order for these inputs: a provider
// price's own PSP, the first matching merchant rule, else the default: the
// merchant's charge PSPs, then the other rails.
func (s *OrderAttemptService) routingOrder(ctx context.Context, in RoutingInput) ([]string, string, *int, error) {
	if key, _, ok := in.Price.ProviderLink(); ok {
		return []string{key}, models.PSPRoutingPolicyDefault, nil, nil
	}
	conf, err := s.routingConfiguration(ctx)
	if err != nil {
		return nil, "", nil, err
	}
	rules := conf.PSPRouting
	for i, rule := range rules {
		if !routingRuleMatches(rule.Match, in) {
			continue
		}
		idx := i
		order := make([]string, 0, len(rule.Prefer))
		for _, selector := range rule.Prefer {
			if selector = strings.ToLower(strings.TrimSpace(selector)); selector != "" {
				order = append(order, selector)
			}
		}
		return order, models.PSPRoutingPolicyMerchant, &idx, nil
	}
	if len(conf.ChargePSPs) > 0 {
		return append(append([]string{}, conf.ChargePSPs...), string(models.RailCCBill), string(models.RailSolana)), models.PSPRoutingPolicyDefault, nil, nil
	}
	return defaultRoutingOrder, models.PSPRoutingPolicyDefault, nil, nil
}

// routingConfiguration loads the merchant's routing policy and charge PSPs.
// No stored config (or no DB, as in unit fixtures) means neither.
func (s *OrderAttemptService) routingConfiguration(ctx context.Context) (models.MerchantConfiguration, error) {
	if s.db == nil {
		return models.MerchantConfiguration{}, nil
	}
	conf, _, err := merchantconfig.NewStore(s.db).Get(ctx)
	if err != nil {
		return conf, fmt.Errorf("load PSP routing policy: %w", err)
	}
	return conf, nil
}

// chargePSPSkip refuses a card PSP the merchant's charge_psps leave out.
func (s *OrderAttemptService) chargePSPSkip(ctx context.Context, target railTarget) (string, error) {
	if target.Rail != string(models.RailNMI) && target.Rail != string(models.RailStripe) {
		return "", nil
	}
	conf, err := s.routingConfiguration(ctx)
	if err != nil || len(conf.ChargePSPs) == 0 || slices.Contains(conf.ChargePSPs, target.PSP) {
		return "", err
	}
	return models.PSPRoutingSkipNotChargePSP, nil
}

// routingRuleMatches reports whether every SET condition holds. An all-empty
// match is the catch-all.
func routingRuleMatches(m models.PSPRoutingMatch, in RoutingInput) bool {
	eq := func(want, got string) bool {
		return want == "" || strings.EqualFold(strings.TrimSpace(want), strings.TrimSpace(got))
	}
	priceKey, productKey := "", ""
	if in.Price != nil {
		priceKey = in.Price.Key
	}
	if in.Product != nil {
		productKey = in.Product.Key
	}
	currency := ""
	if in.Price != nil {
		currency = in.Price.Currency
	}
	return eq(m.Currency, currency) &&
		eq(m.Product, productKey) &&
		eq(m.Price, priceKey) &&
		eq(m.Mode, string(in.Mode)) &&
		eq(m.Country, in.Country)
}

// evaluateCandidate resolves one selector and reports whether it can serve this
// price now. The empty skip class means eligible.
func (s *OrderAttemptService) evaluateCandidate(ctx context.Context, targets orderRailTargets, in RoutingInput, selector string) (railTarget, string) {
	target, err := targets.resolveRailTarget(ctx, selector)
	if err != nil {
		var ambiguous *AmbiguousRailError
		var unarmed *UnarmedRailError
		var unknown *UnknownRailError
		// A skipped candidate still reports which rail it was: the trace lists
		// every candidate in order with its skip class, and support reads it.
		// Resolution failure is why the rail is unusable, not grounds to stop
		// naming it — a bare rail kind names itself even when nothing is armed.
		skipped := unresolvedRailTarget(selector)
		switch {
		case errors.As(err, &ambiguous):
			return skipped, models.PSPRoutingSkipAmbiguousSelector
		case errors.As(err, &unarmed):
			return skipped, models.PSPRoutingSkipNotArmed
		case errors.As(err, &unknown):
			// A key that resolves to nothing may still be DECLARED and archived.
			// Say "retired", not "never heard of it" — support reads this.
			if targets.pspKeyArchived(ctx, selector) {
				return skipped, models.PSPRoutingSkipNotArmed
			}
			return skipped, models.PSPRoutingSkipUnknownSelector
		default:
			return skipped, models.PSPRoutingSkipResolveFailed
		}
	}
	providerConfig, err := s.routingConfig(ctx, target)
	if err != nil {
		if errors.Is(err, railresolve.ErrRailNotArmed) {
			return target, models.PSPRoutingSkipNotArmed
		}
		return target, models.PSPRoutingSkipResolveFailed
	}
	if skip := s.railSkipReason(in.Price, target, providerConfig, in.Mode); skip != "" {
		return target, skip
	}
	skip, err := s.chargePSPSkip(ctx, target)
	if err != nil {
		return target, models.PSPRoutingSkipResolveFailed
	}
	return target, skip
}

// DryRunRouting answers "which PSP would this checkout get, and why" without
// creating anything. It resolves the price/product exactly as purchase does and
// runs the SAME Route call, so the trace it returns is the decision a real
// session would record — not a re-implementation that can drift.
func (s *OrderAttemptService) DryRunRouting(ctx context.Context, priceID, productKey, priceKey, country, selector string) (*RoutingDecision, models.PurchaseKind, error) {
	if err := validatePriceSelector(priceID, productKey, priceKey); err != nil {
		return nil, "", err
	}
	if s == nil || s.priceService == nil || s.productService == nil {
		return nil, "", fmt.Errorf("PSP routing unavailable")
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("resolve purchase merchant: %w", err)
	}
	price, err := resolvePurchasePrice(ctx, s.priceService, priceID, productKey, priceKey)
	if err != nil {
		return nil, "", fmt.Errorf("resolve purchase price: %w", err)
	}
	if price.MerchantID != merchantID.UUID() {
		return nil, "", fmt.Errorf("resolve purchase price: price not found")
	}
	product, err := s.productService.GetByID(ctx, price.ProductID)
	if err != nil {
		return nil, "", fmt.Errorf("resolve purchase product: %w", err)
	}
	if product.MerchantID != merchantID.UUID() {
		return nil, "", fmt.Errorf("resolve purchase product: product not found")
	}
	mode := purchaseModeForRail(price, "")
	decision, err := s.Route(ctx, RoutingInput{
		Price:    price,
		Product:  product,
		Mode:     mode,
		Country:  strings.TrimSpace(country),
		Selector: strings.TrimSpace(selector),
	})
	if err != nil && !errors.Is(err, ErrNoRoutableProcessor) {
		return nil, mode, err
	}
	return decision, mode, nil
}
