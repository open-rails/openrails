# Merchant billing archive v1

`Export` streams a repeatable-read snapshot. `Restore` validates the complete
bounded JSONL stream and inserts it in one transaction. The merchant UUID and
retained domain IDs are unchanged. Footer/digest errors, schema drift, unsafe
content, nonempty destinations and failed references roll the transaction back.
Callers must discard any partial export on error. These functions own their
transactions and expect an unbound application-role database handle.

`internal/archivewire` owns bounded framing and footer verification shared with
the Client. The engine-owned `contract` package applies table order, row widths
and stored-value rules; the remote Client does not link these billing contracts.

The final cutover requires all source writers stopped. Repeatable read proves
snapshot consistency, not that writers were stopped. The destination must be
explicitly provisioned under its new host/AuthKit authority and remain unarmed
until cutover is complete. SQL checks every merchant table is empty before the
first import; it cannot inspect manifest credentials or other runtime processes.
Merchant directory/authority settings are never copied over the destination.

The fixed profiles in `contract/profiles.go` cover 42 retained tables, including
ledger accounts/transfers, invoices, grants, entitlements, admission receipts,
rating and provider-refresh watermarks, webhook deduplication, acknowledged host
events, and completed checkout and provider-intent coordinates. Scalar money is
encoded as decimal strings; nested money contracts use integer JSON tokens.
The archive targets the current fresh schema only. Unknown tables (including
unscoped tables), unknown columns on retained or excluded tables, and unsupported
nested shapes refuse export. New schema fields
require an explicit portability decision; a list of known table names alone is
not sufficient coverage.

## Exclusions and refusals

- Operation authorizations, provider billing qualifications/observations,
  destructive before-images and account updater batches refuse any rows. Their
  opaque evidence has no v1 portable contract; provider formats belong to #1010.
- Payment metadata retains declared order, provider transaction, Stripe invoice
  and test-run correlation strings. Unknown keys, nested provider bodies and
  unsafe values refuse before the export header. Payment discount metadata,
  payment method metadata, usage event metadata and invoice item metadata refuse
  when nonempty; empty/null values can be reconstructed without losing facts.
- Maintenance history retains every prune/converge-enforce/merchant-purge row,
  including unreferenced runs. Nonempty coverage, affected, summary or inventory
  fields refuse. Reconciliation runs are diagnostic and excluded. Other run
  kinds refuse. The local billing-restore receipt is excluded from re-export.
- Subscription gateway metadata preserves declared checkout correlation IDs,
  delayed-start/run markers, admin notes and supersession markers. Unknown keys,
  raw provider bodies and unsafe values refuse export and restore.
- Maintenance note/error text is diagnostic and excluded. Generated entitlement
  periods and generated run-class columns are reconstructed by PostgreSQL.
- PSP settings, signer references and declared public configuration are retained.
  Source diagnostic labels, credential-version/validation markers, known API-key
  material and RPC API keys are excluded. Unknown evidence keys refuse. Custodian
  credential-version floors are deployment credential state and are excluded.
- Webhook health/daily counts, admission-denial counts, dashboard layouts,
  notifications, rail mutation logs and reconciliation findings are diagnostic
  exclusions. DEKs, merchant secrets, webhook destinations and destructive policy
  are deployment configuration. Every excluded table's reviewed columns are
  enumerated in `checks.go`; newly added columns refuse.
- Merchant directory, global worker health/fair-sweep cursors, and the global
  destructive-action switch belong to the destination deployment.

Unresolved payments, invoice attempts, open admissions, provider intents,
checkouts, unfinished webhook receipts, undelivered host events and running
maintenance work refuse export. Terminal checkout rows retain IDs, provider
coordinates, request fingerprints and declared safe rail fields/routing facts;
unknown rail state or metadata refuses. The currently supported checkout state
covers NMI completion/failure and its request fingerprint. Redirect/Solana quote
or transaction blobs require a separate safe contract. Terminal intent payloads
and evidence are never filtered: known safe refund/result shapes and typed NMI
sale/subscription creation receipts are preserved verbatim. Successful NMI
checkout intents prune their submission payloads in the ordinary runner; any
nonempty retained NMI purchase payload still refuses. Other provider workflows
require their own safe contract.

## Restore guard

The existing maintenance ledger stores one `billing_restore` receipt per
merchant. A destination merchant-row lock serializes the first import and
FK-backed first writes. Receipt mutation is restricted to the table owner via
fixed-search-path functions. Trigger suppression requires the receipt to be
running for the scoped merchant, inserted by the current transaction, and named
by its transaction-local GUC. A GUC or an old receipt alone cannot suppress work.
A deferred constraint prohibits unfinished receipts and rechecks ledger totals.
Restoring never resolves provider clients or enqueues jobs; payment events,
subscription transition insertion and ledger-counter application are suppressed
only while inserting their retained originals. Historical subscription tiers are
preserved, and live subscriptions (including remaining paid access) must agree
with their product tier before commit. Ordinary constraints remain live.

A retry verifies the entire artifact and compares its digest/row count with the
committed receipt. An identical retry is a no-op even after normal destination
activity; a different artifact refuses. Lost responses therefore do not create
additional rows or provider work.

Integration tests use the RLS-enforcing application role and isolated PostgreSQL
18 databases. They cover populated profiles across three schemas, production NMI
one-off sale and recurring creation through CheckoutSessionService/CheckoutService
against a loopback provider, new destination authority, atomic tamper refusal,
unknown tables/columns, receipt tampering and temporary-catalog shadowing,
concurrent restore receipts, balanced exact-integer ledgers and lost-response
retry. Purchase proofs create payments, subscriptions, grants and entitlements
through production writers, then archive/restore into another schema. A fresh
session service with an empty process cache reads, recreates with the same key
and confirms the original result; the retained intent also replays. Provider
request counts, payments, subscriptions, paid periods and benefits remain
unchanged. Public Client/HTTP/CLI and live-provider qualification are separate
gates.
