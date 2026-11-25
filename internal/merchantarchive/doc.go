// Package merchantarchive moves a retained billing book between deployments
// (Client.ExportBillingArchive and ImportBillingArchive,
// GET and POST /v1/merchant/billing-archive, `openrails billing export` and
// `billing import`). It never resolves provider clients or invokes lifecycle
// services; a restore queues only the wakes of the operations it moves.
//
// Export streams a repeatable-read snapshot; Restore verifies the whole
// bounded JSONL stream (internal/archivewire: format Version 4, a header,
// ordered rows and a footer with the row count and a SHA-256 digest; 1 GiB
// total, 8 MiB per line) and inserts it in one transaction. Merchant UUID,
// record IDs, customer subjects, money, ledger history, catalog identities,
// subscription state and provider references are kept; nothing is charged,
// refunded, rebilled or notified. Not moved: AuthKit identities and
// authority, credentials and secret storage, River and infrastructure state,
// and the provider's or custodian's ownership of stored cards (arrange that
// separately). Merchant configuration is not in the archive: copy its Vault
// documents or manifest entry after the restore, naming the same accounts.
// Application metadata is copied verbatim and is not screened for secrets, so
// protect archives as billing data. The digest detects corruption and binds a
// retry; it is not a signature.
//
// The table profiles in package contract decide every OpenRails table:
// retained, excluded with its reviewed columns enumerated (checks.go), or
// refusing rows. An unclassified table, an unknown column or an unsupported
// nested shape refuses export, and so does unsettled state: unresolved
// payments, attempts, admissions, provider intents, unfinished webhook
// receipts, undelivered host events, running maintenance and platform-vault
// cards (billing_archive_platform_cards). Host, AuthKit and River tables are
// outside the boundary. Open recovery cases keep their saved dunning policy.
//
// Cutover is offline: stop every source writer (--source-stopped and
// --target-stopped are attestations, not checks; a readonly write posture is
// not a substitute), prepare the destination identity with
// `openrails billing prepare-target`, and import into an empty book
// (not_empty otherwise). A successful export leaves the source readonly and an
// import lands the destination readonly and disarmed until
// `openrails merchant writes full`; never restore both. The restore guard is one billing_restore receipt per merchant,
// under a merchant-row lock, with triggers suppressed only for the inserting
// transaction; an identical retry returns already_imported and a different
// archive is refused. New OpenRails tables need an explicit portability
// decision in contract, and a new table also goes in the restore guard.
package merchantarchive
