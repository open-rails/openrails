// Package intents is the durable provider-write ledger. Every outbound
// provider mutation (NMI sales, deletes and vault writes, NMI and Stripe
// refunds, manual rebills, order collections, tier changes, CCBill cancels,
// payment-method swaps, Solana pulls, Stripe payment-method domains) is first
// a provider_intents row with immutable terms, an idempotency key, an origin
// and a relevance window, committed atomically with its River job
// (openrails.provider_operation). The job claims it under a lease, rechecks
// relevance, gates on mode and origin (user and admin run under limited,
// system only under full, nothing under readonly), executes and classifies the
// outcome; each attempt is appended to provider_mutation_logs. A blocked
// intent stays pending with its reason and drains when the blocker lifts; one
// past its window is superseded or expires instead of firing stale.
//
// Uncertainty: once the write-ahead fence is crossed, every adapter error is a
// possible submission. The operation stays unknown_needs_verify until an exact
// receipt is read back: an empty search, query unavailability or delayed
// visibility never authorizes a resend, and a deadline never supersedes an
// unknown attempt. A known pre-submission refusal parks without consuming an
// attempt; a parsed definitive decline ends the attempt. Provider-idempotent
// calls may replay their key (Stripe, within its 23-hour retention). A
// receipt is inserted once against the frozen terms (account, order, vault,
// amount, currency, instrument); one that contradicts them settles nothing
// and is kept as provider_contradiction. An instrument changed before
// submission is provable non-execution (instrument_changed). Nothing triggers
// a compensating cancel or refund automatically.
//
// An order collection freezes its instrument, owns its order while
// unresolved and charges NMI only under the currency's active unscheduled
// mandate. A tier change (nmi_upgrade, stripe_tier_change) is one operation
// keyed by the request's Idempotency-Key: the same key replays the stored
// result, an unresolved one answers 202, another key is
// subscription_change_in_flight, a refusal subscription_change_refused.
// Stripe steps send "<operation id>:<step>" and stamp
// metadata[openrails_tier_change]. NMI refunds render the native amount in the
// original payment's currency scale (100 JPY is sent as 100.00).
//
// An unknown operation provider reads cannot settle is closed by an operator
// with `openrails intents resolve` (Resolution): --receipt reads the named
// provider object back and must match the frozen operation; --not-executed
// needs the type's own non-execution proof (for NMI, a definitive decline as
// the one transaction under the order; refused for submitted collections,
// rebills and prorations) or an operation that never crossed its fence. Each
// resolution records its actor and reason. Unresolved outcomes raise
// life.submission.unresolved or life.sale.unresolved.
package intents
