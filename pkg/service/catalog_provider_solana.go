package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// solanaAdapter implements providerAdapter for the official Solana Subscriptions
// program (solana-program/subscriptions, De1egAFMkMWZSN5rYXRj9CAdheBamobVNubTsi9avR44).
// A recurring Solana price is a merchant-published on-chain Plan: AutoCreate signs
// create_plan with the merchant's key (via the runtime's SolanaPlanService) and
// stores the plan handle in rails["solana"]. Verify reads the Plan account
// back and diffs its immutable terms (amount / period / mint) for drift.
//
//   - AutoCreate: one-off prices need no plan; recurring prices publish (or
//     idempotently attach to) a USDC plan by default.
//   - Attach: token selects a non-default token for a published plan; plan_pda
//     attaches an existing plan and resolves its token on-chain.
//   - Verify: GetAccountData(plan_pda) -> DecodePlanAccount -> diff vs the stored
//     snapshot. RPC unavailable -> sync_disabled.
//   - Update: no-op. Plan core terms are immutable on-chain; an amount/period change
//     is modeled as a new price (new plan), and archiving a Solana price stops new
//     subscriptions while existing on-chain subscriptions continue (grandfathered).
type solanaAdapter struct{ svc *Service }

// Solana rail-config keys (mirror recurring.PlanHandle.ToRailConfig).
const (
	solanaDefaultRecurringToken = "USDC"
	solanaUSD1RecurringToken    = "USD1"
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

func (a *solanaAdapter) PendingActionTemplate(priceID uuid.UUID) PendingAction {
	return PendingAction{
		Provider: "solana",
		Action:   "configure_solana_recurring",
		Hint:     "Configure the merchant's Solana provider-account signer, then re-apply to publish the on-chain plan for price " + priceID.String() + " (USDC by default; set psp_links.solana.token to use another supported stablecoin)",
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

// solanaPlanID derives the deterministic on-chain plan id from the price CONTENT
// key (product key + immutable money terms) plus the token mint, hashed to a
// uint64 (first 8 bytes, big-endian). Content-addressing — NOT the per-DB price
// UUID — makes re-apply idempotent AND stable across a FRESH OpenRails DB: a
// rebuilt catalog derives the same plan PDA, so find-or-attach re-attaches to the
// existing on-chain plan rather than publishing a duplicate. Cosmetic edits
// (display_name/description/providers) do not change it.
func solanaPlanID(productKey, currency string, unitAmount int64, accessDurationHours *int, mint string) uint64 {
	hourPart := "one-time"
	if accessDurationHours != nil {
		hourPart = "h" + strconv.Itoa(*accessDurationHours)
	}
	key := openRailsPriceContentKey(productKey, currency, unitAmount, nil) + "." + hourPart + ":" + strings.TrimSpace(mint)
	sum := sha256.Sum256([]byte(key))
	return binary.BigEndian.Uint64(sum[:8])
}

func (a *solanaAdapter) AutoCreate(ctx context.Context, in autoCreateContext) (map[string]string, error) {
	// A one-off Solana price (no recurring cadence, #622) needs no on-chain Plan
	// PDA — it settles as a direct transfer validated at payment time. Mark the
	// rail present so checkout offers Solana; publish nothing on-chain.
	if in.BillingCycleDays == nil {
		return map[string]string{"provider": "solana"}, nil
	}
	if err := requireUSDBillingForSolanaPublish(in.Currency); err != nil {
		return nil, err
	}
	return a.createRecurringPlan(ctx, in, solanaDefaultRecurringToken)
}

// requireUSDBillingForSolanaPublish gates publishing a NEW recurring plan on
// the #745 model: the price bills in USD and the settlement token is declared
// separately when it differs from the USDC default. Pre-#745 rows attached by
// plan_pda are exempt (see Attach).
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
	if in.AccessDurationHours == nil || *in.AccessDurationHours <= 0 {
		return nil, fmt.Errorf("solana create-mode requires a positive recurring interval")
	}

	// symbol arrives already normalized (uppercased/trimmed) by Attach or is the
	// canonical USDC default from AutoCreate.
	mint, decimals, err := plan.ResolveMint(symbol)
	if err != nil {
		return nil, err
	}
	// #817: the price is MICROS; the plan amount is token BASE UNITS at the
	// token's configured decimals — shipping micros verbatim only worked at 6.
	amountBaseUnits, err := solanamodule.FiatMicrosToBaseUnitsAtPeg(moneyutil.Micros(in.UnitAmount), symbol, decimals)
	if err != nil {
		return nil, err
	}
	planID := solanaPlanID(in.ProductKey, in.Currency, in.UnitAmount, in.AccessDurationHours, mint)
	periodHours := uint64(*in.AccessDurationHours)

	// Idempotent find-or-attach: create_plan fails on an already-occupied PDA, so
	// on re-apply we attach to the existing on-chain plan instead of republishing.
	if existing, found := a.findExistingPlan(ctx, plan, tid, planID, symbol); found {
		return existing, nil
	}
	if in.RemoteWritesDisabled {
		// Existing-plan discovery above is read-only and remains available in
		// limited/readonly mode. Gate only the actual publish boundary.
		return nil, errRemoteWritesDisabled
	}

	handle, err := plan.PublishPlan(ctx, recurring.PublishPlanInput{
		MerchantID:        tid,
		PlanID:            planID,
		TokenSymbol:       symbol, // PublishPlan rejects non-allowlisted (non-stablecoin) tokens
		AmountBaseUnits:   amountBaseUnits,
		PeriodHours:       periodHours,
		BillingCycleHours: *in.AccessDurationHours,
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
func (a *solanaAdapter) findExistingPlan(ctx context.Context, plan *recurring.PlanService, tid merchant.ID, planID uint64, symbol string) (map[string]string, bool) {
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
// operator-supplied existing plan handle. When the Solana RPC is available the
// plan account is read back and its IMMUTABLE terms are verified against the
// OpenRails price: the account must exist and match
// amount (base units), period (billing cycle * 24h), mint (resolved from token
// or inferred from the plan), and — when a merchant is in context — the merchant/owner. A
// missing or mismatched plan is a loud error. On a successful read the verified
// on-chain terms are stamped onto the stored ids so Verify has a snapshot. When
// RPC is unavailable, plan_pda attachment fails closed.
func (a *solanaAdapter) Attach(ctx context.Context, link map[string]string, in autoCreateContext) (map[string]string, error) {
	_, mintSymbolSupplied := link[solanaKeyMintSymbol]
	link = normalizeLinkMap(link)
	if mintSymbolSupplied {
		return nil, fmt.Errorf("psp_links.solana.mint_symbol is output metadata; use psp_links.solana.token")
	}
	pda := strings.TrimSpace(link[solanaKeyPlanPDA])
	symbol := strings.ToUpper(strings.TrimSpace(link[solanaKeyToken]))
	if symbol != "" && !isSolanaRecurringToken(symbol) {
		return nil, fmt.Errorf("psp_links.solana.token must be USDC or USD1, got %q", symbol)
	}
	if pda != "" && symbol != "" {
		return nil, fmt.Errorf("psp_links.solana.token selects a new plan; omit it when plan_pda is supplied because the existing plan's token is resolved on-chain")
	}
	if pda == "" {
		// The guard lives here — not on the plan_pda path — so eligible
		// pre-#745 USDC rows can derive the token from their currency and stay
		// operable for link verification and rotation.
		if in.BillingCycleDays != nil {
			if err := requireUSDBillingForSolanaPublish(in.Currency); err != nil {
				return nil, err
			}
		}
		if in.BillingCycleDays == nil {
			// A one-off price consumes no link keys (it settles as a direct
			// transfer quoted at checkout). Callers only reach Attach with a
			// non-empty link, so silently returning the bare rail marker would
			// drop the operator's declared values and leave catalog plans
			// re-flagging the same drift forever — reject loudly instead.
			return nil, fmt.Errorf("solana one-off prices take no psp_links (the settlement token is chosen at checkout); remove psp_links.solana")
		}
		if symbol == "" {
			symbol = solanaDefaultRecurringToken
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
	if in.AccessDurationHours != nil && *in.AccessDurationHours > 0 {
		wantPeriod := uint64(*in.AccessDurationHours)
		if acct.PeriodHours != wantPeriod {
			return nil, fmt.Errorf("solana plan %q period (%d hours) does not match catalog price (%d hours)", pda, acct.PeriodHours, wantPeriod)
		}
	}
	// Amount + mint + merchant validation requires the plan service (token
	// decimals, mint allowlist, merchant resolution); skip gracefully when it is
	// unconfigured — the price is in MICROS and only the token's decimals turn
	// that into the plan's base units (#817).
	if plan, ok := a.planService(); ok {
		if symbol == "" {
			symbol, err = resolveSolanaTokenFromMint(plan, acct.Mint.String())
			if err != nil {
				return nil, err
			}
		}
		if symbol != "" {
			mint, decimals, err := plan.ResolveMint(symbol)
			if err != nil {
				return nil, err
			}
			if !strings.EqualFold(acct.Mint.String(), strings.TrimSpace(mint)) {
				return nil, fmt.Errorf("solana plan %q mint (%s) does not match settlement token %s mint (%s)", pda, acct.Mint, symbol, mint)
			}
			if in.UnitAmount > 0 {
				want, err := solanamodule.FiatMicrosToBaseUnitsAtPeg(moneyutil.Micros(in.UnitAmount), symbol, decimals)
				if err != nil {
					return nil, err
				}
				if acct.Amount != want {
					return nil, fmt.Errorf("solana plan %q amount (%d base units) does not match catalog price (%d micros = %d base units)", pda, acct.Amount, in.UnitAmount, want)
				}
			}
		}
		if tid, ok := merchant.FromContext(ctx); ok {
			if merchant, err := plan.MerchantAddress(ctx, tid); err == nil && !strings.EqualFold(acct.Owner.String(), merchant.String()) {
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

// existingSolanaSettlementToken keeps safely translatable pre-#745 USDC
// prices operable while preserving strict validation for the #745 model.
// Before #745, currency=usdc identified both the billing denomination and the
// settlement token. New prices bill in USD; an empty token is resolved from the
// supplied plan_pda.
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
	for _, symbol := range [...]string{solanaDefaultRecurringToken, solanaUSD1RecurringToken} {
		configuredMint, _, err := plan.ResolveMint(symbol)
		if err == nil && strings.EqualFold(strings.TrimSpace(configuredMint), strings.TrimSpace(mint)) {
			return symbol, nil
		}
	}
	return "", fmt.Errorf("solana plan mint %s is not a configured recurring token", mint)
}

func isSolanaRecurringToken(symbol string) bool {
	return symbol == solanaDefaultRecurringToken || symbol == solanaUSD1RecurringToken
}

// Verify reads the on-chain Plan account and diffs its immutable terms against the
// stored snapshot. The terms (amount/period/mint) cannot change on-chain, so any
// divergence means tampering or a deleted+recreated plan (created_at fingerprint
// shift); both are real drift the operator must resolve. RPC unavailable ->
// sync_disabled (nil,false,nil). Account gone -> missing=true.
func (a *solanaAdapter) Verify(ctx context.Context, ids map[string]string, _ *priceVerifyContext) ([]DriftField, bool, error) {
	if a.svc == nil || a.svc.rt == nil || a.svc.rt.SolanaRPCResolver == nil {
		return nil, false, nil
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
	if err != nil || len(data) == 0 {
		return nil, true, nil // plan account gone from chain
	}
	acct, err := subscriptions.DecodePlanAccount(data)
	if err != nil {
		return nil, false, fmt.Errorf("decode solana plan account: %w", err)
	}

	drift := []DriftField{}
	cmp := func(field, want, got string) {
		if want != "" && want != got {
			drift = append(drift, DriftField{Field: field, OpenRailsValue: want, RemoteValue: got})
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

// Update is a no-op: Solana plan core terms are immutable on-chain. Archiving
// (updatePlan status=sunset, subscriptions.BuildUpdatePlan) is wired through
// the provider intent ledger instead — the `push-catalog --prune`
// sweep enqueues solana_sunset_plan intents for plans whose local price is no
// longer purchasable (#357/#358 phase D).
func (a *solanaAdapter) Update(_ context.Context, _ map[string]string, _ mutableUpdate) error {
	return nil
}
