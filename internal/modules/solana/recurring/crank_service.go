package recurring

import (
	"context"
	"fmt"

	solanago "github.com/doujins-org/solana-go"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/pkg/tenant"
)

// CrankService executes the recurring pull ("cranking") — one
// transfer_subscription per subscriber per cycle (issue #256). The merchant
// (cranker) signs and pays the (tiny) gas; funds move from the subscriber's ATA
// to the merchant/destination ATA. The transfer_subscription path is verified
// live on devnet.
type CrankService struct {
	submitter Submitter
}

// NewCrankService builds a CrankService over a per-tenant Submitter.
func NewCrankService(submitter Submitter) *CrankService {
	return &CrankService{submitter: submitter}
}

// Crank pulls amountBaseUnits for one subscription. On Solana, an underfunded
// pull reverts atomically (no partial charge); the caller classifies the error
// (insufficient USDC -> dunning vs operational -> retry; see #257).
func (s *CrankService) Crank(ctx context.Context, tenantID tenant.ID, sub *models.SolanaSubscription, amountBaseUnits uint64) (string, error) {
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

	sig, err := s.submitter.Submit(ctx, tenantID, []solanago.Instruction{ix})
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
