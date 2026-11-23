# Imports

How a merchant's existing subscribers, cards and payment history come into
OpenRails. A PSP import reads them from the provider, read-only, stages them,
links them to customers and lands what a person applies. A billing-book import
lands facts a host declares about its legacy book at an `as_of` horizon. Both
land through `billingimport.Import`, which classifies every subscription with
the decider webhooks and pulls use.

## Concepts

- **PSP import** (`pim_`, `psp_imports`): one read-only pull of one PSP; `pulling`, `pulled`, `applying`, `applied` or `failed`.
- **Staged record** (`pir_`, `psp_import_records`): a provider `customer` (a vault and its cards), `subscription` or `transaction`, with its identity facts.
- **Record state**: `unmatched`, `conflict`, `linked`, `imported`, `blocked`, or `held` (OpenRails already holds it).
- **Rule**: one fact (`email`, `order_id`, `mdf.<field>`, `metadata.<key>`, …) looked up `via` `directory`, `mapping` or `customer_id`.
- **Mapping** (`psp_import_mappings`): the merchant's own identifiers mapped to its customers.
- **Provider price**: a price bound to a provider plan (`price_psp_bindings`); a subscription lands only on one.
- **Observed catalog** (`provider_catalog_objects`): the PSP's products, prices and plans as last observed.
- **Basis** (`psp_import_basis`): every subscription of the PSP at its row version, recorded before the roster read.
- **Declared book** (`billing.DeclaredBilling`): customers, payment methods, PSP customers, subscriptions, transactions, purchases, historical payments and admin grants, true at `as_of`.
- **Facts authority** (`subscriptions.facts_authority`): `host` or `provider`, changing one way only, host to provider.

## How it works

### PSP import

`internal/pspimport`. Each step (`StepPull`, `StepRelink`, `StepApply`) is an
`openrails.psp_import` River job on its own `psp_import` queue.

1. **Create** (`Importer.Create`): a person names the PSP and rules; the row and
   its job commit together. One import per PSP pulls or applies at a time
   (`psp_imports_in_flight_key`, `409 psp_import_in_progress`). Loading or
   rotating a PSP pulls nothing. Solana has no import.
2. **Pull** (`Importer.Pull`): record the basis; list the catalog; read the
   roster and check it (`plausibleRoster`: an NMI roster under a tenth of the
   live book fails with `pull_roster_implausible`); stage records; read history
   in 30-day windows, committing `history_through` after each. Then link,
   classify held records and propose the differences as `pull.*` findings
   tagged `pull_id`.
3. **Link** (`Importer.Link`): manual links (`manual_customer_id`) and CCBill
   price picks (`manual_price_id`), then a relink without reading the provider.
   Customers link first, then subscriptions, then transactions; every rule and
   parent must agree, or the record is a `conflict`.
4. **Apply** (`Importer.RequestApply`, `Importer.Apply`): re-classify, so a row
   that landed since the pull is held; land linked customers, 50 per book,
   through `billingimport.Import` with `reconcile.ProviderFacts` at the pull's
   horizon; approve the access-keeping corrections (`ListPullCorrections`) in
   one approval run; flip unchanged host rows to `provider` (`FlipPSPImportBasis`).
5. **Delete**: removes the import, records and basis; what landed stays.

Per rail: NMI reads v5 customers and subscriptions and the Query API reports; a
live schedule lands with its own terms as an accepted schedule, and a sale no
subscription holds as a `DeclaredHistoricalPayment`. Stripe reads customers,
cards, subscriptions in every status, charges, refunds and invoices; a linked
customer lands in `psp_customers`. CCBill reads DataLink members and daily
rebills; a member lands on a picked price or the one recurring provider price
its latest rebill and cadence match.

An import's book skips every held row (`reconcile.SkippedHeld`) and proposes its
differences instead; a host book skips `provider` rows
(`reconcile.SkippedProviderAuthority`). So provider facts win in either order.
While an import is pulling or holds a subscription staged it owns it
(`parking.ImportOwns`): converge adopts nothing and the park raises no finding.
Failing or deleting the import wakes those parks (`parking.WakeImportOwned`).
An unheld live NMI schedule seen by webhook lands through
`Importer.AdoptNMISubscription`.

### Billing-book import

`internal/billingimport`, through `Client.ImportBilling` or
`POST /v1/merchant/billing-import`.

1. Refuse card numbers in declared references (`rejectDeclaredPANs`), then, in
   one merchant transaction: customers, payment methods, PSP customers,
   transactions (a charge whose NMI or CCBill id is not digits becomes a finding,
   not a payment; an approved one without an amount, a `provider_charge_attributions`
   row), subscriptions (`reconcile.ImportDeclaredSubscriptions`), mandates, admin grants.
2. After commit, each in a short transaction: provider bill history
   (`reconcile.RecordDeclaredHistory`; `provider_pre_schedule` for a charge
   before the first accepted cycle), purchases, historical payments.

Explicit cancel evidence is written as settled history; a subscription without
it lands `unverified` and the decider resolves it at `as_of`.
`subscriptions_exhaustive` cancels every omitted row and needs
`expected_subscriptions` to equal the count. A live NMI schedule with no
recurring anchor stays provider-collected (`life.import.no_recurring_anchor`).

Tables owned: `psp_imports`, `psp_import_records`, `psp_import_basis`,
`psp_import_mappings`, `provider_catalog_objects`, `provider_charge_attributions`.

## Interfaces

- **Go** (`*Client`): `CreatePSPImport`, `ListPSPImports`, `GetPSPImport`,
  `UpdatePSPImport`, `DeletePSPImport`, `ListPSPImportRecords`, `LinkPSPImport`,
  `ApplyPSPImport`, `SetPSPImportMappings`, `ListProviderCatalog`,
  `ListParkedSubscriptions`, `CreateProviderPrice`, `ImportBilling`.
- **HTTP** (`MerchantConfig`; `merchant:config:read` or `merchant:config:manage`):
  `/v1/merchant/psp-imports`, `/v1/merchant/psp-imports/{id}`,
  `GET /v1/merchant/psp-imports/{id}/records`,
  `POST /v1/merchant/psp-imports/{id}/link`,
  `POST /v1/merchant/psp-imports/{id}/apply` (`Person: true`),
  `POST /v1/merchant/psp-import-mappings`, `GET /v1/merchant/provider-catalog`,
  `POST /v1/merchant/billing-import`; proposals by `GET /v1/merchant/proposals`.
- **CLI**: `openrails proposals list --pull`, `openrails undo-run`; no import
  command. **Docs**: [PSP import](../psp-import.md), [batch import](../batch-import.md).

## Guarantees

### 1. Exactly-once provider calls

A PSP import and an adoption only read; a billing-book import calls no
provider. Its one provider effect, a declared cancel whose schedule may be live,
is a `provider_intents` row enqueued in the book's transaction
(`intents.NewProviderCancelScheduler`).

Checks:
- `internal/pspimport/pull.go` sets `ReadOnly` on the NMI and DataLink clients and reads Stripe through `ReadOnlyClient`.
- Every `TestPSPImport` case asserts `stripe.Mutations("")` and `ccbill.Mutations()` are empty and NMI saw no write; OR-J38-S35 asserts the same for an applied correction.

### 2. Agreement with providers

Nothing is guessed: an unlinked or conflicting record, or one on an unmapped
plan, stays staged. A state change is proposed only for a row still at its
basis version, and Apply re-classifies. Absence counts only from an exhaustive
roster (non-empty, not CCBill) that passes the NMI breaker.

Checks:
- `FlipPSPImportBasis` updates only rows whose `row_version` is unchanged.
- OR-J38-S20, S23, S24, S26, S29; OR-J38-S37 to S40 for CCBill ids stored without a leading zero.

### 3. Exact money

Provider decimals are read in the mapped price's currency
(`moneyutil.DecimalToRailMinor`, then `RailMinorToNative`); an amount not exact
in it blocks the record. Declared transactions must be whole minor units
(`NativeToRailMinorExact`); a schedule in another currency than its price is
refused.

Checks:
- OR-J38-S42: NMI `1000.00` lands as 1000 JPY and `12.50` as 12.50 EUR; an inexact amount stays blocked.
- OR-J38-S03; `denominate` (`internal/pspimport/apply.go`) returns a blocking reason, never a rounded amount.

### 4. Access control and tenant isolation

Every route is `MerchantConfig` with a read or manage permission, and Apply is
`Person: true` (`403 person_required` for an API key). Jobs run in their
merchant's scope, queries name `merchant_id`, and records reach their import by
composite key.

Checks:
- OR-J38-S27: only a person or the in-process host applies or approves; OR-J38-S34: a configuration reader writes nothing.
- `internal/db/queries/psp_imports.sql` filters every statement on `merchant_id`.

### 5. Contracts match the code

Routes are declared in `internal/http/routes/merchant.go` and snapshotted in
`api/openapi.json`, `Client` methods in `api/go.txt`, tables in
`api/schema.txt`. In-process and remote clients share the methods.

Checks:
- `go run ./scripts/contracts` reports no change under `api/`.
- `psp_imports_status_check` and `psp_import_records_state_check` fix the statuses and states.

### 6. Data lifecycle

`psp_imports`, `psp_import_records` and
`psp_import_basis` are `State` and go when a person deletes the import;
`psp_import_mappings` (until replaced) and `provider_catalog_objects` (the latest
observation) are `State` and outlive it; `provider_charge_attributions` is
`Permanent`. None is partitioned.

Checks:
- Each table is in `retention.Tables` and in `internal/merchantarchive/owned_tables.go`.
- Deleting an import cascades to its records and basis and sets `reconciliation_findings.pull_id` to NULL.

### 7. Work scales with activity

Nothing pulls on a schedule; only `Importer.Create` queues a pull, and history
resumes from `history_through`. Charge attributions read only declared rows,
once due. One pull reads the whole PSP, by design.

Checks:
- `internal/river/periodic.go` declares no import job; OR-J38-S06 and S12 pull nothing on load or rotation.
- OR-J38-S09: a retried pull resumes without re-reading the roster; OR-J21-S60: one attribution read per import.

### 8. Abuse protection

Staff routes only; no public or customer entry point. A declared book is
scanned for card numbers before anything is written (`rejectDeclaredPANs`,
`cardguard.ContainsPAN`), and bodies are capped at 1 MiB.

Checks:
- OR-J38-S36: numeric CCBill references pass, real card numbers are refused.
- `internal/billingimport/pan_firewall.go` lists the fields it scans.

### 9. Destructive-action safeguards

Apply approves only corrections that keep access, never one a person
dismissed. Cancels, past due, refunds and product moves wait for
`ApproveProposals`, which admits each cancel through the destructive policy
and spends the actor's allowance. Each Apply is one approval run in its
requester's name, reversible by `openrails undo-run`. A billing-book import is
an operator command: its cancels run under `reconcile.WithAdmittedCancels`, and
absence cancels need `subscriptions_exhaustive` with a matching
`expected_subscriptions`.

Checks:
- `ListPullCorrections` (`internal/db/queries/reconciliation.sql`) selects only access-keeping kinds; OR-J38-S21, S25, S28, S32.
- OR-J38-S71: proposal cancels spend the allowance once per subscription; a replay spends nothing.
- `ImportDeclaredSubscriptions` refuses an exhaustive book whose `expected_subscriptions` differs from its count.

## Tests

- `TestPSPImport`: OR-J38-S01 to S06, S08 to S13, S42.
- `TestPSPReconcile`: OR-J38-S20 to S29, S35, S71.
- `TestMerchantConsole`: OR-J38-S30 to S34, S41.
- `TestCCBillIdentity`: OR-J38-S37 to S40, S43; `TestCardGuard`: S36; `TestManualProviderRefresh`: S72.
- Elsewhere: OR-J06-S07, OR-J21-S05, S60 (`TestBackgroundJobs`), OR-J15-S14, S21 (`TestProviderTrailing`), OR-J21-S04 (`TestImportedReceipt`), OR-J13-S03 (`TestEnrollmentContracts`), OR-J27-S10 to S12 (admin grant imports).

Neither package has its own tests.

Gaps:

- The billing-book import has no scenario for classification at `as_of` (active, past due, `unverified`, stale cancel); tests use it to set up books.
- Untested: `subscriptions_exhaustive` absence cancels, `historical_payments`, NMI standalone history through a PSP import, `life.import.no_recurring_anchor`, `life.import.schedule_unresolved`, `life.import.purchase_refused`.
- The billing-book import records no actor, and its cancels skip the destructive switch and cancel budget without an admission step.
