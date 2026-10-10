package recurring

import (
	"context"
	"fmt"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
)

// tierChangeRPC reads the SubscriptionAuthority, the subscriber's balance and a
// recent blockhash.
type tierChangeRPC interface {
	GetAccountData(ctx context.Context, address solanago.PublicKey) ([]byte, error)
	GetLatestBlockhash(ctx context.Context) (solanago.Hash, error)
	GetTokenBalanceForMint(ctx context.Context, owner solanago.PublicKey, mint solanago.PublicKey) (uint64, error)
}

// PrepareTierChangeService builds the single atomic tier-change transaction.
// Plan terms are immutable and a subscription binds one plan, so a tier change
// is cancel-old + subscribe-new in one all-or-nothing tx:
//
//	UPGRADE   = [cancel(old) + subscribe(new) + transfer_subscription(new,
//	            prorated)]; the cranker pre-signs the transfer, the wallet
//	            completes and submits.
//	DOWNGRADE = [cancel(old) + subscribe(new)], subscriber-only, no charge; the
//	            first pull is deferred to the old period end.
//
// The SubscriptionAuthority already exists (same-mint subscription), so there is
// no init step.
type PrepareTierChangeService struct {
	signer  solanaint.Signer // the cranker: provides the merchant address + co-signs upgrades
	rpc     tierChangeRPC
	network string
	tokens  map[string]config.TokenConfig
}

// NewPrepareTierChangeService builds a PrepareTierChangeService.
func NewPrepareTierChangeService(signer solanaint.Signer, rpc tierChangeRPC, network string, tokens ...map[string]config.TokenConfig) *PrepareTierChangeService {
	return &PrepareTierChangeService{signer: signer, rpc: rpc, network: network, tokens: normalizeRecurringTokens(firstTokenMap(tokens))}
}

// PrepareTierChangeInput describes the old subscription + the new plan. New-plan
// terms are the canonical, server-resolved values from the new price's config;
// the old on-chain identifiers come from the stored solana_subscriptions row.
type PrepareTierChangeInput struct {
	MerchantID       billing.MerchantID
	SubscriberWallet string
	MintSymbol       string

	// Old subscription on-chain identifiers (from the solana_subscriptions row).
	OldPlanPDA         string
	OldSubscriptionPDA string

	// New plan terms.
	NewPlanID          uint64
	NewAmountBaseUnits uint64
	NewPeriodHours     uint64
	NewPlanCreatedAt   int64

	// IsUpgrade selects the bundle: true -> charge the prorated first pull now
	// (atomic, co-signed); false (downgrade) -> no charge, deferred.
	IsUpgrade bool
	// FirstChargeBaseUnits is the prorated first pull for an upgrade
	// (new_full - old_unused, in token base units). Ignored for downgrades.
	FirstChargeBaseUnits uint64

	// Reference, when set, tags the tx with a Solana Pay reference
	// (referenceTagInstruction) so a checkout attempt can drive the tier change.
	Reference string
}

// PrepareTierChangeResult is the transaction the wallet must sign + send.
type PrepareTierChangeResult struct {
	// Transaction is base64-encoded: PARTIALLY signed (cranker pre-signed) for an
	// upgrade, or fully UNSIGNED for a downgrade.
	Transaction string
	// Kind is "upgrade" or "downgrade".
	Kind string
	// NewSubscriptionPDA is the new on-chain subscription account (for the confirm
	// step + the persisted row).
	NewSubscriptionPDA string
}

// Prepare builds the atomic tier-change transaction. For an upgrade it returns a
// partially-signed bundle (cranker co-signed the transfer); for a downgrade a
// plain unsigned [cancel+subscribe] the wallet signs alone.
func (s *PrepareTierChangeService) Prepare(ctx context.Context, in PrepareTierChangeInput) (*PrepareTierChangeResult, error) {
	if in.SubscriberWallet == "" {
		return nil, fmt.Errorf("recurring: subscriber wallet is required")
	}
	if in.NewAmountBaseUnits == 0 || in.NewPeriodHours == 0 {
		return nil, fmt.Errorf("recurring: invalid new plan terms (amount/period)")
	}
	if in.IsUpgrade && in.FirstChargeBaseUnits == 0 {
		return nil, fmt.Errorf("recurring: upgrade requires a non-zero prorated first charge")
	}

	mintStr, err := ResolveRecurringMintFromTokens(in.MintSymbol, s.tokens)
	if err != nil {
		return nil, err
	}
	mint, err := solanago.PublicKeyFromBase58(mintStr)
	if err != nil {
		return nil, fmt.Errorf("recurring: invalid mint: %w", err)
	}
	subscriber, err := solanago.PublicKeyFromBase58(in.SubscriberWallet)
	if err != nil {
		return nil, fmt.Errorf("recurring: invalid subscriber wallet: %w", err)
	}
	oldPlanPDA, err := parseKey("old_plan_pda", in.OldPlanPDA)
	if err != nil {
		return nil, err
	}
	oldSubPDA, err := parseKey("old_subscription_pda", in.OldSubscriptionPDA)
	if err != nil {
		return nil, err
	}
	merchant, err := s.signer.PublicKey(ctx, in.MerchantID)
	if err != nil {
		return nil, fmt.Errorf("recurring: resolve merchant: %w", err)
	}

	newPlanPDA, newPlanBump, err := subscriptions.DerivePlanPDA(merchant, in.NewPlanID)
	if err != nil {
		return nil, err
	}
	newSubPDA, _, err := subscriptions.DeriveSubscriptionPDA(newPlanPDA, subscriber)
	if err != nil {
		return nil, err
	}
	saPDA, _, err := subscriptions.DeriveSubscriptionAuthority(subscriber, mint)
	if err != nil {
		return nil, err
	}
	eventAuth, _, err := subscriptions.DeriveEventAuthority()
	if err != nil {
		return nil, fmt.Errorf("recurring: derive event authority: %w", err)
	}

	// The subscriber already has an authority for this mint (they're changing tier
	// on an existing same-mint subscription) — read its initId for the subscribe.
	initID, exists, err := readAuthorityInitID(ctx, s.rpc, saPDA)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("recurring: subscription authority %s not found — expected an existing same-mint subscription for a tier change", saPDA)
	}

	cancelOld := subscriptions.BuildCancelSubscription(subscriptions.CancelOrResumeParams{
		Subscriber:      subscriber,
		PlanPDA:         oldPlanPDA,
		SubscriptionPDA: oldSubPDA,
		EventAuthority:  eventAuth,
	})
	subscribeNew := subscriptions.BuildSubscribe(subscriptions.SubscribeParams{
		Subscriber:                     subscriber,
		Merchant:                       merchant,
		PlanPDA:                        newPlanPDA,
		SubscriptionPDA:                newSubPDA,
		SubscriptionAuthorityPDA:       saPDA,
		EventAuthority:                 eventAuth,
		PlanID:                         in.NewPlanID,
		PlanBump:                       newPlanBump,
		ExpectedMint:                   mint,
		ExpectedAmount:                 in.NewAmountBaseUnits,
		ExpectedPeriodHours:            in.NewPeriodHours,
		ExpectedCreatedAt:              in.NewPlanCreatedAt,
		ExpectedSubscriptionAuthInitID: initID,
	})

	result := &PrepareTierChangeResult{NewSubscriptionPDA: newSubPDA.String()}

	if !in.IsUpgrade {
		// Downgrade: cancel old + subscribe new, no immediate charge. Wallet signs
		// alone. The cranker defers the first pull to the old period end.
		ixs, err := withReference([]solanago.Instruction{cancelOld, subscribeNew}, subscriber, in.Reference)
		if err != nil {
			return nil, err
		}
		tx, err := buildTierChangeUnsignedTxBase64(ctx, s.rpc, subscriber, ixs)
		if err != nil {
			return nil, err
		}
		result.Transaction = tx
		result.Kind = "downgrade"
		return result, nil
	}

	// Pre-flight: the upgrade pulls the prorated charge now, so an underfunded
	// wallet gets the same InsufficientUSDCError as subscribe. Fail-open on a
	// read error: the atomic tx reverts if short.
	if have, berr := s.rpc.GetTokenBalanceForMint(ctx, subscriber, mint); berr == nil && have < in.FirstChargeBaseUnits {
		return nil, &InsufficientUSDCError{HaveBaseUnits: have, NeedBaseUnits: in.FirstChargeBaseUnits}
	}

	// Upgrade: add the prorated first pull. transfer_subscription requires the
	// merchant (cranker) as the caller-signer -> partial-sign that slot; the wallet
	// completes the cancel/subscribe/fee-payer slot.
	delegatorATA, _, err := subscriptions.DeriveATA(subscriber, mint, solanago.TokenProgramID)
	if err != nil {
		return nil, fmt.Errorf("recurring: derive delegator ata: %w", err)
	}
	receiverATA, _, err := subscriptions.DeriveATA(merchant, mint, solanago.TokenProgramID)
	if err != nil {
		return nil, fmt.Errorf("recurring: derive receiver ata: %w", err)
	}
	transferNew := subscriptions.BuildTransferSubscription(subscriptions.TransferSubscriptionParams{
		SubscriptionPDA:       newSubPDA,
		PlanPDA:               newPlanPDA,
		SubscriptionAuthority: saPDA,
		DelegatorATA:          delegatorATA,
		ReceiverATA:           receiverATA,
		Caller:                merchant,
		Mint:                  mint,
		TokenProgram:          solanago.TokenProgramID,
		EventAuthority:        eventAuth,
		Amount:                in.FirstChargeBaseUnits,
		Delegator:             subscriber,
	})

	ixs, err := withReference([]solanago.Instruction{cancelOld, subscribeNew, transferNew}, subscriber, in.Reference)
	if err != nil {
		return nil, err
	}
	tx, err := solanaint.BuildPartiallySignedTx(ctx, in.MerchantID, s.signer, s.rpc, subscriber, ixs)
	if err != nil {
		return nil, err
	}
	result.Transaction = tx
	result.Kind = "upgrade"
	return result, nil
}

// buildTierChangeUnsignedTxBase64 assembles an unsigned transaction (payer signs + sends)
// with a recent blockhash, base64-encoded. Shared by the downgrade path.
func buildTierChangeUnsignedTxBase64(ctx context.Context, rpc tierChangeRPC, payer solanago.PublicKey, ixs []solanago.Instruction) (string, error) {
	blockhash, err := rpc.GetLatestBlockhash(ctx)
	if err != nil {
		return "", fmt.Errorf("recurring: get recent blockhash: %w", err)
	}
	tx, err := solanago.NewTransaction(ixs, blockhash, solanago.TransactionPayer(payer))
	if err != nil {
		return "", fmt.Errorf("recurring: build transaction: %w", err)
	}
	return marshalUnsignedTxBase64(tx)
}
