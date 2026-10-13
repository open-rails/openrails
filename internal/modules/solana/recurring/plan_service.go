package recurring

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
)

const maxPeriodHours = 8760 // 1 year — the program's upper bound for period_hours

// Submitter is the recurring services' Solana submit surface: the
// deployment's crankers, signing and submitting on the merchant's network.
type Submitter interface {
	// Publisher is the key new plans are published by, the first listed
	// cranker's. ErrNoCranker when none is listed.
	Publisher(ctx context.Context) (solanago.PublicKey, error)
	// Signer is the listed cranker holding owner's key: a plan's owner pulls
	// it and pays the fees. ErrUnheldPlan when none does.
	Signer(ctx context.Context, owner solanago.PublicKey) (solanaint.Signer, error)
	// Submit signs as owner and submits the instructions on the merchant's
	// network, returning the confirmed transaction signature.
	Submit(ctx context.Context, tenantID billing.MerchantID, owner solanago.PublicKey, instructions []solanago.Instruction) (solanago.Signature, error)
}

// RPCResolver arms the merchant's Solana RPC client at use time. nil client
// with nil error = not armed.
type RPCResolver func(ctx context.Context, merchantID billing.MerchantID) (*solanaint.RPCClient, error)

// signerSubmitter is the production Submitter: the crankers over
// solana.BuildSignSubmit, on the RPC of the merchant whose work they sign.
type signerSubmitter struct {
	crankers *Crankers
	resolve  RPCResolver
}

// NewSignerSubmitter builds the production Submitter; no crankers turn
// rebilling off.
func NewSignerSubmitter(crankers *Crankers, resolve RPCResolver) Submitter {
	return &signerSubmitter{crankers: crankers, resolve: resolve}
}

func (s *signerSubmitter) rpcFor(ctx context.Context, tenantID billing.MerchantID) (*solanaint.RPCClient, error) {
	if s.resolve != nil {
		rpc, err := s.resolve(ctx, tenantID)
		if err != nil {
			return nil, err
		}
		if rpc != nil {
			return rpc, nil
		}
	}
	return nil, fmt.Errorf("solana: no RPC client armed for merchant %s (#728)", tenantID.String())
}

func (s *signerSubmitter) Publisher(ctx context.Context) (solanago.PublicKey, error) {
	_, key, err := s.crankers.Publisher(ctx)
	return key, err
}

func (s *signerSubmitter) Signer(ctx context.Context, owner solanago.PublicKey) (solanaint.Signer, error) {
	return s.crankers.Owner(ctx, owner)
}

func (s *signerSubmitter) Submit(ctx context.Context, tenantID billing.MerchantID, owner solanago.PublicKey, instructions []solanago.Instruction) (solanago.Signature, error) {
	return s.SubmitWithPresubmit(ctx, tenantID, owner, instructions, nil)
}

// SubmitWithPresubmit persists the signed tx signature via presubmit before
// submission.
func (s *signerSubmitter) SubmitWithPresubmit(ctx context.Context, tenantID billing.MerchantID, owner solanago.PublicKey, instructions []solanago.Instruction, presubmit func(solanago.Signature) error) (solanago.Signature, error) {
	signer, err := s.crankers.Owner(ctx, owner)
	if err != nil {
		return solanago.Signature{}, err
	}
	rpc, err := s.rpcFor(ctx, tenantID)
	if err != nil {
		return solanago.Signature{}, err
	}
	return solanaint.BuildSignSubmitPresubmit(ctx, signer, rpc, instructions, presubmit)
}

// planReader reads the Plan PDA back before create_plan for PublishPlan's
// idempotent re-publish guard. Optional: without it create_plan is submitted
// directly and the program rejects a duplicate PDA.
type planReader interface {
	GetAccountData(ctx context.Context, address solanago.PublicKey) ([]byte, error)
}

// PlanService publishes recurring Solana plans on-chain: owned by the
// deployment's first cranker, paying into the merchant's treasury.
type PlanService struct {
	submitter Submitter
	reader    planReader // optional; enables the idempotent re-publish guard
	tokens    Tokens
	treasury  Treasury
	now       func() time.Time
}

// NewPlanServiceWithReader builds a PlanService that reads the Plan PDA back
// before submitting create_plan, so a re-publish with MATCHING terms is an
// idempotent no-op and a re-publish with DIFFERING terms is rejected (plans are
// immutable on-chain — see PublishPlan).
func NewPlanServiceWithReader(submitter Submitter, reader planReader, tokens Tokens, treasury Treasury) *PlanService {
	return &PlanService{submitter: submitter, reader: reader, tokens: tokens, treasury: treasury, now: time.Now}
}

// Publisher is the key new plans are published by, the owner half of their
// PDAs; ErrNoCranker when the deployment lists no cranker.
func (s *PlanService) Publisher(ctx context.Context) (solanago.PublicKey, error) {
	return s.submitter.Publisher(ctx)
}

// Owns refuses a plan owner no listed cranker holds: its plans cannot be
// pulled.
func (s *PlanService) Owns(ctx context.Context, owner solanago.PublicKey) error {
	_, err := s.submitter.Signer(ctx, owner)
	return err
}

// Treasury is the merchant's treasury: every plan's destination.
func (s *PlanService) Treasury(ctx context.Context, tenantID billing.MerchantID) (solanago.PublicKey, error) {
	if s.treasury == nil {
		return solanago.PublicKey{}, fmt.Errorf("recurring: no Solana treasury resolver")
	}
	return s.treasury(ctx, tenantID)
}

// PaysInto refuses a published plan whose destination is not the merchant's
// treasury.
func (s *PlanService) PaysInto(ctx context.Context, tenantID billing.MerchantID, plan solanago.PublicKey, destinations [4]solanago.PublicKey) error {
	treasury, err := s.Treasury(ctx, tenantID)
	if err != nil {
		return err
	}
	return planPaysInto(plan, destinations, treasury)
}

// Network is the network the ctx merchant's Solana PSP pays on.
func (s *PlanService) Network(ctx context.Context) string { return s.tokens.network(ctx) }

// ResolveMint returns the merchant's configured mint for a recurring-eligible
// symbol. Decimals come from the mint on-chain (MintDecimals).
func (s *PlanService) ResolveMint(ctx context.Context, symbol string) (string, error) {
	return ResolveRecurringMintFromTokens(symbol, s.tokens.of(ctx))
}

// MintDecimals returns the ON-CHAIN base-unit precision of the mint backing
// symbol. This is the ONLY decimals source on the recurring path: a plan's
// amount is written immutably on-chain, so a wrong shift cannot be corrected
// afterwards. Fails closed when no reader is wired or the mint is unreadable.
func (s *PlanService) MintDecimals(ctx context.Context, symbol string) (int, error) {
	mintStr, err := s.ResolveMint(ctx, symbol)
	if err != nil {
		return 0, err
	}
	return s.mintDecimals(ctx, mintStr)
}

// mintDecimals reads + validates a mint's on-chain decimals through the wired
// reader.
func (s *PlanService) mintDecimals(ctx context.Context, mintStr string) (int, error) {
	if s.reader == nil {
		return 0, fmt.Errorf("recurring: no chain reader armed to read mint %s decimals (#817)", mintStr)
	}
	mint, err := solanago.PublicKeyFromBase58(mintStr)
	if err != nil {
		return 0, fmt.Errorf("recurring: invalid configured mint %q: %w", mintStr, err)
	}
	decimals, err := solanaint.ReadMintDecimals(ctx, s.reader, mint)
	if err != nil {
		return 0, err
	}
	if err := config.ValidateTokenDecimals(mintStr, decimals); err != nil {
		return 0, err
	}
	return decimals, nil
}

// PublishPlanInput describes a recurring plan to publish on-chain.
type PublishPlanInput struct {
	MerchantID      billing.MerchantID
	PlanID          uint64 // caller-chosen unique id (the plan PDA derives from it)
	TokenSymbol     string // must be in RecurringStablecoins
	AmountBaseUnits uint64 // fixed charge per period, in token base units
	PeriodHours     uint64 // billing period (0 < h <= 8760)

	// AmountDecimals is the precision AmountBaseUnits was computed at;
	// PublishPlan refuses it unless it matches the mint's on-chain decimals,
	// since the plan amount is immutable once created.
	AmountDecimals int
	MetadataURI    string // optional (<=128 bytes)
	EndTs          int64  // 0 = perpetual

	// BillingCycleHours, when > 0, is the source price's billing cycle. PublishPlan
	// then enforces period_hours == BillingCycleHours so the on-chain period can
	// never silently disagree with the price the plan backs. 0 = not provided
	// (consistency check skipped).
	BillingCycleHours int
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
	// MerchantAddress is the plan's owner: the cranker that published it.
	MerchantAddress string
	// Treasury is the plan's destination, fixed when it was published.
	Treasury  string
	Signature string
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
		"treasury":          h.Treasury,
	}
}

// PublishPlan validates the token and terms, then has the cranker sign and
// submit create_plan with the merchant's treasury as the plan's only
// destination, returning the durable plan handle. Plan terms (mint, amount,
// period, destinations) are immutable on-chain (update_plan touches only
// status/end_ts/pullers/metadata), so with a reader wired a publish is
// idempotent-or-reject: absent PDA -> create_plan; matching terms -> the
// existing handle; differing terms -> refused (publish a new plan_id).
// Token-2022 extension checks are unneeded: the allowlist admits only classic
// SPL mints.
func (s *PlanService) PublishPlan(ctx context.Context, in PublishPlanInput) (*PlanHandle, error) {
	if in.AmountBaseUnits == 0 {
		return nil, fmt.Errorf("recurring: plan amount must be > 0")
	}
	if in.PeriodHours == 0 || in.PeriodHours > maxPeriodHours {
		return nil, fmt.Errorf("recurring: period_hours must be in (0, %d]", maxPeriodHours)
	}
	// The on-chain period must match the price's billing cycle when given.
	if in.BillingCycleHours > 0 {
		want := uint64(in.BillingCycleHours)
		if in.PeriodHours != want {
			return nil, fmt.Errorf("recurring: period_hours %d disagrees with billing_cycle_hours %d", in.PeriodHours, in.BillingCycleHours)
		}
	}
	symbol := normalizeSymbol(in.TokenSymbol)
	mintStr, err := ResolveRecurringMintFromTokens(symbol, s.tokens.of(ctx))
	if err != nil {
		return nil, err // not recurring-eligible / no mint for network
	}
	mint, err := solanago.PublicKeyFromBase58(mintStr)
	if err != nil {
		return nil, fmt.Errorf("recurring: invalid configured mint %q: %w", mintStr, err)
	}

	// The plan amount is immutable on-chain, so the caller's shift must be the
	// mint's on-chain decimals; a guess is refused.
	onchainDecimals, err := s.mintDecimals(ctx, mintStr)
	if err != nil {
		return nil, err
	}
	if in.AmountDecimals != onchainDecimals {
		return nil, fmt.Errorf(
			"recurring: plan amount %d was computed at %d decimals but mint %s (%s) has %d on-chain — "+
				"refusing to publish an immutable plan at the wrong scale (#817)",
			in.AmountBaseUnits, in.AmountDecimals, symbol, mintStr, onchainDecimals)
	}

	merchant, err := s.submitter.Publisher(ctx)
	if err != nil {
		return nil, fmt.Errorf("recurring: resolve cranker: %w", err)
	}
	treasury, err := s.Treasury(ctx, in.MerchantID)
	if err != nil {
		return nil, err
	}
	planPDA, _, err := subscriptions.DerivePlanPDA(merchant, in.PlanID)
	if err != nil {
		return nil, fmt.Errorf("recurring: derive plan pda: %w", err)
	}

	// Idempotent re-publish guard: a matching plan at the PDA returns its
	// handle; differing terms are refused. One read suffices: this is
	// already-confirmed state, not our own write, so no slot gate.
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
			if err := planPaysInto(planPDA, existing.Destinations, treasury); err != nil {
				return nil, err
			}
			if existing.Mint.Equals(mint) &&
				existing.Amount == in.AmountBaseUnits &&
				existing.PeriodHours == in.PeriodHours {
				// Terms match: no second create_plan. A publish that failed after
				// create_plan landed still lacks its receiving ATAs, so ensure them.
				if err := s.ensureReceivingATA(ctx, in.MerchantID, treasury, mint); err != nil {
					return nil, err
				}
				return &PlanHandle{
					PlanPDA:         planPDA.String(),
					PlanID:          in.PlanID,
					Mint:            mintStr,
					MintSymbol:      symbol,
					AmountBaseUnits: existing.Amount,
					PeriodHours:     existing.PeriodHours,
					CreatedAt:       existing.CreatedAt,
					MerchantAddress: merchant.String(),
					Treasury:        treasury.String(),
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

	// The treasury is the only destination, so the cranker can pull into
	// nothing else; pullers stay empty: only the owner pulls.
	var destinations, pullers [4]solanago.PublicKey
	destinations[0] = treasury

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

	sig, err := s.submitter.Submit(ctx, in.MerchantID, merchant, []solanago.Instruction{ix})
	if err != nil {
		return nil, fmt.Errorf("recurring: submit create_plan: %w", err)
	}

	// transfer_subscription reverts without the receiver ATA, so ensure it
	// before the first crank; a failure is retried by the next publish.
	if err := s.ensureReceivingATA(ctx, in.MerchantID, treasury, mint); err != nil {
		return nil, err
	}

	// create_plan overwrites terms.created_at with the cluster clock, and
	// subscribe echoes it (a mismatch is PlanTermsMismatch, 519), so return the
	// on-chain value. This reads our own write: ReadUntilConsistent absorbs RPC
	// read-lag. The client value is used only without a reader.
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
		Treasury:        treasury.String(),
		Signature:       sig.String(),
	}, nil
}

// ensureReceivingATA idempotently provisions owner's associated token account for
// mint (classic SPL Token), paid + signed by the cranker; an ATA already on
// chain submits nothing. A failure is a hard error: the plan cannot be billed
// without a receiving ATA.
func (s *PlanService) ensureReceivingATA(ctx context.Context, tenantID billing.MerchantID, owner, mint solanago.PublicKey) error {
	ata, _, err := subscriptions.DeriveATA(owner, mint, solanago.TokenProgramID)
	if err != nil {
		return fmt.Errorf("recurring: derive receiving ata for %s: %w", owner, err)
	}
	if s.reader != nil {
		data, err := s.reader.GetAccountData(ctx, ata)
		if err != nil {
			return fmt.Errorf("recurring: read receiving ata for %s: %w", owner, err)
		}
		if len(data) > 0 {
			return nil
		}
	}
	payer, err := s.submitter.Publisher(ctx)
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
	if _, err := s.submitter.Submit(ctx, tenantID, payer, []solanago.Instruction{ix}); err != nil {
		return fmt.Errorf("recurring: ensure receiving ata for %s: %w", owner, err)
	}
	return nil
}

func normalizeSymbol(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}
