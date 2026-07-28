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
//   - AutoCreate: publish (or idempotently attach to) the on-chain plan. Requires a
//     merchant-scoped context, a configured SolanaPlanService, a recurring interval,
//     and a stablecoin currency (USDC/USD1). Unconfigured -> pending_manual_link.
//   - Attach: store an operator-supplied existing plan handle (plan_pda required).
//   - Verify: GetAccountData(plan_pda) -> DecodePlanAccount -> diff vs the stored
//     snapshot. RPC unavailable -> sync_disabled.
//   - Update: no-op. Plan core terms are immutable on-chain; an amount/period change
//     is modeled as a new price (new plan), and archiving a Solana price stops new
//     subscriptions while existing on-chain subscriptions continue (grandfathered).
type solanaAdapter struct{ svc *Service }

// Solana rail-config keys (mirror recurring.PlanHandle.ToRailConfig).
const (
	solanaKeyPlanPDA         = "plan_pda"
	solanaKeyPlanID          = "plan_id"
	solanaKeyMint            = "mint"
	solanaKeyMintSymbol      = "mint_symbol"
	solanaKeyAmountBaseUnits = "amount_base_units"
	solanaKeyPeriodHours     = "period_hours"
	solanaKeyCreatedAt       = "created_at"
	solanaKeyMerchant        = "merchant_address"
)

func (a *solanaAdapter) Name() string { return "solana" }

func (a *solanaAdapter) PendingActionTemplate(priceID uuid.UUID) PendingAction {
	return PendingAction{
		Provider: "solana",
		Action:   "configure_solana_recurring",
		Hint:     "Configure the merchant's Solana provider-account signer and set this price's currency to an allowlisted recurring stablecoin (USDC/USD1), then re-apply to publish the on-chain plan for price " + priceID.String(),
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
	if in.AccessDurationHours == nil || *in.AccessDurationHours <= 0 {
		return map[string]string{"provider": "solana"}, nil
	}
	plan, ok := a.planService()
	if !ok {
		// Solana recurring not configured here: defer to a manual/late publish.
		return nil, errPendingManualLink
	}
	tid, ok := merchant.FromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("solana create-mode requires a merchant-scoped context")
	}
	if in.UnitAmount <= 0 {
		return nil, fmt.Errorf("solana create-mode requires a positive unit_amount (micros)")
	}

	symbol := strings.ToUpper(strings.TrimSpace(in.Currency))
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

// Attach stores an operator-supplied existing on-chain plan handle. When the
// Solana RPC is available the plan account is read back and its IMMUTABLE terms
// are verified against the OpenRails price: the account must exist and match
// amount (base units), period (billing cycle * 24h), mint (resolved from the
// price currency), and — when a merchant is in context — the merchant/owner. A
// missing or mismatched plan is a loud error. On a successful read the verified
// on-chain terms are stamped onto the stored ids so Verify has a snapshot. When
// RPC is unavailable the operator-supplied fields are stored as-is.
func (a *solanaAdapter) Attach(ctx context.Context, link map[string]string, in autoCreateContext) (map[string]string, error) {
	link = normalizeLinkMap(link)
	pda := strings.TrimSpace(link[solanaKeyPlanPDA])
	if pda == "" {
		return nil, fmt.Errorf("solana link requires provider_links.solana.plan_pda")
	}
	out := map[string]string{solanaKeyPlanPDA: pda}
	for _, k := range []string{
		solanaKeyPlanID, solanaKeyMint, solanaKeyMintSymbol,
		solanaKeyAmountBaseUnits, solanaKeyPeriodHours, solanaKeyCreatedAt, solanaKeyMerchant,
	} {
		if v := strings.TrimSpace(link[k]); v != "" {
			out[k] = v
		}
	}

	if a.svc == nil || a.svc.rt == nil || a.svc.rt.SolanaRPCResolver == nil {
		// No RPC to verify against: store the operator-owned handle as-is.
		return out, nil
	}
	pubkey, err := solanago.PublicKeyFromBase58(pda)
	if err != nil {
		return nil, fmt.Errorf("invalid solana plan_pda %q: %w", pda, err)
	}
	data, err := a.svc.rt.SolanaRPCResolver.ChainReader().GetAccountData(ctx, pubkey)
	if err != nil || len(data) == 0 {
		return nil, fmt.Errorf("solana plan account %q not found on-chain; publish it or fix provider_links.solana.plan_pda", pda)
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
		if symbol := strings.ToUpper(strings.TrimSpace(in.Currency)); symbol != "" {
			if mint, decimals, err := plan.ResolveMint(symbol); err == nil {
				if !strings.EqualFold(acct.Mint.String(), strings.TrimSpace(mint)) {
					return nil, fmt.Errorf("solana plan %q mint (%s) does not match catalog currency %s mint (%s)", pda, acct.Mint, symbol, mint)
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
		}
		if tid, ok := merchant.FromContext(ctx); ok {
			if merchant, err := plan.MerchantAddress(ctx, tid); err == nil && !strings.EqualFold(acct.Owner.String(), merchant.String()) {
				return nil, fmt.Errorf("solana plan %q merchant (%s) does not match this merchant's merchant (%s)", pda, acct.Owner, merchant)
			}
		}
	}

	// Stamp the authoritative on-chain terms (override any operator-supplied
	// values) so Verify diffs against the real account.
	out[solanaKeyMint] = acct.Mint.String()
	out[solanaKeyAmountBaseUnits] = strconv.FormatUint(acct.Amount, 10)
	out[solanaKeyPeriodHours] = strconv.FormatUint(acct.PeriodHours, 10)
	out[solanaKeyCreatedAt] = strconv.FormatInt(acct.CreatedAt, 10)
	out[solanaKeyMerchant] = acct.Owner.String()
	return out, nil
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
