package solana

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	log "github.com/sirupsen/logrus"
)

// ChainReader is the chain-read surface: *RPCClient, or a reader that resolves
// the merchant's client per call.
type ChainReader interface {
	// GetAccountData is an account's data; nil when it does not exist.
	GetAccountData(ctx context.Context, address solanago.PublicKey) ([]byte, error)
	GetLatestBlockhash(ctx context.Context) (solanago.Hash, error)
	// GetTokenBalanceForMint is the owner's ATA balance of mint in base units;
	// 0 when the ATA does not exist.
	GetTokenBalanceForMint(ctx context.Context, owner solanago.PublicKey, mint solanago.PublicKey) (uint64, error)
	// GetTransaction reads a transaction at confirmed commitment.
	GetTransaction(ctx context.Context, signature solanago.Signature) (*rpc.GetTransactionResult, error)
	WatchTransaction(ctx context.Context, sig solanago.Signature, commitment rpc.CommitmentType, terminal ChainTerminal) (*TransactionOutcome, error)
}

// Network is the configured cluster. CheckPosture verifies the endpoints.
func (c *RPCClient) Network() string { return c.network }

// GetTokenBalance returns the balance of an SPL token account.
func (c *RPCClient) GetTokenBalance(ctx context.Context, tokenAccount solanago.PublicKey) (*rpc.UiTokenAmount, error) {
	resp, err := c.getTokenAccountBalance(ctx, tokenAccount)
	if err != nil {
		return nil, err
	}
	return resp.Value, nil
}

// GetTokenBalanceForMintAtSlot is GetTokenBalanceForMint gated on minSlot: the
// ATA balance is read only against a node that has reached minSlot (the slot a
// preceding write — e.g. a pull/crank — confirmed at), so a lagging node returns
// a retryable error instead of a stale balance. minSlot == 0 behaves exactly like
// GetTokenBalanceForMint. Returns 0 when the ATA does not exist.
func (c *RPCClient) GetTokenBalanceForMintAtSlot(ctx context.Context, owner solanago.PublicKey, mint solanago.PublicKey, minSlot uint64) (uint64, error) {
	if minSlot == 0 {
		return c.GetTokenBalanceForMint(ctx, owner, mint)
	}
	ata, _, err := solanago.FindAssociatedTokenAddress(owner, mint)
	if err != nil {
		return 0, fmt.Errorf("failed to derive ATA for mint %s: %w", mint.String(), err)
	}
	resp, err := c.getTokenAccountBalanceAtSlot(ctx, ata, minSlot)
	if err != nil {
		if strings.Contains(err.Error(), "could not find account") ||
			strings.Contains(err.Error(), "Invalid param") {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to get token balance: %w", err)
	}
	if resp.Value == nil {
		return 0, nil
	}
	balance, err := strconv.ParseUint(resp.Value.Amount, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse token balance %q: %w", resp.Value.Amount, err)
	}
	return balance, nil
}

func (c *RPCClient) GetTransactionWithRetry(ctx context.Context, signature solanago.Signature, attempts int, delay time.Duration) (*rpc.GetTransactionResult, error) {
	var lastErr error
	for i := 0; i < attempts; i++ {
		resp, err := c.GetTransaction(ctx, signature)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !isNotFoundError(err) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, lastErr
}

func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "not found")
}

// ConfirmTransaction waits for an already-seen signature (e.g. found by
// GetSignaturesForAddress) to reach commitment. The chain terminal is unknown
// here, so the wait ends only on the commitment, an on-chain error, or ctx end.
func (c *RPCClient) ConfirmTransaction(ctx context.Context, signature solanago.Signature, commitment rpc.CommitmentType) error {
	outcome, err := c.WatchTransaction(ctx, signature, commitment, ChainTerminal{})
	if err != nil {
		return fmt.Errorf("transaction confirmation for %s: %w", signature.String(), err)
	}
	if outcome.Err != nil {
		return fmt.Errorf("%w: %v", ErrFailedOnChain, outcome.Err)
	}
	return nil
}

// SignatureInfo is a pared-down view of a signature lookup.
type SignatureInfo struct {
	Signature string
	HasError  bool
	// BlockTime is the cluster-reported block time, nil when the node did not
	// return one (older ledger ranges).
	BlockTime *time.Time
}

// GetSignaturesForAddress finds transactions that reference a specific address.
func (c *RPCClient) GetSignaturesForAddress(ctx context.Context, address string, limit int) ([]SignatureInfo, error) {
	return c.GetSignaturesForAddressPage(ctx, address, "", limit)
}

// GetSignaturesForAddressPage is GetSignaturesForAddress with a pagination
// cursor: before != "" continues the newest-first walk strictly below that
// signature. limit caps the page (RPC max 1000).
func (c *RPCClient) GetSignaturesForAddressPage(ctx context.Context, address string, before string, limit int) ([]SignatureInfo, error) {
	return c.signaturesForAddress(ctx, address, before, "", limit, rpc.CommitmentFinalized)
}

// SignaturesKnown reports whether the node answering has each signature in its
// history: an empty page below a cursor proves nothing from a node that does
// not know the cursor.
func (c *RPCClient) SignaturesKnown(ctx context.Context, signatures ...string) (bool, error) {
	sigs := make([]solanago.Signature, 0, len(signatures))
	for _, s := range signatures {
		sig, err := solanago.SignatureFromBase58(s)
		if err != nil {
			return false, fmt.Errorf("invalid signature %q: %w", s, err)
		}
		sigs = append(sigs, sig)
	}
	st, err := c.getSignatureStatuses(ctx, true, sigs...)
	if err != nil {
		return false, err
	}
	if st == nil || len(st.Value) != len(sigs) {
		return false, nil
	}
	// A node whose finalized history lags can still report a cursor it only
	// holds at confirmed; only a finalized status proves the history below it.
	for _, v := range st.Value {
		if v == nil || v.ConfirmationStatus != rpc.ConfirmationStatusFinalized {
			return false, nil
		}
	}
	return true, nil
}

// ConfirmedSignatures lists up to limit signatures naming address at confirmed
// commitment, newest first, strictly older than before (empty = newest).
func (c *RPCClient) ConfirmedSignatures(ctx context.Context, address, before string, limit int) ([]SignatureInfo, error) {
	return c.signaturesForAddress(ctx, address, before, "", limit, rpc.CommitmentConfirmed)
}

// SignaturesBetween lists finalized signatures naming address, newest first,
// strictly older than before and strictly newer than until (either may be empty).
func (c *RPCClient) SignaturesBetween(ctx context.Context, address, before, until string, limit int) ([]SignatureInfo, error) {
	return c.signaturesForAddress(ctx, address, before, until, limit, rpc.CommitmentFinalized)
}

func (c *RPCClient) signaturesForAddress(ctx context.Context, address, before, until string, limit int, commitment rpc.CommitmentType) ([]SignatureInfo, error) {
	pubkey, err := solanago.PublicKeyFromBase58(strings.TrimSpace(address))
	if err != nil {
		return nil, fmt.Errorf("invalid address: %w", err)
	}

	opts := &rpc.GetSignaturesForAddressOpts{
		Commitment: commitment,
	}
	if limit > 0 {
		limitVal := limit
		opts.Limit = &limitVal
	}
	if before = strings.TrimSpace(before); before != "" {
		sig, err := solanago.SignatureFromBase58(before)
		if err != nil {
			return nil, fmt.Errorf("invalid before cursor: %w", err)
		}
		opts.Before = sig
	}
	if until = strings.TrimSpace(until); until != "" {
		sig, err := solanago.SignatureFromBase58(until)
		if err != nil {
			return nil, fmt.Errorf("invalid until cursor: %w", err)
		}
		opts.Until = sig
	}

	resp, err := c.getSignaturesForAddressWithOpts(ctx, pubkey, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to get signatures for address %s: %w", pubkey.String(), err)
	}

	results := make([]SignatureInfo, 0, len(resp))
	for _, sig := range resp {
		info := SignatureInfo{
			Signature: sig.Signature.String(),
			HasError:  sig.Err != nil,
		}
		if sig.BlockTime != nil {
			t := sig.BlockTime.Time().UTC()
			info.BlockTime = &t
		}
		results = append(results, info)
	}

	return results, nil
}

// ProgramAccountFilter is one memcmp filter for GetProgramAccounts: account
// data must equal Bytes at Offset. Filters AND together.
type ProgramAccountFilter struct {
	Offset uint64
	Bytes  []byte
}

// ProgramAccount is one account returned by GetProgramAccounts.
type ProgramAccount struct {
	Address solanago.PublicKey
	Data    []byte
}

// GetProgramAccounts lists the accounts owned by program that match every
// memcmp filter: a bulk point-in-time listing (no slot gating).
func (c *RPCClient) GetProgramAccounts(ctx context.Context, program solanago.PublicKey, filters []ProgramAccountFilter) ([]ProgramAccount, error) {
	opts := &rpc.GetProgramAccountsOpts{
		Commitment: rpc.CommitmentFinalized,
		Encoding:   solanago.EncodingBase64,
	}
	for _, f := range filters {
		opts.Filters = append(opts.Filters, rpc.RPCFilter{
			Memcmp: &rpc.RPCFilterMemcmp{Offset: f.Offset, Bytes: solanago.Base58(f.Bytes)},
		})
	}
	resp, err := c.getProgramAccountsWithOpts(ctx, program, opts)
	if err != nil {
		return nil, fmt.Errorf("failed to get program accounts for %s: %w", program.String(), err)
	}
	out := make([]ProgramAccount, 0, len(resp))
	for _, acc := range resp {
		pa := ProgramAccount{Address: acc.Pubkey}
		if acc.Account != nil && acc.Account.Data != nil {
			pa.Data = acc.Account.Data.GetBinary()
		}
		out = append(out, pa)
	}
	return out, nil
}

// TokenAccountInfo represents an SPL token account balance for a specific mint.
type TokenAccountInfo struct {
	Mint    string
	Balance uint64
}

// GetTokenBalanceForMint returns the SPL token balance for a specific mint owned by a wallet.
// It derives the Associated Token Account (ATA) address and queries its balance.
// Returns 0 if the account doesn't exist or has no balance.
func (c *RPCClient) GetTokenBalanceForMint(ctx context.Context, owner solanago.PublicKey, mint solanago.PublicKey) (uint64, error) {
	ata, _, err := solanago.FindAssociatedTokenAddress(owner, mint)
	if err != nil {
		return 0, fmt.Errorf("failed to derive ATA for mint %s: %w", mint.String(), err)
	}

	resp, err := c.getTokenAccountBalance(ctx, ata)
	if err != nil {
		// Account might not exist (user has never held this token)
		if strings.Contains(err.Error(), "could not find account") ||
			strings.Contains(err.Error(), "Invalid param") {
			return 0, nil
		}
		return 0, fmt.Errorf("failed to get token balance: %w", err)
	}

	if resp.Value == nil {
		return 0, nil
	}

	balance, err := strconv.ParseUint(resp.Value.Amount, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse token balance %q: %w", resp.Value.Amount, err)
	}
	return balance, nil
}

// GetTokenBalances returns token balances for multiple mints owned by a wallet.
// Mints that don't exist or have zero balance are included with Balance=0.
func (c *RPCClient) GetTokenBalances(ctx context.Context, owner solanago.PublicKey, mints []string) ([]TokenAccountInfo, error) {
	if len(mints) == 0 {
		return nil, nil
	}
	accounts := make([]TokenAccountInfo, 0, len(mints))
	for _, mintStr := range mints {
		mint, err := solanago.PublicKeyFromBase58(mintStr)
		if err != nil {
			log.WithError(err).WithField("mint", mintStr).Debug("Failed to parse token mint")
			continue
		}

		balance, err := c.GetTokenBalanceForMint(ctx, owner, mint)
		if err != nil {
			log.WithError(err).WithField("mint", mintStr).Debug("Failed to get token balance")
			continue
		}

		accounts = append(accounts, TokenAccountInfo{Mint: mintStr, Balance: balance})
	}

	return accounts, nil
}
