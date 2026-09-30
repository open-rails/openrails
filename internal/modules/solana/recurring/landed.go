package recurring

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"

	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
)

// landedTxReader reads a transaction at confirmed commitment (satisfied by the
// merchant chain reader and *solanaint.RPCClient).
type landedTxReader interface {
	GetTransaction(ctx context.Context, signature solanago.Signature) (*rpc.GetTransactionResult, error)
}

var (
	// ErrPaymentNotLanded: the signature is not (yet) a confirmed transaction.
	// Retryable: the wallet may have just sent it.
	ErrPaymentNotLanded = errors.New("recurring: transaction not confirmed on-chain")
	// ErrPaymentUnverified: the landed transaction does not prove the payment.
	ErrPaymentUnverified = errors.New("recurring: transaction does not prove the payment")
	// ErrPaymentLate: the payment landed after its quote's validity.
	ErrPaymentLate = errors.New("recurring: payment landed after its quote expired")
)

// LatePaymentError is ErrPaymentLate with the times the operator needs.
type LatePaymentError struct {
	LandedAt, ValidUntil time.Time
}

func (e *LatePaymentError) Error() string {
	return fmt.Sprintf("%v: landed %s, valid until %s", ErrPaymentLate, e.LandedAt.Format(time.RFC3339), e.ValidUntil.UTC().Format(time.RFC3339))
}

func (e *LatePaymentError) Unwrap() error { return ErrPaymentLate }

// landedTx is a confirmed, successful transaction whose every required
// signature verifies against its message.
type landedTx struct {
	signature string
	result    *rpc.GetTransactionResult
	tx        *solanago.Transaction
}

// fetchLanded reads the signature's transaction from the chain. The signature
// names the transaction; nothing else the caller supplies is trusted.
func fetchLanded(ctx context.Context, chain landedTxReader, signature string) (*landedTx, error) {
	sig, err := solanago.SignatureFromBase58(strings.TrimSpace(signature))
	if err != nil {
		return nil, fmt.Errorf("%w: invalid signature", ErrPaymentUnverified)
	}
	result, err := solanaint.ReadUntilConsistent(ctx, solanaint.ReadUntilConsistentOpts{},
		func(ctx context.Context) (*rpc.GetTransactionResult, error) { return chain.GetTransaction(ctx, sig) },
		func(r *rpc.GetTransactionResult) bool { return r != nil },
	)
	if err != nil || result == nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrPaymentNotLanded, sig, err)
	}
	if result.Meta == nil || result.Transaction == nil {
		return nil, fmt.Errorf("%w: %s has no transaction data", ErrPaymentUnverified, sig)
	}
	if result.Meta.Err != nil {
		return nil, fmt.Errorf("%w: %s failed on-chain", ErrPaymentUnverified, sig)
	}
	tx, err := result.Transaction.GetTransaction()
	if err != nil {
		return nil, fmt.Errorf("%w: decode %s: %v", ErrPaymentUnverified, sig, err)
	}
	if len(tx.Signatures) == 0 || !tx.Signatures[0].Equals(sig) {
		return nil, fmt.Errorf("%w: the chain returned a different transaction for %s", ErrPaymentUnverified, sig)
	}
	if err := tx.VerifySignatures(); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrPaymentUnverified, sig, err)
	}
	return &landedTx{signature: sig.String(), result: result, tx: tx}, nil
}

// landedAt is the block time, when the node reports one.
func (l *landedTx) landedAt() *time.Time {
	if l.result.BlockTime == nil {
		return nil
	}
	t := l.result.BlockTime.Time().UTC()
	return &t
}

func (l *landedTx) signedBy(key solanago.PublicKey) bool {
	for _, signer := range l.tx.Message.Signers() {
		if signer.Equals(key) {
			return true
		}
	}
	return false
}

func (l *landedTx) carries(key solanago.PublicKey) bool {
	for _, k := range l.tx.Message.AccountKeys {
		if k.Equals(key) {
			return true
		}
	}
	return false
}

type landedIx struct {
	accounts []solanago.PublicKey
	data     []byte
}

// instructions returns the top-level subscriptions-program instructions of kind.
func (l *landedTx) instructions(kind subscriptions.InstructionKind) []landedIx {
	var out []landedIx
	for _, inst := range l.tx.Message.Instructions {
		program, err := l.tx.ResolveProgramIDIndex(inst.ProgramIDIndex)
		if err != nil || !program.Equals(subscriptions.ProgramID) {
			continue
		}
		if got, ok := subscriptions.ParseInstructionKind(inst.Data); !ok || got != kind {
			continue
		}
		metas, err := inst.ResolveInstructionAccounts(&l.tx.Message)
		if err != nil {
			continue
		}
		keys := make([]solanago.PublicKey, len(metas))
		for i, m := range metas {
			keys[i] = m.PublicKey
		}
		out = append(out, landedIx{accounts: keys, data: inst.Data})
	}
	return out
}

func accountsAre(keys []solanago.PublicKey, want ...solanago.PublicKey) bool {
	if len(keys) < len(want) {
		return false
	}
	for i, w := range want {
		if !keys[i].Equals(w) {
			return false
		}
	}
	return true
}

// subscription names one on-chain subscription and the terms it binds.
type subscription struct {
	subscriber, merchant, mint solanago.PublicKey
	planID                     uint64
	amount, periodHours        uint64
	createdAt                  int64
}

func (s subscription) addresses() (plan, sub, authority solanago.PublicKey, err error) {
	if plan, _, err = subscriptions.DerivePlanPDA(s.merchant, s.planID); err != nil {
		return
	}
	if sub, _, err = subscriptions.DeriveSubscriptionPDA(plan, s.subscriber); err != nil {
		return
	}
	authority, _, err = subscriptions.DeriveSubscriptionAuthority(s.subscriber, s.mint)
	return
}

// subscribes proves the subscriber signed a subscribe to exactly these terms.
func (l *landedTx) subscribes(s subscription) error {
	plan, sub, _, err := s.addresses()
	if err != nil {
		return err
	}
	if !l.signedBy(s.subscriber) {
		return fmt.Errorf("%w: not signed by subscriber %s", ErrPaymentUnverified, s.subscriber)
	}
	for _, ix := range l.instructions(subscriptions.KindSubscribe) {
		d, err := subscriptions.DecodeSubscribeData(ix.data)
		if err != nil || !accountsAre(ix.accounts, s.subscriber, s.merchant, plan, sub) {
			continue
		}
		if d.PlanID == s.planID && d.Mint.Equals(s.mint) && d.Amount == s.amount && d.PeriodHours == s.periodHours && d.CreatedAt == s.createdAt {
			return nil
		}
	}
	return fmt.Errorf("%w: no subscribe of %s to plan %d at the quoted terms", ErrPaymentUnverified, s.subscriber, s.planID)
}

// pulls proves the merchant co-signed a pull of amount from the subscriber's
// token account into the merchant's for this subscription, and that the
// merchant's account was credited. amount 0 accepts the co-signed amount (the
// merchant only co-signs amounts it quoted). Returns the pulled amount.
func (l *landedTx) pulls(s subscription, amount uint64) (uint64, error) {
	plan, sub, authority, err := s.addresses()
	if err != nil {
		return 0, err
	}
	from, _, err := subscriptions.DeriveATA(s.subscriber, s.mint, solanago.TokenProgramID)
	if err != nil {
		return 0, err
	}
	to, _, err := subscriptions.DeriveATA(s.merchant, s.mint, solanago.TokenProgramID)
	if err != nil {
		return 0, err
	}
	if !l.signedBy(s.merchant) {
		return 0, fmt.Errorf("%w: no pull co-signed by the merchant", ErrPaymentUnverified)
	}
	for _, ix := range l.instructions(subscriptions.KindTransferSubscription) {
		d, err := subscriptions.DecodeTransferData(ix.data)
		if err != nil || !accountsAre(ix.accounts, sub, plan, authority, from, to, s.merchant, s.mint) {
			continue
		}
		if !d.Delegator.Equals(s.subscriber) || !d.Mint.Equals(s.mint) || d.Amount == 0 || (amount != 0 && d.Amount != amount) {
			continue
		}
		credited, err := solanaint.TokenCredited(l.result, l.tx, to, s.mint)
		if err != nil {
			return 0, fmt.Errorf("%w: %v", ErrPaymentUnverified, err)
		}
		if credited < d.Amount {
			return 0, fmt.Errorf("%w: merchant credited %d, pull was %d", ErrPaymentUnverified, credited, d.Amount)
		}
		return d.Amount, nil
	}
	if amount != 0 {
		return 0, fmt.Errorf("%w: no pull of %d from %s to the merchant", ErrPaymentUnverified, amount, s.subscriber)
	}
	return 0, fmt.Errorf("%w: no pull from %s to the merchant", ErrPaymentUnverified, s.subscriber)
}

// cancels proves the subscriber signed a cancel of the subscription account.
func (l *landedTx) cancels(subscriber, plan, sub solanago.PublicKey) error {
	for _, ix := range l.instructions(subscriptions.KindCancelSubscription) {
		if accountsAre(ix.accounts, subscriber, plan, sub) && l.signedBy(subscriber) {
			return nil
		}
	}
	return fmt.Errorf("%w: no cancel of %s by %s", ErrPaymentUnverified, sub, subscriber)
}

// references proves the transaction carries a checkout's Solana Pay reference.
// The merchant co-signs the whole message, so a reference inside a co-signed
// transaction was put there by the checkout that prepared it.
func (l *landedTx) references(reference string) error {
	key, err := solanago.PublicKeyFromBase58(strings.TrimSpace(reference))
	if err != nil {
		return fmt.Errorf("%w: checkout has no valid reference", ErrPaymentUnverified)
	}
	if !l.carries(key) {
		return fmt.Errorf("%w: transaction is not for this checkout", ErrPaymentUnverified)
	}
	return nil
}
