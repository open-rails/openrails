package recurring

import (
	"context"
	"fmt"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// cancelConfirmRPC watches a wallet-sent signature land. It does not assert
// success: the outcome reports the on-chain result.
type cancelConfirmRPC interface {
	WatchTransaction(ctx context.Context, sig solanago.Signature, commitment rpc.CommitmentType, terminal solanaint.ChainTerminal) (*solanaint.TransactionOutcome, error)
}

// membershipCanceller mirrors a confirmed on-chain cancel into the DB; its
// cascade flips the linked solana_subscriptions row to canceled, stopping the
// cranker.
type membershipCanceller interface {
	CancelMembership(ctx context.Context, params *subscriptions.CancelMembershipParams) error
}

// ConfirmCancelService confirms a wallet-signed on-chain cancel landed, then
// mirrors it into the DB. The chain is the source of truth: a Solana
// subscription is never canceled in the DB without an observed on-chain cancel.
// cancel_subscription ends it at period end, so the mirror is a scheduled
// cancel (RevokeAccess=false), as on the card rails.
type ConfirmCancelService struct {
	rpc        cancelConfirmRPC
	canceller  membershipCanceller
	commitment rpc.CommitmentType
}

// NewConfirmCancelService confirms at Confirmed commitment. The wallet built
// the transaction, so its blockhash is unknown here: the watch runs until the
// caller's context ends.
func NewConfirmCancelService(rpcClient cancelConfirmRPC, canceller membershipCanceller) *ConfirmCancelService {
	return &ConfirmCancelService{
		rpc:        rpcClient,
		canceller:  canceller,
		commitment: rpc.CommitmentConfirmed,
	}
}

// Confirm verifies the wallet's cancel transaction landed and succeeded, then
// mirrors it as a scheduled (period-end) cancellation; a cancel that did not
// land never touches the DB. reason is recorded with the cancel.
func (s *ConfirmCancelService) Confirm(ctx context.Context, subscriptionID uuid.UUID, signature, reason string) error {
	if subscriptionID == uuid.Nil {
		return fmt.Errorf("recurring: subscription id is required")
	}
	if signature == "" {
		return fmt.Errorf("recurring: signature is required")
	}
	sig, err := solanago.SignatureFromBase58(signature)
	if err != nil {
		return fmt.Errorf("recurring: invalid signature: %w", err)
	}

	outcome, err := s.rpc.WatchTransaction(ctx, sig, s.commitment, solanaint.ChainTerminal{})
	if err != nil {
		// Never observed at the requested commitment (still pending, RPC down, or
		// the caller's context ended). Do NOT mirror — the on-chain cancel is not proven.
		return fmt.Errorf("recurring: confirm cancel signature %s: %w", signature, err)
	}
	if !outcome.Succeeded() {
		// Landed but reverted: the subscription is NOT canceled on-chain.
		return fmt.Errorf("recurring: cancel transaction did not succeed on-chain: %w", outcome.OnChainError())
	}

	// Scheduled cancel: access runs to the membership's CurrentPeriodEndsAt,
	// which mirrors the chain's expires_at_ts. The cascade stops the cranker;
	// independently, a pull past expires_at_ts fails 508 -> Terminal.
	params := &subscriptions.CancelMembershipParams{
		SubscriptionID: &subscriptionID,
		CancelType:     models.CancelTypeUser,
		RevokeAccess:   false,
	}
	if reason != "" {
		params.CancelFeedback = &reason
	}
	if err := s.canceller.CancelMembership(ctx, params); err != nil {
		return fmt.Errorf("recurring: mirror on-chain cancel to membership: %w", err)
	}
	return nil
}
