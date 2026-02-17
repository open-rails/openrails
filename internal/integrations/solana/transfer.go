package solana

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	solanago "github.com/doujins-org/solana-go"
	"github.com/doujins-org/solana-go/programs/system"
	"github.com/doujins-org/solana-go/programs/token"
	"github.com/doujins-org/solana-go/rpc"
	log "github.com/sirupsen/logrus"
)

// TransferRequest describes a Solana transfer to build.
type TransferRequest struct {
	FromWallet  string
	ToWallet    string
	TokenSymbol string
	TokenMint   string
	Amount      uint64
	Reference   string
}

// TransferResponse contains a base64-encoded transaction payload.
type TransferResponse struct {
	TransactionBase64 string
}

// VerifyTransferRequest describes the expected values for verifying a transfer.
type VerifyTransferRequest struct {
	Signature         string
	ExpectedAmount    uint64
	ExpectedRecipient string
	ExpectedTokenMint string
	ExpectedPayer     string
	ExpectedReference string
}

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

	blockhash, err := c.GetLatestBlockhash(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get blockhash: %w", err)
	}

	var referencePub *solanago.PublicKey
	if ref := strings.TrimSpace(req.Reference); ref != "" {
		refKey, err := solanago.PublicKeyFromBase58(ref)
		if err != nil {
			return nil, fmt.Errorf("invalid reference key: %w", err)
		}
		referencePub = &refKey
	}

	var instructions []solanago.Instruction
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

	transaction, err := solanago.NewTransaction(
		instructions,
		blockhash,
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
		TransactionBase64: base64.StdEncoding.EncodeToString(txBytes),
	}, nil
}

// VerifyTransfer confirms the transaction and validates that it matches expected values.
func (c *RPCClient) VerifyTransfer(ctx context.Context, req VerifyTransferRequest) error {
	if req.ExpectedAmount == 0 {
		return fmt.Errorf("expected amount must be greater than 0")
	}
	expectedRecipient := strings.TrimSpace(req.ExpectedRecipient)
	if expectedRecipient == "" {
		return fmt.Errorf("expected recipient is required")
	}
	expectedReference := strings.TrimSpace(req.ExpectedReference)
	if expectedReference == "" {
		return fmt.Errorf("expected reference is required")
	}

	txResult, err := c.fetchConfirmedTransaction(ctx, req.Signature)
	if err != nil {
		return err
	}

	reference := expectedReference
	if err := validateTransactionContent(txResult, req.ExpectedAmount, expectedRecipient, req.ExpectedTokenMint, req.ExpectedPayer, &reference); err != nil {
		return fmt.Errorf("transaction content validation failed: %w", err)
	}

	log.WithFields(log.Fields{
		"signature": req.Signature,
		"slot":      txResult.Slot,
		"fee":       txResult.Meta.Fee,
	}).Info("Transaction verified on-chain")

	return nil
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

func validateTransactionContent(txResult *rpc.GetTransactionResult, expectedAmount uint64, expectedRecipient string, expectedTokenMint string, expectedPayer string, expectedReference *string) error {
	if txResult.Transaction == nil {
		return fmt.Errorf("transaction data not available")
	}

	tx, err := txResult.Transaction.GetTransaction()
	if err != nil {
		return fmt.Errorf("failed to decode transaction: %w", err)
	}

	if expectedPayer != "" {
		payerPub, err := solanago.PublicKeyFromBase58(expectedPayer)
		if err != nil {
			return fmt.Errorf("invalid expected payer: %w", err)
		}
		if len(tx.Message.AccountKeys) == 0 || !tx.Message.AccountKeys[0].Equals(payerPub) {
			return fmt.Errorf("transaction fee payer does not match expected wallet")
		}
	}

	if expectedReference != nil && *expectedReference != "" {
		referencePub, err := solanago.PublicKeyFromBase58(*expectedReference)
		if err != nil {
			return fmt.Errorf("invalid reference key: %w", err)
		}
		if !messageContainsKey(&tx.Message, referencePub, txResult.Meta.LoadedAddresses) {
			return fmt.Errorf("reference key not included in transaction")
		}
	}

	recipientCandidates := make(map[string]struct{})
	recipientCandidates[expectedRecipient] = struct{}{}
	if derived, err := deriveRecipientTokenAccount(expectedRecipient, expectedTokenMint); err == nil && derived != "" {
		recipientCandidates[derived] = struct{}{}
	}

	match, err := findTransferMatch(tx, txResult, recipientCandidates, expectedTokenMint, expectedAmount, expectedPayer)
	if err != nil {
		return err
	}
	if match == nil {
		return fmt.Errorf("no qualifying transfer found for recipient %s", expectedRecipient)
	}

	if err := verifyBalanceChanges(txResult, match.accountIndex, match.destination.String(), expectedAmount); err != nil {
		return err
	}

	return nil
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
	if tx == nil {
		return nil, errors.New("transaction message unavailable")
	}

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

	for instIdx, inst := range tx.Message.Instructions {
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
			sysInstr, err := system.DecodeInstruction(accounts, inst.Data)
			if err != nil || sysInstr == nil {
				continue
			}
			transfer, ok := sysInstr.Impl.(*system.Transfer)
			if !ok {
				continue
			}
			match := evaluateSystemTransfer(transfer, accountIndexFromInstruction(&inst, 1), candidateKeys, expectedAmount, expectedPayer)
			if match != nil {
				bestMatch = pickBetterMatch(bestMatch, match, instIdx)
			}
		case programID.Equals(token.ProgramID):
			tokenInstr, err := token.DecodeInstruction(accounts, inst.Data)
			if err != nil || tokenInstr == nil {
				continue
			}
			switch dec := tokenInstr.Impl.(type) {
			case *token.Transfer:
				match := evaluateTokenTransfer(txResult, accounts, dec.Amount, accountIndexFromInstruction(&inst, 1), candidateKeys, expectedMintNorm, expectedAmount, expectedPayer)
				if match != nil {
					bestMatch = pickBetterMatch(bestMatch, match, instIdx)
				}
			case *token.TransferChecked:
				match := evaluateTokenTransferChecked(txResult, accounts, dec.Amount, accountIndexFromInstruction(&inst, 2), candidateKeys, expectedMintNorm, expectedAmount, expectedPayer)
				if match != nil {
					bestMatch = pickBetterMatch(bestMatch, match, instIdx)
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
	if expectedMint != "" && mint != "" && !mintMatches(expectedMint, mint) {
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
	mint := ""
	if len(accounts) > 1 {
		mint = accounts[1].PublicKey.String()
	}
	if mint == "" {
		mint = mintForAccount(txResult, accountIdx)
	}
	if expectedMint != "" && mint != "" && !mintMatches(expectedMint, mint) {
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

func pickBetterMatch(current, candidate *transferMatch, _ int) *transferMatch {
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

func accountIndexFromInstruction(inst *solanago.CompiledInstruction, accountPosition int) int {
	if inst == nil || accountPosition >= len(inst.Accounts) {
		return -1
	}
	return int(inst.Accounts[accountPosition])
}

func mintForAccount(txResult *rpc.GetTransactionResult, accountIndex int) string {
	if txResult == nil || txResult.Meta == nil {
		return ""
	}
	for i := range txResult.Meta.PostTokenBalances {
		if int(txResult.Meta.PostTokenBalances[i].AccountIndex) == accountIndex {
			return txResult.Meta.PostTokenBalances[i].Mint.String()
		}
	}
	return ""
}

func normalizeMint(m string) string {
	return strings.ToUpper(strings.TrimSpace(m))
}

func mintMatches(expected, actual string) bool {
	exp := normalizeMint(expected)
	act := normalizeMint(actual)
	if exp == "" || act == "" {
		return exp == act
	}
	if isNativeSOLMint(exp) && isNativeSOLMint(act) {
		return true
	}
	return exp == act
}

func messageContainsKey(msg *solanago.Message, key solanago.PublicKey, loaded rpc.LoadedAddresses) bool {
	if msg != nil {
		for _, k := range msg.AccountKeys {
			if k.Equals(key) {
				return true
			}
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

func verifyBalanceChanges(txResult *rpc.GetTransactionResult, accountIndex int, account string, expectedAmount uint64) error {
	if accountIndex < len(txResult.Meta.PostBalances) && accountIndex < len(txResult.Meta.PreBalances) {
		post := txResult.Meta.PostBalances[accountIndex]
		pre := txResult.Meta.PreBalances[accountIndex]
		if post >= pre {
			delta := post - pre
			if delta >= expectedAmount {
				return nil
			}
		}
	}

	postToken, preToken, err := tokenBalanceDelta(txResult, accountIndex)
	if err != nil {
		return fmt.Errorf("unable to confirm balance change for account %s: %w", account, err)
	}
	if postToken < preToken {
		return fmt.Errorf("token balance decreased for account %s", account)
	}
	if postToken-preToken < expectedAmount {
		return fmt.Errorf("token transfer amount insufficient: expected >= %d, observed %d", expectedAmount, postToken-preToken)
	}
	return nil
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

var nativeSOLMintAliases = map[string]struct{}{
	"":                              {},
	strings.ToUpper(wrappedSOLMint): {},
}

func isNativeSOLMint(tokenMint string) bool {
	mint := strings.ToUpper(strings.TrimSpace(tokenMint))
	if _, ok := nativeSOLMintAliases[mint]; ok {
		return true
	}
	return false
}

func isNativeSOLSymbol(symbol string) bool {
	return strings.EqualFold(strings.TrimSpace(symbol), "SOL")
}
