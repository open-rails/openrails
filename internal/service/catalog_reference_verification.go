package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/internal/modules/catalog"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// verifyCatalogProviderReference prepares existing references before acquiring
// the catalog transaction. The caller must revalidate its merchant revision and
// selected PSP identity before committing these results. Nothing here creates,
// updates, signs, or submits a provider object.
func (s *Service) verifyCatalogProviderReference(ctx context.Context, providerKey, rail, accountID, productKey string, req CreatePriceRequest, link map[string]string) (map[string]string, error) {
	if s == nil || s.catalogWriteLocked || s.catalogTx != nil {
		return nil, fmt.Errorf("provider reference verification must run outside the catalog transaction")
	}
	var out map[string]string
	var err error
	switch rail {
	case "nmi":
		out, err = verifyNMICatalogReference(ctx, &nmiAdapter{svc: s}, accountID, req, link)
	case "stripe":
		out, err = verifyStripeCatalogReference(ctx, &stripeAdapter{svc: s}, accountID, req, link)
	case "solana":
		if s.rt == nil {
			return nil, fmt.Errorf("Solana catalog reader is unavailable")
		}
		// Recurring PlanService resolves the merchant's configured signer. It must
		// not verify a primary wallet and then bind the result to a secondary PSP.
		if accountID != "" && s.rt.RailConfigs != nil {
			primary, resolveErr := s.rt.RailConfigs.RailConfig(ctx, "solana", "")
			if resolveErr != nil {
				return nil, resolveErr
			}
			if primary == nil || primary.EffectiveAccountID() != accountID {
				return nil, apperr.Invalidf("Solana catalog references require the configured primary account")
			}
		}
		var reader catalogReferenceChainReader
		if s.rt.SolanaRPCResolver != nil {
			reader = s.rt.SolanaRPCResolver.ChainReader()
		}
		out, err = verifySolanaCatalogReference(ctx, s.rt.SolanaPlanService, reader, (&solanaAdapter{svc: s}).defaultRecurringToken(), productKey, req, link)
	case "ccbill":
		out, err = declaredCCBillCatalogReference(link)
	default:
		return nil, apperr.Invalidf("unsupported catalog reference rail %q", rail)
	}
	if err != nil {
		return nil, fmt.Errorf("catalog PSP %q: %w", providerKey, err)
	}
	if given := strings.TrimSpace(link[models.RailKeyRail]); given != "" && given != rail {
		return nil, apperr.Invalidf("catalog PSP %q rail does not match its selected account", providerKey)
	}
	if given := strings.TrimSpace(link[models.RailKeyProvider]); given != "" && given != providerKey && given != rail {
		return nil, apperr.Invalidf("catalog PSP %q provider does not match its selected account", providerKey)
	}
	out[models.RailKeyRail] = rail
	return out, nil
}

func catalogReferenceKeys(link map[string]string, allowed ...string) error {
	for key := range link {
		if key == models.RailKeyRail || key == models.RailKeyProvider {
			continue
		}
		found := false
		for _, name := range allowed {
			found = found || key == name
		}
		if !found {
			return apperr.Invalidf("unsupported catalog reference field %q", key)
		}
	}
	return nil
}

func verifyNMICatalogReference(ctx context.Context, adapter *nmiAdapter, accountID string, req CreatePriceRequest, link map[string]string) (map[string]string, error) {
	if err := catalogReferenceKeys(link, models.RailKeyPlanID); err != nil {
		return nil, err
	}
	planID := strings.TrimSpace(link[models.RailKeyPlanID])
	if planID == "" {
		return nil, apperr.Invalidf("an existing NMI plan_id is required; create plans in the separate provider workflow")
	}
	if !req.AutoRenew || req.AccessDurationHours == nil || *req.AccessDurationHours <= 0 || *req.AccessDurationHours%24 != 0 || req.TrialUnitAmount != nil || req.TrialDurationHours != nil {
		return nil, apperr.Invalidf("NMI references require recurring whole-day terms without a trial")
	}
	client, key, ok := adapter.nmiClientFor(ctx, accountID)
	if !ok {
		return nil, fmt.Errorf("NMI account is not configured for reference verification")
	}
	detail, err := client.GetRecurringPlanDetailByID(ctx, planID, req.Currency)
	if err != nil {
		return nil, err
	}
	if !detail.Found {
		return nil, apperr.Invalidf("NMI plan %q is missing; create it through the separate provider workflow", planID)
	}
	amount, err := moneyutil.RailMinorToNative(req.Currency, moneyutil.Cents(detail.AmountCents))
	if err != nil {
		return nil, err
	}
	if amount != req.UnitAmount || detail.DayFrequency != *req.AccessDurationHours/24 {
		return nil, apperr.Invalidf("NMI plan %q does not match the catalog amount and day frequency", planID)
	}
	if detail.ID != planID || detail.Payments == nil || *detail.Payments != 0 {
		return nil, apperr.Invalidf("NMI plan %q must confirm its identity and an open-ended payment schedule", planID)
	}
	return map[string]string{models.RailKeyPlanID: planID, models.RailKeyProvider: key}, nil
}

func verifyStripeCatalogReference(ctx context.Context, adapter *stripeAdapter, accountID string, req CreatePriceRequest, link map[string]string) (map[string]string, error) {
	if err := catalogReferenceKeys(link, models.RailKeyStripePriceID, models.RailKeyStripeProductID, providerLookupKey); err != nil {
		return nil, err
	}
	id := strings.TrimSpace(link[models.RailKeyStripePriceID])
	if id == "" {
		return nil, apperr.Invalidf("an existing Stripe price_id is required; lookup-key creation is a separate provider workflow")
	}
	if req.TrialUnitAmount != nil || req.TrialDurationHours != nil {
		return nil, apperr.Invalidf("Stripe trial offers require their separate provider workflow")
	}
	client, ok := adapter.stripeServiceFor(ctx, accountID)
	if !ok {
		return nil, fmt.Errorf("Stripe account is not configured for reference verification")
	}
	remote, found, err := client.FindPrice(ctx, id)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, apperr.Invalidf("Stripe price %q is missing; provision it through the separate provider workflow", id)
	}
	amount, err := moneyutil.NativeToRailMinorExact(req.Currency, req.UnitAmount)
	if err != nil {
		return nil, err
	}
	if remote.ID != id || remote.Product == "" || remote.UnitAmount != int64(amount) || !strings.EqualFold(remote.Currency, req.Currency) || (!req.Archived && !remote.Active) {
		return nil, apperr.Invalidf("Stripe price does not match catalog identity, amount, currency or active state")
	}
	if req.AutoRenew {
		if req.AccessDurationHours == nil || *req.AccessDurationHours <= 0 || *req.AccessDurationHours%24 != 0 {
			return nil, apperr.Invalidf("Stripe recurring references require whole-day duration")
		}
		interval, count := catalog.StripeIntervalForDays(*req.AccessDurationHours / 24)
		if remote.Recurring == nil || remote.Recurring.Interval != interval || remote.Recurring.Count != count {
			return nil, apperr.Invalidf("Stripe price recurring terms do not match the catalog")
		}
	} else if remote.Recurring != nil {
		return nil, apperr.Invalidf("Stripe price is recurring but the catalog offer is one-time")
	}
	out := map[string]string{models.RailKeyStripePriceID: id, models.RailKeyStripeProductID: remote.Product}
	if remote.LookupKey != "" {
		out[providerLookupKey] = remote.LookupKey
	}
	for _, field := range []string{models.RailKeyStripeProductID, providerLookupKey} {
		if value := strings.TrimSpace(link[field]); value != "" && value != out[field] {
			return nil, apperr.Invalidf("Stripe price %s differs from the declared reference", field)
		}
	}
	return out, nil
}

type catalogReferenceChainReader interface {
	GetAccountData(context.Context, solanago.PublicKey) ([]byte, error)
}

func verifySolanaCatalogReference(ctx context.Context, plan *recurring.PlanService, reader catalogReferenceChainReader, defaultToken, productKey string, req CreatePriceRequest, link map[string]string) (map[string]string, error) {
	if err := catalogReferenceKeys(link, solanaKeyPlanPDA, solanaKeyPlanID, solanaKeyMint, solanaKeyToken, solanaKeyMintSymbol, solanaKeyAmountBaseUnits, solanaKeyPeriodHours, solanaKeyCreatedAt, solanaKeyMerchant); err != nil {
		return nil, err
	}
	if !req.AutoRenew {
		for key, value := range link {
			if key != models.RailKeyRail && key != models.RailKeyProvider && strings.TrimSpace(value) != "" {
				return nil, apperr.Invalidf("one-time Solana offers take no plan reference")
			}
		}
		// One-time settlement is a direct transfer; no on-chain plan exists to
		// verify or create. Preserve the declared rail instead of dropping it.
		return map[string]string{models.RailKeyProvider: "solana"}, nil
	}
	if req.TrialUnitAmount != nil || req.TrialDurationHours != nil || req.AccessDurationHours == nil || *req.AccessDurationHours <= 0 {
		return nil, apperr.Invalidf("Solana recurring references require a positive period without a trial")
	}
	if plan == nil || reader == nil {
		return nil, fmt.Errorf("Solana plan and mint readers must be configured before applying recurring references")
	}
	if !strings.EqualFold(req.Currency, "USD") && !strings.EqualFold(req.Currency, "USDC") {
		return nil, apperr.Invalidf("Solana recurring references require USD billing or legacy USDC")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	owner, err := plan.MerchantAddress(ctx, mid)
	if err != nil {
		return nil, fmt.Errorf("resolve Solana merchant: %w", err)
	}
	pdaText := strings.TrimSpace(link[solanaKeyPlanPDA])
	selectedToken := strings.ToUpper(strings.TrimSpace(link[solanaKeyToken]))
	if pdaText == "" {
		if selectedToken == "" {
			selectedToken = defaultToken
		}
		mint, err := plan.ResolveMint(selectedToken)
		if err != nil {
			return nil, err
		}
		planID := solanaPlanID(productKey, req.Currency, req.UnitAmount, req.AccessDurationHours, mint)
		pda, _, err := subscriptions.DerivePlanPDA(owner, planID)
		if err != nil {
			return nil, err
		}
		pdaText = pda.String()
	}
	pda, err := solanago.PublicKeyFromBase58(pdaText)
	if err != nil {
		return nil, apperr.Invalidf("invalid Solana plan_pda")
	}
	raw, err := reader.GetAccountData(ctx, pda)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return nil, apperr.Invalidf("Solana plan %s is missing; provision it through the separate provider workflow before applying this artifact", pdaText)
	}
	account, err := subscriptions.DecodePlanAccount(raw)
	if err != nil {
		return nil, apperr.Invalidf("referenced Solana account is not a valid subscription plan")
	}
	derived, bump, err := subscriptions.DerivePlanPDA(owner, account.PlanID)
	if err != nil {
		return nil, err
	}
	if account.Owner != owner || derived != pda || account.Bump != bump {
		return nil, apperr.Invalidf("Solana plan does not belong to the selected merchant and program address")
	}
	if account.PeriodHours != uint64(*req.AccessDurationHours) || account.EndTs != 0 || (!req.Archived && account.Status != subscriptions.PlanStatusActive) {
		return nil, apperr.Invalidf("Solana plan period, expiry or active state does not match the catalog")
	}
	token, err := resolveSolanaTokenFromMint(plan, account.Mint.String())
	if err != nil {
		return nil, err
	}
	if selectedToken != "" && token != selectedToken {
		return nil, apperr.Invalidf("Solana plan mint differs from the declared settlement token")
	}
	decimals, err := plan.MintDecimals(ctx, token)
	if err != nil {
		return nil, err
	}
	amount, err := solanamodule.FiatMicrosToBaseUnitsAtPeg(moneyutil.Micros(req.UnitAmount), token, decimals)
	if err != nil {
		return nil, err
	}
	if account.Amount != amount {
		return nil, apperr.Invalidf("Solana plan amount does not match the catalog")
	}
	out := map[string]string{solanaKeyPlanPDA: pdaText, solanaKeyPlanID: strconv.FormatUint(account.PlanID, 10), solanaKeyMint: account.Mint.String(), solanaKeyAmountBaseUnits: strconv.FormatUint(account.Amount, 10), solanaKeyPeriodHours: strconv.FormatUint(account.PeriodHours, 10), solanaKeyCreatedAt: strconv.FormatInt(account.CreatedAt, 10), solanaKeyMerchant: owner.String(), solanaKeyMintSymbol: token}
	for key, value := range link {
		if key == models.RailKeyRail || key == models.RailKeyProvider || key == solanaKeyToken {
			continue
		}
		if strings.TrimSpace(value) != out[key] {
			return nil, apperr.Invalidf("Solana %s does not match the verified plan", key)
		}
	}
	return out, nil
}

// CCBill provides no reference-read API. These remain explicitly operator-owned
// form identifiers, matching the existing local declaration contract.
func declaredCCBillCatalogReference(link map[string]string) (map[string]string, error) {
	if err := catalogReferenceKeys(link, models.RailKeyCCBillFormName, models.RailKeyCCBillFlexID, models.RailKeyCCBillRecurringBillingOption); err != nil {
		return nil, err
	}
	out := map[string]string{}
	form, flex, option := strings.TrimSpace(link[models.RailKeyCCBillFormName]), strings.TrimSpace(link[models.RailKeyCCBillFlexID]), strings.TrimSpace(link[models.RailKeyCCBillRecurringBillingOption])
	if (form == "") != (flex == "") {
		return nil, apperr.Invalidf("CCBill form_name and flex_id must be declared together")
	}
	if form != "" {
		out[models.RailKeyCCBillFormName] = form
		out[models.RailKeyCCBillFlexID] = flex
	}
	if option != "" {
		out[models.RailKeyCCBillRecurringBillingOption] = option
	}
	if len(out) == 0 {
		return nil, apperr.Invalidf("an existing CCBill form or billing-option identifier is required")
	}
	return out, nil
}
