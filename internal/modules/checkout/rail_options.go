package checkout

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// CheckoutOption is a locally ready payment-provider choice for a price.
// Selector is the exact value checkout accepts, PSPID is the provider's stable
// internal identity, and Rail is the canonical gateway.
type CheckoutOption struct {
	Selector string
	PSPID    uuid.UUID
	Rail     string
	Mode     string
	// Token is the settlement token the price binds on this PSP (Solana), or "".
	Token string
}

// ListCheckoutOptions returns payment providers that this runtime can use
// for new checkout against exactly one price ID or key, in routing order.
// It performs no remote provider probes and no writes; readiness means the
// active local account, required credentials, price link, checkout mode, and
// runtime services are all present.
func (s *CheckoutAttemptService) ListCheckoutOptions(ctx context.Context, priceID, productKey, priceKey string) ([]CheckoutOption, error) {
	if err := validateCheckoutPriceSelector(priceID, productKey, priceKey); err != nil {
		return nil, err
	}
	if s == nil || s.priceService == nil || s.productService == nil {
		return nil, fmt.Errorf("checkout rail options unavailable")
	}
	checkoutService, ok := s.checkoutService.(*CheckoutService)
	if !ok || checkoutService == nil || checkoutService.Rails == nil {
		return nil, fmt.Errorf("checkout rail options unavailable")
	}
	if s.config == nil || config.IsProviderReadOnly(s.config) {
		return []CheckoutOption{}, nil
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve checkout merchant: %w", err)
	}

	price, err := resolveCheckoutPrice(ctx, s.priceService, priceID, productKey, priceKey)
	if err != nil {
		return nil, fmt.Errorf("resolve checkout price: %w", err)
	}
	if price.MerchantID != merchantID.UUID() {
		return nil, fmt.Errorf("resolve checkout price: price not found")
	}
	if !price.IsPurchasable() {
		return []CheckoutOption{}, nil
	}
	product, err := s.productService.GetByID(ctx, price.ProductID)
	if err != nil {
		return nil, fmt.Errorf("resolve checkout product: %w", err)
	}
	if product.MerchantID != merchantID.UUID() {
		return nil, fmt.Errorf("resolve checkout product: product not found")
	}
	if !product.IsPurchasable() {
		return []CheckoutOption{}, nil
	}
	return s.listCheckoutOptionsForPrice(ctx, price, product)
}

// listCheckoutOptionsForPrice is a projection of the routing decision: the
// options a frontend may offer are exactly the candidates routing found
// eligible, in the order routing would pick them.
func (s *CheckoutAttemptService) listCheckoutOptionsForPrice(ctx context.Context, price *models.Price, product *models.Product) ([]CheckoutOption, error) {
	mode := checkoutModeForRail(price, "")
	decision, err := s.Route(ctx, RoutingInput{Price: price, Product: product, Mode: mode})
	if err != nil && !errors.Is(err, ErrNoRoutableProcessor) {
		return nil, err
	}
	eligible := decision.Eligible()
	if len(eligible) == 0 {
		// Nothing eligible AND something failed to resolve: report the failure
		// rather than an empty list that reads like "this merchant is unarmed".
		for _, candidate := range decision.Candidates {
			if candidate.Skip == models.CheckoutRoutingSkipResolveFailed {
				return nil, fmt.Errorf("resolve checkout rail %s: resolution failed", candidate.Selector)
			}
		}
		return []CheckoutOption{}, nil
	}
	options := make([]CheckoutOption, 0, len(eligible))
	for _, candidate := range decision.Candidates {
		if candidate.Skip != "" {
			continue
		}
		options = append(options, CheckoutOption{
			Selector: candidate.Selector,
			PSPID:    candidate.PSPID,
			Rail:     candidate.Rail,
			Mode:     string(mode),
			Token:    boundSettlementToken(price, candidate),
		})
	}
	return options, nil
}

// boundSettlementToken is the token a Solana sale must settle in: a price
// denominated in a token is paid in that token; otherwise its link fixes it,
// the published plan's mint for a subscription or a declared token.
func boundSettlementToken(price *models.Price, candidate RoutingCandidate) string {
	if candidate.Rail != string(models.RailSolana) {
		return ""
	}
	if units, ok := moneyutil.LookupCurrency(price.Currency); ok && units.Kind == "crypto" {
		return units.Code
	}
	var link map[string]string
	if candidate.PSPID != uuid.Nil {
		link = price.ForPSP(candidate.PSPID).PSPLinkForRail(models.RailSolana)
	}
	if link == nil {
		link = price.PSPLinkForRail(models.RailSolana)
	}
	for _, key := range []string{"mint_symbol", "token"} {
		if token := strings.ToUpper(strings.TrimSpace(link[key])); token != "" {
			return token
		}
	}
	return ""
}

func checkoutModeForRail(price *models.Price, rail string) models.CheckoutAttemptMode {
	_ = rail
	if price != nil && price.IsRecurring() {
		return models.CheckoutAttemptModeSubscription
	}
	return models.CheckoutAttemptModeOneOff
}

// checkoutRailSkipReason reports why this PSP cannot serve the price under mode,
// or "" when it can. It is the single readiness verdict behind both the option
// list and routing's fallback classes (or#288) — one place decides, so the
// advertised list and the routed choice can never disagree. Which sale kinds a
// rail supports is the rail registry's capability, never a list here (#1078).
func (s *CheckoutAttemptService) checkoutRailSkipReason(price *models.Price, target railTarget, providerConfig *config.ResolvedPSP, mode models.CheckoutAttemptMode) string {
	price = priceForCheckoutTarget(price, target)
	if price == nil || providerConfig == nil {
		return models.CheckoutRoutingSkipNotArmed
	}
	rail := models.Rail(target.Rail)
	if _, known := rails.Lookup(rail); !known {
		return models.CheckoutRoutingSkipUnknownSelector
	}
	if rail == models.RailStripe || rail == models.RailNMI || rail == models.RailCCBill {
		if err := moneyutil.RequireFiatCurrency(price.Currency); err != nil {
			return models.CheckoutRoutingSkipCurrencyUnsupported
		}
	}
	// Customer-selected deposits execute through the existing card-sale
	// intent, whose frozen amount is honored by NMI and Stripe.
	if price.CustomerAmount != nil && rail != models.RailNMI && rail != models.RailStripe {
		return models.CheckoutRoutingSkipModeUnsupported
	}
	subscription := mode == models.CheckoutAttemptModeSubscription
	if subscription && (price.Amount <= 0 || price.RecurringCycleHours() == nil) {
		return models.CheckoutRoutingSkipModeUnsupported
	}
	trial := price.TrialUnitAmount != nil || price.TrialDurationHours != nil
	if !rails.CanSellNew(rail, subscription, trial) {
		return models.CheckoutRoutingSkipModeUnsupported
	}
	if s.pspDisarmed != nil && target.Scope != nil && s.pspDisarmed(target.Scope.ID) {
		return models.CheckoutRoutingSkipPostureDisarmed
	}
	link := checkoutPSPLinkForTarget(price, target)
	switch rail {
	case models.RailStripe:
		if providerConfig.Stripe == nil || strings.TrimSpace(providerConfig.Stripe.SecretKey) == "" {
			return models.CheckoutRoutingSkipCredentialsMissing
		}
		return ""
	case models.RailNMI:
		// A one-time NMI sale is a direct gateway charge on the tokenized or
		// vaulted card: an armed PSP is enough, no provider plan (#1055).
		if providerConfig.NMI == nil || strings.TrimSpace(providerConfig.NMI.SecurityKey) == "" {
			return models.CheckoutRoutingSkipCredentialsMissing
		}
		return ""
	case models.RailSolana:
		if providerConfig.Solana == nil || len(providerConfig.Solana.Tokens) == 0 {
			return models.CheckoutRoutingSkipCredentialsMissing
		}
		if link == nil {
			return models.CheckoutRoutingSkipLinkMissing
		}
		if units, ok := moneyutil.LookupCurrency(price.Currency); ok && units.Kind == "crypto" {
			if subscription {
				return models.CheckoutRoutingSkipCurrencyUnsupported
			}
			if _, ok := providerConfig.Solana.Tokens[units.Code]; !ok {
				return models.CheckoutRoutingSkipCurrencyUnsupported
			}
		}
		if subscription {
			if s.solanaPrepareSubscribe == nil || s.solanaEnroll == nil {
				return models.CheckoutRoutingSkipServiceUnavailable
			}
			// The subscriber signs into the price's published on-chain plan.
			targetTerms, targetErr := parseSolanaPlanTerms(link)
			executionTerms, executionErr := parseSolanaPlanTerms(price.PSPLinkForRail(models.RailSolana))
			if targetErr != nil || executionErr != nil || targetTerms != executionTerms {
				return models.CheckoutRoutingSkipLinkMissing
			}
			return ""
		}
		if s.solanaPayService == nil && s.solanaTransactionService == nil {
			return models.CheckoutRoutingSkipServiceUnavailable
		}
		return ""
	default:
		// A rail the registry lets sell but checkout cannot execute.
		return models.CheckoutRoutingSkipModeUnsupported
	}
}

// checkoutPSPLinkForTarget resolves the price link for the same account chosen
// for credentials. It never substitutes another link from the same rail.
func checkoutPSPLinkForTarget(price *models.Price, target railTarget) map[string]string {
	if price == nil {
		return nil
	}
	if target.Scope != nil && target.Scope.ID != uuid.Nil {
		for _, link := range price.PSPLinks {
			if link[models.RailKeyPSPID] == target.Scope.ID.String() && link[models.RailKeyRail] == target.Rail {
				return link
			}
		}
	}
	lookup := func(key string) map[string]string {
		link := price.PSPLinks[key]
		if link == nil || (link[models.RailKeyPSPID] != "" && (target.Scope == nil || link[models.RailKeyPSPID] != target.Scope.ID.String())) || !strings.EqualFold(strings.TrimSpace(link[models.RailKeyRail]), target.Rail) {
			return nil
		}
		return link
	}
	if link := lookup(target.PSP); link != nil {
		return link
	}
	return nil
}

// priceForCheckoutTarget freezes the provider plan selected alongside credentials.
func priceForCheckoutTarget(price *models.Price, target railTarget) *models.Price {
	if price == nil {
		return nil
	}
	copy := *price
	copy.PSPLinks = nil
	if link := checkoutPSPLinkForTarget(price, target); link != nil {
		copy.PSPLinks = map[string]map[string]string{target.PSP: link}
	}
	return &copy
}
