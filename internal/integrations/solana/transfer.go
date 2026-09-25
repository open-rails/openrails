package solana

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/google/uuid"
)

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
}

// TransferObservation is what one landed transaction paid the recipient.
// Amount is the base units of the mint the recipient actually received; zero
// means the transaction carried the reference but paid nothing.
type TransferObservation struct {
	Amount   uint64
	Payer    string
	LandedAt *time.Time
}

// ErrForeignTransfer: the transaction does not belong to the reference (the
// reference key is absent or the purchase memo names another record).
var ErrForeignTransfer = errors.New("solana: transaction does not belong to this reference")

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

	blockhash, err := c.LatestBlockhash(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get blockhash: %w", err)
	}

	instructions, err := buildTransferInstructions(req, fromWallet, toWallet)
	if err != nil {
		return nil, err
	}

	transaction, err := solanago.NewTransaction(
		instructions,
		blockhash.Hash,
		solanago.TransactionPayer(fromWallet),
	)
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

// buildTransferInstructions assembles the one-off purchase instruction list:
// optional #713 SPL Memo FIRST, then the SOL/SPL transfer (Solana Pay orders
// the memo immediately before the transfer), with the Solana Pay reference
// meta appended to the transfer.
func buildTransferInstructions(req TransferRequest, fromWallet, toWallet solanago.PublicKey) ([]solanago.Instruction, error) {
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
	if isNativeSOLSymbol(req.TokenSymbol) {
		transfer := system.NewTransferInstruction(
			req.Amount,
			fromWallet,
			toWallet,
		)
		if referencePub != nil {
			transfer.AccountMetaSlice = append(transfer.AccountMetaSlice, solanago.Meta(*referencePub))
		}
		instructions = append(instructions, transfer.Build())
	} else {
		if strings.TrimSpace(req.TokenMint) == "" {
			return nil, fmt.Errorf("token mint is required")
		}
		tokenMint, err := solanago.PublicKeyFromBase58(req.TokenMint)
		if err != nil {
			return nil, fmt.Errorf("invalid token mint address: %w", err)
		}

		fromTokenAccount, _, err := solanago.FindAssociatedTokenAddress(fromWallet, tokenMint)
		if err != nil {
			return nil, fmt.Errorf("failed to find from token account: %w", err)
		}
		toTokenAccount, _, err := solanago.FindAssociatedTokenAddress(toWallet, tokenMint)
		if err != nil {
			return nil, fmt.Errorf("failed to find to token account: %w", err)
		}

		transfer := token.NewTransferInstruction(
			req.Amount,
			fromTokenAccount,
			toTokenAccount,
			fromWallet,
			[]solanago.PublicKey{},
		)
		if referencePub != nil {
			transfer.Accounts = append(transfer.Accounts, solanago.Meta(*referencePub))
		}
		instructions = append(instructions, transfer.Build())
	}
	return instructions, nil
}

// ObserveTransfer reads a confirmed transaction and reports what it paid the
// recipient in the mint. Whether that amount settles anything is the caller's
// decision; this only states what the chain did.
func (c *RPCClient) ObserveTransfer(ctx context.Context, req ObserveTransferRequest) (*TransferObservation, error) {
	recipient := strings.TrimSpace(req.Recipient)
	reference := strings.TrimSpace(req.Reference)
	if recipient == "" || reference == "" {
		return nil, fmt.Errorf("recipient and reference are required")
	}
	txResult, err := c.fetchConfirmedTransaction(ctx, req.Signature)
	if err != nil {
		return nil, err
	}
	amount, payer, err := observeTransactionContent(txResult, recipient, strings.TrimSpace(req.TokenMint), reference, req.MemoLocalID, req.MemoPolicy)
	if err != nil {
		return nil, err
	}
	out := &TransferObservation{Amount: amount, Payer: payer}
	if txResult.BlockTime != nil {
		t := txResult.BlockTime.Time().UTC()
		out.LandedAt = &t
	}
	return out, nil
}

func (c *RPCClient) fetchConfirmedTransaction(ctx context.Context, signature string) (*rpc.GetTransactionResult, error) {
	sig, err := solanago.SignatureFromBase58(signature)
	if err != nil {
		return nil, fmt.Errorf("invalid signature format: %w", err)
	}

	if err = c.ConfirmTransaction(ctx, sig, rpc.CommitmentConfirmed); err != nil {
		return nil, fmt.Errorf("transaction confirmation failed: %w", err)
	}

	txResult, err := c.GetTransactionWithRetry(ctx, sig, 5, 1*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to get transaction: %w", err)
	}

	if txResult.Meta == nil {
		return nil, fmt.Errorf("transaction metadata not available")
	}

	if txResult.Meta.Err != nil {
		return nil, fmt.Errorf("transaction failed on-chain: %v", txResult.Meta.Err)
	}

	return txResult, nil
}

func observeTransactionContent(txResult *rpc.GetTransactionResult, recipient, tokenMint, reference string, memoLocalID uuid.UUID, memoPolicy PurchaseMemoPolicy) (uint64, string, error) {
	if txResult.Transaction == nil {
		return 0, "", fmt.Errorf("transaction data not available")
	}
	tx, err := txResult.Transaction.GetTransaction()
	if err != nil {
		return 0, "", fmt.Errorf("failed to decode transaction: %w", err)
	}
	if err := VerifyPurchaseMemo(tx, memoLocalID, memoPolicy); err != nil {
		return 0, "", fmt.Errorf("%w: %v", ErrForeignTransfer, err)
	}
	referencePub, err := solanago.PublicKeyFromBase58(reference)
	if err != nil {
		return 0, "", fmt.Errorf("invalid reference key: %w", err)
	}
	if !messageContainsKey(tx.Message, referencePub, txResult.Meta.LoadedAddresses) {
		return 0, "", fmt.Errorf("%w: reference key not included in transaction", ErrForeignTransfer)
	}
	payer := ""
	if len(tx.Message.AccountKeys) > 0 {
		payer = tx.Message.AccountKeys[0].String()
	}

	candidates := map[string]struct{}{recipient: {}}
	if derived, err := deriveRecipientTokenAccount(recipient, tokenMint); err == nil && derived != "" {
		candidates[derived] = struct{}{}
	}
	match, err := findTransferMatch(tx, txResult, candidates, tokenMint, 1, "")
	if err != nil {
		return 0, payer, err
	}
	if match == nil {
		return 0, payer, nil
	}
	received, err := receivedAmount(txResult, match.accountIndex, isNativeSOLMint(tokenMint))
	if err != nil {
		return 0, payer, fmt.Errorf("unable to confirm balance change for account %s: %w", match.destination, err)
	}
	return min(match.amount, received), payer, nil
}

func deriveRecipientTokenAccount(recipient string, tokenMint string) (string, error) {
	if strings.TrimSpace(tokenMint) == "" {
		return "", nil
	}
	recipientPub, err := solanago.PublicKeyFromBase58(recipient)
	if err != nil {
		return "", err
	}
	mintPub, err := solanago.PublicKeyFromBase58(tokenMint)
	if err != nil {
		return "", err
	}
	ata, _, err := solanago.FindAssociatedTokenAddress(recipientPub, mintPub)
	if err != nil {
		return "", err
	}
	return ata.String(), nil
}

type transferMatch struct {
	program      string
	amount       uint64
	source       solanago.PublicKey
	destination  solanago.PublicKey
	mint         string
	accountIndex int
}

func findTransferMatch(tx *solanago.Transaction, txResult *rpc.GetTransactionResult, recipientCandidates map[string]struct{}, expectedTokenMint string, expectedAmount uint64, expectedPayer string) (*transferMatch, error) {
	if len(recipientCandidates) == 0 {
		return nil, errors.New("no recipient candidates provided")
	}

	candidateKeys := make(map[string]solanago.PublicKey, len(recipientCandidates))
	for addr := range recipientCandidates {
		pub, err := solanago.PublicKeyFromBase58(addr)
		if err != nil {
			return nil, fmt.Errorf("invalid recipient candidate %s: %w", addr, err)
		}
		candidateKeys[addr] = pub
	}

	var bestMatch *transferMatch
	expectedMintNorm := normalizeMint(expectedTokenMint)

	for _, inst := range tx.Message.Instructions {
		programID, err := tx.ResolveProgramIDIndex(inst.ProgramIDIndex)
		if err != nil {
			continue
		}
		accounts, err := inst.ResolveInstructionAccounts(&tx.Message)
		if err != nil {
			continue
		}

		switch {
		case programID.Equals(system.ProgramID):
			if !isNativeSOLMint(expectedMintNorm) {
				continue
			}
			sysInstr, err := system.DecodeInstruction(accounts, inst.Data)
			if err != nil {
				continue
			}
			transfer, ok := sysInstr.Impl.(*system.Transfer)
			if !ok {
				continue
			}
			match := evaluateSystemTransfer(transfer, accountIndexFromInstruction(inst, 1), candidateKeys, expectedAmount, expectedPayer)
			if match != nil {
				bestMatch = pickBetterMatch(bestMatch, match)
			}
		case programID.Equals(token.ProgramID):
			tokenInstr, err := token.DecodeInstruction(accounts, inst.Data)
			if err != nil {
				continue
			}
			switch dec := tokenInstr.Impl.(type) {
			case *token.Transfer:
				match := evaluateTokenTransfer(txResult, accounts, dec.Amount, accountIndexFromInstruction(inst, 1), candidateKeys, expectedMintNorm, expectedAmount, expectedPayer)
				if match != nil {
					bestMatch = pickBetterMatch(bestMatch, match)
				}
			case *token.TransferChecked:
				match := evaluateTokenTransferChecked(txResult, accounts, dec.Amount, accountIndexFromInstruction(inst, 2), candidateKeys, expectedMintNorm, expectedAmount, expectedPayer)
				if match != nil {
					bestMatch = pickBetterMatch(bestMatch, match)
				}
			}
		default:
			continue
		}
	}

	return bestMatch, nil
}

func evaluateSystemTransfer(dec *system.Transfer, accountIdx int, candidates map[string]solanago.PublicKey, expectedAmount uint64, expectedPayer string) *transferMatch {
	if dec == nil || dec.Lamports == nil {
		return nil
	}
	if accountIdx < 0 {
		return nil
	}

	sourceMeta := dec.GetFundingAccount()
	destMeta := dec.GetRecipientAccount()

	if sourceMeta == nil || destMeta == nil {
		return nil
	}
	if expectedPayer != "" && sourceMeta.PublicKey.String() != expectedPayer {
		return nil
	}
	if _, ok := candidates[destMeta.PublicKey.String()]; !ok {
		return nil
	}
	if *dec.Lamports < expectedAmount {
		return nil
	}

	return &transferMatch{
		program:      "system",
		amount:       *dec.Lamports,
		source:       sourceMeta.PublicKey,
		destination:  destMeta.PublicKey,
		mint:         "",
		accountIndex: accountIdx,
	}
}

func evaluateTokenTransfer(txResult *rpc.GetTransactionResult, accounts []*solanago.AccountMeta, amountPtr *uint64, accountIdx int, candidates map[string]solanago.PublicKey, expectedMint string, expectedAmount uint64, expectedPayer string) *transferMatch {
	if amountPtr == nil || accountIdx < 0 {
		return nil
	}
	if len(accounts) < 3 {
		return nil
	}
	dest := accounts[1].PublicKey
	if _, ok := candidates[dest.String()]; !ok {
		return nil
	}
	if expectedPayer != "" && accounts[2].PublicKey.String() != expectedPayer {
		return nil
	}
	mint := mintForAccount(txResult, accountIdx)
	if !mintMatches(expectedMint, mint) {
		return nil
	}
	if *amountPtr < expectedAmount {
		return nil
	}
	return &transferMatch{
		program:      "token",
		amount:       *amountPtr,
		source:       accounts[0].PublicKey,
		destination:  dest,
		mint:         mint,
		accountIndex: accountIdx,
	}
}

func evaluateTokenTransferChecked(txResult *rpc.GetTransactionResult, accounts []*solanago.AccountMeta, amountPtr *uint64, accountIdx int, candidates map[string]solanago.PublicKey, expectedMint string, expectedAmount uint64, expectedPayer string) *transferMatch {
	if amountPtr == nil || accountIdx < 0 {
		return nil
	}
	if len(accounts) < 4 {
		return nil
	}
	dest := accounts[2].PublicKey
	if _, ok := candidates[dest.String()]; !ok {
		return nil
	}
	if expectedPayer != "" && accounts[3].PublicKey.String() != expectedPayer {
		return nil
	}
	mint := accounts[1].PublicKey.String()
	if !mintMatches(expectedMint, mint) {
		return nil
	}
	if *amountPtr < expectedAmount {
		return nil
	}
	return &transferMatch{
		program:      "token",
		amount:       *amountPtr,
		source:       accounts[0].PublicKey,
		destination:  dest,
		mint:         mint,
		accountIndex: accountIdx,
	}
}

func pickBetterMatch(current, candidate *transferMatch) *transferMatch {
	if candidate == nil {
		return current
	}
	if current == nil {
		return candidate
	}
	if candidate.amount > current.amount {
		return candidate
	}
	return current
}

func accountIndexFromInstruction(inst solanago.CompiledInstruction, accountPosition int) int {
	if accountPosition >= len(inst.Accounts) {
		return -1
	}
	return int(inst.Accounts[accountPosition])
}

func mintForAccount(txResult *rpc.GetTransactionResult, accountIndex int) string {
	for i := range txResult.Meta.PostTokenBalances {
		if int(txResult.Meta.PostTokenBalances[i].AccountIndex) == accountIndex {
			return txResult.Meta.PostTokenBalances[i].Mint.String()
		}
	}
	return ""
}

// Base58 is case-sensitive: mints compare exactly.
func normalizeMint(m string) string {
	return strings.TrimSpace(m)
}

func mintMatches(expected, actual string) bool {
	exp, act := normalizeMint(expected), normalizeMint(actual)
	if isNativeSOLMint(exp) {
		return act == wrappedSOLMint
	}
	return act != "" && exp == act
}

func messageContainsKey(msg solanago.Message, key solanago.PublicKey, loaded rpc.LoadedAddresses) bool {
	for _, k := range msg.AccountKeys {
		if k.Equals(key) {
			return true
		}
	}
	for _, k := range loaded.Writable {
		if k.Equals(key) {
			return true
		}
	}
	for _, k := range loaded.ReadOnly {
		if k.Equals(key) {
			return true
		}
	}
	return false
}

// receivedAmount is the recipient account's balance increase in this
// transaction: lamports for native SOL, token base units otherwise.
func receivedAmount(txResult *rpc.GetTransactionResult, accountIndex int, native bool) (uint64, error) {
	if native {
		if accountIndex >= len(txResult.Meta.PostBalances) || accountIndex >= len(txResult.Meta.PreBalances) {
			return 0, fmt.Errorf("lamport balance not found")
		}
		post, pre := txResult.Meta.PostBalances[accountIndex], txResult.Meta.PreBalances[accountIndex]
		if post < pre {
			return 0, nil
		}
		return post - pre, nil
	}
	post, pre, err := tokenBalanceDelta(txResult, accountIndex)
	if err != nil {
		return 0, err
	}
	if post < pre {
		return 0, nil
	}
	return post - pre, nil
}

func tokenBalanceDelta(txResult *rpc.GetTransactionResult, accountIndex int) (uint64, uint64, error) {
	var (
		postAmount uint64
		preAmount  uint64
		found      bool
	)
	for _, post := range txResult.Meta.PostTokenBalances {
		if int(post.AccountIndex) == accountIndex {
			if post.UiTokenAmount == nil {
				return 0, 0, fmt.Errorf("post token amount missing")
			}
			amt, err := strconv.ParseUint(post.UiTokenAmount.Amount, 10, 64)
			if err != nil {
				return 0, 0, err
			}
			postAmount = amt
			found = true
			break
		}
	}
	if !found {
		return 0, 0, fmt.Errorf("token balance not found")
	}
	for _, pre := range txResult.Meta.PreTokenBalances {
		if int(pre.AccountIndex) == accountIndex {
			if pre.UiTokenAmount == nil {
				return 0, 0, fmt.Errorf("pre token amount missing")
			}
			amt, err := strconv.ParseUint(pre.UiTokenAmount.Amount, 10, 64)
			if err != nil {
				return 0, 0, err
			}
			preAmount = amt
			break
		}
	}
	return postAmount, preAmount, nil
}

const wrappedSOLMint = "So11111111111111111111111111111111111111112"

func isNativeSOLMint(tokenMint string) bool {
	mint := strings.TrimSpace(tokenMint)
	return mint == "" || mint == wrappedSOLMint
}

func isNativeSOLSymbol(symbol string) bool {
	return strings.EqualFold(strings.TrimSpace(symbol), "SOL")
}
