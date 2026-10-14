package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/internal/merchant"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// A recurring price of ours sells on Solana through an on-chain Plan of the
// Subscriptions program (De1egAFMkMWZSN5rYXRj9CAdheBamobVNubTsi9avR44),
// published from its terms in USDC, owned by the deployment's first cranker
// and paying the merchant's treasury. Plan terms are immutable on-chain, so
// new terms are a new price and a new plan. One-time prices need no plan.

// Solana rail-config keys (mirror recurring.PlanHandle.ToRailConfig).
const (
	solanaDefaultRecurringToken = solanatokens.USDC
	solanaKeyPlanPDA            = "plan_pda"
	solanaKeyPlanID             = "plan_id"
	solanaKeyMint               = "mint"
	solanaKeyToken              = "token"
	solanaKeyMintSymbol         = "mint_symbol"
	solanaKeyAmountBaseUnits    = "amount_base_units"
	solanaKeyPeriodHours        = "period_hours"
	solanaKeyCreatedAt          = "created_at"
	solanaKeyMerchant           = "merchant_address"
	solanaKeyTreasury           = "treasury"
)

// solanaPlanID addresses an immutable local price, the chosen token mint and
// the treasury the plan pays: a new treasury takes a new plan. Existing
// bindings retain their stored plan PDA; only new publications use it.
func solanaPlanID(priceID uuid.UUID, mint string, treasury solanago.PublicKey) uint64 {
	sum := sha256.Sum256([]byte(priceID.String() + ":" + strings.TrimSpace(mint) + ":" + treasury.String()))
	return binary.BigEndian.Uint64(sum[:8])
}

// solanaPlanRefusal is why a price of ours cannot sell on a Solana plan, ""
// when it can: a recurring, USD, trial-free, positive price. SOL prices sell
// one-time only.
func solanaPlanRefusal(req billing.CreatePriceParams) string {
	switch {
	case req.BillingIntervalHours == nil:
		return ""
	case !strings.EqualFold(req.Currency, "USD"):
		return "a recurring Solana price bills in USD (USDC); SOL and other currencies sell one-time only"
	case req.TrialUnitAmount != nil || req.TrialDurationHours != nil:
		return "a Solana plan bills one amount every period, so it runs no trial"
	case req.UnitAmount <= 0:
		return "a Solana plan needs a positive amount"
	case req.CustomerAmount != nil || req.Quantity != nil:
		return "a Solana plan bills one fixed amount"
	}
	return ""
}

// publishSolanaPlan finds or publishes the plan of a recurring price of ours
// and returns its stored reference. The plan id derives from the stored price
// id, mint and treasury, so a re-apply after a failed commit reattaches the
// same plan. Limited and readonly provider_write_mode only attach a plan
// already published.
func (s *Service) publishSolanaPlan(ctx context.Context, in solanaPlanContext) (map[string]string, error) {
	if s.rt == nil || s.rt.SolanaPlanService == nil {
		return nil, recurring.ErrNoCranker
	}
	plan := s.rt.SolanaPlanService
	// A new plan's owner is the deployment's first cranker: the plan's address
	// derives from it, and it signs the publication.
	if _, err := plan.Publisher(ctx); err != nil {
		return nil, fmt.Errorf("a recurring Solana price needs a listed solana.crankers to own its plan: %w", err)
	}
	tid, ok := merchant.FromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("publishing a Solana plan requires a merchant-scoped context")
	}
	if in.PriceID == uuid.Nil || in.BillingIntervalHours == nil || *in.BillingIntervalHours <= 0 || in.UnitAmount <= 0 {
		return nil, fmt.Errorf("a Solana plan needs the price id, a positive amount and a recurring interval")
	}
	symbol := solanaDefaultRecurringToken
	mint, err := plan.ResolveMint(ctx, symbol)
	if err != nil {
		return nil, err
	}
	// Decimals come from the SPL mint on-chain, never from config.
	decimals, err := plan.MintDecimals(ctx, symbol)
	if err != nil {
		return nil, err
	}
	amountBaseUnits, err := solanamodule.FiatMicrosToBaseUnitsAtPeg(moneyutil.Micros(in.UnitAmount), symbol, decimals)
	if err != nil {
		return nil, err
	}
	treasury, err := plan.Treasury(ctx, tid)
	if err != nil {
		return nil, err
	}
	planID := solanaPlanID(in.PriceID, mint, treasury)
	var handle map[string]string
	if in.RemoteWritesDisabled {
		existing, found := s.findExistingSolanaPlan(ctx, plan, tid, planID, symbol)
		if !found {
			return nil, fmt.Errorf("its Solana plan is not published, and provider_write_mode=%s publishes nothing: %w", config.GetProviderWriteMode(s.rt.Config), errRemoteWritesDisabled)
		}
		handle = existing
	} else {
		published, err := plan.PublishPlan(ctx, recurring.PublishPlanInput{
			MerchantID: tid, PlanID: planID, TokenSymbol: symbol, AmountBaseUnits: amountBaseUnits, AmountDecimals: decimals,
			PeriodHours: uint64(*in.BillingIntervalHours), BillingCycleHours: *in.BillingIntervalHours, EndTs: 0,
		})
		if err != nil {
			return nil, fmt.Errorf("solana publish plan: %w", err)
		}
		handle = published.ToRailConfig()
	}
	// The plan's owner is fixed on-chain: the reference keeps it, so a cranker
	// listed first later never re-derives another plan.
	return map[string]string{solanaKeyPlanID: handle[solanaKeyPlanID], solanaKeyToken: symbol, solanaKeyMerchant: handle[solanaKeyMerchant]}, nil
}

// findExistingSolanaPlan reads a plan already at the price's deterministic
// address; found=false on any read or decode failure.
func (s *Service) findExistingSolanaPlan(ctx context.Context, plan *recurring.PlanService, tid billing.MerchantID, planID uint64, symbol string) (map[string]string, bool) {
	if s.rt == nil || s.rt.SolanaRPCResolver == nil {
		return nil, false
	}
	owner, err := plan.Publisher(ctx)
	if err != nil {
		return nil, false
	}
	pda, _, err := subscriptions.DerivePlanPDA(owner, planID)
	if err != nil {
		return nil, false
	}
	data, err := s.rt.SolanaRPCResolver.ChainReader().GetAccountData(ctx, pda)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	acct, err := subscriptions.DecodePlanAccount(data)
	if err != nil || plan.PaysInto(ctx, tid, pda, acct.Destinations) != nil {
		return nil, false
	}
	handle := &recurring.PlanHandle{PlanPDA: pda.String(), PlanID: planID, Mint: acct.Mint.String(), MintSymbol: symbol, AmountBaseUnits: acct.Amount,
		PeriodHours: acct.PeriodHours, CreatedAt: acct.CreatedAt, MerchantAddress: owner.String(), Treasury: acct.Destinations[0].String()}
	return handle.ToRailConfig(), true
}

// solanaPlanLink is the Solana plan a price of ours sells on: the version's
// stored plan when its terms are unchanged, else a newly published and
// verified one bound to the merchant's Solana PSP. account is that PSP.
func (s *Service) solanaPlanLink(ctx context.Context, account solanaAccount, req billing.CreatePriceParams, priceID uuid.UUID, stored map[string]string) (map[string]string, error) {
	if stored != nil && strings.TrimSpace(stored[solanaKeyPlanPDA]) != "" {
		return stored, nil
	}
	if req.Archived {
		return nil, apperr.Invalidf("an archived price publishes no Solana plan")
	}
	ref, err := s.publishSolanaPlan(ctx, solanaPlanContext{PriceID: priceID, UnitAmount: req.UnitAmount, Currency: req.Currency,
		BillingIntervalHours: req.BillingIntervalHours, RemoteWritesDisabled: s.catalogRemoteWritesDisabled()})
	if err != nil {
		return nil, err
	}
	var reader catalogReferenceChainReader
	if s.rt.SolanaRPCResolver != nil {
		reader = s.rt.SolanaRPCResolver.ChainReader()
	}
	verified, err := verifySolanaCatalogReference(ctx, s.rt.SolanaPlanService, reader, req, ref)
	if err != nil {
		return nil, err
	}
	verified[models.RailKeyRail] = string(models.RailSolana)
	verified[models.RailKeyPSPID] = account.id.String()
	return verified, nil
}

// solanaAccount is the merchant's Solana PSP a published plan binds to.
type solanaAccount struct {
	key string
	id  uuid.UUID
}

func resolveSolanaTokenFromMint(ctx context.Context, plan *recurring.PlanService, mint string) (string, error) {
	configuredMint, err := plan.ResolveMint(ctx, solanaDefaultRecurringToken)
	if err == nil && strings.TrimSpace(configuredMint) == strings.TrimSpace(mint) {
		return solanaDefaultRecurringToken, nil
	}
	return "", fmt.Errorf("solana plan mint %s is not a configured recurring token", mint)
}

// isNoCranker reports a deployment that lists no Solana cranker.
func isNoCranker(err error) bool { return errors.Is(err, recurring.ErrNoCranker) }
