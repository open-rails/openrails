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

// A confirmation watch ends on the chain's terminal, never on a clock. A
// transaction's blockhash is accepted only while block height <=
// lastValidBlockHeight (~150 blocks, the chain's number); past it the
// transaction can never land, the only honest "failed to confirm". A clock
// timeout can report a late landing as a failure after the money moved.
//
// Without the blockhash (a signature the buyer's wallet produced) the watch
// polls until the caller's context ends.

// ChainTerminal is the chain's own statement of when a transaction stops
// being landable: the blockhash's last valid block height. Zero = unknown.
type ChainTerminal struct {
	LastValidBlockHeight uint64
	BlockhashSlot        uint64
}

// RecentBlockhash is a blockhash together with the chain terminal it carries.
type RecentBlockhash struct {
	Hash                 solanago.Hash
	LastValidBlockHeight uint64
	Slot                 uint64
}

// Terminal is the ChainTerminal for a transaction built on this blockhash.
func (b RecentBlockhash) Terminal() ChainTerminal {
	return ChainTerminal{LastValidBlockHeight: b.LastValidBlockHeight, BlockhashSlot: b.Slot}
}

// TransactionExpiredUnseen proves expiry before checking transaction history.
// A missing transaction alone is not enough: a lagging RPC may not have seen it.
func (c *RPCClient) TransactionExpiredUnseen(ctx context.Context, sig solanago.Signature, terminal ChainTerminal) (bool, error) {
	if terminal.LastValidBlockHeight == 0 || terminal.BlockhashSlot == 0 {
		return false, nil
	}
	var expired bool
	err := c.fallback.withFallback(ctx, "TransactionExpiredUnseen", func(client *rpc.Client) error {
		expired = false
		// Epoch info supplies height and slot from the same finalized bank.
		info, err := client.GetEpochInfo(ctx, rpc.CommitmentFinalized)
		if err != nil {
			return err
		}
		if info == nil || info.AbsoluteSlot == 0 || info.BlockHeight <= terminal.LastValidBlockHeight {
			return nil
		}
		statuses, err := client.GetSignatureStatuses(ctx, true, sig)
		if err != nil {
			return err
		}
		// A load-balanced endpoint can route this read to an older node.
		if statuses == nil || statuses.Context.Slot < info.AbsoluteSlot || len(statuses.Value) != 1 {
			return nil
		}
		if statuses.Value[0] != nil {
			return nil
		}
		// Check retention after the lookup so pruned history is not treated
		// as proof that the transaction never executed.
		first, err := client.GetFirstAvailableBlock(ctx)
		if err != nil {
			return err
		}
		expired = first <= terminal.BlockhashSlot
		return nil
	})
	return expired, err
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

// WatchTransaction polls any signature (ours or a wallet's) until it reaches
// commitment and returns its on-chain outcome. It does not assert success: a
// landed-but-failed transaction returns an outcome with Err set.
//
// The watch ends only when the signature reaches the commitment, the block
// height passes terminal.LastValidBlockHeight with the signature unseen
// (ErrTransactionExpired), or ctx ends. A zero terminal skips the second.
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

// SubmitAndConfirm submits a signed transaction and watches it to Confirmed.
// The error covers submission/watch failures only (RPC down, expired
// blockhash, ctx ended); a landed revert is in Outcome.Err. terminal comes from
// RecentBlockhash.Terminal(); zero watches until ctx ends.
func (c *RPCClient) SubmitAndConfirm(ctx context.Context, tx *solanago.Transaction, terminal ChainTerminal) (*TransactionOutcome, error) {
	// Skip preflight: its simulation runs against a bank lagging just-confirmed
	// writes, so a tx touching very recent accounts (a pull right after
	// subscribe) spuriously fails with InvalidAccountOwner. WatchTransaction
	// below reports real on-chain failures.
	sig, err := c.fallback.SendTransactionSkipPreflight(ctx, tx)
	if err != nil {
		return nil, err
	}
	return c.WatchTransaction(ctx, sig, rpc.CommitmentConfirmed, terminal)
}
