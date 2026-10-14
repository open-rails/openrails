package recurring

import (
	"context"
	"fmt"

	bin "github.com/gagliardetto/binary"
	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	submod "github.com/open-rails/openrails/internal/modules/subscriptions"
)

// accountReader reads a program account; empty once it is closed.
type accountReader interface {
	GetAccountData(ctx context.Context, address solanago.PublicKey) ([]byte, error)
}

// CanceledOnChain reports whether the subscriber canceled the subscription in
// their wallet (cancel_subscription, or the account closed): the program then
// refuses the next period's pull, so OpenRails cannot renew it.
func CanceledOnChain(ctx context.Context, chain accountReader, row *models.SolanaSubscription) (bool, error) {
	pda, err := parseKey("subscription_pda", row.SubscriptionPDA)
	if err != nil {
		return false, err
	}
	data, err := chain.GetAccountData(ctx, pda)
	if err != nil {
		return false, fmt.Errorf("recurring: read subscription %s: %w", pda, err)
	}
	if len(data) == 0 {
		return true, nil
	}
	account, err := subscriptions.DecodeSubscriptionAccount(data)
	if err != nil {
		return false, fmt.Errorf("recurring: decode subscription %s: %w", pda, err)
	}
	return account.Canceled(), nil
}

// RenewableOnChain also checks that the token account still delegates pulls
// to this subscription authority. A wallet revoke cannot be undone locally.
func RenewableOnChain(ctx context.Context, chain accountReader, row *models.SolanaSubscription) (bool, error) {
	canceled, err := CanceledOnChain(ctx, chain, row)
	if err != nil || canceled {
		return false, err
	}
	wallet, err := parseKey("subscriber_wallet", row.SubscriberWallet)
	if err != nil {
		return false, err
	}
	mint, err := parseKey("mint", row.Mint)
	if err != nil {
		return false, err
	}
	authority, err := parseKey("authority_pda", row.AuthorityPDA)
	if err != nil {
		return false, err
	}
	ata, _, err := subscriptions.DeriveATA(wallet, mint, solanago.TokenProgramID)
	if err != nil {
		return false, err
	}
	data, err := chain.GetAccountData(ctx, ata)
	if err != nil {
		return false, fmt.Errorf("recurring: read token account %s: %w", ata, err)
	}
	if len(data) == 0 {
		return false, nil
	}
	var account token.Account
	if err := account.UnmarshalWithDecoder(bin.NewBinDecoder(data)); err != nil {
		return false, fmt.Errorf("recurring: decode token account %s: %w", ata, err)
	}
	return account.Mint.Equals(mint) && account.Owner.Equals(wallet) && account.Delegate != nil && account.Delegate.Equals(authority) && account.DelegatedAmount > 0, nil
}

// cancelConfirmRPC watches a wallet-sent signature land. It does not assert
// success: the outcome reports the on-chain result.
type cancelConfirmRPC interface {
	WatchTransaction(ctx context.Context, sig solanago.Signature, commitment rpc.CommitmentType, terminal solanaint.ChainTerminal) (*solanaint.TransactionOutcome, error)
}

// membershipCanceller applies the customer's period-end cancel; its cascade
// flips the linked solana_subscriptions row to canceled, stopping the cranker.
type membershipCanceller interface {
	CancelMembership(ctx context.Context, params *submod.CancelMembershipParams) error
}

// ConfirmCancelService confirms the optional on-chain cancel a customer's
// wallet signed landed. A Solana cancel needs none: OpenRails stops pulling.
// The customer's period-end cancel is applied with it (a no-op when already
// canceled); cancel_subscription also ends the subscription at period end.
type ConfirmCancelService struct {
	rpc        cancelConfirmRPC
	repo       solanaSubscriptionReader
	chain      accountReader
	canceller  membershipCanceller
	commitment rpc.CommitmentType
}

// NewConfirmCancelService confirms at Confirmed commitment. The wallet built
// the transaction, so its blockhash is unknown here: the watch runs until the
// caller's context ends.
func NewConfirmCancelService(rpcClient cancelConfirmRPC, canceller membershipCanceller, repo solanaSubscriptionReader, chain accountReader) *ConfirmCancelService {
	return &ConfirmCancelService{
		rpc:        rpcClient,
		repo:       repo,
		chain:      chain,
		canceller:  canceller,
		commitment: rpc.CommitmentConfirmed,
	}
}

// Confirm verifies the wallet's cancel transaction landed and succeeded, then
// applies the period-end cancellation; a cancel that did not land never
// touches the DB. reason is recorded with the cancel.
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

	row, err := s.repo.GetBySubscriptionID(ctx, subscriptionID)
	if err != nil {
		return fmt.Errorf("recurring: load canceled subscription: %w", err)
	}
	canceled, err := CanceledOnChain(ctx, s.chain, row)
	if err != nil {
		return err
	}
	if !canceled {
		return fmt.Errorf("recurring: transaction did not cancel this subscription on-chain")
	}

	// Access runs to CurrentPeriodEndsAt, the chain's expires_at_ts too.
	params := &submod.CancelMembershipParams{
		SubscriptionID:     &subscriptionID,
		CancelType:         models.CancelTypeUser,
		Requested:          true,
		RefuseOwnedRenewal: true,
	}
	if reason != "" {
		params.CancelFeedback = &reason
	}
	if err := s.canceller.CancelMembership(ctx, params); err != nil {
		return fmt.Errorf("recurring: cancel membership: %w", err)
	}
	return nil
}
