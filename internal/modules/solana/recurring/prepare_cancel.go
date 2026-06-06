package recurring

import (
	"context"
	"fmt"

	solanago "github.com/gagliardetto/solana-go"
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

// PrepareCancelService builds the UNSIGNED on-chain cancel transaction the
// subscriber's wallet signs to cancel one recurring Solana subscription (#266).
//
// Model: Solana is the source of truth; the cancel is an ON-CHAIN action the
// user authorizes, and OpenRails' DB merely mirrors it. There is NO "soft
// cancel" (DB-only) — the user signs `cancel_subscription` for the specific
// subscription PDA, OpenRails observes the confirmed transaction and mirrors it
// (marks the row cancelled so the cranker stops). Cancellation is immediate (no
// NMI-style undo window).
//
// Per-subscription cancel uses `cancel_subscription` (NOT an SPL token Revoke):
// the token delegate is the SubscriptionAuthority, which is shared across ALL of
// the user's subscriptions for that mint (it is per user+mint), so revoking the
// delegate would nuke EVERY USDC subscription at once. `cancel_subscription`
// targets one subscription PDA. (A wallet-level "revoke all access" is a separate
// nuclear option, not this per-subscription cancel.)
type PrepareCancelService struct {
	repo solanaSubscriptionReader
	rpc  cancelPrepareRPC
}

// NewPrepareCancelService builds a PrepareCancelService.
func NewPrepareCancelService(repo solanaSubscriptionReader, rpc cancelPrepareRPC) *PrepareCancelService {
	return &PrepareCancelService{repo: repo, rpc: rpc}
}

// PrepareCancelResult is the unsigned cancel transaction the wallet must sign,
// plus the subscription PDA being cancelled (for the confirm/observe step).
type PrepareCancelResult struct {
	// Transaction is the base64-encoded unsigned cancel_subscription transaction
	// (subscriber = signer + fee payer).
	Transaction string
	// SubscriptionPDA is the on-chain subscription account being cancelled.
	SubscriptionPDA string
}

// Prepare loads the on-chain subscription row linked to the lifecycle
// subscription and builds the unsigned `cancel_subscription` transaction
// (subscriber signs + pays gas) for the wallet to sign + send. OpenRails then
// observes the confirmed cancel and mirrors it (cranker stops).
func (s *PrepareCancelService) Prepare(ctx context.Context, subscriptionID uuid.UUID) (*PrepareCancelResult, error) {
	return s.PrepareWithReference(ctx, subscriptionID, "")
}

// PrepareWithReference is Prepare with an optional Solana Pay REFERENCE attached
// to the cancel instruction (read-only, non-signer) so the reference poller can
// detect the landed cancel via getSignaturesForAddress — the same mechanism the
// one-off Solana Pay path uses. An empty reference behaves exactly like Prepare.
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
	ix, err = withReferenceMeta(ix, reference)
	if err != nil {
		return nil, err
	}

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
	return marshalUnsignedTxBase64(tx)
}
