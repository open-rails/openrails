package checkout

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
)

// CheckoutRailOption is a locally ready payment-provider choice for a price.
// Selector is the exact value checkout accepts; Rail is the canonical gateway.
type CheckoutRailOption struct {
	Selector string
	Rail     string
	Mode     string
}

var checkoutRailOptionOrder = []string{
	string(models.RailStripe),
	string(models.RailNMI),
	string(models.RailCCBill),
	string(models.RailSolana),
}

// ListCheckoutRailOptions returns payment providers that this runtime can use
// for new checkout against priceRef. It performs no remote provider probes and
// no writes; readiness means the active local account, required credentials,
// price link, checkout mode, and runtime services are all present.
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
	return s.listCheckoutRailOptionsForPrice(ctx, checkoutService, price)
}

func (s *CheckoutSessionService) listCheckoutRailOptionsForPrice(ctx context.Context, checkoutService *CheckoutService, price *models.Price) ([]CheckoutRailOption, error) {
	options := make([]CheckoutRailOption, 0, len(checkoutRailOptionOrder))
	var resolutionErr error
	for _, requestedRail := range checkoutRailOptionOrder {
		target, err := checkoutService.resolveRailTarget(ctx, requestedRail)
		if err != nil {
			resolutionErr = errors.Join(resolutionErr, fmt.Errorf("resolve checkout rail %s: %w", requestedRail, err))
			continue
		}
		accountID := ""
		if target.Scope != nil {
			accountID = target.Scope.AccountID
		}
		providerConfig, err := checkoutService.Rails.RailConfig(ctx, target.Rail, accountID)
		if err != nil {
			if errors.Is(err, railresolve.ErrRailNotArmed) {
				continue
			}
			resolutionErr = errors.Join(resolutionErr, fmt.Errorf("resolve checkout rail config %s: %w", requestedRail, err))
			continue
		}

		mode := checkoutModeForRail(price, target.Rail)
		if !s.checkoutRailOptionReady(price, target, providerConfig, mode) {
			continue
		}
		options = append(options, CheckoutRailOption{
			Selector: target.Rail,
			Rail:     target.Rail,
			Mode:     string(mode),
		})
	}
	if len(options) == 0 && resolutionErr != nil {
		return nil, resolutionErr
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

func (s *CheckoutSessionService) checkoutRailOptionReady(price *models.Price, target railTarget, providerConfig *config.PSPConfig, mode models.CheckoutSessionMode) bool {
	if price == nil || providerConfig == nil {
		return false
	}
	switch target.Rail {
	case string(models.RailStripe):
		if providerConfig.Stripe == nil || strings.TrimSpace(providerConfig.Stripe.SecretKey) == "" || stripePaidIntroUnsupported(price) {
			return false
		}
		targetPriceID := strings.TrimSpace(checkoutPSPLinkForTarget(price, target)[models.RailKeyStripePriceID])
		executionPriceID, err := getStripePriceID(price)
		return err == nil && targetPriceID != "" && targetPriceID == executionPriceID
	case string(models.RailNMI):
		if providerConfig.NMI == nil || strings.TrimSpace(providerConfig.NMI.SecurityKey) == "" {
			return false
		}
		if checkoutPSPLinkForTarget(price, target) == nil {
			return false
		}
		if mode == models.CheckoutSessionModeSubscription {
			_, err := requireNMIPlanForTarget(price, target)
			return err == nil
		}
		return true
	case string(models.RailCCBill):
		if mode != models.CheckoutSessionModeSubscription || providerConfig.CCBill == nil {
			return false
		}
		if _, _, err := config.SplitCCBillAccountID(providerConfig.EffectiveAccountID()); err != nil {
			return false
		}
		link := checkoutPSPLinkForTarget(price, target)
		targetForm := strings.TrimSpace(link[models.RailKeyCCBillFormName])
		targetFlexID := strings.TrimSpace(link[models.RailKeyCCBillFlexID])
		executionForm, executionFlexID, ok := price.GetCCBillFlexForm()
		return ok && targetForm != "" && targetFlexID != "" &&
			targetForm == executionForm && targetFlexID == executionFlexID
	case string(models.RailSolana):
		if providerConfig.Solana == nil || len(providerConfig.Solana.Tokens) == 0 {
			return false
		}
		if checkoutPSPLinkForTarget(price, target) == nil {
			return false
		}
		if mode == models.CheckoutSessionModeSubscription {
			if s.solanaPrepareSubscribe == nil || s.solanaEnroll == nil {
				return false
			}
			targetTerms, targetErr := parseSolanaPlanTerms(checkoutPSPLinkForTarget(price, target))
			executionTerms, executionErr := parseSolanaPlanTerms(price.PSPLinkForRail(models.RailSolana))
			return targetErr == nil && executionErr == nil && targetTerms == executionTerms
		}
		return s.solanaPayService != nil || s.solanaTransactionService != nil
	default:
		return false
	}
}

// checkoutPSPLinkForTarget resolves the price link for the same account chosen
// for credentials. A rail-named fallback preserves single-account manifests.
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
	if target.Scope == nil && target.PSP != target.Rail {
		return lookup(target.Rail)
	}
	return nil
}
