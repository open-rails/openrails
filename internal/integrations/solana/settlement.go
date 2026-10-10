package solana

import (
	"fmt"
	"strconv"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

// LateSettlementWindow bounds how long past its validity a quote is still
// honoured. A landing inside it settles at the quoted amount; a later one
// grants nothing.
const LateSettlementWindow = 30 * time.Minute

// SettlementTooLate reports whether a transaction that landed at landedAt is
// past validUntil plus the late window. An unknown landing time or validity is
// not late.
func SettlementTooLate(landedAt *time.Time, validUntil time.Time) bool {
	return landedAt != nil && !validUntil.IsZero() && landedAt.After(validUntil.Add(LateSettlementWindow))
}

// TokenCredited is how many base units of mint the token account received in
// a landed transaction, from its pre/post token balances.
func TokenCredited(txResult *rpc.GetTransactionResult, tx *solanago.Transaction, account, mint solanago.PublicKey) (uint64, error) {
	if txResult == nil || txResult.Meta == nil || tx == nil {
		return 0, fmt.Errorf("transaction metadata not available")
	}
	index := -1
	for i, key := range tx.Message.AccountKeys {
		if key.Equals(account) {
			index = i
			break
		}
	}
	if index < 0 {
		return 0, fmt.Errorf("token account %s not in transaction", account)
	}
	for _, post := range txResult.Meta.PostTokenBalances {
		if int(post.AccountIndex) == index && !post.Mint.Equals(mint) {
			return 0, fmt.Errorf("token account %s holds mint %s, want %s", account, post.Mint, mint)
		}
	}
	post, pre, err := tokenBalanceDelta(txResult, index)
	if err != nil {
		return 0, fmt.Errorf("token account %s: %w", account, err)
	}
	if post < pre {
		return 0, fmt.Errorf("token balance decreased for account %s", account)
	}
	return post - pre, nil
}

// tokenBalanceDelta is the post and pre token balance of the account at
// accountIndex; a missing pre balance is zero.
func tokenBalanceDelta(txResult *rpc.GetTransactionResult, accountIndex int) (uint64, uint64, error) {
	var post, pre uint64
	found := false
	for _, b := range txResult.Meta.PostTokenBalances {
		if int(b.AccountIndex) == accountIndex {
			if b.UiTokenAmount == nil {
				return 0, 0, fmt.Errorf("post token amount missing")
			}
			amt, err := strconv.ParseUint(b.UiTokenAmount.Amount, 10, 64)
			if err != nil {
				return 0, 0, err
			}
			post, found = amt, true
			break
		}
	}
	if !found {
		return 0, 0, fmt.Errorf("token balance not found")
	}
	for _, b := range txResult.Meta.PreTokenBalances {
		if int(b.AccountIndex) == accountIndex {
			if b.UiTokenAmount == nil {
				return 0, 0, fmt.Errorf("pre token amount missing")
			}
			amt, err := strconv.ParseUint(b.UiTokenAmount.Amount, 10, 64)
			if err != nil {
				return 0, 0, err
			}
			pre = amt
			break
		}
	}
	return post, pre, nil
}
