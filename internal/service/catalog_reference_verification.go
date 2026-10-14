package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/internal/merchant"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

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

type catalogReferenceChainReader interface {
	GetAccountData(context.Context, solanago.PublicKey) ([]byte, error)
}

// verifySolanaCatalogReference reads a published plan back on-chain: it must
// be the cranker's plan at its program address, pay the merchant's treasury
// and match the price's period and amount. It returns the stored reference.
func verifySolanaCatalogReference(ctx context.Context, plan *recurring.PlanService, reader catalogReferenceChainReader, req billing.CreatePriceParams, link map[string]string) (map[string]string, error) {
	if err := catalogReferenceKeys(link, solanaKeyPlanPDA, solanaKeyPlanID, solanaKeyMint, solanaKeyToken, solanaKeyMintSymbol, solanaKeyAmountBaseUnits, solanaKeyPeriodHours, solanaKeyCreatedAt, solanaKeyMerchant, solanaKeyTreasury); err != nil {
		return nil, err
	}
	if req.BillingIntervalHours == nil {
		for key, value := range link {
			if key != models.RailKeyRail && key != models.RailKeyProvider && strings.TrimSpace(value) != "" {
				return nil, apperr.Invalidf("one-time Solana prices take no plan reference")
			}
		}
		// One-time settlement is a direct transfer; no on-chain plan exists to
		// verify or create. Preserve the declared rail instead of dropping it.
		return map[string]string{models.RailKeyProvider: "solana"}, nil
	}
	if req.TrialUnitAmount != nil || req.TrialDurationHours != nil || *req.BillingIntervalHours <= 0 {
		return nil, apperr.Invalidf("Solana recurring references require a positive period without a trial")
	}
	if plan == nil || reader == nil {
		return nil, fmt.Errorf("Solana plan and mint readers must be configured before applying recurring references")
	}
	if !strings.EqualFold(req.Currency, "USD") {
		return nil, apperr.Invalidf("Solana recurring references require USD billing")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	// The plan's owner: the one its reference names, else the cranker that
	// publishes new plans.
	var owner solanago.PublicKey
	if text := strings.TrimSpace(link[solanaKeyMerchant]); text != "" {
		if owner, err = solanago.PublicKeyFromBase58(text); err != nil {
			return nil, apperr.Invalidf("invalid Solana merchant_address")
		}
	} else if owner, err = plan.Publisher(ctx); err != nil {
		return nil, fmt.Errorf("resolve the Solana cranker: %w", err)
	}
	pdaText := strings.TrimSpace(link[solanaKeyPlanPDA])
	selectedToken := strings.ToUpper(strings.TrimSpace(link[solanaKeyToken]))
	var declaredPlanID *uint64
	if text := strings.TrimSpace(link[solanaKeyPlanID]); text != "" {
		id, err := strconv.ParseUint(text, 10, 64)
		if err != nil {
			return nil, apperr.Invalidf("Solana plan_id must be an unsigned 64-bit decimal integer")
		}
		declaredPlanID = &id
	}
	if pdaText == "" {
		if declaredPlanID == nil {
			return nil, apperr.Invalidf("a Solana plan_pda or plan_id is required")
		}
		// The program address depends only on the selected merchant and this
		// explicit provider ID, never on mutable catalog labels or money terms.
		address, _, err := subscriptions.DerivePlanPDA(owner, *declaredPlanID)
		if err != nil {
			return nil, err
		}
		pdaText = address.String()
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
		return nil, apperr.Invalidf("Solana plan %s is missing on-chain", pdaText)
	}
	account, err := subscriptions.DecodePlanAccount(raw)
	if err != nil {
		return nil, apperr.Invalidf("referenced Solana account is not a valid subscription plan")
	}
	if declaredPlanID != nil && account.PlanID != *declaredPlanID {
		return nil, apperr.Invalidf("Solana plan_id does not match the verified plan")
	}
	derived, bump, err := subscriptions.DerivePlanPDA(owner, account.PlanID)
	if err != nil {
		return nil, err
	}
	if account.Owner != owner || derived != pda || account.Bump != bump {
		return nil, apperr.Invalidf("Solana plan is not owned by %s at its program address", owner)
	}
	if err := plan.Owns(ctx, owner); err != nil {
		return nil, apperr.Invalidf("Solana plan %s: %v", pdaText, err)
	}
	if err := plan.PaysInto(ctx, mid, pda, account.Destinations); err != nil {
		return nil, apperr.Invalidf("%v", err)
	}
	if account.PeriodHours != uint64(*req.BillingIntervalHours) || account.EndTs != 0 || (!req.Archived && account.Status != subscriptions.PlanStatusActive) {
		return nil, apperr.Invalidf("Solana plan period, expiry or active state does not match the catalog")
	}
	token, err := resolveSolanaTokenFromMint(ctx, plan, account.Mint.String())
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
	out := map[string]string{solanaKeyPlanPDA: pdaText, solanaKeyPlanID: strconv.FormatUint(account.PlanID, 10), solanaKeyMint: account.Mint.String(), solanaKeyAmountBaseUnits: strconv.FormatUint(account.Amount, 10), solanaKeyPeriodHours: strconv.FormatUint(account.PeriodHours, 10), solanaKeyCreatedAt: strconv.FormatInt(account.CreatedAt, 10), solanaKeyMerchant: owner.String(), solanaKeyMintSymbol: token, solanaKeyToken: token}
	if !account.Destinations[0].IsZero() {
		out[solanaKeyTreasury] = account.Destinations[0].String()
	}
	for key, value := range link {
		if key == models.RailKeyRail || key == models.RailKeyProvider || key == solanaKeyToken || key == solanaKeyPlanID {
			continue
		}
		if strings.TrimSpace(value) != out[key] {
			return nil, apperr.Invalidf("Solana %s does not match the verified plan", key)
		}
	}
	return out, nil
}
