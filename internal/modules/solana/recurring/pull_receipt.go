package recurring

import (
	"context"
	"fmt"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
)

// PullReceipt is positive chain evidence for one accepted, full-period pull.
// Its service dates use the chain clock, never the verifier's later clock.
type PullReceipt struct {
	Signature              string
	Amount                 uint64
	PeriodStart, PeriodEnd time.Time
}

// VerifyPullReceipt proves the recorded signature is the cranker-signed pull
// of the accepted amount into the accepted mint/treasury for this episode.
// Missing metadata and a successful unrelated transaction prove nothing.
func VerifyPullReceipt(ctx context.Context, chain landedTxReader, row *models.SolanaSubscription, signature string, amount, periodHours uint64, decimals int, intentID uuid.UUID) (*PullReceipt, error) {
	if chain == nil || row == nil || amount == 0 || periodHours == 0 || periodHours > 24*366 || intentID == uuid.Nil {
		return nil, fmt.Errorf("%w: incomplete accepted pull", ErrPaymentUnverified)
	}
	payment, err := fetchLanded(ctx, chain, signature)
	if err != nil {
		return nil, err
	}
	if err := solanaint.VerifyPurchaseMemo(payment.tx, intentID, solanaint.MemoRequired); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPaymentUnverified, err)
	}
	var keys [7]solanago.PublicKey
	for i, value := range []string{row.SubscriberWallet, row.MerchantAddress, row.Mint, row.PlanPDA, row.SubscriptionPDA, row.AuthorityPDA, row.Treasury} {
		if keys[i], err = solanago.PublicKeyFromBase58(value); err != nil {
			return nil, fmt.Errorf("%w: malformed accepted account", ErrPaymentUnverified)
		}
	}
	wallet, owner, mint, plan, sub, authority, treasury := keys[0], keys[1], keys[2], keys[3], keys[4], keys[5], keys[6]
	derived, _, err := subscriptions.DeriveSubscriptionPDA(plan, wallet)
	if err != nil || !derived.Equals(sub) {
		return nil, fmt.Errorf("%w: subscription identity mismatch", ErrPaymentUnverified)
	}
	derived, _, err = subscriptions.DeriveSubscriptionAuthority(wallet, mint)
	if err != nil || !derived.Equals(authority) {
		return nil, fmt.Errorf("%w: authority identity mismatch", ErrPaymentUnverified)
	}
	from, _, err := subscriptions.DeriveATA(wallet, mint, solanago.TokenProgramID)
	if err != nil {
		return nil, err
	}
	to, _, err := subscriptions.DeriveATA(treasury, mint, solanago.TokenProgramID)
	if err != nil {
		return nil, err
	}
	if !payment.signedBy(owner) {
		return nil, fmt.Errorf("%w: cranker did not sign", ErrPaymentUnverified)
	}
	matched := 0
	for _, ix := range payment.instructions(subscriptions.KindTransferSubscription) {
		data, err := subscriptions.DecodeTransferData(ix.data)
		if err == nil && accountsAre(ix.accounts, sub, plan, authority, from, to, owner, mint, solanago.TokenProgramID) && data.Delegator.Equals(wallet) && data.Mint.Equals(mint) && data.Amount == amount {
			matched++
		}
	}
	if matched != 1 {
		return nil, fmt.Errorf("%w: not one matching accepted pull", ErrPaymentUnverified)
	}
	credited, err := solanaint.TokenCredited(payment.result, payment.tx, to, mint)
	if err != nil || credited != amount {
		return nil, fmt.Errorf("%w: treasury credit does not equal accepted pull", ErrPaymentUnverified)
	}
	found := false
	for _, balance := range payment.result.Meta.PostTokenBalances {
		index := int(balance.AccountIndex)
		if index >= len(payment.tx.Message.AccountKeys) || !payment.tx.Message.AccountKeys[index].Equals(to) {
			continue
		}
		if !balance.Mint.Equals(mint) || balance.Owner != nil && !balance.Owner.Equals(treasury) || balance.UiTokenAmount == nil || int(balance.UiTokenAmount.Decimals) != decimals {
			return nil, fmt.Errorf("%w: treasury token metadata mismatch", ErrPaymentUnverified)
		}
		found = true
	}
	if !found {
		return nil, fmt.Errorf("%w: treasury token metadata missing", ErrPaymentUnverified)
	}
	landedAt := payment.landedAt()
	if landedAt == nil {
		return nil, fmt.Errorf("%w: chain block time missing", ErrPaymentUnverified)
	}
	// A full pull resets the paid period to the chain's transfer time. The
	// duration was accepted before signing and bound to the immutable plan.
	return &PullReceipt{Signature: payment.signature, Amount: amount, PeriodStart: *landedAt, PeriodEnd: landedAt.Add(time.Duration(periodHours) * time.Hour)}, nil
}
