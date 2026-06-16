package recurring

import (
	"context"
	"errors"
	"strings"
)

// operationalSignatures are substrings (matched case-insensitively against the
// crank error) that indicate the pull failed for reasons that are NOT the
// subscriber's fault: RPC/network trouble or the cranker (fee-payer) wallet
// being out of SOL. These must be retried and must NEVER dun the subscriber
// (#257) — a SOL-gas outage hits a merchant's entire book at once, so dunning on
// it would wrongly past-due every subscriber.
var operationalSignatures = []string{
	// Transport / RPC availability.
	"connection refused",
	"connection reset",
	"timeout",
	"timed out",
	"deadline exceeded",
	"no such host",
	"network is unreachable",
	"rate limit",
	"too many requests",
	"service unavailable",
	"503",
	"502",
	"500 internal",
	"rpc error",
	"failed to send transaction",
	// Transaction liveness (transient; the same pull succeeds on a later run).
	"transaction was not confirmed",
	"was not confirmed",
	"blockhash not found",
	"block height exceeded",
	"node is behind",
	// Fee-payer (cranker wallet) is out of SOL — operational top-up, not the
	// subscriber's problem.
	"insufficient funds for fee",
	"insufficient lamports",
	"insufficient funds for rent",
	"found no record of a prior credit",
}

// terminalSignatures are substrings (matched case-insensitively against the
// crank error) that indicate the subscription is GONE/inactive on-chain: the
// subscriber cancelled or revoked the delegation directly via a wallet/explorer,
// bypassing the app (#265). These are terminal: immediately cancel the
// subscription and stop — NEVER dun, since dunning would burn ~15 days of
// pointless retries against a subscription the user already killed.
//
// IMPORTANT: the EXACT on-chain program error code/string is NOT yet
// devnet-confirmed. These are conservative matches against common phrasings of
// "subscription cancelled / delegation revoked / not active / PDA closed". The
// exact program error MUST be confirmed on devnet (#265) and this matcher
// tightened to that string then — kept deliberately narrow here to avoid
// misclassifying a RECOVERABLE subscriber-fault (e.g. insufficient USDC) as
// terminal, which would wrongly cancel a paying subscriber's access.
var terminalSignatures = []string{
	"cancelled",
	"canceled",
	"revoked",
	"delegation",
	"not active",
	"inactive",
	"subscription closed",
	"account does not exist",
	"accountnotfound",
}

// IsTerminalFailure reports whether a crank error is terminal: the subscription
// no longer exists / is no longer active on-chain (subscriber cancelled or
// revoked the delegation out-of-band, #265). Terminal errors must cancel the
// subscription and stop — they must NEVER be dunned. Operational errors are
// checked FIRST (a transient RPC/network blip must not be mistaken for a
// terminal cancel), so callers MUST order: IsOperationalFailure -> retry, then
// IsTerminalFailure -> cancel, else -> dun.
func IsTerminalFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, sig := range terminalSignatures {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

// IsOperationalFailure reports whether a crank error is operational (retry, do
// not dun) rather than a subscriber fault (insufficient USDC, revoked
// delegation, plan mismatch -> dun via the existing dunning machine). Context
// cancellation/deadline is always operational. Unknown errors default to
// subscriber-fault: dunning has a grace window and recovers on the next
// successful pull, so a wrong dun-start is recoverable — whereas wrongly
// suppressing dunning lets a genuinely-unpaid subscriber keep access for free.
func IsOperationalFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, sig := range operationalSignatures {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}
