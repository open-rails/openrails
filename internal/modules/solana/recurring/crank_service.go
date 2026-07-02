package recurring

import (
	"context"
	"fmt"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// CrankService executes the recurring pull ("cranking") — one
// transfer_subscription per subscriber per cycle (issue #256). The merchant
// (cranker) signs and pays the (tiny) gas; funds move from the subscriber's ATA
// to the merchant/destination ATA. The transfer_subscription path is verified
// live on devnet.
type CrankService struct {
	submitter Submitter
}

type merchantAddressSubmitter interface {
	SubmitForMerchantAddress(ctx context.Context, tenantID merchant.ID, merchantAddress solanago.PublicKey, instructions []solanago.Instruction) (solanago.Signature, error)
}

// Presubmit capabilities (#674): the production signerSubmitter persists the
// signed tx signature via the caller's hook BEFORE submission, so a crash
// mid-submit is resolvable by a chain read. Optional — fakes without them
// simply skip the write-ahead.
type presubmitSubmitter interface {
	SubmitWithPresubmit(ctx context.Context, tenantID merchant.ID, instructions []solanago.Instruction, presubmit func(solanago.Signature) error) (solanago.Signature, error)
}

type presubmitMerchantAddressSubmitter interface {
	SubmitForMerchantAddressWithPresubmit(ctx context.Context, tenantID merchant.ID, merchantAddress solanago.PublicKey, instructions []solanago.Instruction, presubmit func(solanago.Signature) error) (solanago.Signature, error)
}

// NewCrankService builds a CrankService over a per-merchant Submitter.
func NewCrankService(submitter Submitter) *CrankService {
	return &CrankService{submitter: submitter}
}

// Crank pulls amountBaseUnits for one subscription. On Solana, an underfunded
// pull reverts atomically (no partial charge); the caller classifies the error
// (insufficient USDC -> dunning vs operational -> retry; see #257).
func (s *CrankService) Crank(ctx context.Context, tenantID merchant.ID, sub *models.SolanaSubscription, amountBaseUnits uint64) (string, error) {
	return s.CrankWithPresubmit(ctx, tenantID, sub, amountBaseUnits, nil)
}

// CrankWithPresubmit is Crank with a signature write-ahead hook (#674):
// presubmit(sig) runs after signing, before submission. nil presubmit = plain
// Crank.
func (s *CrankService) CrankWithPresubmit(ctx context.Context, tenantID merchant.ID, sub *models.SolanaSubscription, amountBaseUnits uint64, presubmit func(signature string) error) (string, error) {
	if sub == nil {
		return "", fmt.Errorf("recurring: nil subscription")
	}
	if amountBaseUnits == 0 {
		return "", fmt.Errorf("recurring: crank amount must be > 0")
	}

	merchant, err := parseKey("merchant_address", sub.MerchantAddress)
	if err != nil {
		return "", err
	}
	mint, err := parseKey("mint", sub.Mint)
	if err != nil {
		return "", err
	}
	subscriber, err := parseKey("subscriber_wallet", sub.SubscriberWallet)
	if err != nil {
		return "", err
	}
	planPDA, err := parseKey("plan_pda", sub.PlanPDA)
	if err != nil {
		return "", err
	}
	subPDA, err := parseKey("subscription_pda", sub.SubscriptionPDA)
	if err != nil {
		return "", err
	}
	saPDA, err := parseKey("authority_pda", sub.AuthorityPDA)
	if err != nil {
		return "", err
	}

	delegatorATA, _, err := subscriptions.DeriveATA(subscriber, mint, solanago.TokenProgramID)
	if err != nil {
		return "", fmt.Errorf("recurring: derive delegator ata: %w", err)
	}
	// Receiver = the merchant's ATA. (When a plan whitelists a cold destination,
	// that wallet's ATA is the receiver; tracked as a #258 refinement.)
	receiverATA, _, err := subscriptions.DeriveATA(merchant, mint, solanago.TokenProgramID)
	if err != nil {
		return "", fmt.Errorf("recurring: derive receiver ata: %w", err)
	}
	eventAuth, _, err := subscriptions.DeriveEventAuthority()
	if err != nil {
		return "", fmt.Errorf("recurring: derive event authority: %w", err)
	}

	ix := subscriptions.BuildTransferSubscription(subscriptions.TransferSubscriptionParams{
		SubscriptionPDA:       subPDA,
		PlanPDA:               planPDA,
		SubscriptionAuthority: saPDA,
		DelegatorATA:          delegatorATA,
		ReceiverATA:           receiverATA,
		Caller:                merchant,
		Mint:                  mint,
		TokenProgram:          solanago.TokenProgramID,
		EventAuthority:        eventAuth,
		Amount:                amountBaseUnits,
		Delegator:             subscriber,
	})

	instructions := []solanago.Instruction{ix}
	var sigPresubmit func(solanago.Signature) error
	if presubmit != nil {
		sigPresubmit = func(sig solanago.Signature) error { return presubmit(sig.String()) }
	}
	var sig solanago.Signature
	switch submitter := s.submitter.(type) {
	case presubmitMerchantAddressSubmitter:
		sig, err = submitter.SubmitForMerchantAddressWithPresubmit(ctx, tenantID, merchant, instructions, sigPresubmit)
	case merchantAddressSubmitter:
		sig, err = submitter.SubmitForMerchantAddress(ctx, tenantID, merchant, instructions)
	default:
		if ps, ok := s.submitter.(presubmitSubmitter); ok {
			sig, err = ps.SubmitWithPresubmit(ctx, tenantID, instructions, sigPresubmit)
		} else {
			sig, err = s.submitter.Submit(ctx, tenantID, instructions)
		}
	}
	if err != nil {
		return "", err
	}
	return sig.String(), nil
}

func parseKey(field, value string) (solanago.PublicKey, error) {
	k, err := solanago.PublicKeyFromBase58(value)
	if err != nil {
		return solanago.PublicKey{}, fmt.Errorf("recurring: invalid %s %q: %w", field, value, err)
	}
	return k, nil
}
