// Package pspimport imports a merchant's subscribers straight from its PSPs:
// it observes the PSP's catalog, pulls each provider object read-only, stages
// it with the identity facts the provider holds, links it to a customer by
// the merchant's rules and mappings, and lands the linked ones through
// billingimport on the provider prices mapping their plans. Objects no rule
// links, and subscriptions whose plan no one mapped, stay staged and listed;
// nothing is guessed.
//
// Safety: the pull only lists, reports and gets on a read-only client and
// never writes to the provider; a landed subscription stays provider-run.
// Nothing pulls on its own: each import is created by a person or the host
// (CreatePSPImport), and runs even under readonly. Sources: NMI v5 plans,
// customers and subscriptions plus the Query API reports (a schedule's amount
// is read in the mapped price's currency; a paused one ends at its next
// billing date); Stripe products, prices, customers, subscriptions in every
// status and the window's charges and refunds; CCBill DataLink active members
// and, a day at a time, signups, rebills, cancellations and expiries.
//
// History: an ended subscription lands canceled where its paid service ended
// (Stripe's ended_at, else its last paid invoice's period; CCBill's expire
// date) for the provider's reason, with its charges; nothing is canceled or
// deleted at the provider.
//
// Steps run on the psp_import River queue. Transient failures (network,
// timeout, 5xx, 429, an unreachable secret store) retry attempt^4 seconds
// apart up to an hour, resuming from the last committed 30-day history
// window; other failures retry 5 times, then fail the import with its error.
// psp_credentials_rejected and psp_not_readable fail at once. One import per
// PSP pulls or applies at a time (psp_import_in_progress). Apply is
// person-only (person_required for an API key) and lands what is linked, a
// few customers per book, then approves the import's safe corrections.
//
// Reconcile: every import compares what it read with what OpenRails holds of
// the PSP, against row versions recorded before the roster read. An object
// already held is held, never landed twice; a rule naming another customer is
// a conflict. Differences become pull.* proposals tagged with the import:
// Apply approves record_charge, update_card, renewal and period-end
// adopt_subscription, and access-keeping correct_subscription ones; cancels,
// refunds, revokes and product moves wait for a person. An NMI roster under a
// tenth of the live book fails with pull_roster_implausible. Provider facts
// outrank a host's book: a subscription's authority moves one way, host to
// provider (a host book skips provider rows with provider_authority).
//
// Observed catalog (ListProviderCatalog): Stripe products and prices and NMI
// plans as last observed, never written. An object is deleted only on the
// provider's missing answer or a complete list lacking it; the latest
// observation wins. A retired object's provider price is not for sale but
// still resolves for its subscribers. While a PSP's latest import is pulling
// or stages a subscription, the import owns it (parks are import_owned).
// CCBill has no catalog: a member lands on a hand-picked price or the one
// recurring provider price matching its latest rebill's amount and cadence
// (within a tenth, at least two days); otherwise it stays blocked.
//
// Rules read one fact (email, username, order_id, customer_ref, plan,
// mdf.<field>, Stripe metadata.<key>, ...), optionally through a {key}
// pattern, and look it up via directory, mapping or customer_id; the default
// is billing.DefaultPSPImportRules. Linking runs customers, then
// subscriptions, then transactions, and every rule and parent must agree.
package pspimport
