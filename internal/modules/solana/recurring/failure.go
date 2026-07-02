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
