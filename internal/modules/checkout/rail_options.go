package checkout

import (
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

func checkoutModeForRail(price *models.Price, rail string) models.CheckoutAttemptMode {
	_ = rail
	if price != nil && price.IsRecurring() {
		return models.CheckoutAttemptModeSubscription
	}
	return models.CheckoutAttemptModeOneOff
}

// checkoutRailSkipReason reports why this PSP cannot serve the price under
// mode, or "" when it can. It is the one readiness verdict behind both the
// option list and routing's fallback classes, so they never disagree. Which
// sale kinds a rail supports is the rail registry's capability.
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
		// vaulted card: an armed PSP is enough, no provider plan.
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
