# Batch import: migrating legacy billing data into OpenRails

How to move an existing site's subscribers, payment history, and provider-side
subscription/vault records from a legacy billing system onto OpenRails
(embedded or standalone). The import surface here is the #737 DeclaredBilling
seam; the cutover doctrine it feeds into lives in
[operations.md](operations.md).

### The mental model

Import **seeds OpenRails' local truth** — customers, subscriptions, payment
history, vault *references* — while the **provider stays the source of truth
for live money state**. For provider-vaulted rails (NMI, CCBill, Stripe) the
provider owns the card vault and the recurring billing schedule: it keeps
charging on its own clock after your cutover. Importing brings OpenRails'
mirror up to date as of a declared horizon; from then on webhooks, the
scheduled [Provider Refresh](operations.md#provider-refresh-574-and-the-unknown-cohort-632664665),
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

**Client**: `client.ImportBilling(ctx, openrails.DeclaredBilling{...})` on the
merchant-bound Client, in every deployment (`rt.Client()` in process, or
`openrails.NewRemote`). Resolve public names once with `rt.ResolveMerchant`
and bind the Client to the captured UUID. **HTTP**: `POST /v1/import/billing`
with the identical JSON body — merchant from the authenticated credential,
gated on the owner-level `merchant:billing:import` permission. The HTTP body
cap (1 MiB) forces large books to batch.

The book (`DeclaredBilling`) carries four record kinds:

| Kind | Idempotency key | Notes |
|---|---|---|
| `customers` | customer UUID (upsert) | the host's stable subject id = `customers.id` |
| `payment_methods` | (rail, rail_customer_ref, rail_method_ref) | vault refs + card metadata (last four, type, expiry); `psp` attributes the vault entry |
| `subscriptions` | (rail, rail_subscription_id) | `source_id` (host's stable id) keys per-row results; `psp` binds the row to the PSP that owns it at the provider; optional `payment_method` ref, `cancel` / `dunning` evidence, raw `evidence` JSON stored verbatim on `gateway_response` |
| `transactions` | (rail, transaction_id) | successes **and** declines — the true attempt history; `amount_cents` is provider-wire cents, converted to ledger micros inside OpenRails |

**Attribution is required (or#893).** Every provider-bound row an import writes
carries the PSP it came from — the same `psp_id` a pull stamps — because the
same prune, rollback and uniqueness rules apply to an imported row as to a
pulled one. State it once for the whole book with `default_psp`, or per row
with `psp`; either form names a PSP by `{"id": "<uuid>"}` or by its manifest
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

For NMI `provider_dunning`, declare the saved method's
`recurring_transaction_id` when verified legacy evidence identifies the existing
recurring stored-card agreement. It is separate from `initial_transaction_id`:
an ordinary initial sale does not establish permission for recurring use.
Import stores the recurring reference once; replaying it is allowed, while a
different reference for the same merchant/account/vault method is a conflict.
Omitting it never synthesizes an agreement, and recovery charging remains
unavailable until a qualified reference exists. Importing this fact performs
no provider request and does not transfer ownership of NMI's billing schedule.

Each subscription fact takes one of two lanes:

- **Explicit cancel evidence** (`user_cancelled` / `chargeback` /
  `provider_terminated`) is settled history — written directly with faithful
  `cancel_type` and dates (cancel-with-runway keeps the paid-through end).
- **No cancel evidence** — seeded `unknown` and resolved by the decider at
  `as_of`: paid-through in the future → `active`; mid-dunning within the
  window → `past_due` with grace; roster-dead → cancelled; evidence-starved →
  stays parked `unknown` (cancellation-last-resort by construction).

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
`POST /v1/import/billing`.

### The migration playbook

The ordered phases, from a production host that migrated many years of legacy
billing data over this seam:

1. **Apply migrations.** Call `embed.ApplyMigrations` with a privileged pool;
   OpenRails owns and applies its billing baseline and managed River tables.
   Apply your application schemas separately, then validate the target shape
   before writing anything.
2. **Declare the merchant, PSPs, and catalog.** Upsert the merchant + its
   operator-declared PSP rows through `embed.Options.Merchant`
   (or manifest boot), then push the catalog — including *retired* historical
   price points, so every legacy subscription resolves a price. Resolve the
   `psps` row ids to stamp on imported rows. A PSP declared without
   credentials (`MerchantDeclaration.PSPs`) is an identity for attribution and price
   links only: it is never armed, so checkout discovery does not advertise it,
   a checkout naming it is refused as unroutable, and links to it are stored
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
and [Materialized backlog under mode=limited](operations.md#materialized-backlog-under-modelimited-366).

### Gotchas

- **Stale `past_due` is cancelled, never charged.** Anything past the
  [dunning staleness window](operations.md#dunning-359) (derived from the
  billing cycle; 14 days for monthly) gets the local no-charge cancel +
  downgrade. Missed billing periods are never back-billed.
- **`unknown` is healthy.** Evidence-starved rows park as `unknown` and keep
  projecting standing access; Provider Refresh and probes resolve them with
  real provider evidence. Absent evidence never costs a customer access.
- **Adoption alone never grants access.** An adopted-active row re-anchors
  its period end only; entitlement windows come from real charges and the
  derive pass the import runs.
- **Period anchoring**: `current_period_ends_at` = the declared
  `paid_through`; the period start is one billing cycle back, clamped to
  `started_at`. Provider-billed rails then renew on the *provider's*
  schedule — the renewal reaches you as a webhook/backfilled charge, not as
  something OpenRails initiates.
- **Dates stay faithful.** `cancelled_at`/`ended_at` come from the declared
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
  `unknown`.
- `openrails pull-provider report` shows a clean (or explained) diff against
  each provider's roster.
- `openrails intents` shows only the drains you expect before raising the
  mode; nothing fires at `full` that the forecast didn't show.
- The `unknown` cohort shrinks over subsequent Provider Refresh cycles as
  provider evidence arrives.

### Runbook: migrating a legacy NMI book

For a book whose recurring billing NMI owns (NMI plans and subscriptions on
Customer Vault cards). OpenRails mirrors these memberships and never charges
them; NMI keeps billing until you hand a membership over.

1. **Import.** Declare the book (customers, vault cards, subscriptions with
   `collection_policy` empty = NMI-owned, and the charge history including
   declines) at a fixed `as_of`. Declare `recurring_transaction_id` on a card
   only when legacy evidence proves the recurring stored-card agreement; the
   later engine takeover requires it. A paused NMI schedule is declared as
   `cancel: {kind: user_cancelled, at: <pause time>}`: access runs to the paid
   date and OpenRails never deletes the paused schedule. A card reference the
   book does not declare blocks its row. Declared refunds and chargebacks are
   not recorded as charges; unlinked ones surface as `pull.reversal.unlinked`.
   Re-post the same book: everything must report `skipped`. Imported members
   are entitled when the call returns.
2. **Review findings.** Boot, let one provider refresh pull run (advisory until
   armed), then triage every open finding: `pull.subscription.missing` (NMI
   schedules absent from the book), `pull.subscription.dead`,
   `pull.subscription.mismatch`, `pull.subscription.drift` (amount, plan,
   vault or pause changed at NMI), `pull.payment_method.mismatch` (vault card removed)
   and blocked import rows. Fix the book and re-import rather than editing rows.
3. **Arm the destructive switch** for the merchant
   ([operations.md](operations.md#arming-a-merchant-the-835-first-enforce-gate)).
   Until then a member's cancel of an NMI-owned membership is refused with
   `provider_cancel_held` and raises `life.provider_cancel.held`: OpenRails
   will not cancel locally while NMI would keep charging. Account deletion
   cancels locally and holds the NMI delete until armed.
4. **Operate.** Renewals arrive from NMI webhooks and from the refresh pull
   (each NMI transaction becomes one local payment); cancels delete the NMI
   schedule once; card updates repoint the schedule to the new vault; refunds
   go to NMI, and a refund with `revoke_access` also ends the membership and
   deletes its NMI schedule (refused with `provider_cancel_held` while
   disarmed). `Client.RefreshProviders` (`POST /v1/merchant/provider-refresh`)
   runs the merchant's provider refresh now, from embedded or remote hosts;
   otherwise it runs every four hours, and NMI's own subscription webhooks
   converge the schedule they name at once. Tier changes (same tier group,
   same billing cycle) modify the member's existing NMI schedule in place
   (Direct Post `update_subscription` sets its amount; its next billing date
   E is kept; no second schedule is ever created). An upgrade charges now the
   new price's share of the time left to E less the old price's unused credit,
   as a customer-initiated sale on the vault card, then updates the schedule,
   then switches access; if NMI refuses the update the operation retries and
   raises `life.tier_change.provider_update_stuck` until it lands (never a
   second charge). A downgrade charges nothing, updates the schedule amount
   now (NMI applies it from the next charge) and the mirrored renewal at E
   opens the lower tier. Another billing cycle answers
   `409 tier_change_cadence_unsupported`; drift checks expect these amounts.
5. **(Optional) staged takeover to OpenRails billing.** `TakeOverBilling`
   (one membership) or `TakeOverBillingBatch` (a capped batch) deletes the NMI
   schedule at least 24 hours before the paid period ends, verifies NMI's
   tombstone, then replaces the membership with an engine-owned successor on
   the same vault card whose first OpenRails charge is at that period end:
   no gap in access and no period billed twice. A takeover held by the switch
   or the destructive-volume breaker past that cutoff ends `not_executed` and
   NMI bills the period as before; run it again next cycle. `AbandonEngineTakeover`
   works until the NMI delete is sent. Start with a handful, watch one renewal
   cycle, then raise the batch.
