package recurring

import (
	"context"
	"errors"
	"strings"
)

// operationalSignatures are case-insensitive substrings of crank errors that are
// not the subscriber's fault (RPC trouble, cranker out of SOL). They retry and
// never dun: a SOL-gas outage would past-due a merchant's whole book.
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
// not dun). Context cancellation is always operational. Unknown errors count as
// subscriber faults: a wrong dun recovers within grace, while wrongly skipping
// dunning gives an unpaid subscriber free access.
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
