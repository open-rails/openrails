package checkout

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/pkg/merchant"
)

// CheckoutRailOption is a locally ready payment-provider choice for a price.
// Selector is the exact value checkout accepts, PSPID is the provider's stable
// internal identity, and Rail is the canonical gateway.
type CheckoutRailOption struct {
	Selector string
	PSPID    uuid.UUID
	Rail     string
	Mode     string
}

// ListCheckoutRailOptions returns payment providers that this runtime can use
// for new checkout against priceRef, IN THE MERCHANT'S ROUTING ORDER (or#288).
// It performs no remote provider probes and no writes; readiness means the
// active local account, required credentials, price link, checkout mode, and
// runtime services are all present.
func (s *CheckoutSessionService) ListCheckoutRailOptions(ctx context.Context, priceRef string) ([]CheckoutRailOption, error) {
	priceRef = strings.TrimSpace(priceRef)
	if priceRef == "" {
		return nil, fmt.Errorf("price reference is required")
	}
	if s == nil || s.priceService == nil || s.productService == nil {
		return nil, fmt.Errorf("checkout rail options unavailable")
	}
	checkoutService, ok := s.checkoutService.(*CheckoutService)
	if !ok || checkoutService == nil || checkoutService.Rails == nil {
		return nil, fmt.Errorf("checkout rail options unavailable")
	}
	if s.config == nil || s.config.IsProviderReadOnly() {
		return []CheckoutRailOption{}, nil
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve checkout merchant: %w", err)
	}

	price, err := catalog.ResolveReference(ctx, s.priceService, priceRef)
	if err != nil {
		return nil, fmt.Errorf("resolve checkout price: %w", err)
	}
	if price.MerchantID != merchantID.UUID() {
		return nil, fmt.Errorf("resolve checkout price: price not found")
	}
	if !price.IsPurchasable() {
		return []CheckoutRailOption{}, nil
	}
	product, err := s.productService.GetByID(ctx, price.ProductID)
	if err != nil {
		return nil, fmt.Errorf("resolve checkout product: %w", err)
	}
	if product.MerchantID != merchantID.UUID() {
		return nil, fmt.Errorf("resolve checkout product: product not found")
	}
	if !product.IsPurchasable() {
		return []CheckoutRailOption{}, nil
	}
	return s.listCheckoutRailOptionsForPrice(ctx, price, product)
}

// listCheckoutRailOptionsForPrice is a projection of the routing decision: the
// options a frontend may offer are exactly the candidates routing found
// eligible, in the order routing would pick them.
func (s *CheckoutSessionService) listCheckoutRailOptionsForPrice(ctx context.Context, price *models.Price, product *models.Product) ([]CheckoutRailOption, error) {
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
		return []CheckoutRailOption{}, nil
	}
	options := make([]CheckoutRailOption, 0, len(eligible))
	for _, candidate := range decision.Candidates {
		if candidate.Skip != "" {
			continue
		}
		options = append(options, CheckoutRailOption{
			Selector: candidate.Selector,
			PSPID:    candidate.PSPID,
			Rail:     candidate.Rail,
			Mode:     string(mode),
		})
	}
	return options, nil
}

func checkoutModeForRail(price *models.Price, rail string) models.CheckoutSessionMode {
	_ = rail
	if price != nil && price.AutoRenew {
		return models.CheckoutSessionModeSubscription
	}
	return models.CheckoutSessionModeOneOff
}

// checkoutRailSkipReason reports why this PSP cannot serve the price under mode,
// or "" when it can. It is the single readiness verdict behind both the option
// list and routing's fallback classes (or#288) — one place decides, so the
// advertised list and the routed choice can never disagree.
func (s *CheckoutSessionService) checkoutRailSkipReason(price *models.Price, target railTarget, providerConfig *config.PSPConfig, mode models.CheckoutSessionMode) string {
	if price == nil || providerConfig == nil {
		return models.CheckoutRoutingSkipNotArmed
	}
	switch target.Rail {
	case string(models.RailStripe):
		if providerConfig.Stripe == nil || strings.TrimSpace(providerConfig.Stripe.SecretKey) == "" {
			return models.CheckoutRoutingSkipCredentialsMissing
		}
		if stripePaidIntroUnsupported(price) {
			return models.CheckoutRoutingSkipModeUnsupported
		}
		targetPriceID := strings.TrimSpace(checkoutPSPLinkForTarget(price, target)[models.RailKeyStripePriceID])
		executionPriceID, err := getStripePriceID(price)
		if err != nil || targetPriceID == "" || targetPriceID != executionPriceID {
			return models.CheckoutRoutingSkipLinkMissing
		}
		return ""
	case string(models.RailNMI):
		if providerConfig.NMI == nil || strings.TrimSpace(providerConfig.NMI.SecurityKey) == "" {
			return models.CheckoutRoutingSkipCredentialsMissing
		}
		if checkoutPSPLinkForTarget(price, target) == nil {
			return models.CheckoutRoutingSkipLinkMissing
		}
		if mode == models.CheckoutSessionModeSubscription {
			if _, err := requireNMIPlanForTarget(price, target); err != nil {
				return models.CheckoutRoutingSkipLinkMissing
			}
		}
		return ""
	case string(models.RailCCBill):
		if mode != models.CheckoutSessionModeSubscription {
			return models.CheckoutRoutingSkipModeUnsupported
		}
		// The dash-joined composite account id (#697) is CCBill's identity, so a
		// malformed one is a credential fault, not a link fault.
		if providerConfig.CCBill == nil {
			return models.CheckoutRoutingSkipCredentialsMissing
		}
		if _, _, err := config.SplitCCBillAccountID(providerConfig.EffectiveAccountID()); err != nil {
			return models.CheckoutRoutingSkipCredentialsMissing
		}
		link := checkoutPSPLinkForTarget(price, target)
		targetForm := strings.TrimSpace(link[models.RailKeyCCBillFormName])
		targetFlexID := strings.TrimSpace(link[models.RailKeyCCBillFlexID])
		executionForm, executionFlexID, ok := price.GetCCBillFlexForm()
		if !ok || targetForm == "" || targetFlexID == "" ||
			targetForm != executionForm || targetFlexID != executionFlexID {
			return models.CheckoutRoutingSkipLinkMissing
		}
		return ""
	case string(models.RailSolana):
		if providerConfig.Solana == nil || len(providerConfig.Solana.Tokens) == 0 {
			return models.CheckoutRoutingSkipCredentialsMissing
		}
		if checkoutPSPLinkForTarget(price, target) == nil {
			return models.CheckoutRoutingSkipLinkMissing
		}
		if mode == models.CheckoutSessionModeSubscription {
			if s.solanaPrepareSubscribe == nil || s.solanaEnroll == nil {
				return models.CheckoutRoutingSkipServiceUnavailable
			}
			targetTerms, targetErr := parseSolanaPlanTerms(checkoutPSPLinkForTarget(price, target))
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
		return models.CheckoutRoutingSkipUnknownSelector
	}
}

// checkoutPSPLinkForTarget resolves the price link for the same account chosen
// for credentials. It never substitutes another link from the same rail.
func checkoutPSPLinkForTarget(price *models.Price, target railTarget) map[string]string {
	if price == nil {
		return nil
	}
	lookup := func(key string) map[string]string {
		link := price.PSPLinks[key]
		if link == nil || !strings.EqualFold(strings.TrimSpace(link[models.RailKeyRail]), target.Rail) {
			return nil
		}
		return link
	}
	if link := lookup(target.PSP); link != nil {
		return link
	}
	return nil
}
