package checkout

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Deterministic processor routing (or#288).
//
// One function decides which PSP a checkout lands on, and the SAME function
// backs the option list, session creation and the dry-run trace — there is no
// second implementation to drift. Routing is a pre-session act: it ranks
// declared candidates, drops the ones that cannot serve this price right now,
// and takes the first survivor. It never retries after a charge — a decline is
// a per-charge outcome, not a routing failure.

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
	// Mode is the checkout mode this price implies (one_off | subscription).
	Mode models.CheckoutSessionMode
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
func (d *RoutingDecision) Reason() *models.CheckoutRoutingReason {
	if d == nil || d.Target.PSP == "" {
		return nil
	}
	reason := &models.CheckoutRoutingReason{
		Policy:   d.Policy,
		Rule:     d.Rule,
		Selected: d.Target.PSP,
		Rail:     d.Target.Rail,
	}
	for _, c := range d.Candidates {
		switch {
		case c.Skip != "":
			reason.Skipped = append(reason.Skipped, models.CheckoutRoutingSkip{Selector: c.Selector, Reason: c.Skip})
		case c.Selector != reason.Selected:
			reason.Fallbacks = append(reason.Fallbacks, c.Selector)
		}
	}
	return reason
}

// ErrNoRoutableProcessor is returned when no declared candidate can serve the
// price. Fail closed: routing never invents a processor.
var ErrNoRoutableProcessor = errors.New("no payment provider is available for this price")

// Route picks the PSP for a checkout. The returned decision always carries the
// full candidate trace, including on the no-winner error path.
func (s *CheckoutSessionService) Route(ctx context.Context, in RoutingInput) (*RoutingDecision, error) {
	targets := s.checkoutService
	if targets == nil || targets.railSource() == nil {
		return nil, fmt.Errorf("checkout routing unavailable")
	}
	if in.Price == nil {
		return nil, fmt.Errorf("checkout routing requires a price")
	}

	// An explicitly named PSP is resolved and used as named — no eligibility
	// sweep, no fallback. The caller asked for this processor.
	if selector := strings.ToLower(strings.TrimSpace(in.Selector)); selector != "" {
		target, err := targets.resolveRailTarget(ctx, selector)
		if err != nil {
			return nil, err
		}
		return &RoutingDecision{
			Target:     target,
			Policy:     models.CheckoutRoutingPolicyExplicit,
			Candidates: []RoutingCandidate{{Selector: target.PSP, Rail: target.Rail, PSPID: targetPSPID(target)}},
		}, nil
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
			// rail kind resolves to the armed account's key (#848).
			candidate.Selector = target.PSP
			if decision.Target.PSP == "" {
				decision.Target = target
			}
		}
		decision.Candidates = append(decision.Candidates, candidate)
	}
	if decision.Target.PSP == "" {
		return decision, ErrNoRoutableProcessor
	}
	return decision, nil
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

// routingOrder resolves the candidate order for these inputs: the first
// matching merchant rule, else the built-in default.
func (s *CheckoutSessionService) routingOrder(ctx context.Context, in RoutingInput) ([]string, string, *int, error) {
	rules, err := s.routingRules(ctx)
	if err != nil {
		return nil, "", nil, err
	}
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
		return order, models.CheckoutRoutingPolicyMerchant, &idx, nil
	}
	return defaultRoutingOrder, models.CheckoutRoutingPolicyDefault, nil, nil
}

// routingRules loads the merchant's declared policy. No stored config (or no
// DB, as in unit fixtures) means no policy, which means the default order.
func (s *CheckoutSessionService) routingRules(ctx context.Context) ([]models.CheckoutRoutingRule, error) {
	if s.db == nil {
		return nil, nil
	}
	conf, found, err := merchantconfig.NewStore(s.db).Get(ctx)
	if err != nil {
		return nil, fmt.Errorf("load checkout routing policy: %w", err)
	}
	if !found {
		return nil, nil
	}
	return conf.CheckoutRouting, nil
}

// routingRuleMatches reports whether every SET condition holds. An all-empty
// match is the catch-all.
func routingRuleMatches(m models.CheckoutRoutingMatch, in RoutingInput) bool {
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
func (s *CheckoutSessionService) evaluateCandidate(ctx context.Context, targets checkoutRailTargets, in RoutingInput, selector string) (railTarget, string) {
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
			return skipped, models.CheckoutRoutingSkipAmbiguousSelector
		case errors.As(err, &unarmed):
			return skipped, models.CheckoutRoutingSkipNotArmed
		case errors.As(err, &unknown):
			// A key that resolves to nothing may still be DECLARED and archived.
			// Say "retired", not "never heard of it" — support reads this.
			if targets.pspKeyArchived(ctx, selector) {
				return skipped, models.CheckoutRoutingSkipNotArmed
			}
			return skipped, models.CheckoutRoutingSkipUnknownSelector
		default:
			return skipped, models.CheckoutRoutingSkipResolveFailed
		}
	}
	accountID := ""
	if target.Scope != nil {
		accountID = target.Scope.AccountID
	}
	providerConfig, err := targets.railSource().RailConfig(ctx, target.Rail, accountID)
	if err != nil {
		if errors.Is(err, railresolve.ErrRailNotArmed) {
			return target, models.CheckoutRoutingSkipNotArmed
		}
		return target, models.CheckoutRoutingSkipResolveFailed
	}
	return target, s.checkoutRailSkipReason(in.Price, target, providerConfig, in.Mode)
}

// DryRunRouting answers "which PSP would this checkout get, and why" without
// creating anything. It resolves the price/product exactly as checkout does and
// runs the SAME Route call, so the trace it returns is the decision a real
// session would record — not a re-implementation that can drift.
func (s *CheckoutSessionService) DryRunRouting(ctx context.Context, priceRef, country, selector string) (*RoutingDecision, models.CheckoutSessionMode, error) {
	priceRef = strings.TrimSpace(priceRef)
	if priceRef == "" {
		return nil, "", fmt.Errorf("price reference is required")
	}
	if s == nil || s.priceService == nil || s.productService == nil {
		return nil, "", fmt.Errorf("checkout routing unavailable")
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("resolve checkout merchant: %w", err)
	}
	price, err := catalog.ResolveReference(ctx, s.priceService, priceRef)
	if err != nil {
		return nil, "", fmt.Errorf("resolve checkout price: %w", err)
	}
	if price.MerchantID != merchantID.UUID() {
		return nil, "", fmt.Errorf("resolve checkout price: price not found")
	}
	product, err := s.productService.GetByID(ctx, price.ProductID)
	if err != nil {
		return nil, "", fmt.Errorf("resolve checkout product: %w", err)
	}
	if product.MerchantID != merchantID.UUID() {
		return nil, "", fmt.Errorf("resolve checkout product: product not found")
	}
	mode := checkoutModeForRail(price, "")
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
