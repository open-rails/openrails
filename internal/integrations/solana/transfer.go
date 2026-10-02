package solana

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/google/uuid"
)

// Token2022ProgramID owns Token-2022 mints (PYUSD, USDG). Legacy SPL mints are
// owned by token.ProgramID; a mint's owner decides its token accounts'
// addresses and the program its transfers call.
var Token2022ProgramID = solanago.MustPublicKeyFromBase58("TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb")

// TransferRequest describes a Solana transfer to build.
type TransferRequest struct {
	FromWallet  string
	ToWallet    string
	TokenSymbol string
	TokenMint   string
	Amount      uint64
	Reference   string
	// Memo, when set, is stamped as an SPL Memo instruction BEFORE the transfer
	// (#713 self-recognition; Solana Pay ordering). Discovery hint, never money truth.
	Memo string
}

// TransferResponse contains a base64-encoded transaction payload and the last
// block height at which it can still land.
type TransferResponse struct {
	TransactionBase64    string
	LastValidBlockHeight uint64
}

// ObserveTransferRequest names the reference, recipient and mint a landed
// transaction is read against.
type ObserveTransferRequest struct {
	Signature string
	Recipient string
	TokenMint string
	Reference string
	// MemoLocalID, when set, is checked against the #713 purchase memo under
	// MemoPolicy: a mismatch always makes the transaction foreign; absence
	// does only when we built the transaction ourselves.
	MemoLocalID uuid.UUID
	MemoPolicy  PurchaseMemoPolicy
	// Commitment the transaction must reach before it is read (default finalized).
	Commitment rpc.CommitmentType
}

// TransferObservation is what one landed transaction paid the recipient,
// read from the recipient's balance changes, so split transfers, transfers
// made inside another program and Token-2022 all count. Amount is the net
// base units of the mint received by the recipient wallet or any token
// account it owns. Other reports value it received in anything else.
type TransferObservation struct {
	Amount   uint64
	Other    bool
	Payer    string
	LandedAt *time.Time
}

var (
	// ErrForeignTransfer: the transaction does not belong to the reference (the
	// reference key is absent or the purchase memo names another record).
	ErrForeignTransfer = errors.New("solana: transaction does not belong to this reference")
	// ErrUnreadableTransfer: the transaction landed but cannot be read; reading
	// it again gives the same answer.
	ErrUnreadableTransfer = errors.New("solana: landed transaction cannot be read")
	// ErrFailedOnChain: the transaction landed with an error and moved nothing.
	ErrFailedOnChain = errors.New("solana: transaction failed on-chain")
)

// BuildTransferTransaction constructs a transfer transaction and returns its base64 encoding.
func (c *RPCClient) BuildTransferTransaction(ctx context.Context, req TransferRequest) (*TransferResponse, error) {
	fromWallet, err := solanago.PublicKeyFromBase58(strings.TrimSpace(req.FromWallet))
	if err != nil {
		return nil, fmt.Errorf("invalid sender address: %w", err)
	}
	toWallet, err := solanago.PublicKeyFromBase58(strings.TrimSpace(req.ToWallet))
	if err != nil {
		return nil, fmt.Errorf("invalid recipient address: %w", err)
	}
	if req.Amount == 0 {
		return nil, fmt.Errorf("amount is required")
	}
	var mint *MintInfo
	if !isNativeSOLSymbol(req.TokenSymbol) {
		mintKey, err := solanago.PublicKeyFromBase58(strings.TrimSpace(req.TokenMint))
		if err != nil {
			return nil, fmt.Errorf("invalid token mint address: %w", err)
		}
		if mint, err = c.GetMintInfo(ctx, mintKey); err != nil {
			return nil, err
		}
	}
	blockhash, err := c.LatestBlockhash(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get blockhash: %w", err)
	}
	instructions, err := buildTransferInstructions(req, fromWallet, toWallet, mint)
	if err != nil {
		return nil, err
	}
	transaction, err := solanago.NewTransaction(instructions, blockhash.Hash, solanago.TransactionPayer(fromWallet))
	if err != nil {
		return nil, fmt.Errorf("failed to create transaction: %w", err)
	}
	txBytes, err := transaction.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("failed to serialize transaction: %w", err)
	}
	return &TransferResponse{
		TransactionBase64:    base64.StdEncoding.EncodeToString(txBytes),
		LastValidBlockHeight: blockhash.LastValidBlockHeight,
	}, nil
}

// MintInfo is what a transfer needs from its mint account.
type MintInfo struct {
	Mint     solanago.PublicKey
	Program  solanago.PublicKey
	Decimals uint8
	// Fee is the Token-2022 transfer fee in force now; nil when the mint has none.
	Fee *TransferFee
	// Hook is the Token-2022 transfer hook program; zero when none.
	Hook solanago.PublicKey
}

// GrossFor is what a payer must send so the recipient receives net, and the fee.
func (m *MintInfo) GrossFor(net uint64) (uint64, uint64, error) {
	if m == nil || m.Fee == nil {
		return net, 0, nil
	}
	return m.Fee.GrossFor(net)
}

// AssociatedTokenAddress derives owner's token account for mint under the
// mint's owning token program.
func AssociatedTokenAddress(owner, mint, program solanago.PublicKey) (solanago.PublicKey, error) {
	addr, _, err := solanago.FindProgramAddress([][]byte{owner[:], program[:], mint[:]}, solanago.SPLAssociatedTokenAccountProgramID)
	return addr, err
}

// buildTransferInstructions assembles the one-off purchase instruction list:
// optional #713 SPL Memo FIRST, then the SOL or TransferChecked transfer under
// the mint's own token program, with the Solana Pay reference appended.
func buildTransferInstructions(req TransferRequest, fromWallet, toWallet solanago.PublicKey, mint *MintInfo) ([]solanago.Instruction, error) {
	var referencePub *solanago.PublicKey
	if ref := strings.TrimSpace(req.Reference); ref != "" {
		refKey, err := solanago.PublicKeyFromBase58(ref)
		if err != nil {
			return nil, fmt.Errorf("invalid reference key: %w", err)
		}
		referencePub = &refKey
	}
	var instructions []solanago.Instruction
	if m := strings.TrimSpace(req.Memo); m != "" {
		instructions = append(instructions, NewMemoInstruction(m))
	}
	if mint == nil {
		transfer := system.NewTransferInstruction(req.Amount, fromWallet, toWallet)
		if referencePub != nil {
			transfer.AccountMetaSlice = append(transfer.AccountMetaSlice, solanago.Meta(*referencePub))
		}
		return append(instructions, transfer.Build()), nil
	}
	from, err := AssociatedTokenAddress(fromWallet, mint.Mint, mint.Program)
	if err != nil {
		return nil, fmt.Errorf("failed to find from token account: %w", err)
	}
	to, err := AssociatedTokenAddress(toWallet, mint.Mint, mint.Program)
	if err != nil {
		return nil, fmt.Errorf("failed to find to token account: %w", err)
	}
	// The recipient must receive the quoted amount: under a transfer fee the
	// payer sends the gross, and the fee is asserted on-chain.
	var transfer solanago.Instruction
	if mint.Fee != nil {
		gross, fee, err := mint.GrossFor(req.Amount)
		if err != nil {
			return nil, err
		}
		transfer = transferCheckedWithFee(mint.Program, from, mint.Mint, to, fromWallet, gross, mint.Decimals, fee)
	} else {
		built := token.NewTransferCheckedInstruction(req.Amount, mint.Decimals, from, mint.Mint, to, fromWallet, nil).Build()
		data, err := built.Data()
		if err != nil {
			return nil, err
		}
		transfer = solanago.NewInstruction(mint.Program, built.Accounts(), data)
	}
	accounts := transfer.Accounts()
	if referencePub != nil {
		accounts = append(accounts, solanago.Meta(*referencePub))
	}
	data, err := transfer.Data()
	if err != nil {
		return nil, err
	}
	return append(instructions, solanago.NewInstruction(mint.Program, accounts, data)), nil
}

// GetMintInfo reads a mint's owning token program and decimals.
func (c *RPCClient) GetMintInfo(ctx context.Context, mint solanago.PublicKey) (*MintInfo, error) {
	owner, data, err := c.fallback.GetAccountOwnerAndData(ctx, mint)
	if err != nil {
		return nil, fmt.Errorf("solana: read mint %s: %w", mint, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrMintAccountNotFound, mint)
	}
	if !owner.Equals(token.ProgramID) && !owner.Equals(Token2022ProgramID) {
		return nil, fmt.Errorf("solana: mint %s is owned by %s, not a token program", mint, owner)
	}
	decimals, err := DecodeMintDecimals(data)
	if err != nil {
		return nil, err
	}
	d, err := mintDecimals(decimals)
	if err != nil {
		return nil, fmt.Errorf("solana: mint %s: %w", mint, err)
	}
	info := &MintInfo{Mint: mint, Program: owner, Decimals: d}
	if owner.Equals(Token2022ProgramID) {
		epoch, err := c.fallback.GetEpoch(ctx)
		if err != nil {
			return nil, fmt.Errorf("solana: read epoch: %w", err)
		}
		if info.Fee, info.Hook, err = mintExtensions(data, epoch); err != nil {
			return nil, fmt.Errorf("solana: mint %s: %w", mint, err)
		}
	}
	return info, nil
}

// mintDecimals narrows a decoded decimals value to the u8 a TransferChecked
// instruction carries, refusing anything outside it.
func mintDecimals(decimals int) (uint8, error) {
	if decimals < 0 || decimals > math.MaxUint8 {
		return 0, fmt.Errorf("mint decimals %d outside 0..%d", decimals, math.MaxUint8)
	}
	return uint8(decimals), nil
}

// ObserveTransfer reads a landed transaction at the requested commitment
// (finalized by default) and reports what it paid the recipient. Whether that
// settles anything is the caller's decision.
func (c *RPCClient) ObserveTransfer(ctx context.Context, req ObserveTransferRequest) (*TransferObservation, error) {
	recipient := strings.TrimSpace(req.Recipient)
	reference := strings.TrimSpace(req.Reference)
	if recipient == "" || reference == "" {
		return nil, fmt.Errorf("recipient and reference are required")
	}
	commitment := req.Commitment
	if commitment == "" {
		commitment = rpc.CommitmentFinalized
	}
	txResult, err := c.fetchLandedTransaction(ctx, req.Signature, commitment)
	if err != nil {
		return nil, err
	}
	out, err := observeTransactionContent(txResult, recipient, strings.TrimSpace(req.TokenMint), reference, req.MemoLocalID, req.MemoPolicy)
	if err != nil {
		return nil, err
	}
	if txResult.BlockTime != nil {
		t := txResult.BlockTime.Time().UTC()
		out.LandedAt = &t
	}
	return out, nil
}

func (c *RPCClient) fetchLandedTransaction(ctx context.Context, signature string, commitment rpc.CommitmentType) (*rpc.GetTransactionResult, error) {
	sig, err := solanago.SignatureFromBase58(signature)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid signature: %v", ErrUnreadableTransfer, err)
	}
	if err = c.ConfirmTransaction(ctx, sig, commitment); err != nil {
		return nil, fmt.Errorf("transaction confirmation failed: %w", err)
	}
	var txResult *rpc.GetTransactionResult
	for attempt := 0; attempt < 5; attempt++ {
		txResult, err = c.fallback.GetTransactionAt(ctx, sig, commitment)
		if err == nil && txResult != nil || err != nil && !isNotFoundError(err) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get transaction: %w", err)
	}
	if txResult == nil || txResult.Meta == nil {
		return nil, fmt.Errorf("transaction %s not yet readable", signature)
	}
	if txResult.Meta.Err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFailedOnChain, txResult.Meta.Err)
	}
	return txResult, nil
}

func observeTransactionContent(txResult *rpc.GetTransactionResult, recipient, tokenMint, reference string, memoLocalID uuid.UUID, memoPolicy PurchaseMemoPolicy) (*TransferObservation, error) {
	if txResult.Transaction == nil {
		return nil, fmt.Errorf("%w: transaction data not available", ErrUnreadableTransfer)
	}
	tx, err := txResult.Transaction.GetTransaction()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreadableTransfer, err)
	}
	if err := VerifyPurchaseMemo(tx, memoLocalID, memoPolicy); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrForeignTransfer, err)
	}
	referencePub, err := solanago.PublicKeyFromBase58(reference)
	if err != nil {
		return nil, fmt.Errorf("invalid reference key: %w", err)
	}
	recipientPub, err := solanago.PublicKeyFromBase58(recipient)
	if err != nil {
		return nil, fmt.Errorf("invalid recipient: %w", err)
	}
	keys := accountKeys(tx, txResult.Meta.LoadedAddresses)
	if !containsKey(keys, referencePub) {
		return nil, fmt.Errorf("%w: reference key not included in transaction", ErrForeignTransfer)
	}
	out := &TransferObservation{}
	if len(keys) > 0 {
		out.Payer = keys[0].String()
	}
	native := isNativeSOLMint(tokenMint)

	var expected, other int64
	for i, key := range keys {
		if !key.Equals(recipientPub) || i >= len(txResult.Meta.PreBalances) || i >= len(txResult.Meta.PostBalances) {
			continue
		}
		delta, err := signedDelta(txResult.Meta.PostBalances[i], txResult.Meta.PreBalances[i])
		if err != nil {
			return nil, err
		}
		if native {
			expected += delta
		} else if delta > 0 {
			other += delta
		}
	}
	deltas, err := recipientTokenDeltas(txResult, keys, recipientPub)
	if err != nil {
		return nil, err
	}
	for mint, delta := range deltas {
		if !native && mint == tokenMint {
			expected += delta
		} else if delta > 0 {
			other += delta
		}
	}
	if expected > 0 {
		out.Amount = uint64(expected)
	}
	out.Other = other > 0
	return out, nil
}

// recipientTokenDeltas sums, per mint, the balance change of every token
// account the recipient owns (or that is the recipient) in the transaction.
func recipientTokenDeltas(txResult *rpc.GetTransactionResult, keys []solanago.PublicKey, recipient solanago.PublicKey) (map[string]int64, error) {
	type account struct {
		mint      string
		pre, post uint64
	}
	accounts := map[uint16]*account{}
	collect := func(balances []rpc.TokenBalance, post bool) error {
		for _, b := range balances {
			owned := b.Owner != nil && b.Owner.Equals(recipient) || int(b.AccountIndex) < len(keys) && keys[b.AccountIndex].Equals(recipient)
			if !owned {
				continue
			}
			if b.UiTokenAmount == nil {
				return fmt.Errorf("%w: token amount missing", ErrUnreadableTransfer)
			}
			amount, err := strconv.ParseUint(b.UiTokenAmount.Amount, 10, 64)
			if err != nil {
				return fmt.Errorf("%w: token amount %q", ErrUnreadableTransfer, b.UiTokenAmount.Amount)
			}
			a := accounts[b.AccountIndex]
			if a == nil {
				a = &account{mint: b.Mint.String()}
				accounts[b.AccountIndex] = a
			}
			if post {
				a.post = amount
			} else {
				a.pre = amount
			}
		}
		return nil
	}
	if err := collect(txResult.Meta.PreTokenBalances, false); err != nil {
		return nil, err
	}
	if err := collect(txResult.Meta.PostTokenBalances, true); err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, a := range accounts {
		delta, err := signedDelta(a.post, a.pre)
		if err != nil {
			return nil, err
		}
		out[a.mint] += delta
	}
	return out, nil
}

func signedDelta(post, pre uint64) (int64, error) {
	if post > math.MaxInt64 || pre > math.MaxInt64 {
		return 0, fmt.Errorf("%w: balance exceeds range", ErrUnreadableTransfer)
	}
	return int64(post) - int64(pre), nil
}

// accountKeys is the transaction's full account list in index order: static
// keys, then loaded writable, then loaded read-only (v0 lookup tables).
func accountKeys(tx *solanago.Transaction, loaded rpc.LoadedAddresses) []solanago.PublicKey {
	keys := append([]solanago.PublicKey{}, tx.Message.AccountKeys...)
	keys = append(keys, loaded.Writable...)
	return append(keys, loaded.ReadOnly...)
}

func containsKey(keys []solanago.PublicKey, key solanago.PublicKey) bool {
	for _, k := range keys {
		if k.Equals(key) {
			return true
		}
	}
	return false
}

const wrappedSOLMint = "So11111111111111111111111111111111111111112"

func isNativeSOLMint(tokenMint string) bool {
	mint := strings.TrimSpace(tokenMint)
	return mint == "" || mint == wrappedSOLMint
}

func isNativeSOLSymbol(symbol string) bool {
	return strings.EqualFold(strings.TrimSpace(symbol), "SOL")
}
