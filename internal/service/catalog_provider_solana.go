package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/internal/merchant"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/shared/cadence"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// solanaAdapter implements providerAdapter for the Solana Subscriptions program
// (solana-program/subscriptions, De1egAFMkMWZSN5rYXRj9CAdheBamobVNubTsi9avR44).
// A recurring price is an on-chain Plan published with the merchant's key.
//
//   - AutoCreate: one-off prices need no plan; recurring prices publish (or
//     attach to) a USDC plan in live mode, DUSD in test mode.
//   - Attach: token selects a non-default token for a published plan; plan_pda
//     attaches an existing plan and resolves its token on-chain.
//   - Verify: reads the Plan account and diffs its immutable terms against the
//     stored snapshot. No RPC -> sync_disabled.
//   - Update: no-op. Plan terms are immutable on-chain, so a term change is a
//     new price (new plan); archiving stops new subscriptions, existing ones
//     continue.
type solanaAdapter struct{ svc *Service }

// Solana rail-config keys (mirror recurring.PlanHandle.ToRailConfig).
const (
	solanaDefaultRecurringToken = "USDC"
	solanaUSD1RecurringToken    = "USD1"
	solanaDUSDRecurringToken    = "DUSD"
	solanaKeyPlanPDA            = "plan_pda"
	solanaKeyPlanID             = "plan_id"
	solanaKeyMint               = "mint"
	solanaKeyToken              = "token"
	solanaKeyMintSymbol         = "mint_symbol"
	solanaKeyAmountBaseUnits    = "amount_base_units"
	solanaKeyPeriodHours        = "period_hours"
	solanaKeyCreatedAt          = "created_at"
	solanaKeyMerchant           = "merchant_address"
)

func (a *solanaAdapter) Name() string { return "solana" }

func (a *solanaAdapter) PendingActionTemplate(priceID uuid.UUID) billing.PendingAction {
	return billing.PendingAction{
		PSP:    "solana",
		Action: "configure_solana_recurring",
		Hint:   "Configure the merchant's Solana PSP signer, then re-apply to publish the on-chain plan for price " + priceID.String() + " (USDC in live mode, DUSD in test mode; set psp_links.solana.token to select another supported stablecoin)",
	}
}

// planService returns the runtime's SolanaPlanService, or false when Solana
// recurring is not configured on this deployment/merchant.
func (a *solanaAdapter) planService() (*recurring.PlanService, bool) {
	if a.svc == nil || a.svc.rt == nil || a.svc.rt.SolanaPlanService == nil {
		return nil, false
	}
	return a.svc.rt.SolanaPlanService, true
}

// solanaPlanID addresses an immutable local price and the chosen token mint.
// Existing bindings retain their stored plan PDA even when their ID predates
// this scheme; only new publications use the current local identity.
func solanaPlanID(priceID uuid.UUID, mint string) uint64 {
	sum := sha256.Sum256([]byte(priceID.String() + ":" + strings.TrimSpace(mint)))
	return binary.BigEndian.Uint64(sum[:8])
}

func (a *solanaAdapter) AutoCreate(ctx context.Context, in autoCreateContext) (map[string]string, error) {
	// A one-off price needs no Plan: it settles as a direct transfer validated
	// at payment time. Mark the rail present so checkout offers Solana.
	if in.BillingCycleDays == nil {
		return map[string]string{"provider": "solana"}, nil
	}
	if err := requireUSDBillingForSolanaPublish(in.Currency); err != nil {
		return nil, err
	}
	return a.createRecurringPlan(ctx, in, a.defaultRecurringToken())
}

func (a *solanaAdapter) defaultRecurringToken() string {
	if a.svc != nil && a.svc.rt != nil && a.svc.rt.Config != nil && config.IsTestMode(a.svc.rt.Config) {
		return solanaDUSDRecurringToken
	}
	return solanaDefaultRecurringToken
}

// requireUSDBillingForSolanaPublish requires a new recurring plan's price to
// bill in USD; a non-USDC settlement token is declared separately. Plans
// attached by plan_pda are exempt (see Attach).
func requireUSDBillingForSolanaPublish(currency string) error {
	if !strings.EqualFold(strings.TrimSpace(currency), "usd") {
		return fmt.Errorf("solana recurring currently requires USD billing currency, got %q", currency)
	}
	return nil
}

func (a *solanaAdapter) createRecurringPlan(ctx context.Context, in autoCreateContext, symbol string) (map[string]string, error) {
	plan, ok := a.planService()
	if !ok {
		// Solana recurring not configured here: defer to a manual/late publish.
		if in.RemoteWritesDisabled {
			return nil, errRemoteWritesDisabled
		}
		return nil, errPendingManualLink
	}
	tid, ok := merchant.FromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("solana create-mode requires a merchant-scoped context")
	}
	if in.UnitAmount <= 0 {
		return nil, fmt.Errorf("solana create-mode requires a positive unit_amount (micros)")
	}
	if in.BillingIntervalHours == nil || *in.BillingIntervalHours <= 0 {
		return nil, fmt.Errorf("solana create-mode requires a positive recurring interval")
	}

	// symbol arrives already normalized (uppercased/trimmed) by Attach or is the
	// canonical USDC default from AutoCreate.
	mint, err := plan.ResolveMint(symbol)
	if err != nil {
		return nil, err
	}
	// Decimals come from the SPL mint on-chain, never from config.
	decimals, err := plan.MintDecimals(ctx, symbol)
	if err != nil {
		return nil, err
	}
	// The price is micros; the plan amount is token base units at the mint's
	// on-chain decimals.
	amountBaseUnits, err := solanamodule.FiatMicrosToBaseUnitsAtPeg(moneyutil.Micros(in.UnitAmount), symbol, decimals)
	if err != nil {
		return nil, err
	}
	if in.PriceID == uuid.Nil {
		return nil, fmt.Errorf("solana auto-create requires a local price ID")
	}
	planID := solanaPlanID(in.PriceID, mint)
	periodHours := uint64(*in.BillingIntervalHours)

	if in.RemoteWritesDisabled {
		// Read-only attach to an existing plan stays available in limited/readonly
		// mode. With writes enabled PublishPlan owns re-apply: it attaches to the
		// existing plan and re-ensures the receiving ATAs a failed publish missed.
		if existing, found := a.findExistingPlan(ctx, plan, tid, planID, symbol); found {
			return existing, nil
		}
		return nil, errRemoteWritesDisabled
	}

	handle, err := plan.PublishPlan(ctx, recurring.PublishPlanInput{
		MerchantID:        tid,
		PlanID:            planID,
		TokenSymbol:       symbol, // PublishPlan rejects non-allowlisted (non-stablecoin) tokens
		AmountBaseUnits:   amountBaseUnits,
		AmountDecimals:    decimals,
		PeriodHours:       periodHours,
		BillingCycleHours: *in.BillingIntervalHours,
		EndTs:             0, // perpetual; OpenRails models open-ended subscriptions
	})
	if err != nil {
		return nil, fmt.Errorf("solana publish plan: %w", err)
	}
	return handle.ToRailConfig(), nil
}

// findExistingPlan checks whether the price's deterministic plan PDA already holds
// a decodable Plan account, and if so returns its rail-config map (attach).
// Best-effort: any read/decode failure returns found=false so AutoCreate proceeds
// to publish (a genuinely-occupied PDA then surfaces as a loud create_plan error).
func (a *solanaAdapter) findExistingPlan(ctx context.Context, plan *recurring.PlanService, tid billing.MerchantID, planID uint64, symbol string) (map[string]string, bool) {
	if a.svc == nil || a.svc.rt == nil || a.svc.rt.SolanaRPCResolver == nil {
		return nil, false
	}
	merchant, err := plan.MerchantAddress(ctx, tid)
	if err != nil {
		return nil, false
	}
	pda, _, err := subscriptions.DerivePlanPDA(merchant, planID)
	if err != nil {
		return nil, false
	}
	data, err := a.svc.rt.SolanaRPCResolver.ChainReader().GetAccountData(ctx, pda)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	acct, err := subscriptions.DecodePlanAccount(data)
	if err != nil {
		return nil, false
	}
	handle := &recurring.PlanHandle{
		PlanPDA:         pda.String(),
		PlanID:          planID,
		Mint:            acct.Mint.String(),
		MintSymbol:      symbol,
		AmountBaseUnits: acct.Amount,
		PeriodHours:     acct.PeriodHours,
		CreatedAt:       acct.CreatedAt,
		MerchantAddress: merchant.String(),
	}
	return handle.ToRailConfig(), true
}

// Attach publishes a plan from a declarative token or stores an
// operator-supplied plan handle, read back on-chain: it must exist and match
// the price's amount (base units), period, mint and, with a merchant in
// context, owner. The verified terms are stamped onto the ids for Verify.
// Without RPC, plan_pda attachment fails closed.
func (a *solanaAdapter) Attach(ctx context.Context, link map[string]string, in autoCreateContext) (map[string]string, error) {
	_, mintSymbolSupplied := link[solanaKeyMintSymbol]
	link = normalizeLinkMap(link)
	if mintSymbolSupplied {
		return nil, fmt.Errorf("psp_links.solana.mint_symbol is output metadata; use psp_links.solana.token")
	}
	pda := strings.TrimSpace(link[solanaKeyPlanPDA])
	symbol := strings.ToUpper(strings.TrimSpace(link[solanaKeyToken]))
	if symbol != "" && !isSolanaRecurringToken(symbol) {
		return nil, fmt.Errorf("psp_links.solana.token must be USDC, USD1, or DUSD, got %q", symbol)
	}
	if pda != "" && symbol != "" {
		return nil, fmt.Errorf("psp_links.solana.token selects a new plan; omit it when plan_pda is supplied because the existing plan's token is resolved on-chain")
	}
	if pda == "" {
		// The guard lives here, not on the plan_pda path, so USDC-currency rows
		// can derive their token from the currency and stay operable.
		if in.BillingCycleDays != nil {
			if err := requireUSDBillingForSolanaPublish(in.Currency); err != nil {
				return nil, err
			}
		}
		if in.BillingCycleDays == nil {
			// A one-off price takes no link keys (it settles as a direct transfer
			// quoted at checkout). Refuse rather than drop the operator's values
			// and re-flag the same drift forever.
			return nil, fmt.Errorf("solana one-off prices take no psp_links (the settlement token is chosen at checkout); remove psp_links.solana")
		}
		if symbol == "" {
			symbol = a.defaultRecurringToken()
		}
		out, err := a.createRecurringPlan(ctx, in, symbol)
		if err != nil {
			return nil, err
		}
		out[solanaKeyToken] = symbol
		return out, nil
	}
	if in.BillingCycleDays != nil {
		var err error
		symbol, err = existingSolanaSettlementToken(in.Currency)
		if err != nil {
			return nil, err
		}
	}
	out := map[string]string{solanaKeyPlanPDA: pda}
	for _, k := range []string{
		solanaKeyPlanID, solanaKeyMint,
		solanaKeyAmountBaseUnits, solanaKeyPeriodHours, solanaKeyCreatedAt, solanaKeyMerchant,
	} {
		if v := strings.TrimSpace(link[k]); v != "" {
			out[k] = v
		}
	}
	if a.svc == nil || a.svc.rt == nil || a.svc.rt.SolanaRPCResolver == nil {
		return nil, fmt.Errorf("solana plan_pda requires an available RPC to resolve and verify its token")
	}
	pubkey, err := solanago.PublicKeyFromBase58(pda)
	if err != nil {
		return nil, fmt.Errorf("invalid solana plan_pda %q: %w", pda, err)
	}
	data, err := a.svc.rt.SolanaRPCResolver.ChainReader().GetAccountData(ctx, pubkey)
	if err != nil || len(data) == 0 {
		return nil, fmt.Errorf("solana plan account %q not found on-chain; publish it or fix psp_links.solana.plan_pda", pda)
	}
	acct, err := subscriptions.DecodePlanAccount(data)
	if err != nil {
		return nil, fmt.Errorf("decode solana plan account %q: %w", pda, err)
	}
	if in.BillingIntervalHours != nil && *in.BillingIntervalHours > 0 {
		wantPeriod := uint64(*in.BillingIntervalHours)
		if acct.PeriodHours != wantPeriod {
			return nil, fmt.Errorf("solana plan %q period (%s) does not match catalog price (%s)", pda, solanaPeriod(acct.PeriodHours), cadence.FormatHours(*in.BillingIntervalHours))
		}
	}
	// Amount, mint and merchant checks need the plan service (mint allowlist,
	// on-chain decimals, merchant resolution); skipped when it is unconfigured.
	if plan, ok := a.planService(); ok {
		if symbol == "" {
			symbol, err = resolveSolanaTokenFromMint(plan, acct.Mint.String())
			if err != nil {
				return nil, err
			}
		}
		if symbol != "" {
			mint, err := plan.ResolveMint(symbol)
			if err != nil {
				return nil, err
			}
			if acct.Mint.String() != strings.TrimSpace(mint) {
				return nil, fmt.Errorf("solana plan %q mint (%s) does not match settlement token %s mint (%s)", pda, acct.Mint, symbol, mint)
			}
			if in.UnitAmount > 0 {
				// Decimals come from the SPL mint on-chain, never from config.
				decimals, err := plan.MintDecimals(ctx, symbol)
				if err != nil {
					return nil, err
				}
				want, err := solanamodule.FiatMicrosToBaseUnitsAtPeg(moneyutil.Micros(in.UnitAmount), symbol, decimals)
				if err != nil {
					return nil, err
				}
				if acct.Amount != want {
					return nil, fmt.Errorf("solana plan %q amount (%s %s) does not match catalog price (%s = %s %s)", pda, solanamodule.FormatBaseUnits(acct.Amount, decimals), symbol, moneyutil.FormatAmount(in.UnitAmount, in.Currency), solanamodule.FormatBaseUnits(want, decimals), symbol)
				}
			}
		}
		if tid, ok := merchant.FromContext(ctx); ok {
			if merchant, err := plan.MerchantAddress(ctx, tid); err == nil && !acct.Owner.Equals(merchant) {
				return nil, fmt.Errorf("solana plan %q merchant (%s) does not match this merchant's merchant (%s)", pda, acct.Owner, merchant)
			}
		}
	} else if symbol == "" {
		return nil, fmt.Errorf("solana plan_pda mint %s cannot be resolved without configured recurring tokens", acct.Mint)
	}

	// Stamp the authoritative on-chain terms (override any operator-supplied
	// values) so Verify diffs against the real account.
	out[solanaKeyMint] = acct.Mint.String()
	out[solanaKeyAmountBaseUnits] = strconv.FormatUint(acct.Amount, 10)
	out[solanaKeyPeriodHours] = strconv.FormatUint(acct.PeriodHours, 10)
	out[solanaKeyCreatedAt] = strconv.FormatInt(acct.CreatedAt, 10)
	out[solanaKeyMerchant] = acct.Owner.String()
	out[solanaKeyMintSymbol] = symbol
	return out, nil
}

// existingSolanaSettlementToken keeps USDC-currency prices, whose currency
// names both the billing denomination and the settlement token, operable. USD
// prices resolve the token from the supplied plan_pda.
func existingSolanaSettlementToken(currency string) (string, error) {
	currency = strings.ToLower(strings.TrimSpace(currency))
	switch currency {
	case "usd":
		return "", nil
	case "usdc":
		return "USDC", nil
	default:
		return "", fmt.Errorf("solana recurring existing-plan links require USD billing currency or a legacy USDC price, got %q", currency)
	}
}

func resolveSolanaTokenFromMint(plan *recurring.PlanService, mint string) (string, error) {
	for _, symbol := range [...]string{
		solanaDefaultRecurringToken,
		solanaUSD1RecurringToken,
		solanaDUSDRecurringToken,
	} {
		configuredMint, err := plan.ResolveMint(symbol)
		if err == nil && strings.TrimSpace(configuredMint) == strings.TrimSpace(mint) {
			return symbol, nil
		}
	}
	return "", fmt.Errorf("solana plan mint %s is not a configured recurring token", mint)
}

func isSolanaRecurringToken(symbol string) bool {
	return symbol == solanaDefaultRecurringToken ||
		symbol == solanaUSD1RecurringToken ||
		symbol == solanaDUSDRecurringToken
}

// Verify reads the on-chain Plan account and diffs its immutable terms (amount,
// period, mint) against the stored snapshot: any divergence is tampering or a
// deleted and recreated plan (created_at shift). No RPC -> sync_disabled;
// account gone -> missing.
func (a *solanaAdapter) Verify(ctx context.Context, ids map[string]string, _ *priceVerifyContext) ([]billing.DriftField, bool, error) {
	if a.svc == nil || a.svc.rt == nil || a.svc.rt.SolanaRPCResolver == nil {
		return nil, false, fmt.Errorf("solana is not configured: %w", errProviderNotArmed)
	}
	pdaStr := strings.TrimSpace(ids[solanaKeyPlanPDA])
	if pdaStr == "" {
		return nil, false, fmt.Errorf("solana plan_pda missing on local rails map")
	}
	pda, err := solanago.PublicKeyFromBase58(pdaStr)
	if err != nil {
		return nil, false, fmt.Errorf("invalid solana plan_pda %q: %w", pdaStr, err)
	}
	data, err := a.svc.rt.SolanaRPCResolver.ChainReader().GetAccountData(ctx, pda)
	if err != nil {
		return nil, false, fmt.Errorf("read solana plan account: %w", err)
	}
	if len(data) == 0 {
		return nil, true, nil // plan account gone from chain
	}
	acct, err := subscriptions.DecodePlanAccount(data)
	if err != nil {
		return nil, false, fmt.Errorf("decode solana plan account: %w", err)
	}

	drift := []billing.DriftField{}
	cmp := func(field, want, got string) {
		if want != "" && want != got {
			drift = append(drift, billing.DriftField{Field: field, OpenRailsValue: want, RemoteValue: got})
		}
	}
	cmp(solanaKeyAmountBaseUnits, ids[solanaKeyAmountBaseUnits], strconv.FormatUint(acct.Amount, 10))
	cmp(solanaKeyPeriodHours, ids[solanaKeyPeriodHours], strconv.FormatUint(acct.PeriodHours, 10))
	cmp(solanaKeyMint, strings.TrimSpace(ids[solanaKeyMint]), acct.Mint.String())
	// created_at is the ghost-plan fingerprint: a shift means the plan was deleted
	// and recreated at the same PDA (subscribers must re-enroll).
	cmp(solanaKeyCreatedAt, ids[solanaKeyCreatedAt], strconv.FormatInt(acct.CreatedAt, 10))
	return drift, false, nil
}

// Update is a no-op: Solana plan terms are immutable on-chain.
func (a *solanaAdapter) Update(_ context.Context, _ map[string]string, _ mutableUpdate) error {
	return nil
}

// solanaPeriod renders an on-chain plan period readably.
func solanaPeriod(hours uint64) string {
	if hours > uint64(math.MaxInt64/int64(time.Hour)) {
		return fmt.Sprintf("%d hours", hours)
	}
	return cadence.FormatHours(int(hours)) // #nosec G115 -- bounded above
}
