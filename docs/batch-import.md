# Batch import: migrating legacy billing data into OpenRails

Use batch import to bring an existing site's subscribers, payment history and
vault references into OpenRails. If the PSP holds the book, use
[PSP import](psp-import.md) to read it from the provider.

### Provider ownership

Supply subscription, payment, cancellation and vault facts as of a fixed
`as_of` time. OpenRails classifies subscription standing at that time and
derives access. Provider-managed schedules keep collecting at the PSP.
Webhooks keep the local mirror current after import, and an
[operator refresh](operations.md#provider-refresh) catches up on missed events.

Vault references (`rail_customer_ref` and `rail_method_ref`) stay scoped to the
PSP account that created them. When moving accounts, collect cards again at
the new PSP. [PSP binding and credential rotation](operations.md#psps-and-credentials)

The import itself writes local state. Workers can later read charge evidence
or process a rail-delete intent queued by a cancellation with
`schedule_live: true`, subject to the deployment's
[write mode](operations.md#safety-levers).

### The import surface

**Client**: `client.ImportBilling(ctx, billing.DeclaredBilling{...})` on the
merchant's handle, in every deployment (an `openrails.New` engine's
`Merchant(id)` in process, or `openrails.NewRemoteMerchant`). Resolve a public
name once and keep the merchant's UUID. **HTTP**: `POST /v1/merchant/billing-import`
with the identical JSON body — merchant from the authenticated credential,
mounted with `RouteGroups.MerchantConfig` and checked like the merchant's
other configuration. Split large HTTP imports to fit the default 1 MiB
request limit.

See [`billing.DeclaredBilling`](../billing/billing_import.go) for the Go types and
JSON field names.

| Kind | Identity within the merchant | What to declare |
|---|---|---|
| `customers` | Customer UUID | The host's stable subject ID in `customer`. |
| `payment_methods` | PSP and vault references | Vault references and optional `card` display details. |
| `psp_customers` | PSP and `remote_customer_ref` | The provider's customer object, such as Stripe's `cus_` reference. Existing customer links are kept. A reference held by another customer refuses the book. |
| `subscriptions` | PSP and `rail_subscription_id` | Stable `source_id`, price, payment method if known, and paid-through, cancellation or dunning evidence. Raw `evidence` is retained in `gateway_response`. |
| `transactions` | PSP and `transaction_id` | Subscription charge attempts, including declines, and supported linked refunds. |
| `purchases` | `source_id` and PSP transaction | [Settled one-off sales](#declared-purchases), including the sold lines and the payment. |
| `historical_payments` | PSP and `transaction_id` | [Verified NMI payments and refunds](#historical-payments) whose original purchase or subscription is unavailable. |
| `admin_grants` | `source_id` | [Comped access windows](#imported-admin-grant-changes). |

**Attribution is required.** Every provider-bound row an import writes
carries the PSP it came from — the same `psp_id` a pull stamps — because the
same prune, rollback and uniqueness rules apply to an imported row as to a
pulled one. State it once for the whole book with `default_psp`, or per row
with `psp`; either form names a PSP by `{"id": "psp_…"}` or by its key
`{"key": "mobius"}`. A row that resolves to neither REFUSES the import, naming
the row and listing the merchant's known PSPs. There is no unattributed lane.

```json
{
  "as_of": "2026-01-01T00:00:00Z",
  "default_psp": {"key": "mobius"},
  "subscriptions": [
    {"source_id": "legacy-1", "rail": "nmi", "rail_subscription_id": "9911",
     "psp": {"key": "paykings"}, "...": "..."}
  ]
}
```

Set `as_of` to the RFC 3339 timestamp the legacy data represents. OpenRails
evaluates subscription standing at that time. Send `subscriptions_exhaustive:
true` only when a single call covers the entire merchant subscription book and
can serve as evidence of absence. Set `expected_subscriptions` to the nonzero
number of subscription rows in that request. Keep the flag false for batches.
[Coverage validation](../internal/reconcile/declared_import.go)

OpenRails can retry declines from live NMI schedules when it has a recurring
stored-card anchor. It takes that anchor from the method's
`recurring_transaction_id` or the schedule's first approved sale in the book,
which must be the customer's recurring signup. Without an anchor, NMI continues
collecting and OpenRails reports `life.import.no_recurring_anchor`. Replaying
the same anchor is allowed. A different reference for the same PSP and vault
method conflicts.

Each subscription fact takes one of two lanes:

- **Explicit cancel evidence** (`user_canceled` / `chargeback` /
  `provider_terminated`) is settled history — written directly with faithful
  `cancel_type` and dates (cancel-with-runway keeps the paid-through end).
- **No cancel evidence** — seeded `unverified` and resolved by the decider at
  `as_of`: paid-through in the future → `active`; mid-dunning within the
  window → `past_due` with grace; roster-dead → canceled; evidence-starved →
  stays parked `unverified` (cancellation-last-resort by construction).

**Re-run semantics**: re-posting the same book at the same `as_of` is a pure
no-op (everything reports `skipped`; no duplicate payments, no lifecycle
regression). A later import at a **newer** `as_of` converges existing rows
forward through the decider — e.g. a row re-declared as stalled beyond the
dunning window lands a terminal cancel dated at the new horizon. Settled
history is never rewritten. Per-`source_id` results come back as
`imported` / `skipped` / `blocked` (+ reason); a blocked row (unknown price,
rail id owned by another customer, lifecycle-slot conflict) never fails the
batch — it stays parked and is reported loudly.

The import derives access for the book's customers before it returns. To
declare access independent of payment or subscription standing, use
`admin_grants`.

### Imported admin grant changes

An `admin_grants` row (`billing.DeclaredAdminGrant`) permanently binds its
`source_id` to one customer and product within the merchant. Re-declare it
with changed `starts_at` / `ends_at` to replace its window at the book's
`as_of`. A nil `ends_at` is indefinite. The first declaration retains its
original start; replacement access starts no earlier than `as_of`. Earlier
access remains recorded up to that horizon. A later end can revive an
expired grant. Identical declarations report `skipped` without new history.

Set `removed: true` to end an imported grant at `as_of`; only `customer`,
`product` and `source_id` are required for removal. Omitting the row does
nothing. Re-declaring its window at a later horizon restores access from
that horizon. A staff revoke also yields to an identical re-declaration at a
later horizon: the host's declaration re-grants access. Removing an unknown
source is skipped.

Only grants created by billing import are reconciled. A source id owned by
staff, or re-used for another customer/product, is reported in `blocked`
with a `reasons[source_id]` explanation; the rest of the book still applies.
Purchase and subscription grants are untouched. Changes append immutable supersede/revoke
and replacement events and project access in the same transaction. A changed
state requires an `as_of` later than that source's last recorded change;
conflicting equal or older horizons are also reported as blocked per source.
Exact state replay remains a no-op even at a later horizon. Missing required
fields, duplicate admin grant source ids, and an end not after its start make
the book malformed and roll back its transaction.

### Declared purchases

A `purchases` row (`DeclaredPurchase`) is a one-off sale the legacy system
settled, such as a wallet top-up. Import records it as a complete
engine-owned order with frozen `lines` (`DeclaredOrderLine`: `price`,
optional `quantity`, `unit_amount` as sold, optional `description` and
`access_ends_at`), its succeeded rail payment (`transaction_id`, `paid_at`),
and the access each line's product bought: from `paid_at` to `access_ends_at`,
or for the price's access duration. A credit pack's credits are not
deposited. `settlement` (`asset`, `quantity` in base units, `payer`,
`recipient`; for Solana the transfer's token, amount and wallets) and raw
`evidence` are stored on the payment. Each purchase records in its own
transaction after the book commits; no provider is called.

`amount` must equal the lines' total in a fiat currency. A row that does not
(or has no fiat value, or reuses another purchase's `source_id` or
transaction) is reported `blocked` and kept as a `life.import.purchase_refused`
finding. Re-importing a recorded purchase reports it `skipped`.

```json
{"source_id": "wallet-1", "customer": "…", "rail": "solana", "psp": {"key": "solana"},
 "transaction_id": "5h…", "currency": "USD", "amount": "23000000", "paid_at": "2026-01-02T03:04:05Z",
 "lines": [{"price": "price_…", "unit_amount": "23000000", "access_ends_at": "2026-02-01T03:04:05Z"}],
 "settlement": {"asset": "…", "quantity": "23000000", "payer": "…", "recipient": "…"}}
```

### Historical payments

Use `historical_payments` for a confirmed NMI sale when the original
subscription or catalog purchase is unavailable. Supply `source_id`,
`customer`, `rail: nmi`, the PSP reference or `default_psp`, the provider's
`transaction_id`, `amount`, `currency` and `paid_at`. Use a positive amount
in native fiat units that converts exactly to the rail's minor units. The
payment date must be at or before `as_of`.

An optional `refunds` array records verified refunds of that sale. Supply each
refund's distinct NMI `transaction_id`, positive `amount` and `refunded_at`
between the sale date and `as_of`. The cumulative refund amount cannot exceed
the sale. The import records a settled order and its payments for financial
history without creating access or a subscription. Each receipt and its
refunds commit together. Replays match the PSP transaction, and conflicting
facts are reported as blocked.
[Historical payment import](../internal/billingimport/historical_payments.go)

### Charges before the accepted schedule

Approved NMI and CCBill charges that predate the accepted schedule, or whose
subscription was canceled without an accepted schedule, import as
[settled history](provider-managed-orders.md#charges-before-the-schedule).
A decline in that history is excluded. A live subscription with no accepted
schedule keeps its receipts unresolved until one is accepted.
[Declared history recording](../internal/reconcile/declared_import.go)

### Charge evidence and refunds

A declared NMI or CCBill charge names the provider's own transaction id,
which is digits only. Any other id (a host's synthetic id) is evidence: a
`life.import.transaction_unverified` finding, no payment. An approved charge
with a real id but no amount is kept as an attribution of that provider
transaction to its subscription (`provider_charge_attributions`). The import
queues, with its own commit, one job that reads each one it declared from
NMI's Query API, by transaction id, and records the
provider's exact amount and time; a provider pull that reads the id binds it
to the same subscription. An id NMI does not have, or reports declined or
voided, records no payment and becomes a `life.import.transaction_unverified`
finding. OpenRails reads only NMI by id. A CCBill attribution waits for the
DataLink pull, which records the charge when it reads it; one the pull read
past without recording, or one on a PSP without DataLink credentials, becomes
that finding, and a pull that records the charge later closes it.
Every path records a provider transaction once per PSP.

For a confirmed subscription refund on NMI, include the original successful
sale in the same subscription group and declare the refund with
`type: refund`, `success: true`, its own provider `transaction_id`, and
`original_transaction_id` pointing to that sale. Supply `amount`, `currency`
and `occurred_at` for both. A transaction's optional `source_id` reports a
separate per-receipt outcome and must be unique across the book's source IDs.
[Receipt validation](../internal/billingimport/receipt_sources.go) ·
[Refund recording](../internal/reconcile/declared_refunds.go)

### The migration playbook

1. **Apply migrations.** Build the engine with `openrails.New` on the pool it
   will use; it creates its billing baseline and its River tables first.
   Apply your application schemas separately, then validate the target shape
   before writing anything.
2. **Declare the merchant, PSPs, and catalog.** Upsert the merchant + its
   operator-declared PSP rows through `Config.Merchants` (or `LocalClient.DeclarePSP` from `Engine.LocalMerchant`)
   (or manifest boot), then push the catalog — including *retired* historical
   price points, so every legacy subscription resolves a price. Resolve the
   `psps` row ids to stamp on imported rows. A PSP declared without
   credentials (`MerchantDeclaration.PSPs`) is an identity for attribution and price
   links only: it is never armed, so purchase discovery does not advertise it,
   a purchase naming it is refused as unroutable, and links to it are stored
   as operator-owned with `sync_status: sync_disabled`.
3. **Fix the horizon.** Derive `as_of` from the legacy dump itself (e.g. the
   max source `updated_at`) or declare it explicitly; never default to
   wall-clock.
4. **Import in dependency order, batched**: customers → payment methods →
   subscriptions + their transactions. Keep the host's stable ids as
   `source_id` so re-runs are exact and results are auditable per row. Declare
   admin/manual comps as `admin_grants` in the same book.
5. **Review provider differences and pending writes.** Follow the
   [production cutover procedure](operations.md#booting-against-production-credentials).
   Run a [PSP import](psp-import.md) to review proposals before enabling full
   provider writes.

### Provider facts outrank the book

A book's subscriptions are the host's (`facts_authority` `host`). Re-sending
the book keeps converging them, so incremental legacy runs work before a
cutover. Once a PSP import reads a row, a person approves a proposal on it,
or a webhook converges it, the row is the provider's: a later book skips it
(`skipped`, reason `provider_authority`) and writes nothing for it, neither
state, schedule, mandate nor charges. A PSP import corrects the host's rows
with the provider's price, schedule terms and start ([PSP
import](psp-import.md#provider-facts-outrank-a-hosts-book)).

### Gotchas

- **Stale `past_due` is canceled, never charged.** Anything past the
  [dunning staleness window](dunning.md) (derived from the
  billing cycle; 10 days for monthly) gets the local no-charge cancel +
  downgrade. Missed billing periods are never back-billed.
- **`unverified` is healthy.** Evidence-starved rows park as `unverified` and keep
  projecting standing access; targeted checks and an operator refresh resolve them with
  real provider evidence. Absent evidence never costs a customer access.
- **Adoption alone never grants access.** An adopted-active row re-anchors
  its period end only; product-access windows come from real charges and the
  derive pass the import runs. A `past_due` row is never adopted: only a
  verified charge lifts a recorded decline.
- **Period anchoring**: `current_period_ends_at` = the declared
  `paid_through`; the period start is one billing cycle back, clamped to
  `started_at`. Provider-billed rails then renew on the *provider's*
  schedule — the renewal reaches you as a webhook/backfilled charge, not as
  something OpenRails initiates.
- **Dates stay faithful.** `canceled_at`/`ended_at` come from the declared
  evidence, not import wall-clock; grace = missed period end + the grace
  window; a re-import never regresses them.
- **Amounts are native units.** Every `amount` is a decimal string in the
  currency's native units, micros for USD (`"23000000"` is $23). There is no
  cents field.

### Verifying success

- The final import pass reports zero `blocked` (or every blocked reason is
  triaged), and an immediate re-post of the same book is all `skipped`.
- Payment counts and amounts match verified receipts in the legacy ledger.
  Review decline attempts separately from settled payments.
- Status distribution is sane at `as_of`: runway → `active`, mid-dunning →
  `past_due`, explicit cancels carry their true `cancel_type`, the rest
  `unverified`.
- A manual PSP import of each PSP raises no unexplained proposals.
- Review the `openrails intents` forecast before raising the provider write
  mode. Resolve any unexpected pending operations.
- The `unverified` cohort shrinks as targeted checks or an operator refresh
  obtain provider evidence.

### Runbook: migrating a legacy NMI book

For a book whose recurring billing NMI owns (NMI plans and subscriptions on
Customer Vault cards). OpenRails mirrors and monitors these memberships and
never takes them over: NMI keeps billing them. New purchases are billed by
OpenRails.

1. **Import.** Declare the book (customers, vault cards, subscriptions with
   their NMI schedules, and the charge history including declines) at a fixed
   `as_of`. Declare `recurring_transaction_id` on a card
   only when legacy evidence proves the recurring stored-card agreement. A paused NMI schedule is declared as
   `cancel: {kind: user_canceled, at: <pause time>}`: access runs to the paid
   date and OpenRails leaves the paused schedule in place. A card reference
   must resolve to a payment method in the book or one already stored for that
   customer and PSP. Supply the original sale when declaring a verified refund.
   Unlinked reversals can surface as `pull.reversal.unlinked` during
   reconciliation.
2. **Review findings.** Start the workers and request one [provider refresh](operations.md#provider-refresh).
   It accepts unproven schedules confirmed by NMI's record under the configured
   write posture. See [Accepted bills and provider schedules](#accepted-bills-and-provider-schedules).
   Triage every open finding: `pull.subscription.missing` (NMI
   schedules absent from the book), `pull.subscription.dead`,
   `pull.subscription.mismatch`, `pull.subscription.drift` (amount, plan,
   vault or pause changed at NMI), `pull.payment_method.mismatch` (vault card removed)
   and blocked import rows. Fix the book and re-import rather than editing rows.
3. **Arm the destructive switch** for the merchant
   ([operations.md](operations.md#the-kill-switch)).
   Until then a member's cancel of an NMI-owned membership is refused with
   `provider_cancel_held` and raises `life.provider_cancel.held`: OpenRails
   will not cancel locally while NMI would keep charging. Account deletion
   cancels locally and holds the NMI delete until armed.
4. **Operate.** Renewals arrive from NMI webhooks and from the refresh pull
   (each NMI transaction becomes one local payment); cancels delete the NMI
   schedule once; card updates repoint the schedule to the new vault; refunds
   go to NMI; a refund with `revoke_access` ends the refunded payment's
   access, and the NMI schedule keeps billing until the membership is canceled,
   which deletes it (refused with `provider_cancel_held` while disarmed).
   NMI subscription webhooks converge the named schedule. Request a
   [provider refresh](operations.md#provider-refresh) to catch up on missed
   events, or use [PSP import](psp-import.md) for an on-demand read and review
   its proposals.

   [Tier changes](merchant-operations.md#subscription-changes) update the
   existing NMI schedule in place. Link each target price of a named-plan book
   to a plan of the exact amount and cadence on the same PSP before offering
   changes. Use a `provider_prices` entry with `psp` and `plan_id`. If a
   charged upgrade stalls on the schedule update
   (`life.tier_change.provider_update_stuck`), add or correct that link and
   allow the operation to retry. The retry completes the schedule change
   without a second charge.

### Accepted bills and provider schedules

Supply evidence of the accepted bill behind each recurring provider receipt.
Use the provider's accepted schedule or finalized invoice to preserve the
amount and service period that applied at the time of the charge.

For live NMI and CCBill subscriptions, `accepted_schedule` states the accepted
terms: a stable `source_reference`, recurring `amount` in native currency units,
`currency` (the price's; another blocks the subscription), `first_due_at`, and
exactly one of `interval_hours` (whole days) or `interval_months` (calendar
months). The anchor preserves its accepted time of
day. A monthly January 31 anchor yields February's last day and then March 31.
Approved transactions also carry the provider-confirmed `next_billing_at`
boundary. An unchanged transaction receipt remains bound to its original cycle
on replay.

Without `accepted_schedule`, the import derives the terms from the book the way
enrollment freezes them: the declared price's whole-day cadence, a first due one
cycle before `paid_through`, and the book's approved charge of the price's
amount inside that cycle. A book that does not prove them still imports the
subscription, with a `life.import.schedule_unresolved` finding; its receipts stay
`pull.bill.unresolved` findings. A receipt no accepted bill can hold never fails
the import.

The provider's own recurring record then decides. The next scheduled provider
refresh reads each such NMI subscription's schedule: when it is active, its amount equals the
price's and its `day_frequency` equals the price's cadence, its terms become the
accepted schedule (source reference `nmi_subscription:<id>`), first due on NMI's
next charge date. The subscription's unresolved receipts are read again from
NMI by transaction id and placed by it: settled history before that date, the
cycle they pay after it. The finding resolves with NMI's terms as its evidence.
A paused schedule, another amount, a calendar-month or other frequency, or a
schedule NMI no longer lists keeps the finding open with NMI's record and why;
no schedule is guessed, and book rows never shape one. Declare
`accepted_schedule` and import again to settle such a row. A re-import keeps
an accepted schedule. CCBill's reads state no recurring terms yet, so a CCBill
subscription stays unresolved until its terms are declared.

For Stripe cash receipts, supply a complete `stripe_invoice` on each declared
transaction. It contains invoice identity, customer reference, finalized status,
billing reason, billed currency, totals and settlement amounts, all finalized
lines with their provider references and service periods, and the actual
invoice-payment/Charge allocations with amounts and payment timestamps. Amounts
use native currency units, as other import amounts do. The invoice customer
must match the subscription's declared payment-method customer reference.
If several receipts settle one invoice, repeat the identical full invoice
snapshot; a different subset or changed monetary facts is refused. There is a
limit of 1000 lines and 1000 payment allocations per invoice.

Hosts should fetch and verify this evidence before submitting the book. Import
performs no provider request inside its database transaction. The amount of a
cash receipt must match one actual allocation; credits, zero-due invoices and
out-of-band settlement do not authorize a fabricated cash receipt. Ordinary
provider observation can record those non-cash bills without any payment row.
