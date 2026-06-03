package recurring

import (
	"regexp"
	"strconv"

	"github.com/open-rails/openrails/internal/billing/declinecode"
)

// On-chain error codes observed live on devnet (#263) for a failed
// transfer_subscription. NOTE: a Custom code can originate from the
// subscriptions program OR the SPL token program reached via CPI; the
// InstructionError does not name the program. Empirically the subscriptions
// program uses high codes (e.g. 400) and the token program uses low codes
// (0-9), so the mapping below is keyed on the specific observed values.
const (
	// onchainCapReached — subscriptions program: amount_pulled_in_period is at the
	// plan cap. The period is ALREADY paid -> idempotent (do not re-charge or dun).
	onchainCapReached = 400
	// onchainTokenInsufficientFunds — SPL token program InsufficientFunds: the
	// subscriber's USDC balance is below the pull amount -> recoverable (dun).
	onchainTokenInsufficientFunds = 1
	// onchainTokenOwnerMismatch — SPL token program OwnerMismatch: the subscriber
	// revoked the token delegate (the trustless cancel). transfer_subscription can
	// no longer move funds -> terminal (cancel + stop). cancel_subscription alone
	// does NOT produce an error (it does not block pulls; #263).
	onchainTokenOwnerMismatch = 4
)

var (
	customCodeRe = regexp.MustCompile(`(?i)"Custom":\s*(\d+)`)
	hexCustomRe  = regexp.MustCompile(`(?i)custom program error:\s*0x([0-9a-f]+)`)
)

// CrankFailure is the classified outcome of a failed pull: a stable
// processor-agnostic decline code (shared with the card processors) plus the
// action category the cranker acts on, and the raw on-chain code for forensics.
type CrankFailure struct {
	Code        declinecode.Code
	Category    declinecode.Category
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
		return CrankFailure{Code: declinecode.CommunicationError, Category: declinecode.Operational, OnChainCode: -1, Raw: raw}
	}
	code := parseOnChainCode(raw)
	switch code {
	case onchainCapReached:
		return CrankFailure{Code: declinecode.DuplicateTransaction, Category: declinecode.AlreadyPaid, OnChainCode: code, Raw: raw}
	case onchainTokenOwnerMismatch:
		return CrankFailure{Code: declinecode.DeclinedStopRecurring, Category: declinecode.Terminal, OnChainCode: code, Raw: raw}
	case onchainTokenInsufficientFunds:
		return CrankFailure{Code: declinecode.InsufficientFunds, Category: declinecode.Recoverable, OnChainCode: code, Raw: raw}
	}
	// Unknown failure: conservative default -> dunning (grace-protected,
	// recoverable on the next successful pull) rather than silently dropping it.
	return CrankFailure{Code: declinecode.GenericDecline, Category: declinecode.Recoverable, OnChainCode: code, Raw: raw}
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
