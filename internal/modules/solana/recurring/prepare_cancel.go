package recurring

import (
	"context"
	"encoding/base64"
	"fmt"

	solanago "github.com/doujins-org/solana-go"
	"github.com/doujins-org/solana-go/programs/token"
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

// PrepareCancelService builds the UNSIGNED transaction the subscriber's wallet
// signs to TRUSTLESSLY stop a recurring Solana subscription on-chain (#266).
//
// CORRECTION (#266, flagged by devnet #263): the program's cancel_subscription
// instruction does NOT stop pulls — a crank run after cancel still moves tokens.
// The real trustless stop is the subscriber REVOKING the SPL token delegate on
// their ATA via token.NewRevokeInstruction. Once revoked, the program's
// transfer_subscription is rejected with token OwnerMismatch (Custom:4) —
// confirmed on devnet — so OpenRails physically cannot charge again.
//
// Soft cancel (#264) already stops billing because OpenRails is the only puller;
// this is ADDITIVE and is the trustless guarantee. Same prepare->sign->confirm
// shape as subscribe (#261): all instruction encoding stays server-side; the
// wallet only signs + sends.
type PrepareCancelService struct {
	repo solanaSubscriptionReader
	rpc  cancelPrepareRPC
}

// NewPrepareCancelService builds a PrepareCancelService.
func NewPrepareCancelService(repo solanaSubscriptionReader, rpc cancelPrepareRPC) *PrepareCancelService {
	return &PrepareCancelService{repo: repo, rpc: rpc}
}

// PrepareCancelResult is the unsigned revoke transaction the wallet must sign,
// plus the subscription PDA being stopped (for the confirm/observe step).
type PrepareCancelResult struct {
	// Transaction is the base64-encoded unsigned SPL token Revoke transaction
	// (subscriber = signer + fee payer) that strips OpenRails' transfer delegate
	// from the subscriber's ATA.
	Transaction string
	// SubscriptionPDA is the on-chain subscription account being stopped.
	SubscriptionPDA string
}

// Prepare loads the on-chain subscription row linked to the lifecycle
// subscription, derives the subscriber's ATA, builds the SPL token Revoke
// instruction that strips OpenRails' transfer delegate (subscriber signs + pays
// gas), and returns the unsigned transaction base64-encoded. Revoking the
// delegate — not the program's cancel_subscription — is what trustlessly stops
// any further pull (devnet #263).
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
	mint, err := parseKey("mint", row.Mint)
	if err != nil {
		return nil, err
	}

	// Derive the subscriber's ATA — the account whose transfer delegate (granted
	// to OpenRails at subscribe time) we revoke. Same derivation crank_service.go
	// uses to find the source ATA it pulls from.
	subscriberATA, _, err := subscriptions.DeriveATA(subscriber, mint, solanago.TokenProgramID)
	if err != nil {
		return nil, fmt.Errorf("recurring: derive subscriber ata: %w", err)
	}

	// SPL token Revoke: the subscriber (the ATA owner) strips OpenRails' delegate.
	// After this lands, transfer_subscription fails with OwnerMismatch (Custom:4)
	// on devnet — the trustless billing stop (#266, devnet #263). We build the
	// Revoke ALONE: it is sufficient to stop billing, and the program's
	// cancel_subscription is a no-op for that purpose (it does not halt pulls).
	ix := token.NewRevokeInstruction(subscriberATA, subscriber, nil).Build()

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
