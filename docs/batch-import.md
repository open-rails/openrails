# Batch import: migrating legacy billing data into OpenRails

How to move an existing site's subscribers, payment history, and provider-side
subscription/vault records from a legacy billing system onto OpenRails
(embedded or standalone). The import surface is `Client.ImportBilling` with a
`billing.DeclaredBilling` document; the cutover doctrine it feeds into lives in
[operations.md](operations.md).

### The mental model

Import **seeds OpenRails' local truth** — customers, subscriptions, payment
history, vault *references* — while the **provider stays the source of truth
for live money state**. For provider-vaulted rails (NMI, CCBill, Stripe) the
provider owns the card vault and the recurring billing schedule: it keeps
charging on its own clock after your cutover. Importing brings OpenRails'
mirror up to date as of a declared horizon; from then on webhooks, the
scheduled [Provider Refresh](operations.md#provider-refresh-and-the-unknown-cohort),
and manual `pull-provider` runs converge the mirror against provider truth.

You hand over **facts, not classifications**: who, which price, which rail,
the provider's subscription id, paid-through evidence, explicit cancel
evidence, dunning state, and the raw charge history. OpenRails classifies
every row through the same decider pipeline its webhook/pull planes use,
evaluated at the declared `as_of` horizon — so the same book classifies
identically whenever the import runs.

What import does **not** do:

- **No provider I/O.** The import never calls a provider. The one
  provider-affecting output: a declared cancel whose legacy schedule was not
  confirmed dead (`schedule_live`) enqueues a durable rail-delete intent,
  executed later by the intent executor under the
  [mode gate](operations.md#operating-modes-the-safety-levers).
- **No card-data movement.** OpenRails never holds card data. Declared
  payment methods are *references* into the provider's vault
  (`rail_customer_ref` / `rail_method_ref`), meaningful only within the PSP
  account that created them. You cannot import your way onto a different
  PSP — see
  [PSP binding and credential rotation](operations.md#psp-binding-and-credential-rotation);
  between accounts, cards can only be re-captured when customers next pay.

### The import surface

**Client**: `client.ImportBilling(ctx, billing.DeclaredBilling{...})` on the
merchant's handle, in every deployment (an `openrails.New` engine's
`Merchant(id)` in process, or `openrails.NewRemoteMerchant`). Resolve a public
name once and keep the merchant's UUID. **HTTP**: `POST /v1/merchant/billing-import`
with the identical JSON body — merchant from the authenticated credential,
mounted with `RouteGroups.MerchantConfig` and checked like the merchant's
other configuration. The HTTP body cap (1 MiB) forces large books to
batch.

The book (`DeclaredBilling`) carries five record kinds, plus admin grants:

| Kind | Idempotency key | Notes |
|---|---|---|
| `customers` | customer UUID (upsert) | the host's stable subject id = `customers.id`; an optional `email` |
| `payment_methods` | (rail, rail_customer_ref, rail_method_ref) | vault refs + the `card` (`brand`, `last4`, `exp_month`, `exp_year`); `psp` attributes the vault entry |
| `subscriptions` | (rail, rail_subscription_id) | `source_id` (host's stable id) keys per-row results; `psp` binds the row to the PSP that owns it at the provider; optional `payment_method` ref, `cancel` / `dunning` evidence, raw `evidence` JSON stored verbatim on `gateway_response` |
| `transactions` | (rail, transaction_id) | successes **and** declines — the true attempt history; `amount` is a decimal string in the currency's native units (micros for USD) |
| `purchases` | `source_id` and (psp, transaction_id) | settled one-off sales: frozen lines, the rail payment, the access bought |

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

`as_of` (RFC3339) is required: the instant the legacy data was true. All
classification evaluates against it, never wall-clock.
`subscriptions_exhaustive: true` is an absence proof valid only when one call
covers the merchant's entire book — it MUST be false for batched imports.

Every live NMI schedule is dunned by OpenRails (NMI never retries a declined
renewal), whatever `collection_policy` the book declares. Its recurring
stored-card anchor is the method's `recurring_transaction_id` when declared,
otherwise the schedule's first approved sale in the book's transactions: the
customer's recurring signup. A schedule with neither stays provider-collected
and raises `life.import.no_recurring_anchor`. Import stores the anchor once;
replaying it is allowed, while a different reference for the same
merchant/account/vault method is a conflict. Importing this fact performs
no provider request and does not transfer ownership of NMI's billing schedule.

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

**Entitlements are derived, not imported.** The book has no entitlement
record kind. Subscription access follows from subscription standing: each
import runs the derive pass for the book's customers before it returns, so
imported members are entitled immediately (a replay re-derives). Operator/manual
comps — access with no payment behind it — ride the same book as
`admin_grants` (grant-ledger facts, idempotent by `source_id`); OpenRails
derives the windows. `Client.ImportBilling` posts the same book over
`POST /v1/merchant/billing-import`.

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

### Charges before the accepted schedule

A recurring charge pays the cycle it starts. An approved NMI or CCBill
charge made before the subscription's first accepted schedule cycle, and any
approved charge of a canceled subscription that has no accepted schedule, is
settled history: a complete provider-owned order whose line, on the
subscription's price, carries the provider's amount, covering one cadence
from the charge, with the charge as its payment. It grants no access,
renews nothing and infers no schedule. A decline there moves no money and is
not recorded. These orders record in one short transaction per subscription
after the book commits. A live subscription with no accepted schedule keeps
its receipts unresolved until one is accepted (see
[Accepted bills and provider schedules](#accepted-bills-and-provider-schedules)).

A declared NMI or CCBill charge names the provider's own transaction id,
which is digits only. Any other id (a host's synthetic id) is evidence: a
`life.import.transaction_unverified` finding, no payment. An approved charge
with a real id but no amount is kept as an attribution of that provider
transaction to its subscription (`provider_charge_attributions`). OpenRails
reads each one from NMI's Query API once, by transaction id, and records the
provider's exact amount and time; a provider pull that reads the id binds it
to the same subscription. An id NMI does not have, or reports declined or
voided, records no payment and becomes a `life.import.transaction_unverified`
finding. CCBill attributions are kept until a DataLink reader resolves them.
Every path records a provider transaction once, by (psp, transaction_id).

### The migration playbook

The ordered phases, from a production host that migrated many years of legacy
billing data over this seam:

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
5. **Access is derived by the import itself** for the book's customers; no
   merchant-wide `ConvergeMerchant` is required.
6. **Boot with `PROVIDER_WRITE_MODE=limited` — set before first start.**
   Imported stale `past_due` rows are immediately "due"; a full-behavior boot
   would start charging them within hours.
7. **Let the first dunning cycle materialize the backlog**, then review
   `openrails intents` — the
   [drain forecast](operations.md#inspecting-the-ledger) is the dry-run view
   of your cutover ("N execute under limited, M require full").
8. **Verify against the provider** with `openrails pull-provider` /
   `pull-provider report`, then raise to `PROVIDER_WRITE_MODE=full` — it
   drains exactly what the forecast showed.

Steps 6–8 in depth:
[Cutover: booting against production credentials](operations.md#cutover-booting-against-production-credentials)
and [Materialized backlog under mode=limited](operations.md#materialized-backlog-under-modelimited).

### Gotchas

- **Stale `past_due` is canceled, never charged.** Anything past the
  [dunning staleness window](operations.md#dunning) (derived from the
  billing cycle; 14 days for monthly) gets the local no-charge cancel +
  downgrade. Missed billing periods are never back-billed.
- **`unverified` is healthy.** Evidence-starved rows park as `unverified` and keep
  projecting standing access; Provider Refresh and probes resolve them with
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
- **Money is micros locally, cents on the wire.** `amount_cents: 2300` lands
  as `23,000,000` micros. Don't pre-convert.

### Verifying success

- The final import pass reports zero `blocked` (or every blocked reason is
  triaged), and an immediate re-post of the same book is all `skipped`.
- Payment counts and amounts match the legacy ledger; declines are present
  (they are attempt history, not noise).
- Status distribution is sane at `as_of`: runway → `active`, mid-dunning →
  `past_due`, explicit cancels carry their true `cancel_type`, the rest
  `unverified`.
- `openrails pull-provider report` shows a clean (or explained) diff against
  each provider's roster.
- `openrails intents` shows only the drains you expect before raising the
  mode; nothing fires at `full` that the forecast didn't show.
- The `unverified` cohort shrinks over subsequent Provider Refresh cycles as
  provider evidence arrives.

### Runbook: migrating a legacy NMI book

For a book whose recurring billing NMI owns (NMI plans and subscriptions on
Customer Vault cards). OpenRails mirrors and monitors these memberships and
never takes them over: NMI keeps billing them. New purchases are billed by
OpenRails.

1. **Import.** Declare the book (customers, vault cards, subscriptions with
   `collection_policy` empty = NMI-owned, and the charge history including
   declines) at a fixed `as_of`. Declare `recurring_transaction_id` on a card
   only when legacy evidence proves the recurring stored-card agreement. A paused NMI schedule is declared as
   `cancel: {kind: user_canceled, at: <pause time>}`: access runs to the paid
   date and OpenRails never deletes the paused schedule. A card reference the
   book does not declare blocks its row. Declared refunds and chargebacks are
   not recorded as charges; unlinked ones surface as `pull.reversal.unlinked`.
   Re-post the same book: everything must report `skipped`. Imported members
   are entitled when the call returns.
2. **Review findings.** Boot, let one provider refresh pull run (advisory until
   armed; it accepts each unproven schedule NMI's record confirms, see
   [Accepted bills and provider schedules](#accepted-bills-and-provider-schedules)),
   then triage every open finding: `pull.subscription.missing` (NMI
   schedules absent from the book), `pull.subscription.dead`,
   `pull.subscription.mismatch`, `pull.subscription.drift` (amount, plan,
   vault or pause changed at NMI), `pull.payment_method.mismatch` (vault card removed)
   and blocked import rows. Fix the book and re-import rather than editing rows.
3. **Arm the destructive switch** for the merchant
   ([operations.md](operations.md#arming-a-merchant-the-first-enforce-gate)).
   Until then a member's cancel of an NMI-owned membership is refused with
   `provider_cancel_held` and raises `life.provider_cancel.held`: OpenRails
   will not cancel locally while NMI would keep charging. Account deletion
   cancels locally and holds the NMI delete until armed.
4. **Operate.** Renewals arrive from NMI webhooks and from the refresh pull
   (each NMI transaction becomes one local payment); cancels delete the NMI
   schedule once; card updates repoint the schedule to the new vault; refunds
   go to NMI, and a refund with `revoke_access` also ends the membership and
   deletes its NMI schedule (refused with `provider_cancel_held` while
   disarmed). `Client.RefreshPSPs` (`POST /v1/merchant/psps/refresh`)
   runs the merchant's provider refresh now, from embedded or remote hosts;
   otherwise it runs every two hours, and NMI's own subscription webhooks
   converge the schedule they name at once. Tier changes (same tier group,
   same billing cycle) modify the member's existing NMI schedule in place
   (Direct Post `update_subscription`; its next billing date E is kept; no
   second schedule is ever created). A custom-amount schedule takes the new
   `plan_amount`. NMI ignores `plan_amount` on a schedule attached to a named
   plan, so such a schedule switches `plan_id` to the target price's linked NMI
   plan, which must exist on the account with the target's exact amount and
   cycle; otherwise the change is refused before any charge with
   `409 subscription_change_requires_linked_plan`. Link each tier price of a named-plan
   book to its NMI plan (price `psps: {<psp>: {plan_id: ...}}`) before offering tier
   changes. An upgrade charges now the
   new price's share of the time left to E less the old price's unused credit,
   as a customer-initiated sale on the vault card, then updates the schedule,
   then switches access; if NMI refuses the update the operation retries and
   raises `life.tier_change.provider_update_stuck` until it lands (never a
   second charge). A downgrade charges nothing, updates the schedule amount
   now (NMI applies it from the next charge) and the mirrored renewal at E
   opens the lower tier. Another billing cycle answers
   `409 subscription_change_cadence_unsupported`; drift checks expect these amounts.
   A stuck update (`life.tier_change.provider_update_stuck`, e.g. an upgrade
   admitted by v0.178.0 on a named-plan schedule) re-reads the schedule on
   every retry: link the target price to an NMI plan of its amount and cycle
   and the operation switches the plan and completes without charging again.

### Accepted bills and provider schedules

Imports now require the accepted obligation behind each provider receipt. They
never construct historical orders from a transaction amount and today's price.
This is a forward import contract; old prelaunch data needs an explicit reset or
a book that carries these verified facts.

For live NMI and CCBill subscriptions, `accepted_schedule` states the accepted
terms: a stable `source_reference`, recurring `amount` in native currency units,
`currency`, `first_due_at`, and exactly one of `interval_hours` (whole days) or
`interval_months` (calendar months). The anchor preserves its accepted time of
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

The provider's own recurring record then decides. The next provider pull (the
scheduled refresh, `Client.RefreshPSPs` or `openrails pull-provider`) reads each
such NMI subscription's schedule: when it is active, its amount equals the
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
