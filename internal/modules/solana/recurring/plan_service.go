package recurring

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	solanago "github.com/doujins-org/solana-go"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/pkg/tenant"
)

const maxPeriodHours = 8760 // 1 year — the program's upper bound for period_hours

// Submitter is the per-tenant Solana submit surface the recurring services use:
// it resolves the tenant's merchant (cranker) address and signs+submits an
// instruction set with that tenant's key. Declared here (dependency inversion)
// so the services are unit-testable without a live signer/RPC.
type Submitter interface {
	// MerchantAddress returns the tenant's on-chain merchant/cranker address.
	MerchantAddress(ctx context.Context, tenantID tenant.ID) (solanago.PublicKey, error)
	// Submit signs (with the tenant's key) and submits the instructions, returning
	// the confirmed transaction signature.
	Submit(ctx context.Context, tenantID tenant.ID, instructions []solanago.Instruction) (solanago.Signature, error)
}

// signerSubmitter is the production Submitter: a per-tenant solana.Signer + RPC,
// wired through solana.BuildSignSubmit (the verified build/sign/submit path).
type signerSubmitter struct {
	signer solanaint.Signer
	rpc    *solanaint.RPCClient
}

// NewSignerSubmitter builds the production Submitter.
func NewSignerSubmitter(signer solanaint.Signer, rpc *solanaint.RPCClient) Submitter {
	return &signerSubmitter{signer: signer, rpc: rpc}
}

func (s *signerSubmitter) MerchantAddress(ctx context.Context, tenantID tenant.ID) (solanago.PublicKey, error) {
	return s.signer.PublicKey(ctx, tenantID)
}

func (s *signerSubmitter) Submit(ctx context.Context, tenantID tenant.ID, instructions []solanago.Instruction) (solanago.Signature, error) {
	return solanaint.BuildSignSubmit(ctx, tenantID, s.signer, s.rpc, instructions)
}

// PlanService publishes recurring Solana plans on-chain (issue #254). The
// create_plan path it drives is verified live on devnet.
type PlanService struct {
	submitter Submitter
	network   string // "mainnet" | "devnet"
	now       func() time.Time
}

// NewPlanService builds a PlanService for the given network.
func NewPlanService(submitter Submitter, network string) *PlanService {
	return &PlanService{submitter: submitter, network: network, now: time.Now}
}

// MerchantAddress returns the tenant's on-chain merchant (cranker) address — the
// owner half of a plan PDA. The catalog provider adapter uses it to derive a
// price's plan PDA for an idempotent find-or-attach read-back before publishing.
func (s *PlanService) MerchantAddress(ctx context.Context, tenantID tenant.ID) (solanago.PublicKey, error) {
	return s.submitter.MerchantAddress(ctx, tenantID)
}

// PublishPlanInput describes a recurring plan to publish on-chain.
type PublishPlanInput struct {
	TenantID        tenant.ID
	PlanID          uint64 // caller-chosen unique id (the plan PDA derives from it)
	TokenSymbol     string // must be recurring-eligible (USDC/USD1)
	AmountBaseUnits uint64 // fixed charge per period, in token base units
	PeriodHours     uint64 // billing period (0 < h <= 8760)
	ReceivingWallet string // optional cold wallet; sets the plan's destination whitelist
	MetadataURI     string // optional (<=128 bytes)
	EndTs           int64  // 0 = perpetual
}

// PlanHandle is the durable record of a published plan, suitable for storing in
// Price.Processors["solana"].
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

// ToProcessorConfig renders the handle for Price.SetProcessorConfig(ProcessorSolana, ...).
func (h *PlanHandle) ToProcessorConfig() map[string]string {
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
// from the tenant's merchant key, returning the durable plan handle. It fails
// closed: a non-allowlisted token, an out-of-range period, or a zero amount are
// rejected before any on-chain action.
func (s *PlanService) PublishPlan(ctx context.Context, in PublishPlanInput) (*PlanHandle, error) {
	if in.AmountBaseUnits == 0 {
		return nil, fmt.Errorf("recurring: plan amount must be > 0")
	}
	if in.PeriodHours == 0 || in.PeriodHours > maxPeriodHours {
		return nil, fmt.Errorf("recurring: period_hours must be in (0, %d]", maxPeriodHours)
	}
	symbol := normalizeSymbol(in.TokenSymbol)
	mintStr, _, err := ResolveRecurringMint(symbol, s.network)
	if err != nil {
		return nil, err // not recurring-eligible / no mint for network
	}
	mint, err := solanago.PublicKeyFromBase58(mintStr)
	if err != nil {
		return nil, fmt.Errorf("recurring: invalid configured mint %q: %w", mintStr, err)
	}

	merchant, err := s.submitter.MerchantAddress(ctx, in.TenantID)
	if err != nil {
		return nil, fmt.Errorf("recurring: resolve tenant merchant address: %w", err)
	}
	planPDA, _, err := subscriptions.DerivePlanPDA(merchant, in.PlanID)
	if err != nil {
		return nil, fmt.Errorf("recurring: derive plan pda: %w", err)
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

	sig, err := s.submitter.Submit(ctx, in.TenantID, []solanago.Instruction{ix})
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
	if err := s.ensureReceivingATA(ctx, in.TenantID, merchant, mint); err != nil {
		return nil, err
	}
	if in.ReceivingWallet != "" {
		recv, err := solanago.PublicKeyFromBase58(in.ReceivingWallet)
		if err != nil {
			return nil, fmt.Errorf("recurring: invalid receiving wallet %q: %w", in.ReceivingWallet, err)
		}
		if err := s.ensureReceivingATA(ctx, in.TenantID, recv, mint); err != nil {
			return nil, err
		}
	}

	return &PlanHandle{
		PlanPDA:         planPDA.String(),
		PlanID:          in.PlanID,
		Mint:            mintStr,
		MintSymbol:      symbol,
		AmountBaseUnits: in.AmountBaseUnits,
		PeriodHours:     in.PeriodHours,
		CreatedAt:       createdAt,
		MerchantAddress: merchant.String(),
		Signature:       sig.String(),
	}, nil
}

// ensureReceivingATA idempotently provisions owner's associated token account for
// mint (classic SPL Token), paid + signed by the tenant's cranker. A failure here
// is surfaced as a hard error: the plan cannot be billed without a receiving ATA.
func (s *PlanService) ensureReceivingATA(ctx context.Context, tenantID tenant.ID, owner, mint solanago.PublicKey) error {
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
