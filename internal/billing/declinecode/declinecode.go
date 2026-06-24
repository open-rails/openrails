// Package declinecode defines a standardized billing failure/decline vocabulary
// shared across every payment rail (card + Solana), so a rebill failure is
// reported the same way regardless of rail — exactly how Stripe/NMI expose a
// stable `decline_code`.
//
// The codes align with Stripe's decline_code taxonomy and OpenRails' existing
// NMI localization IDs (see internal/integrations/nmi/client.go), so the
// new Solana cranker speaks the same language the card rails already do.
package declinecode

// Code is a stable, rail-agnostic decline/failure code. String-valued (like
// Stripe's decline_code), recorded on the subscription failure + surfaced to
// operators/analytics. Solana cranks map their on-chain program errors onto
// these; cards map their gateway response codes onto these.
type Code string

const (
	// InsufficientFunds — the payer lacked funds (card: NSF; Solana: subscriber
	// USDC balance below the plan amount). Recoverable -> dunning.
	InsufficientFunds Code = "insufficient_funds"

	// CommunicationError — a transient transport/availability problem reaching the
	// rail (card: gateway comms; Solana: RPC/network, or the cranker wallet
	// out of SOL gas). Operational -> retry, never dun the subscriber.
	CommunicationError Code = "communication_error"

	// ProcessingError — a generic rail-side error not otherwise classified.
	ProcessingError Code = "processing_error"

	// DeclinedStopRecurring — the authorization to bill has been withdrawn (card:
	// issuer "stop all recurring payments", NMI 261; Solana: the subscriber
	// cancelled or revoked the delegation on-chain). Terminal -> cancel + stop.
	DeclinedStopRecurring Code = "declined_stop_all_recurring_payments"

	// DuplicateTransaction — this period was already charged (card: duplicate at
	// rail, NMI 430; Solana: amount_pulled_in_period already at the plan cap).
	// Idempotent -> treat as already-paid, advance, do not re-charge or dun.
	DuplicateTransaction Code = "duplicate_transaction"

	// MerchantConfigurationError — the failure is the merchant's setup, not the
	// subscriber (card: invalid merchant config, NMI 410; Solana: plan terms
	// mismatch / ghost plan / wrong cranker). Operational/terminal per context;
	// never the subscriber's fault, never dun.
	MerchantConfigurationError Code = "merchant_configuration_error"

	// DoNotHonor — issuer declined without a specific reason (NMI 201). Recoverable.
	DoNotHonor Code = "do_not_honor"

	// GenericDecline — declined, reason unknown. Conservative default -> dunning
	// (recoverable, grace-protected) rather than silently dropping the failure.
	GenericDecline Code = "generic_decline"
)

// Category is the action a failure code implies for the recurring biller. It is
// the bridge between "what went wrong" (Code) and "what the cranker does next".
type Category string

const (
	// Operational — retry on the next run; NEVER dun (transient infra / merchant
	// gas). A shared outage must not past-due a whole book of subscribers.
	Operational Category = "operational"

	// Recoverable — a genuine subscriber-side decline (e.g. insufficient funds);
	// route to the dunning state machine (retry schedule + grace + eventual cancel).
	Recoverable Category = "recoverable"

	// Terminal — billing authorization is gone (cancelled/revoked); cancel the
	// membership and stop. Dunning would burn the grace window pointlessly.
	Terminal Category = "terminal"

	// AlreadyPaid — the charge for this period already happened on-chain; treat as
	// idempotent success (advance the schedule), never re-charge or dun.
	AlreadyPaid Category = "already_paid"
)

// DefaultCategory returns the category a code implies on its own. Callers may
// override per context (e.g. a merchant-config error that is terminal vs
// retryable), but this gives every code a safe default.
func (c Code) DefaultCategory() Category {
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
