package recurring

import (
	"context"
	"fmt"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
)

// solanaSubscriptionReader loads a lifecycle subscription's stored on-chain
// identifiers.
type solanaSubscriptionReader interface {
	GetBySubscriptionID(ctx context.Context, subscriptionID uuid.UUID) (*models.SolanaSubscription, error)
}

// cancelPrepareRPC supplies a recent blockhash for the unsigned transaction.
type cancelPrepareRPC interface {
	GetLatestBlockhash(ctx context.Context) (solanago.Hash, error)
}

// PrepareCancelService builds the unsigned cancel_subscription transaction the
// subscriber's wallet signs; ConfirmCancelService mirrors it once landed. It
// cancels one subscription PDA, never via an SPL Revoke: the token delegate
// (SubscriptionAuthority) is per user+mint, so revoking it would end every
// subscription on that mint.
type PrepareCancelService struct {
	repo solanaSubscriptionReader
	rpc  cancelPrepareRPC
}

// NewPrepareCancelService builds a PrepareCancelService.
func NewPrepareCancelService(repo solanaSubscriptionReader, rpc cancelPrepareRPC) *PrepareCancelService {
	return &PrepareCancelService{repo: repo, rpc: rpc}
}

// PrepareCancelResult is the unsigned cancel transaction the wallet must sign,
// plus the subscription PDA being canceled (for the confirm/observe step).
type PrepareCancelResult struct {
	// Transaction is the base64-encoded unsigned cancel_subscription transaction
	// (subscriber = signer + fee payer).
	Transaction string
	// SubscriptionPDA is the on-chain subscription account being canceled.
	SubscriptionPDA string
}

// Prepare builds the unsigned cancel_subscription transaction (the subscriber
// signs and pays gas) for the lifecycle subscription's on-chain row.
func (s *PrepareCancelService) Prepare(ctx context.Context, subscriptionID uuid.UUID) (*PrepareCancelResult, error) {
	return s.PrepareWithReference(ctx, subscriptionID, "")
}

// PrepareWithReference is Prepare with an optional Solana Pay reference on a tag
// instruction (referenceTagInstruction), so the reference poller finds the
// landed cancel. An empty reference behaves like Prepare.
func (s *PrepareCancelService) PrepareWithReference(ctx context.Context, subscriptionID uuid.UUID, reference string) (*PrepareCancelResult, error) {
	if subscriptionID == uuid.Nil {
		return nil, fmt.Errorf("recurring: subscription id is required")
	}
	row, err := s.repo.GetBySubscriptionID(ctx, subscriptionID)
	if err != nil {
		return nil, fmt.Errorf("recurring: load solana subscription: %w", err)
	}
	if row == nil {
		return nil, fmt.Errorf("recurring: no solana subscription for %s", subscriptionID)
	}

	subscriber, err := parseKey("subscriber_wallet", row.SubscriberWallet)
	if err != nil {
		return nil, err
	}
	planPDA, err := parseKey("plan_pda", row.PlanPDA)
	if err != nil {
		return nil, err
	}
	subPDA, err := parseKey("subscription_pda", row.SubscriptionPDA)
	if err != nil {
		return nil, err
	}
	eventAuth, _, err := subscriptions.DeriveEventAuthority()
	if err != nil {
		return nil, fmt.Errorf("recurring: derive event authority: %w", err)
	}

	ix := subscriptions.BuildCancelSubscription(subscriptions.CancelOrResumeParams{
		Subscriber:      subscriber,
		PlanPDA:         planPDA,
		SubscriptionPDA: subPDA,
		EventAuthority:  eventAuth,
	})
	ixs, err := withReference([]solanago.Instruction{ix}, subscriber, reference)
	if err != nil {
		return nil, err
	}

	tx, err := s.buildUnsignedTxBase64(ctx, subscriber, ixs)
	if err != nil {
		return nil, err
	}
	return &PrepareCancelResult{
		Transaction:     tx,
		SubscriptionPDA: row.SubscriptionPDA,
	}, nil
}

// buildUnsignedTxBase64 assembles an unsigned transaction (payer = subscriber,
// who signs + pays gas) with a recent blockhash and returns it base64-encoded for
// the wallet to deserialize, sign, and send.
func (s *PrepareCancelService) buildUnsignedTxBase64(ctx context.Context, payer solanago.PublicKey, ixs []solanago.Instruction) (string, error) {
	blockhash, err := s.rpc.GetLatestBlockhash(ctx)
	if err != nil {
		return "", fmt.Errorf("recurring: get recent blockhash: %w", err)
	}
	tx, err := solanago.NewTransaction(ixs, blockhash, solanago.TransactionPayer(payer))
	if err != nil {
		return "", fmt.Errorf("recurring: build transaction: %w", err)
	}
	return marshalUnsignedTxBase64(tx)
}
