package recurring

import (
	"context"
	"encoding/base64"
	"fmt"

	solanago "github.com/doujins-org/solana-go"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
)

// solanaSubscriptionReader is the minimal repo surface PrepareCancel needs:
// load the stored on-chain identifiers for a lifecycle subscription. Declared
// here (dependency inversion) so the service is unit-testable without a DB.
type solanaSubscriptionReader interface {
	GetBySubscriptionID(ctx context.Context, subscriptionID uuid.UUID) (*models.SolanaSubscription, error)
}

// cancelPrepareRPC is the minimal RPC surface PrepareCancel needs: a recent
// blockhash to build the unsigned transaction (satisfied by *solanaint.RPCClient).
type cancelPrepareRPC interface {
	GetLatestBlockhash(ctx context.Context) (solanago.Hash, error)
}

// PrepareCancelService builds the UNSIGNED cancel_subscription transaction the
// subscriber's wallet signs to TRUSTLESSLY revoke a recurring Solana
// subscription on-chain (#266). Soft cancel (#264) already stops billing because
// OpenRails is the only puller; this is ADDITIVE — once the subscriber signs +
// sends this transaction, the program itself refuses any further pull, so
// OpenRails physically cannot charge again. Same prepare->sign->confirm shape as
// subscribe (#261): all instruction encoding stays server-side; the wallet only
// signs + sends.
type PrepareCancelService struct {
	repo solanaSubscriptionReader
	rpc  cancelPrepareRPC
}

// NewPrepareCancelService builds a PrepareCancelService.
func NewPrepareCancelService(repo solanaSubscriptionReader, rpc cancelPrepareRPC) *PrepareCancelService {
	return &PrepareCancelService{repo: repo, rpc: rpc}
}

// PrepareCancelResult is the unsigned cancel transaction the wallet must sign,
// plus the subscription PDA being revoked (for the confirm/observe step).
type PrepareCancelResult struct {
	// Transaction is the base64-encoded unsigned cancel_subscription transaction
	// (subscriber = signer + fee payer).
	Transaction string
	// SubscriptionPDA is the on-chain subscription account being revoked.
	SubscriptionPDA string
}

// Prepare loads the on-chain subscription row linked to the lifecycle
// subscription, derives the event authority, builds the cancel_subscription
// instruction (subscriber signs + pays gas), and returns the unsigned
// transaction base64-encoded.
func (s *PrepareCancelService) Prepare(ctx context.Context, subscriptionID uuid.UUID) (*PrepareCancelResult, error) {
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

	tx, err := s.buildUnsignedTxBase64(ctx, subscriber, []solanago.Instruction{ix})
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
	raw, err := tx.MarshalBinary()
	if err != nil {
		return "", fmt.Errorf("recurring: serialize transaction: %w", err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}
