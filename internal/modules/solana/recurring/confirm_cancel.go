package recurring

import (
	"context"
	"fmt"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// cancelConfirmRPC is the minimal RPC surface ConfirmCancel needs: confirm that a
// signature the WALLET signed + sent has LANDED on-chain (satisfied by
// *solanaint.RPCClient). It does NOT assert success — the returned outcome
// reports the on-chain result via Succeeded()/OnChainError().
type cancelConfirmRPC interface {
	WatchTransaction(ctx context.Context, sig solanago.Signature, commitment rpc.CommitmentType, timeout time.Duration) (*solanaint.TransactionOutcome, error)
}

// membershipCanceller is the minimal lifecycle surface ConfirmCancel needs to
// MIRROR the confirmed on-chain cancel into the DB (satisfied by
// *subscriptions.SubscriptionLifecycleService). The cascade inside
// CancelMembership flips the linked solana_subscriptions row to cancelled so the
// cranker stops — that cascade IS the mirror step. We call it with
// RevokeAccess=false so the local state matches the card "cancel-at-period-end".
type membershipCanceller interface {
	CancelMembership(ctx context.Context, params *subscriptions.CancelMembershipParams) error
}

// ConfirmCancelService is the CONFIRM step of the on-chain cancel loop (#271).
//
// Model: Solana is the source of truth. A user cancel is an ON-CHAIN action the
// subscriber signs (the unsigned tx comes from PrepareCancelService); the wallet
// signs + sends it. OpenRails then CONFIRMS the cancel landed on-chain and only
// then MIRRORS it into the DB by cancelling the membership (whose cascade flips
// the solana_subscriptions row to cancelled, stopping the cranker). There is NO
// DB-only "soft cancel": we never mark a Solana subscription cancelled in the DB
// unless we have observed the on-chain cancel succeed.
//
// The on-chain cancel_subscription sets the subscription's expires_at_ts to the
// END of the current billing period (NOT immediate). So the mirror is option A —
// "cancel at period end": the user keeps the access they already paid for until
// the current period ends, then it stops. This matches the card rails'
// scheduled-cancel state (Stripe cancel_at_period_end; NMI deferred delete). We
// therefore mirror with RevokeAccess=false, which makes CancelMembership set
// EndedAt = CurrentPeriodEndsAt and preserve entitlements until then — exactly
// the same local-state mapping the card paths use.
type ConfirmCancelService struct {
	rpc        cancelConfirmRPC
	canceller  membershipCanceller
	commitment rpc.CommitmentType
	timeout    time.Duration
}

// NewConfirmCancelService builds a ConfirmCancelService. It confirms to the
// Confirmed commitment (a confirmed cancel will not roll back in practice) with
// the RPC client's default watch timeout.
func NewConfirmCancelService(rpcClient cancelConfirmRPC, canceller membershipCanceller) *ConfirmCancelService {
	return &ConfirmCancelService{
		rpc:        rpcClient,
		canceller:  canceller,
		commitment: rpc.CommitmentConfirmed,
		timeout:    0, // 0 -> RPCClient.WatchTransaction default
	}
}

// Confirm verifies the wallet's cancel transaction landed AND executed
// successfully on-chain, then mirrors it as a SCHEDULED (period-end) membership
// cancellation. Returns an error (and does NOT cancel) if the signature never
// confirms or confirmed with an on-chain failure — the source of truth is the
// chain, so a cancel that did not actually land must not touch the DB.
func (s *ConfirmCancelService) Confirm(ctx context.Context, subscriptionID uuid.UUID, signature string) error {
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

	outcome, err := s.rpc.WatchTransaction(ctx, sig, s.commitment, s.timeout)
	if err != nil {
		// Never observed at the requested commitment (still pending, RPC down, or
		// timed out). Do NOT mirror — the on-chain cancel is not proven.
		return fmt.Errorf("recurring: confirm cancel signature %s: %w", signature, err)
	}
	if !outcome.Succeeded() {
		// Landed but reverted: the subscription is NOT cancelled on-chain.
		return fmt.Errorf("recurring: cancel transaction did not succeed on-chain: %w", outcome.OnChainError())
	}

	// Mirror: scheduled (period-end) cancel. RevokeAccess=false makes
	// CancelMembership keep the membership's tracked CurrentPeriodEndsAt as the
	// access-until boundary (EndedAt = CurrentPeriodEndsAt) and preserve
	// entitlements until then — the same local state Stripe/NMI use for a
	// cancel-at-period-end. We rely on the membership record's own period end
	// (no on-chain subscription-account decode); it mirrors the chain's
	// expires_at_ts = end-of-current-period.
	//
	// The cascade inside CancelMembership flips the linked solana_subscriptions
	// row to cancelled, stopping the cranker. Independently, once expires_at_ts
	// passes on-chain the next pull would return Custom:508 (SubscriptionCancelled)
	// which ClassifyCrankError maps to Terminal — so this mirror plus the 508
	// classification together give the complete period-end cancel.
	cancelType := models.CancelTypeUser
	if err := s.canceller.CancelMembership(ctx, &subscriptions.CancelMembershipParams{
		SubscriptionID: &subscriptionID,
		CancelType:     cancelType,
		RevokeAccess:   false,
	}); err != nil {
		return fmt.Errorf("recurring: mirror on-chain cancel to membership: %w", err)
	}
	return nil
}
