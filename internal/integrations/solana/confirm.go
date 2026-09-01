package solana

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

// TransactionOutcome is the resolved on-chain result of a transaction once it
// reaches a requested commitment. Err is the raw on-chain error (nil == the
// transaction executed successfully).
type TransactionOutcome struct {
	Signature solanago.Signature
	Status    rpc.ConfirmationStatusType // processed | confirmed | finalized
	Slot      uint64
	Err       any // on-chain InstructionError; nil on success
}

// Succeeded reports whether the transaction landed AND executed without error.
func (o *TransactionOutcome) Succeeded() bool { return o != nil && o.Err == nil }

// OnChainError renders the on-chain failure as an error (nil if it succeeded).
// The message embeds the error as JSON (e.g. {"InstructionError":[0,{"Custom":4}]})
// so downstream classifiers can parse the program's Custom code.
func (o *TransactionOutcome) OnChainError() error {
	if o == nil || o.Err == nil {
		return nil
	}
	b, err := json.Marshal(o.Err)
	if err != nil {
		return fmt.Errorf("transaction %s failed on-chain: %v", o.Signature, o.Err)
	}
	return fmt.Errorf("transaction %s failed on-chain: %s", o.Signature, string(b))
}

// xs-007 row 36: a confirmation watch ends on the CHAIN'S terminal, never on
// a clock. Every transaction carries a recent blockhash that the cluster
// accepts only while its block height is at most lastValidBlockHeight (~150
// blocks, ~60–90 s in practice, but the chain's number, not ours); past that
// height the transaction can never land, and that is the only honest
// "failed to confirm". A watch that gave up at 90 s reported a landing at
// 100 s as a failure: the money moved and the caller acted on "it did not".
//
// When the watcher does not know the blockhash — a signature the buyer's
// wallet produced — it polls until the caller's own context ends; the
// caller's budget is the caller's declaration, not this package's.

// ChainTerminal is the chain's own statement of when a transaction stops
// being landable: the blockhash's last valid block height. Zero = unknown.
type ChainTerminal struct {
	LastValidBlockHeight uint64
}

// RecentBlockhash is a blockhash together with the chain terminal it carries.
type RecentBlockhash struct {
	Hash                 solanago.Hash
	LastValidBlockHeight uint64
}

// Terminal is the ChainTerminal for a transaction built on this blockhash.
func (b RecentBlockhash) Terminal() ChainTerminal {
	return ChainTerminal{LastValidBlockHeight: b.LastValidBlockHeight}
}

// ErrTransactionExpired: the cluster's block height passed the transaction's
// last valid block height without the signature ever being seen. The chain
// will not include it; resubmission needs a fresh blockhash.
var ErrTransactionExpired = errors.New("solana: transaction expired — block height passed its blockhash's last valid height without landing")

// watchPollInterval is the status poll cadence: a poll interval, not a
// decision — roughly one slot pair, so a landing is noticed within a beat.
var watchPollInterval = 1500 * time.Millisecond

// commitmentRank orders commitment levels so "have >= want" comparisons work.
func commitmentRank(s rpc.ConfirmationStatusType) int {
	switch s {
	case rpc.ConfirmationStatusFinalized:
		return 3
	case rpc.ConfirmationStatusConfirmed:
		return 2
	case rpc.ConfirmationStatusProcessed:
		return 1
	default:
		return 0
	}
}

func wantRank(c rpc.CommitmentType) int {
	switch c {
	case rpc.CommitmentFinalized:
		return 3
	case rpc.CommitmentConfirmed:
		return 2
	default:
		return 1
	}
}

// WatchTransaction polls a signature until it reaches `commitment`, returning
// its resolved on-chain outcome. It works for ANY signature — our own
// submissions OR a third party's wallet transaction we are waiting to observe
// (e.g. a subscribe the user signed in their wallet). It does NOT assert
// success: a transaction that landed but failed returns a non-nil outcome
// with Err set; inspect Outcome.Succeeded()/OnChainError().
//
// The watch ends on one of three observations only: the signature reached the
// commitment; the chain's block height passed terminal.LastValidBlockHeight
// while the signature was still unseen (ErrTransactionExpired); or the
// caller's context ended. With an unknown terminal (zero) only the last two
// apply.
func (c *RPCClient) WatchTransaction(ctx context.Context, sig solanago.Signature, commitment rpc.CommitmentType, terminal ChainTerminal) (*TransactionOutcome, error) {
	ticker := time.NewTicker(watchPollInterval)
	defer ticker.Stop()
	for {
		// Look first, wait after: a signature that is already at the wanted
		// commitment (the verify path's finalized discoveries) answers now.
		if outcome, err := c.watchOnce(ctx, sig, commitment, terminal); err != nil || outcome != nil {
			return outcome, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("watch transaction %s: %w", sig, ctx.Err())
		case <-ticker.C:
		}
	}
}

// watchOnce is one observation: (outcome, nil) when the commitment is reached,
// (nil, err) when the chain says it never will, (nil, nil) to keep watching.
func (c *RPCClient) watchOnce(ctx context.Context, sig solanago.Signature, commitment rpc.CommitmentType, terminal ChainTerminal) (*TransactionOutcome, error) {
	st, err := c.fallback.GetSignatureStatuses(ctx, true, sig)
	if err != nil {
		return nil, nil // transient RPC error — the chain has not spoken
	}
	if len(st.Value) > 0 && st.Value[0] != nil {
		s := st.Value[0]
		if s.ConfirmationStatus != "" && commitmentRank(s.ConfirmationStatus) >= wantRank(commitment) {
			return &TransactionOutcome{
				Signature: sig,
				Status:    s.ConfirmationStatus,
				Slot:      s.Slot,
				Err:       s.Err,
			}, nil
		}
		return nil, nil // seen, below the wanted commitment: it is landing
	}
	// Unseen. Ask the chain whether it still can land. The status read comes
	// FIRST so a transaction landing in the last valid block is never mistaken
	// for an expired one.
	if terminal.LastValidBlockHeight == 0 {
		return nil, nil
	}
	height, err := c.fallback.GetBlockHeight(ctx, commitment)
	if err != nil {
		return nil, nil
	}
	if height > terminal.LastValidBlockHeight {
		return nil, fmt.Errorf("watch transaction %s (block height %d > last valid %d): %w",
			sig, height, terminal.LastValidBlockHeight, ErrTransactionExpired)
	}
	return nil, nil
}

// SubmitAndConfirm submits a signed transaction and waits for it to reach the
// Confirmed commitment, returning the on-chain outcome. The returned error is
// only for submission / confirmation-watch failures (RPC down, expired
// blockhash, caller context ended); a transaction that lands but reverts is
// reported via Outcome.Err (use Outcome.OnChainError() to surface it).
// terminal is the blockhash validity the transaction was built with
// (RecentBlockhash.Terminal()); zero watches until the caller's context ends.
func (c *RPCClient) SubmitAndConfirm(ctx context.Context, tx *solanago.Transaction, terminal ChainTerminal) (*TransactionOutcome, error) {
	// Skip preflight: the node's preflight simulation runs against a bank that
	// lags just-confirmed writes, so a pull submitted right after subscribe (or any
	// tx touching very recent accounts) spuriously fails simulation with
	// InvalidAccountOwner even though it executes fine. We confirm via
	// WatchTransaction below, which authoritatively reports a real on-chain failure.
	sig, err := c.fallback.SendTransactionSkipPreflight(ctx, tx)
	if err != nil {
		return nil, err
	}
	return c.WatchTransaction(ctx, sig, rpc.CommitmentConfirmed, terminal)
}
