package recurring

// Crank failure codes. internal/decline maps them onto the shared
// decline reasons (rail "solana"); the category is the cranker's own next step.

// CrankCode is a stable failure code the crank maps its on-chain program
// errors onto, recorded on the subscription failure.
type CrankCode string

const (
	// InsufficientFunds — the payer lacked funds (card: NSF; Solana: subscriber
	// USDC balance below the plan amount). Recoverable -> dunning.
	InsufficientFunds CrankCode = "insufficient_funds"

	// CommunicationError — a transient transport/availability problem reaching the
	// rail (card: gateway comms; Solana: RPC/network, or the cranker wallet
	// out of SOL gas). Operational -> retry, never dun the subscriber.
	CommunicationError CrankCode = "communication_error"

	// ProcessingError — a generic rail-side error not otherwise classified.
	ProcessingError CrankCode = "processing_error"

	// DeclinedStopRecurring — the authorization to bill has been withdrawn (card:
	// issuer "stop all recurring payments", NMI 261; Solana: the subscriber
	// cancelled or revoked the delegation on-chain). Terminal -> cancel + stop.
	DeclinedStopRecurring CrankCode = "declined_stop_all_recurring_payments"

	// DuplicateTransaction — this period was already charged (card: duplicate at
	// rail, NMI 430; Solana: amount_pulled_in_period already at the plan cap).
	// Idempotent -> treat as already-paid, advance, do not re-charge or dun.
	DuplicateTransaction CrankCode = "duplicate_transaction"

	// MerchantConfigurationError — the failure is the merchant's setup, not the
	// subscriber (card: invalid merchant config, NMI 410; Solana: plan terms
	// mismatch / ghost plan / wrong cranker). Operational/terminal per context;
	// never the subscriber's fault, never dun.
	MerchantConfigurationError CrankCode = "merchant_configuration_error"

	// DoNotHonor — issuer declined without a specific reason (NMI 201). Recoverable.
	DoNotHonor CrankCode = "do_not_honor"

	// GenericDecline — declined, reason unknown. Conservative default -> dunning
	// (recoverable, grace-protected) rather than silently dropping the failure.
	GenericDecline CrankCode = "generic_decline"
)

// CrankCategory is the action a failure code implies for the recurring biller. It is
// the bridge between "what went wrong" (Code) and "what the cranker does next".
type CrankCategory string

const (
	// Operational — retry on the next run; NEVER dun (transient infra / merchant
	// gas). A shared outage must not past-due a whole book of subscribers.
	Operational CrankCategory = "operational"

	// Recoverable — a genuine subscriber-side decline (e.g. insufficient funds);
	// route to the dunning state machine (retry schedule + grace + eventual cancel).
	Recoverable CrankCategory = "recoverable"

	// Terminal — billing authorization is gone (cancelled/revoked); cancel the
	// membership and stop. Dunning would burn the grace window pointlessly.
	Terminal CrankCategory = "terminal"

	// AlreadyPaid — the charge for this period already happened on-chain; treat as
	// idempotent success (advance the schedule), never re-charge or dun.
	AlreadyPaid CrankCategory = "already_paid"
)

// DefaultCategory returns the category a code implies on its own. Callers may
// override per context (e.g. a merchant-config error that is terminal vs
// retryable), but this gives every code a safe default.
func (c CrankCode) DefaultCategory() CrankCategory {
	switch c {
	case CommunicationError:
		return Operational
	case DeclinedStopRecurring:
		return Terminal
	case DuplicateTransaction:
		return AlreadyPaid
	case InsufficientFunds, DoNotHonor, GenericDecline, ProcessingError:
		return Recoverable
	case MerchantConfigurationError:
		// Not the subscriber's fault — retry/alert rather than dun. Callers that
		// know it is unrecoverable (ghost plan) escalate to Terminal explicitly.
		return Operational
	default:
		return Recoverable
	}
}
