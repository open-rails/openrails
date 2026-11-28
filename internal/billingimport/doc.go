// Package billingimport imports a host's legacy billing book as facts and
// classifies them through the same decider the pull, probe and webhook paths
// use, at the declared AsOf. Admin comps ride the same book. The wire
// vocabulary (billing.DeclaredBilling, POST /v1/merchant/billing-import,
// Client.ImportBilling) is aliased here. A book only a provider holds is
// pspimport's.
//
// The import writes local state only and calls no provider inside its
// transaction; the provider stays the source of truth for live money, and
// provider-managed schedules keep collecting there. Every provider-bound row
// names its PSP (default_psp or per-row psp, by id or key); an unattributed
// row refuses the book. Vault references stay scoped to the PSP account that
// created them. subscriptions_exhaustive with expected_subscriptions makes
// one call evidence of absence; batches leave it false.
//
// A subscription with explicit cancel evidence is settled history; one
// without is seeded unverified and decided at AsOf (future paid-through
// active, mid-dunning past_due, past the dunning staleness window canceled
// without a charge, evidence-starved unverified with access kept). Re-posting
// a book at the same AsOf is a no-op (all skipped); a newer AsOf converges
// rows forward and never rewrites settled history. A blocked row never fails
// the batch: one whose customer already holds its product is a lifecycle
// conflict, a canceled row owed a provider stop included, since it holds the
// product until the stop lands. Once a PSP import, an approved proposal or a webhook has read a
// row it is the provider's, and a later book skips it (provider_authority).
// Imported members are entitled when the call returns. What a book lands
// adds MRR from its start to when its paid service ended at their own
// dates (DeclaredSubscriptionFact.ServiceEnd), written in its transaction
// (billing.record_mrr_history): nothing is booked at AsOf.
//
// Declared purchases become complete engine-owned invoices with frozen lines
// and access (a credit pack's credits are not deposited); a mismatched one is
// blocked with life.import.purchase_refused. historical_payments records a
// verified NMI sale and its refunds without access. An NMI or CCBill charge
// must name the provider's digits-only transaction id; one without an amount
// is a provider_charge_attributions row that a job reads from NMI's Query API
// by id (CCBill waits for the DataLink pull), else
// life.import.transaction_unverified. Live NMI declines are retried only with
// a recurring stored-card anchor (recurring_transaction_id or the first
// approved signup sale), else life.import.no_recurring_anchor.
//
// Recurring receipts need their accepted bill: an NMI or CCBill
// accepted_schedule, or terms derived from the book; else
// life.import.schedule_unresolved and pull.bill.unresolved findings. The next
// provider refresh (`openrails provider-refresh --merchant`) accepts an
// active NMI schedule whose amount and day_frequency equal the price's and
// places its receipts. Stripe receipts carry the full finalized
// stripe_invoice. For an NMI-owned book: import at a fixed AsOf, start the
// workers and request one provider refresh, triage the pull.* findings and
// blocked rows (fix the book and re-import), then arm the destructive switch;
// until then a staff cancel of an NMI-owned membership is
// provider_cancel_held (a customer's own cancel goes through).
package billingimport
