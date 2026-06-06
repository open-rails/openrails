package solana

import (
	"context"
	"encoding/json"
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

const defaultWatchTimeout = 90 * time.Second

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

// WatchTransaction polls a signature until it reaches `commitment` (or ctx is
// done / the timeout elapses), returning its resolved on-chain outcome. It works
// for ANY signature — our own submissions OR a third party's wallet transaction
// we are waiting to observe (e.g. a subscribe the user signed in their wallet).
// It does NOT assert success: a transaction that landed but failed returns a
// non-nil outcome with Err set; inspect Outcome.Succeeded()/OnChainError(). A
// signature the cluster has not seen yet keeps polling until the deadline.
//
// timeout <= 0 uses defaultWatchTimeout.
func (c *RPCClient) WatchTransaction(ctx context.Context, sig solanago.Signature, commitment rpc.CommitmentType, timeout time.Duration) (*TransactionOutcome, error) {
	if timeout <= 0 {
		timeout = defaultWatchTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("watch transaction %s: %w", sig, ctx.Err())
		case <-ticker.C:
			st, err := c.fallback.GetSignatureStatuses(ctx, true, sig)
			if err != nil {
				continue // transient RPC error — keep polling until the deadline
			}
			if len(st.Value) == 0 || st.Value[0] == nil {
				continue // not yet visible to the cluster
			}
			s := st.Value[0]
			if s.ConfirmationStatus != "" && commitmentRank(s.ConfirmationStatus) >= wantRank(commitment) {
				return &TransactionOutcome{
					Signature: sig,
					Status:    s.ConfirmationStatus,
					Slot:      s.Slot,
					Err:       s.Err,
				}, nil
			}
		}
	}
}

// SubmitAndConfirm submits a signed transaction and waits for it to reach the
// Confirmed commitment, returning the on-chain outcome. The returned error is
// only for submission / confirmation-watch failures (RPC down, never confirmed);
// a transaction that lands but reverts is reported via Outcome.Err (use
// Outcome.OnChainError() to surface it). timeout <= 0 uses the default.
func (c *RPCClient) SubmitAndConfirm(ctx context.Context, tx *solanago.Transaction, timeout time.Duration) (*TransactionOutcome, error) {
	// Skip preflight: the node's preflight simulation runs against a bank that
	// lags just-confirmed writes, so a pull submitted right after subscribe (or any
	// tx touching very recent accounts) spuriously fails simulation with
	// InvalidAccountOwner even though it executes fine. We confirm via
	// WatchTransaction below, which authoritatively reports a real on-chain failure.
	sig, err := c.fallback.SendTransactionSkipPreflight(ctx, tx)
	if err != nil {
		return nil, err
	}
	return c.WatchTransaction(ctx, sig, rpc.CommitmentConfirmed, timeout)
}
