package recurring

import (
	"context"
	"fmt"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/billing"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

const tierChangeQuotePrefix = "openrails:tier-change:1:"

// tierChangeRPC reads the SubscriptionAuthority, the subscriber's balance and a
// recent blockhash with the height it stops being landable at.
type tierChangeRPC interface {
	GetAccountData(ctx context.Context, address solanago.PublicKey) ([]byte, error)
	LatestBlockhash(ctx context.Context) (solanaint.RecentBlockhash, error)
	GetTokenBalanceForMint(ctx context.Context, owner solanago.PublicKey, mint solanago.PublicKey) (uint64, error)
}

// PrepareTierChangeService builds the single atomic tier-change transaction.
// Plan terms are immutable and a subscription binds one plan, so a tier change
// is cancel-old + subscribe-new in one all-or-nothing tx:
//
//	UPGRADE   = [cancel(old) + subscribe(new) + transfer_subscription(new,
//	            prorated)]; the cranker pre-signs the transfer, the wallet
//	            completes and submits.
//	DOWNGRADE = [cancel(old) + subscribe(new)], no charge; the first pull is
//	            deferred to the old period end.
//
// The new plan's cranker co-signs both, so the wallet cannot alter the
// message: the transaction that lands is the one prepared, on the blockhash
// whose last valid height is recorded with the pending change.
//
// The SubscriptionAuthority already exists (same-mint subscription), so there is
// no init step.
type PrepareTierChangeService struct {
	submitter Submitter // the new plan owner's cranker co-signs upgrades
	rpc       tierChangeRPC
	tokens    Tokens
}

// NewPrepareTierChangeService builds a PrepareTierChangeService.
func NewPrepareTierChangeService(submitter Submitter, rpc tierChangeRPC, tokens Tokens) *PrepareTierChangeService {
	return &PrepareTierChangeService{submitter: submitter, rpc: rpc, tokens: tokens}
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
	// New plan: its owner, the listed cranker that co-signs an upgrade's
	// pull, and its treasury, where its pulls land.
	NewPlanOwner string
	Treasury     string

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
	// FirstChargeMicros and Currency bind the fiat quote to that token pull.
	FirstChargeMicros int64
	Currency          string

	// Reference tags the transaction (referenceTagInstruction): the address
	// its pending change is found by on chain. Required.
	Reference string
}

// PrepareTierChangeResult is the transaction the wallet must sign + send.
type PrepareTierChangeResult struct {
	// Transaction is base64-encoded and partially signed: the cranker has
	// signed, the wallet completes it.
	Transaction string
	// Kind is "upgrade" or "downgrade".
	Kind string
	// NewSubscriptionPDA is the new on-chain subscription account (for the confirm
	// step + the persisted row).
	NewSubscriptionPDA string
	// LastValidBlockHeight: past it the transaction can never land.
	LastValidBlockHeight uint64
}

// Prepare builds the atomic tier-change transaction, co-signed by the new
// plan's cranker: with the prorated transfer for an upgrade, [cancel+subscribe]
// for a downgrade.
func (s *PrepareTierChangeService) Prepare(ctx context.Context, in PrepareTierChangeInput) (*PrepareTierChangeResult, error) {
	if in.SubscriberWallet == "" {
		return nil, fmt.Errorf("recurring: subscriber wallet is required")
	}
	if in.Reference == "" {
		return nil, fmt.Errorf("recurring: a plan change needs the reference it is found by")
	}
	if in.NewAmountBaseUnits == 0 || in.NewPeriodHours == 0 {
		return nil, fmt.Errorf("recurring: invalid new plan terms (amount/period)")
	}
	if in.IsUpgrade && in.FirstChargeBaseUnits == 0 {
		return nil, fmt.Errorf("recurring: upgrade requires a non-zero prorated first charge")
	}
	if _, ok := moneyutil.LookupCurrency(in.Currency); !ok || in.FirstChargeMicros < 0 || (!in.IsUpgrade && in.FirstChargeMicros != 0) {
		return nil, fmt.Errorf("recurring: invalid first-charge quote")
	}

	mintStr, err := ResolveRecurringMintFromTokens(in.MintSymbol, s.tokens.of(ctx))
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
	merchant, err := parseKey("new plan owner", in.NewPlanOwner)
	if err != nil {
		return nil, err
	}
	treasury, err := parseKey("treasury", in.Treasury)
	if err != nil {
		return nil, err
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

	// One blockhash, read once: its last valid height bounds the transaction.
	blockhash, err := s.rpc.LatestBlockhash(ctx)
	if err != nil {
		return nil, fmt.Errorf("recurring: get recent blockhash: %w", err)
	}
	result := &PrepareTierChangeResult{NewSubscriptionPDA: newSubPDA.String(), LastValidBlockHeight: blockhash.LastValidBlockHeight}

	// The cranker signs the fiat quote in the same message. Confirmation must
	// not reconstruct the quote using a later clock or rate.
	quoted := []byte(fmt.Sprintf("%s%s:%d", tierChangeQuotePrefix, moneyutil.NormalizeCurrency(in.Currency), in.FirstChargeMicros))
	signer, err := s.submitter.Signer(ctx, merchant)
	if err != nil {
		return nil, err
	}

	if !in.IsUpgrade {
		// Downgrade: cancel old + subscribe new, no immediate charge; the
		// cranker defers the first pull to the old period end. Nothing else in
		// it needs the cranker, so the quote memo names it as a signer.
		quote := solanago.NewInstruction(solanago.MemoProgramID, solanago.AccountMetaSlice{solanago.NewAccountMeta(merchant, false, true)}, quoted)
		ixs, err := withReference([]solanago.Instruction{cancelOld, subscribeNew, quote}, subscriber, in.Reference)
		if err != nil {
			return nil, err
		}
		tx, err := solanaint.BuildPartiallySignedTx(ctx, signer, fixedBlockhash(blockhash.Hash), subscriber, ixs)
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
	receiverATA, _, err := subscriptions.DeriveATA(treasury, mint, solanago.TokenProgramID)
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

	ixs, err := withReference([]solanago.Instruction{cancelOld, subscribeNew, transferNew, solanaint.NewMemoInstruction(string(quoted))}, subscriber, in.Reference)
	if err != nil {
		return nil, err
	}
	tx, err := solanaint.BuildPartiallySignedTx(ctx, signer, fixedBlockhash(blockhash.Hash), subscriber, ixs)
	if err != nil {
		return nil, err
	}
	result.Transaction = tx
	result.Kind = "upgrade"
	return result, nil
}

// fixedBlockhash hands the co-signer the blockhash already read.
type fixedBlockhash solanago.Hash

func (h fixedBlockhash) GetLatestBlockhash(context.Context) (solanago.Hash, error) {
	return solanago.Hash(h), nil
}
