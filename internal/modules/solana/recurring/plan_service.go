package recurring

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/config"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

const maxPeriodHours = 8760 // 1 year — the program's upper bound for period_hours

// Submitter is the per-merchant Solana submit surface the recurring services use:
// it resolves the merchant's merchant (cranker) address and signs+submits an
// instruction set with that merchant's key. Declared here (dependency inversion)
// so the services are unit-testable without a live signer/RPC.
type Submitter interface {
	// MerchantAddress returns the merchant's on-chain merchant/cranker address.
	MerchantAddress(ctx context.Context, tenantID merchant.ID) (solanago.PublicKey, error)
	// Submit signs (with the merchant's key) and submits the instructions, returning
	// the confirmed transaction signature.
	Submit(ctx context.Context, tenantID merchant.ID, instructions []solanago.Instruction) (solanago.Signature, error)
}

// signerSubmitter is the production Submitter: a per-merchant solana.Signer + RPC,
// wired through solana.BuildSignSubmit (the verified build/sign/submit path).
type signerSubmitter struct {
	signer solanaint.Signer
	rpc    *solanaint.RPCClient
}

// NewSignerSubmitter builds the production Submitter.
func NewSignerSubmitter(signer solanaint.Signer, rpc *solanaint.RPCClient) Submitter {
	return &signerSubmitter{signer: signer, rpc: rpc}
}

func (s *signerSubmitter) MerchantAddress(ctx context.Context, tenantID merchant.ID) (solanago.PublicKey, error) {
	return s.signer.PublicKey(ctx, tenantID)
}

func (s *signerSubmitter) Submit(ctx context.Context, tenantID merchant.ID, instructions []solanago.Instruction) (solanago.Signature, error) {
	return solanaint.BuildSignSubmit(ctx, tenantID, s.signer, s.rpc, instructions)
}

// planReader is the minimal RPC read surface PublishPlan needs for its idempotent
// re-publish guard: read the (possibly-already-created) Plan PDA back before
// submitting create_plan. Satisfied by *solanaint.RPCClient. Optional — a nil
// reader skips the guard and submits create_plan directly (the program then
// rejects a duplicate PDA on-chain, so correctness is preserved either way).
type planReader interface {
	GetAccountData(ctx context.Context, address solanago.PublicKey) ([]byte, error)
}

// PlanService publishes recurring Solana plans on-chain (issue #254). The
// create_plan path it drives is verified live on devnet.
type PlanService struct {
	submitter Submitter
	reader    planReader // optional; enables the idempotent re-publish guard
	network   string     // "mainnet" | "devnet"
	tokens    map[string]config.TokenConfig
	now       func() time.Time
}

// NewPlanService builds a PlanService for the given network. The idempotent
// re-publish guard is disabled (no plan reader); prefer NewPlanServiceWithReader
// in production so a re-publish of an already-created plan is a no-op rather than
// a loud on-chain create_plan failure.
func NewPlanService(submitter Submitter, network string, tokens ...map[string]config.TokenConfig) *PlanService {
	return &PlanService{submitter: submitter, network: network, tokens: normalizeRecurringTokens(firstTokenMap(tokens)), now: time.Now}
}

// NewPlanServiceWithReader builds a PlanService that reads the Plan PDA back
// before submitting create_plan, so a re-publish with MATCHING terms is an
// idempotent no-op and a re-publish with DIFFERING terms is rejected (plans are
// immutable on-chain — see PublishPlan).
func NewPlanServiceWithReader(submitter Submitter, reader planReader, network string, tokens ...map[string]config.TokenConfig) *PlanService {
	return &PlanService{submitter: submitter, reader: reader, network: network, tokens: normalizeRecurringTokens(firstTokenMap(tokens)), now: time.Now}
}

// MerchantAddress returns the merchant's on-chain merchant (cranker) address — the
// owner half of a plan PDA. The catalog provider adapter uses it to derive a
// price's plan PDA for an idempotent find-or-attach read-back before publishing.
func (s *PlanService) MerchantAddress(ctx context.Context, tenantID merchant.ID) (solanago.PublicKey, error) {
	return s.submitter.MerchantAddress(ctx, tenantID)
}

func (s *PlanService) ResolveMint(symbol string) (string, int, error) {
	return ResolveRecurringMintFromTokens(symbol, s.tokens)
}

// PublishPlanInput describes a recurring plan to publish on-chain.
type PublishPlanInput struct {
	MerchantID      merchant.ID
	PlanID          uint64 // caller-chosen unique id (the plan PDA derives from it)
	TokenSymbol     string // must be recurring-eligible (USDC/USD1)
	AmountBaseUnits uint64 // fixed charge per period, in token base units
	PeriodHours     uint64 // billing period (0 < h <= 8760)
	ReceivingWallet string // optional cold wallet; sets the plan's destination whitelist
	MetadataURI     string // optional (<=128 bytes)
	EndTs           int64  // 0 = perpetual

	// BillingCycleDays, when > 0, is the source price's billing cycle. PublishPlan
	// then enforces period_hours == BillingCycleDays*24 so the on-chain period can
	// never silently disagree with the price the plan backs. 0 = not provided
	// (consistency check skipped).
	BillingCycleDays int
}

// PlanHandle is the durable record of a published plan, suitable for storing in
// Price.Rails["solana"].
type PlanHandle struct {
	PlanPDA         string
	PlanID          uint64
	Mint            string
	MintSymbol      string
	AmountBaseUnits uint64
	PeriodHours     uint64
	CreatedAt       int64
	MerchantAddress string
	Signature       string
}

// ToRailConfig renders the handle for Price.SetRailConfig(RailSolana, ...).
func (h *PlanHandle) ToRailConfig() map[string]string {
	return map[string]string{
		"plan_pda":          h.PlanPDA,
		"plan_id":           strconv.FormatUint(h.PlanID, 10),
		"mint":              h.Mint,
		"mint_symbol":       h.MintSymbol,
		"amount_base_units": strconv.FormatUint(h.AmountBaseUnits, 10),
		"period_hours":      strconv.FormatUint(h.PeriodHours, 10),
		"created_at":        strconv.FormatInt(h.CreatedAt, 10),
		"merchant_address":  h.MerchantAddress,
	}
}

// PublishPlan validates the token + terms, then signs and submits create_plan
// from the merchant's merchant key, returning the durable plan handle. It fails
// closed: a non-allowlisted token, an out-of-range period, or a zero amount are
// rejected before any on-chain action.
//
// Immutability reality (issue #254, refined by #357). The deployed
// solana-program/subscriptions program publishes plans whose CORE TERMS
// (mint/amount/period) are IMMUTABLE: update_plan (see SunsetPlan) touches only
// the mutable status/end_ts/pullers/metadata fields, never the terms. So
// PublishPlan never mutates an existing plan. Instead, when a reader is wired,
// it is idempotent-or-reject:
//
//   - Plan PDA absent            -> submit create_plan (the normal path).
//   - Plan PDA present, terms MATCH (mint/amount/period) -> idempotent no-op
//     success: return the existing PlanHandle WITHOUT a second create_plan (a
//     duplicate would fail on-chain anyway).
//   - Plan PDA present, terms DIFFER -> reject: plans are immutable, so an
//     amount/period/mint change must be published under a NEW plan_id (and
//     subscribers migrated), not mutated in place.
//
// Token-2022 rejected-extension validation is N/A here: the recurring mint
// allowlist (ResolveRecurringMint) admits ONLY classic-SPL USDC/USD1, so no
// Token-2022 mint (transfer-fee / transfer-hook / etc.) can ever reach this path.
// The allowlist is the validation — there is nothing further to reject.
func (s *PlanService) PublishPlan(ctx context.Context, in PublishPlanInput) (*PlanHandle, error) {
	if in.AmountBaseUnits == 0 {
		return nil, fmt.Errorf("recurring: plan amount must be > 0")
	}
	if in.PeriodHours == 0 || in.PeriodHours > maxPeriodHours {
		return nil, fmt.Errorf("recurring: period_hours must be in (0, %d]", maxPeriodHours)
	}
	// period_hours <-> billing-cycle consistency: the on-chain period is derived
	// from a price's BillingCycleDays as days*24 (see catalog_provider_solana.go).
	// When a caller threads that cycle through (PublishPlanInput.BillingCycleDays
	// > 0), enforce period_hours == BillingCycleDays*24 so an admin call can't
	// publish a plan whose on-chain period silently disagrees with the price.
	// TODO(#254): the admin HTTP surface does not yet carry the price's cycle; once
	// it does, thread BillingCycleDays from there so the check applies to every
	// publish path, not only callers that already have the cycle in hand.
	if in.BillingCycleDays > 0 {
		want := uint64(in.BillingCycleDays) * 24
		if in.PeriodHours != want {
			return nil, fmt.Errorf("recurring: period_hours %d disagrees with billing_cycle_days %d (expected %d = days*24)", in.PeriodHours, in.BillingCycleDays, want)
		}
	}
	symbol := normalizeSymbol(in.TokenSymbol)
	mintStr, _, err := ResolveRecurringMintFromTokens(symbol, s.tokens)
	if err != nil {
		return nil, err // not recurring-eligible / no mint for network
	}
	mint, err := solanago.PublicKeyFromBase58(mintStr)
	if err != nil {
		return nil, fmt.Errorf("recurring: invalid configured mint %q: %w", mintStr, err)
	}

	merchant, err := s.submitter.MerchantAddress(ctx, in.MerchantID)
	if err != nil {
		return nil, fmt.Errorf("recurring: resolve merchant merchant address: %w", err)
	}
	planPDA, _, err := subscriptions.DerivePlanPDA(merchant, in.PlanID)
	if err != nil {
		return nil, fmt.Errorf("recurring: derive plan pda: %w", err)
	}

	// Idempotent re-publish guard (issue #254). Plans are IMMUTABLE on-chain, so a
	// second create_plan on an occupied PDA always fails. When a reader is wired,
	// read the PDA back FIRST: if it already holds a plan with MATCHING terms,
	// return the existing handle as a no-op success; if the terms DIFFER, reject
	// (the operator must publish a NEW plan_id, not mutate this one). A single read
	// is fine here — this is a pre-submit read of (at most) already-confirmed state,
	// not a read-after-our-own-write, so there is no slot to gate on.
	if s.reader != nil {
		data, rerr := s.reader.GetAccountData(ctx, planPDA)
		if rerr != nil {
			return nil, fmt.Errorf("recurring: read plan pda for re-publish guard: %w", rerr)
		}
		if len(data) > 0 {
			existing, derr := subscriptions.DecodePlanAccount(data)
			if derr != nil {
				return nil, fmt.Errorf("recurring: plan pda %s already occupied but undecodable: %w", planPDA, derr)
			}
			if existing.Mint.Equals(mint) &&
				existing.Amount == in.AmountBaseUnits &&
				existing.PeriodHours == in.PeriodHours {
				// Terms match: idempotent no-op, return the existing on-chain handle
				// WITHOUT submitting a duplicate create_plan.
				return &PlanHandle{
					PlanPDA:         planPDA.String(),
					PlanID:          in.PlanID,
					Mint:            mintStr,
					MintSymbol:      symbol,
					AmountBaseUnits: existing.Amount,
					PeriodHours:     existing.PeriodHours,
					CreatedAt:       existing.CreatedAt,
					MerchantAddress: merchant.String(),
				}, nil
			}
			return nil, fmt.Errorf(
				"recurring: plan %s already published with different IMMUTABLE terms "+
					"(on-chain mint=%s amount=%d period_hours=%d; requested mint=%s amount=%d period_hours=%d) — "+
					"plans cannot be mutated; publish a NEW plan_id and migrate subscribers",
				planPDA, existing.Mint, existing.Amount, existing.PeriodHours,
				mint, in.AmountBaseUnits, in.PeriodHours,
			)
		}
	}

	// Pullers/destinations are an OPTIONAL hardening whitelist and must be left
	// EMPTY in the default case: an empty puller list implicitly authorizes the
	// plan owner (the merchant/cranker) to collect, and an empty destination list
	// allows the merchant to receive into its own ATA. Setting pullers WITHOUT a
	// paired destination (or vice-versa) makes the program reject the pull with
	// InvalidAccountOwner — confirmed on devnet. Only when a separate cold
	// receiving wallet is configured do we pin both (puller[0]=merchant pulls,
	// destination[0]=cold wallet), so a compromised cranker key can only pull into
	// the cold wallet. NOTE: when a cold wallet is set, the cranker must also pull
	// into the COLD wallet's ATA (tracked separately) — the crank currently targets
	// the merchant ATA, so a cold-wallet plan needs that wiring before use.
	var destinations, pullers [4]solanago.PublicKey
	if in.ReceivingWallet != "" {
		recv, err := solanago.PublicKeyFromBase58(in.ReceivingWallet)
		if err != nil {
			return nil, fmt.Errorf("recurring: invalid receiving wallet %q: %w", in.ReceivingWallet, err)
		}
		pullers[0] = merchant
		destinations[0] = recv
	}

	createdAt := s.now().UTC().Unix()
	ix, err := subscriptions.BuildCreatePlan(subscriptions.CreatePlanParams{
		Merchant:     merchant,
		PlanPDA:      planPDA,
		Mint:         mint,
		TokenProgram: solanago.TokenProgramID, // USDC/USD1 are classic SPL Token
		PlanID:       in.PlanID,
		Terms:        subscriptions.PlanTerms{Amount: in.AmountBaseUnits, PeriodHours: in.PeriodHours, CreatedAt: createdAt},
		EndTs:        in.EndTs,
		Destinations: destinations,
		Pullers:      pullers,
		MetadataURI:  in.MetadataURI,
	})
	if err != nil {
		return nil, fmt.Errorf("recurring: build create_plan: %w", err)
	}

	sig, err := s.submitter.Submit(ctx, in.MerchantID, []solanago.Instruction{ix})
	if err != nil {
		return nil, fmt.Errorf("recurring: submit create_plan: %w", err)
	}

	// Ensure the receiving ATA(s) exist before the first crank. transfer_subscription
	// deposits INTO the receiver's associated token account; if that ATA is missing
	// the pull reverts. CreateIdempotent is a no-op when the ATA already exists, so
	// this is safe to repeat on every publish. The cranker (merchant) signs + pays.
	//
	// Default case: the merchant collects into its own ATA. Cold-wallet case: funds
	// land in the configured ReceivingWallet's ATA, so we ensure THAT one too (the
	// crank's destination wiring for cold wallets is a separate follow-up — see the
	// destinations/pullers note above).
	if err := s.ensureReceivingATA(ctx, in.MerchantID, merchant, mint); err != nil {
		return nil, err
	}
	if in.ReceivingWallet != "" {
		recv, err := solanago.PublicKeyFromBase58(in.ReceivingWallet)
		if err != nil {
			return nil, fmt.Errorf("recurring: invalid receiving wallet %q: %w", in.ReceivingWallet, err)
		}
		if err := s.ensureReceivingATA(ctx, in.MerchantID, recv, mint); err != nil {
			return nil, err
		}
	}

	// IMPORTANT (issue #254 / the Custom:519 root cause): create_plan OVERWRITES
	// terms.created_at with the on-chain cluster clock, so our client-side
	// `createdAt` (s.now()) is NOT what the program stored — it differs from the
	// cluster clock by the confirmation delay + skew. subscribe later echoes
	// created_at as a consent field and the program rejects a mismatch with
	// PlanTermsMismatch (519). So read the REAL on-chain created_at back from the
	// just-created plan and return THAT in the handle. The read is a
	// read-after-our-own-write, so poll until the plan is visible + decodable
	// (ReadUntilConsistent absorbs RPC read-lag). Fall back to the client value only
	// if no reader is wired.
	onchainCreatedAt := createdAt
	if s.reader != nil {
		data, rerr := solanaint.ReadUntilConsistent(ctx, solanaint.ReadUntilConsistentOpts{},
			func(ctx context.Context) ([]byte, error) { return s.reader.GetAccountData(ctx, planPDA) },
			func(d []byte) bool {
				if len(d) == 0 {
					return false
				}
				pa, e := subscriptions.DecodePlanAccount(d)
				return e == nil && pa.CreatedAt != 0
			},
		)
		if rerr == nil {
			if pa, e := subscriptions.DecodePlanAccount(data); e == nil && pa.CreatedAt != 0 {
				onchainCreatedAt = pa.CreatedAt
			}
		}
	}

	return &PlanHandle{
		PlanPDA:         planPDA.String(),
		PlanID:          in.PlanID,
		Mint:            mintStr,
		MintSymbol:      symbol,
		AmountBaseUnits: in.AmountBaseUnits,
		PeriodHours:     in.PeriodHours,
		CreatedAt:       onchainCreatedAt,
		MerchantAddress: merchant.String(),
		Signature:       sig.String(),
	}, nil
}

// ErrPlanSunsetNotOwned: the plan account at the PDA is owned by a different
// merchant than this merchant's — sunsetting someone else's plan is refused
// before any on-chain action (and would fail the program's owner check anyway).
var ErrPlanSunsetNotOwned = errors.New("recurring: plan is not owned by this merchant's merchant; refusing to sunset")

// SunsetPlan flips an on-chain plan to status=sunset via update_plan (#357/#358):
// the program then rejects NEW subscribe calls ("Plan is in sunset status")
// while existing subscriptions keep billing — the exact archive semantics of
// Stripe's active=false. The caller supplies the CURRENT decoded plan account
// (it has already verified the plan exists and is not yet sunset); its mutable
// fields are echoed so the update changes status and nothing else. Signed by
// the merchant's merchant key, which must equal the plan's owner.
func (s *PlanService) SunsetPlan(ctx context.Context, tenantID merchant.ID, planPDA solanago.PublicKey, current *subscriptions.PlanAccount) (signature string, err error) {
	if current == nil {
		return "", fmt.Errorf("recurring: sunset requires the current plan account")
	}
	merchant, err := s.submitter.MerchantAddress(ctx, tenantID)
	if err != nil {
		return "", fmt.Errorf("recurring: resolve merchant merchant address: %w", err)
	}
	if !current.Owner.Equals(merchant) {
		return "", fmt.Errorf("%w (plan owner %s, merchant merchant %s)", ErrPlanSunsetNotOwned, current.Owner, merchant)
	}
	ix, err := subscriptions.BuildUpdatePlan(subscriptions.UpdatePlanParams{
		Owner:       merchant,
		PlanPDA:     planPDA,
		Status:      subscriptions.PlanStatusSunset,
		EndTs:       current.EndTs,
		Pullers:     current.Pullers,
		MetadataURI: current.MetadataURI,
	})
	if err != nil {
		return "", fmt.Errorf("recurring: build update_plan: %w", err)
	}
	sig, err := s.submitter.Submit(ctx, tenantID, []solanago.Instruction{ix})
	if err != nil {
		return "", fmt.Errorf("recurring: submit update_plan (sunset): %w", err)
	}
	return sig.String(), nil
}

// ensureReceivingATA idempotently provisions owner's associated token account for
// mint (classic SPL Token), paid + signed by the merchant's cranker. A failure here
// is surfaced as a hard error: the plan cannot be billed without a receiving ATA.
func (s *PlanService) ensureReceivingATA(ctx context.Context, tenantID merchant.ID, owner, mint solanago.PublicKey) error {
	ata, _, err := subscriptions.DeriveATA(owner, mint, solanago.TokenProgramID)
	if err != nil {
		return fmt.Errorf("recurring: derive receiving ata for %s: %w", owner, err)
	}
	payer, err := s.submitter.MerchantAddress(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("recurring: resolve cranker (ata payer) address: %w", err)
	}
	ix := subscriptions.BuildCreateIdempotentATA(subscriptions.CreateIdempotentATAParams{
		Payer:        payer,
		ATA:          ata,
		Owner:        owner,
		Mint:         mint,
		TokenProgram: solanago.TokenProgramID,
	})
	if _, err := s.submitter.Submit(ctx, tenantID, []solanago.Instruction{ix}); err != nil {
		return fmt.Errorf("recurring: ensure receiving ata for %s: %w", owner, err)
	}
	return nil
}

func normalizeSymbol(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}
