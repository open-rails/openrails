package recurring

import (
	"regexp"
	"strconv"
)

// On-chain error codes of a failed transfer_subscription, as observed on
// devnet. A Custom code may come from the subscriptions program or the SPL
// token program via CPI (the InstructionError does not say which), so the
// mapping keys on the observed values: subscriptions codes are high (400, 508,
// 519), token codes low (0-9).
const (
	// onchainCapReached — subscriptions program: amount_pulled_in_period is at the
	// plan cap. The period is ALREADY paid -> idempotent (do not re-charge or dun).
	onchainCapReached = 400
	// onchainTokenInsufficientFunds — SPL token program InsufficientFunds: the
	// subscriber's USDC balance is below the pull amount -> recoverable (dun).
	onchainTokenInsufficientFunds = 1
	// onchainTokenOwnerMismatch — SPL token OwnerMismatch: the subscriber
	// revoked the token delegate (the trustless cancel), so pulls cannot move
	// funds -> terminal. cancel_subscription alone does not block pulls.
	onchainTokenOwnerMismatch = 4
	// onchainSubscriptionCanceled — the pull is past expires_at_ts, which
	// cancel_subscription sets to the period end -> terminal: stop and mark the
	// membership canceled, never dun.
	onchainSubscriptionCanceled = 508
	// onchainPlanTermsMismatch — subscriptions program PlanTermsMismatch: the plan
	// the subscription points at no longer matches its recorded terms (a ghost or
	// changed plan). On a PULL this is hopeless — re-pulling will keep failing the
	// terms check -> terminal: stop.
	onchainPlanTermsMismatch = 519
)

var (
	customCodeRe = regexp.MustCompile(`(?i)"Custom":\s*(\d+)`)
	hexCustomRe  = regexp.MustCompile(`(?i)custom program error:\s*0x([0-9a-f]+)`)
)

// CrankFailure is the classified outcome of a failed pull: a stable
// rail-agnostic decline code (shared with the card rails) plus the
// action category the cranker acts on, and the raw on-chain code for forensics.
type CrankFailure struct {
	Code        CrankCode
	Category    CrankCategory
	OnChainCode int // parsed program Custom code, or -1 if none
	Raw         string
}

// ClassifyCrankError maps a crank submit/confirm error onto the shared billing
// decline-code vocabulary + the cranker's next action. Order matters:
// operational (transport/gas, no program code) is checked first so a transient
// RPC blip is never mistaken for a subscriber decline.
func ClassifyCrankError(err error) CrankFailure {
	if err == nil {
		return CrankFailure{OnChainCode: -1}
	}
	raw := err.Error()
	if IsOperationalFailure(err) {
		return CrankFailure{Code: CommunicationError, Category: Operational, OnChainCode: -1, Raw: raw}
	}
	code := parseOnChainCode(raw)
	switch code {
	case onchainCapReached:
		return CrankFailure{Code: DuplicateTransaction, Category: AlreadyPaid, OnChainCode: code, Raw: raw}
	case onchainTokenOwnerMismatch:
		return CrankFailure{Code: DeclinedStopRecurring, Category: Terminal, OnChainCode: code, Raw: raw}
	case onchainSubscriptionCanceled, onchainPlanTermsMismatch:
		// Canceled-at-period-end pull (508) or ghost/changed plan (519): the
		// subscription will never pull successfully again -> stop and mark the
		// membership canceled. Never dun.
		return CrankFailure{Code: DeclinedStopRecurring, Category: Terminal, OnChainCode: code, Raw: raw}
	case onchainTokenInsufficientFunds:
		return CrankFailure{Code: InsufficientFunds, Category: Recoverable, OnChainCode: code, Raw: raw}
	}
	// Unknown failure: conservative default -> dunning (grace-protected,
	// recoverable on the next successful pull) rather than silently dropping it.
	return CrankFailure{Code: GenericDecline, Category: Recoverable, OnChainCode: code, Raw: raw}
}

func parseOnChainCode(s string) int {
	if m := customCodeRe.FindStringSubmatch(s); len(m) == 2 {
		if n, e := strconv.Atoi(m[1]); e == nil {
			return n
		}
	}
	if m := hexCustomRe.FindStringSubmatch(s); len(m) == 2 {
		if n, e := strconv.ParseInt(m[1], 16, 64); e == nil {
			return int(n)
		}
	}
	return -1
}
