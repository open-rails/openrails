package service

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

	solanago "github.com/doujins-org/solana-go"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/pkg/tenant"
)

// solanaAdapter implements providerAdapter for the official Solana Subscriptions
// program (solana-program/subscriptions, De1egAFMkMWZSN5rYXRj9CAdheBamobVNubTsi9avR44).
// A recurring Solana price is a merchant-published on-chain Plan: AutoCreate signs
// create_plan with the tenant's key (via the runtime's SolanaPlanService) and
// stores the plan handle in processors["solana"]. Verify reads the Plan account
// back and diffs its immutable terms (amount / period / mint) for drift.
//
//   - AutoCreate: publish (or idempotently attach to) the on-chain plan. Requires a
//     tenant-scoped context, a configured SolanaPlanService, a recurring interval,
//     and a stablecoin currency (USDC/USD1). Unconfigured -> pending_manual_link.
//   - Attach: store an operator-supplied existing plan handle (plan_pda required).
//   - Verify: GetAccountData(plan_pda) -> DecodePlanAccount -> diff vs the stored
//     snapshot. RPC unavailable -> sync_disabled.
//   - Update: no-op. Plan core terms are immutable on-chain; an amount/period change
//     is modeled as a new price (new plan), and archiving a Solana price stops new
//     subscriptions while existing on-chain subscriptions continue (grandfathered).
type solanaAdapter struct{ svc *Service }

// Solana processor-config keys (mirror recurring.PlanHandle.ToProcessorConfig).
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
		Hint:     "Configure the tenant's Solana signer (secret solana/private_key) and set this price's currency to an allowlisted recurring stablecoin (USDC/USD1), then re-apply to publish the on-chain plan for price " + priceID.String(),
	}
}

// planService returns the runtime's SolanaPlanService, or false when Solana
// recurring is not configured on this deployment/tenant.
func (a *solanaAdapter) planService() (*recurring.PlanService, bool) {
	if a.svc == nil || a.svc.rt == nil || a.svc.rt.SolanaPlanService == nil {
		return nil, false
	}
	return a.svc.rt.SolanaPlanService, true
}

// solanaPlanID derives the deterministic on-chain plan id from the OpenRails price
// UUID (first 8 bytes, big-endian). Determinism makes re-apply idempotent: the
// same price always derives the same plan PDA, so find-or-attach re-attaches
// rather than duplicating.
func solanaPlanID(priceID uuid.UUID, mint string) uint64 {
	sum := sha256.Sum256([]byte(priceID.String() + ":" + strings.TrimSpace(mint)))
	return binary.BigEndian.Uint64(sum[:8])
}

func (a *solanaAdapter) AutoCreate(ctx context.Context, in autoCreateContext) (map[string]string, error) {
	plan, ok := a.planService()
	if !ok {
		// Solana recurring not configured here: defer to a manual/late publish.
		return nil, errPendingManualLink
	}
	tid, ok := tenant.FromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("solana create-mode requires a tenant-scoped context")
	}
	if in.BillingCycleDays == nil || *in.BillingCycleDays <= 0 {
		return nil, fmt.Errorf("solana create-mode requires billing_cycle_days (recurring plans need a fixed on-chain period)")
	}
	if in.UnitAmount <= 0 {
		return nil, fmt.Errorf("solana create-mode requires a positive unit_amount (stablecoin base units)")
	}

	symbol := strings.ToUpper(strings.TrimSpace(in.Currency))
	mint, _, err := plan.ResolveMint(symbol)
	if err != nil {
		return nil, err
	}
	planID := solanaPlanID(in.PriceID, mint)
	periodHours := uint64(*in.BillingCycleDays) * 24

	// Idempotent find-or-attach: create_plan fails on an already-occupied PDA, so
	// on re-apply we attach to the existing on-chain plan instead of republishing.
	if existing, found := a.findExistingPlan(ctx, plan, tid, planID, symbol); found {
		return existing, nil
	}

	handle, err := plan.PublishPlan(ctx, recurring.PublishPlanInput{
		TenantID:         tid,
		PlanID:           planID,
		TokenSymbol:      symbol, // PublishPlan rejects non-allowlisted (non-stablecoin) tokens
		AmountBaseUnits:  uint64(in.UnitAmount),
		PeriodHours:      periodHours,
		BillingCycleDays: *in.BillingCycleDays, // enforce period_hours == days*24
		EndTs:            0,                    // perpetual; OpenRails models open-ended subscriptions
	})
	if err != nil {
		return nil, fmt.Errorf("solana publish plan: %w", err)
	}
	return handle.ToProcessorConfig(), nil
}

// findExistingPlan checks whether the price's deterministic plan PDA already holds
// a decodable Plan account, and if so returns its processor-config map (attach).
// Best-effort: any read/decode failure returns found=false so AutoCreate proceeds
// to publish (a genuinely-occupied PDA then surfaces as a loud create_plan error).
func (a *solanaAdapter) findExistingPlan(ctx context.Context, plan *recurring.PlanService, tid tenant.ID, planID uint64, symbol string) (map[string]string, bool) {
	if a.svc == nil || a.svc.rt == nil || a.svc.rt.SolanaRPC == nil {
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
	data, err := a.svc.rt.SolanaRPC.GetAccountData(ctx, pda)
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
	return handle.ToProcessorConfig(), true
}

func (a *solanaAdapter) Attach(_ context.Context, link map[string]string) (map[string]string, error) {
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
	return out, nil
}

// Verify reads the on-chain Plan account and diffs its immutable terms against the
// stored snapshot. The terms (amount/period/mint) cannot change on-chain, so any
// divergence means tampering or a deleted+recreated plan (created_at fingerprint
// shift); both are real drift the operator must resolve. RPC unavailable ->
// sync_disabled (nil,false,nil). Account gone -> missing=true.
func (a *solanaAdapter) Verify(ctx context.Context, ids map[string]string, _ *priceVerifyContext) ([]DriftField, bool, error) {
	if a.svc == nil || a.svc.rt == nil || a.svc.rt.SolanaRPC == nil {
		return nil, false, nil
	}
	pdaStr := strings.TrimSpace(ids[solanaKeyPlanPDA])
	if pdaStr == "" {
		return nil, false, fmt.Errorf("solana plan_pda missing on local processors map")
	}
	pda, err := solanago.PublicKeyFromBase58(pdaStr)
	if err != nil {
		return nil, false, fmt.Errorf("invalid solana plan_pda %q: %w", pdaStr, err)
	}
	data, err := a.svc.rt.SolanaRPC.GetAccountData(ctx, pda)
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

// Update is a no-op: Solana plan core terms are immutable on-chain, and the
// program's updatePlan (status/metadata) is not yet wired (no BuildUpdatePlan).
func (a *solanaAdapter) Update(_ context.Context, _ map[string]string, _ mutableUpdate) error {
	return nil
}
