# Catalog

What a merchant sells: products, prices, meters, rate cards and usage limits, and how recurring subscribers move to other prices and products ([price migrations](#moving-subscribers-to-other-prices-and-products)). It changes by documents (`catalog.Application`) an operator applies and by edits from the console, the catalog routes and `Client` methods; starting OpenRails never applies one (`internal/engine/engine.go`). Guides: [catalog](../client.md#catalog), [catalog export](../client.md#catalog).

## Concepts

- **Product**: a merchant-set `key` (unique in the merchant), display fields, entitlements, tier group and rank, ownership rule, credit grant, rate cards and limit capacities. Products are edited in place; `products.revision`, set only by OpenRails, advances on every change to the product, its keys or its rate cards, never on a change to its prices.
- **Price**: an immutable financial version of a price key, id `product:key:vN` (`billing.PriceID`). The key is merchant-set and unique within its product; changing a price's terms writes the next version (`v2`, `v3`, …) and archives the current one, whose subscribers stay on it until a price migration moves them. `Price.Revision` is the key's concurrency counter: it advances on any change to the key's versions or their PSP links.
- **Version and revision**: a *version* is a new immutable row (only prices have versions); a *revision* counts edits to something mutable (products and price keys).
- **Provider price**: mirrors an NMI plan, Stripe price or CCBill FlexForm; read,
  never written. Its **binding** (`price_psp_bindings`) names the object.
- **Manager**: who last set a field, `apply` (a document) or `edit`
  (`catalog_field_owners`).
- **Catalog assistant**: a model that answers questions and drafts changes.
- **Price migration** (`price_migrations`, id `pmig_`): moves a price's subscribers to another recurring price, possibly of another product; each subscriber's **move** is a `scheduled_changes` row.

## How it works

**One write path.** Every edit and application is a catalog command
(`catalogCommand`, `internal/service/catalog_admission.go`):

1. Plan under the catalog lock, writing nothing (`catalogReadLocked`,
   `readCatalogState`): list the existing objects it changes.
2. Admit them, if any, as one destructive command (`destructive.CatalogChange`).
3. Commit in one merchant transaction (`catalogMutation`): lock the revision
   (`LockCatalogRevision`), record edit ownership (`recordCatalogEdit`) and the
   change's history (`recordCatalogHistory`), refuse a new recurring benefit
   overlap (`catalog_benefit_overlap`), finish the admission.
4. If the catalog moved since planning, or a changed object was not admitted
   (`errCatalogSnapshotMoved`), release the admission as no-effect and plan
   again, up to three times, then answer `409 catalog_revision_conflict`.

**History.** The commit compares the catalog before and after (`readCatalogState`) and appends one `change_log` entry per changed object (`catalogHistory`, `internal/service/catalog_history.go`) through `history.Record`, in the same transaction, so an entry commits and rolls back with its change. A product, price key, meter or limit gets `create`, `update`, `archive`, `unarchive` or `delete` with each changed field before and after; a product's keys get `key_add`, `key_remove` and `key_rename` entries of their own (a rename is what `replaceEntitlements` reported). Every entry but a delete carries the object's revision after the change: a product's, which a price change never moves, or a price key's. The actor is the route gate's invoker (`staff:<id>`) or `system`; the source is `console` for a signed-in person, `api`, `catalog_apply` for a document (with its `application_id`) or `cli` for the operator's apply.

The `catalog_authored_*` triggers advance `merchants.catalog_revision` on every
row written, unless `openrails.catalog_batch_merchant_id` names the merchant (a
document, which advances it once with `AdvanceCatalogRevision`).

**Prices are versions.** `writeCatalogPrice` archives the key's current row,
creates or revives the row whose id derives from the terms (`RevivePrice`), and
logs the move in `price_key_movements`. `immutable_price_terms` lets only
`archived`, `updated_at`, `deleted_at` and `sell_via` change, so an order keeps
its terms. Product keys are valid-time rows in `product_entitlements`; a change
emits `product.entitlements_changed`. `DeleteProduct` soft-deletes only what
nothing refers to (`ProductHoldsReferences`, `PricesHoldReferences`), else
`409 product_in_use`.

**Applying a document** (`internal/service/catalog_application.go`):

1. Validate (1 MiB, 2000 items), normalize through the JSON wire form, and hash
   (`CanonicalDigest`, plus the Solana PSPs in `treasuryDigest`).
2. Prepare (`prepareCatalogApplication`): under the lock, return the stored
   receipt if the hash applied before (`catalogApplicationGate`); then, with no
   transaction open, read provider terms, find or publish Solana plans, collect warnings.
3. Commit: plan against `catalog_field_owners` (`planCatalogApplication`), apply
   products, prices, meters, rate cards and limits, prune, take ownership of the
   named fields (`recordCatalogApply`). Only a document that skipped and warned
   nothing is stored in `catalog_applications`; any other runs again when
   re-applied. A skip also raises `life.catalog.conflicts`, which the next apply
   that skips nothing resolves.

An object is skipped whole when a field it names has a live value an edit set
differently (`fieldConflicts`), unless `Force`. `prune: true` deletes an omitted
product or price only documents set and nothing refers to, archives it when
something does, and releases one an edit touched to the edit
(`pruneCatalogObjects`); meters and limits are never pruned. The stored receipt
keeps ids, revisions and counts, not the change list.

**Provider prices.** NMI and Stripe terms come from `provider_catalog_objects`
via `providerobjects.Observer`, read by id when absent (`readProviderPriceTerms`,
`observedTerms`); CCBill declares its terms. The row is `provider_owned` with a
`provider_snapshot`, its id salted with the provider object (`providerPriceID`).
A provider that does not answer refuses a new provider price and leaves an
existing one on cached terms, with a warning. Stripe product and price events queue
`ProviderObjectObserveWorker`, which syncs the unmapped-price finding.

**Solana plans.** A recurring USD price of ours that sells on Solana gets an
on-chain plan (`publishSolanaPlan`) whose id derives from price, mint and
treasury (`solanaPlanID`); `verifySolanaCatalogReference` reads it back. Under
`provider_write_mode` `limited` or `readonly` only an existing plan is attached.

**Catalog assistant** (`internal/modules/copilot`). `Service.Ask` runs a capped
tool loop (`askMaxToolCalls` 6, `askMaxTokens` 2048) over read-only tools
(`list_catalog`, `get_price`, `price_history`, `list_reprice_batches`); drafting
adds `draft_price_change` and `draft_catalog_diff`, which return the requests a
person would send. The model sees aggregate catalog data and subscriber counts.

`internal/catalogrules` validates charge models and rates usage in integer micros ([usage billing](usage-billing.md)); `StripeCatalogService` (webhook endpoints) is [payment providers](payment-providers.md).

Tables: `products`, `product_entitlements`, `prices`, `price_keys`,
`price_key_movements`, `price_psp_bindings`, `catalog_meters`,
`catalog_rate_cards`, `catalog_limits`, `catalog_product_limits`,
`catalog_field_owners`, `catalog_applications`, `catalog_restore_receipts`;
read-only here, `provider_catalog_objects`.

### Moving subscribers to other prices and products

A price migration (`PriceMigrationService`, `internal/modules/subscriptions/price_migration.go`) moves the subscribers of one price (`from_price_id`), or of every version of a price key, archived ones included, but the target (`product_key` and `price_key`, stored as `from_product_id` and `from_price_key`), to `to_price_id`. The target must be recurring and live (`400 invalid_param`, `422 price_change_target_archived`) and may belong to another product; a `from_price_id` in another currency is refused (`422 price_change_currency_mismatch`). Nothing is prorated, charged early or re-anchored: the first renewal on or after `effective_at` (now when omitted) bills the target, as [subscriptions](subscriptions.md) applies it.

**Preview and create.** Both read the cohort (`ListMigratableSubscriptionsByPriceIDs`: every `active`, `past_due` or `awaiting_method` subscription on a source price) and `classify` it without writing; `PreviewPriceMigration` answers the counts by rail and each subscription's disposition (by key, with no `to_price_id`, only the count). `CreatePriceMigration` admits the command, writes the `price_migrations` row, then gives each subscriber, in its own transaction under its lock, its move (`ScheduleChange`), notice and push, or a terminal `blocked` row. With `archive_source` (default true) it archives the sources so they stop selling.

**Who moves.** A subscription is skipped, with no move, when it already has a scheduled change, is in another currency, or would pay more inside the notice window unacknowledged; or, while recording, when it left the source price or an accepted renewal owns its terms (`rebill_terms_committed`). It is blocked, by what bills it:

- OpenRails charges (engine): a calendar price on either side.
- Stripe Billing: a source or target with no Stripe price (`target_price_missing_rail_config`).
- An NMI schedule: another billing cycle (`nmi_interval_mismatch`), an amount finer than cents (`nmi_amount_not_cent_representable`), or no declared account reaching it (`rail_requires_user_action`).
- CCBill and Solana: always (`rail_requires_user_action`); only the customer's own redirect or signature changes them.

`fallback_policy` records the operator's wish for blocked subscriptions: `keep_grandfathered` (default) leaves them on the old price; `cancel_at_period_end` is recorded only, OpenRails cancels nothing.

**Notice.** `costRises` compares amount × the seats the renewal bills, over the cadence, exactly; a month counts at its least favourable length and a price without a cadence as a rise. A rise whose `effective_at` falls inside `reprice_notice_window_days` (default 30, `DefaultPriceIncreaseNoticeDays`; 0 disables it) is skipped unless `acknowledge_short_notice`, which the move keeps as `acknowledged_short_notice`. Every move queues the customer's price-change notice in its transaction: `subscription_reprice_scheduled`, or `subscription_plan_change_scheduled` naming `to_product_id` when the product changes, with old and new amounts and `effective_at`.

**Admission.** `admitMigration` admits one `destructive.CatalogChange` before anything is written: a unit per moved or blocked subscription and per source it archives, within the destructive ceilings (a breach is a hold, `429 rate_limit_exceeded`). While the command is open, the same request from the same actor answers the migration it prepared. The command stays open, its units reserved, until its last scheduled move resolves (`SettlePriceMigration`).

**Provider-run subscriptions.** A Stripe or NMI move carries a `price_migration_push` operation (`QueuePush`), due at once, or at the period end when `effective_at` lies beyond it, so no rebill flips early. `PushMove` plants a Stripe subscription schedule that switches at period end with the same seats (`ScheduleSubscriptionPriceChange`), or sets the NMI schedule's amount with `update_subscription`, keeping `plan_payments`, and reads it back (`PushPlanAmount`). A failed push retries on the backoff; one the subscription's records cannot support (no provider reference, period end or Stripe price) blocks the move (`rail_push_failed`); one that no longer applies (subscription ended or moved, target gone or archived) skips it. CCBill and Solana are never pushed.

**Cancel and list.** `CancelPriceMigration` sets `canceled_at` and cancels each still-scheduled move under its subscription's lock, with its waiting push; resolved moves stay, archived sources stay archived, and it spends no unit. A move an accepted renewal owns refuses it (`409 rebill_terms_committed`) until that renewal resolves. A provider-run Stripe subscription whose schedule may be planted is listed in `rail_release_required`: release that schedule in Stripe, or the price still changes. A staff change back to an engine subscription's current price and seats takes that one out (`ReplaceScheduledChange`). `ListPriceMigrations` pages newest first, by `ids` or by `product_key` and `price_key`; `matched` and creation skips are stored, the other counts are read from the moves.

**Retiring prices and products.** Archiving stops sales and keeps subscribers; a migration moves them. A price or product a migration or move refers to is never deleted (`PricesHoldReferences`, `ProductHoldsReferences`): `DeleteProduct` answers `409 product_in_use` and `prune: true` archives it. The catalog assistant drafts only same-product migrations (`cross_product` refusal).

Tables: `price_migrations`; moves are `scheduled_changes` ([subscriptions](subscriptions.md)).

## Interfaces

- Go (`*Client`): the product, price (`CreateProviderPrice`, `ListPriceHistory`
  included), meter and limit methods and `ApplyCatalog`;
  `CatalogApplicationReceipt.String`, `catalog.ReadFile`, `billing.BuyerPrices`.
  Reads are cached per handle (`ReadCatalog`, `ReadMeters`).
- HTTP: `POST /v1/catalog/apply`, `/v1/catalog/products`, `/v1/catalog/prices`,
  `POST /v1/catalog/provider-prices`, `/v1/catalog/meters`,
  `/v1/catalog/limits`, console-only `POST /v1/console/catalog/ask`; reads need
  `merchant:catalog:read`, writes `merchant:catalog:manage` (or `root:` twins).
  No public route reads the catalog.
- Price migrations: `PreviewPriceMigration`, `CreatePriceMigration`, `ListPriceMigrations`, `GetPriceMigration`, `CancelPriceMigration` on `POST /v1/catalog/price-migrations/preview`, `POST /v1/catalog/price-migrations`, `GET /v1/catalog/price-migrations`, `GET /v1/catalog/price-migrations/{id}`, `POST /v1/catalog/price-migrations/{id}/cancel`, with the catalog permissions (preview is a read); events `subscription_reprice_scheduled`, `subscription_plan_change_scheduled` ([events](../client.md#events)); configuration `reprice_notice_window_days`.
- CLI: `openrails catalog apply FILE --merchant NAME [--force-conflicts]`,
  `openrails catalog export|import`, `openrails dump-merchant-catalog`.
- Configuration: `llm.api_key`, `llm.catalog_copilot_enabled`,
  `llm.catalog_drafting_enabled`.

## Guarantees

### 1. Exactly-once provider calls

Catalog edits and documents write nothing to NMI, Stripe or CCBill. Their one provider write, a Solana plan, is published before the price row commits: in a document's preparation, or in `createPriceLinks` for one price (inside the inline product's transaction when `ProductData` is used). The address derives from price, mint and treasury, so a publish whose commit failed is reattached, not repeated.

A price migration writes to Stripe and NMI only through its `price_migration_push` operations, each a `provider_intents` row committed with its move and keyed by it. Both Stripe writes carry the move's key, so a retry after a lost answer replays them; the NMI amount push is idempotent and read back.

Checks:
- `internal/service/catalog_*.go` and `providerobjects` only read NMI and Stripe
  (`ObserveByID` with `providerobjects.ViaRead`).
- `PublishPlan`'s re-publish guard returns a matching plan and refuses others.
- With `RemoteWritesDisabled`, `publishSolanaPlan` only reads the chain.
- `QueuePush` enqueues in the move's transaction with key `price_migration_push:` and the move's id.
- OR-J19-S02: a failed NMI push retries without a second migration or push; OR-J19-S07: a lost Stripe schedule create replays its key.

### 2. Agreement with providers

A provider price keeps the terms observed in `provider_catalog_objects`, where
pulls, webhooks and by-id reads keep the latest observation. New terms become a
new version only at the next apply. A lost Stripe event is caught by the next
event or a pull ([imports](imports.md)); `GET /v1/catalog/prices/{id}?verify=true`
re-reads the provider (`VerifyPublicPriceSync`).

A migration's NMI move applies only after the schedule reads back the target amount (`PushPlanAmount`); a Stripe move waits for Stripe to bill the target ([subscriptions](subscriptions.md)).

Checks:
- `observedTerms` keeps a deleted object's last terms and refuses one with no
  fixed amount.
- OR-J38-S05 and OR-J15-S08 cover the Stripe observation path.
- OR-J19-S07: after the push, the Stripe move stays scheduled until Stripe switches.

### 3. Exact money

`parseAmount` reads `9.99 USD` into the registry's integer scale, refusing extra
decimals and int64 overflow; `currency.ForPrice` refuses a stablecoin price. NMI
and Stripe decimals convert through `moneyutil.DecimalToRailMinor` and
`RailMinorToNative`; a Solana plan amount uses the mint's on-chain decimals, and
`PublishPlan` refuses any other scale. No catalog path converts FX.

A migration never changes a subscriber's currency, pushes an NMI amount only when it is whole cents (`NativeToRailMinorExact`), and compares costs for the notice in `big.Int` (`costRises`).

Checks:
- OR-J05-S01 and OR-J05-S02 pin exact amounts and refuse bad ones.
- OR-J38-S42 pins NMI decimals per currency.
- `immutable_price_terms` refuses an update to `amount` or `currency`.
- OR-J19-S02 pins the pushed NMI amount `"20.00"`; OR-J19-S06 the notice per unit of time and seats.

### 4. Access control and tenant isolation

Catalog routes are `Group: CatalogAdmin, Auth: AuthMerchant`; their level maps
to the read or manage pair (`internal/http/routes/permissions.go`). The service
takes the merchant from the context (`merchant.Require`) and writes in
`MerchantTx`; tables key on `(merchant_id, …)`. The CLI requires `--merchant`.

The migration routes are catalog routes too, declared in `internal/http/routes/subscriptions.go`; `PriceMigrationService` takes the merchant the same way.

Checks:
- OR-J01-S05 and OR-J01-S07 send catalog calls across merchants.
- OR-J04-S02 keeps cached catalog reads bound to their merchant.
- Every query in `products.sql` and `prices.sql` names `merchant_id`.
- Every query in `price_migrations.sql`, and `ListMigratableSubscriptionsByPriceIDs`, names `merchant_id`; OR-J01-S02 sends each migration route without a credential.

### 5. Contracts match the code

The `catalog` package, `billing` catalog types and `Client` methods are in
`api/go.txt`, the routes in `api/openapi.json`, the tables in `api/schema.txt`;
documents carry `schema_version` (`catalog.ApplicationSchemaVersion`). The
in-process `Client` reaches the same handlers (`internal/http/inprocess`), and
`ApplyOperatorCatalog` normalizes its document through the JSON wire form.

Checks:
- `go run ./scripts/contracts` passes with no catalog line removed.
- A replay answers `replayed: true` with the first `application_id` (OR-J05-S03).
- Each migration route has one generated `Client` method (`client_gen.go`).

### 6. Data lifecycle

`products`, `product_entitlements`, `prices`, `price_key_movements`, `catalog_applications`, `catalog_restore_receipts` and `price_migrations` are permanent, the rest state; none is partitioned. Products and prices soft-delete.

`change_log`, the history of every catalog and configuration change, is permanent and append-only (`immutable_change_log`): no update or delete, a merchant purge keeps it, and the merchant archive moves it. It is not in the billing restore guard: a destination records its own configuration's history as soon as that configuration loads, and a restore adds the archive's entries to it, once per merchant (the restore receipt makes a replay a no-op).

Checks:
- `internal/retention/tables.go` lists every catalog table (by hand: nothing
  checks it against the schema).
- `no_product_delete`, `immutable_price_terms` and
  `immutable_catalog_application_receipt` triggers exist.

### 7. Work scales with activity

No catalog job is periodic (`internal/river/periodic.go`); the background work is a `ProviderObjectObserveWorker` job per Stripe product or price an event names, unique per object and debounced, so a burst is one read, and a `price_migration_push` operation per Stripe or NMI move, woken at its `next_attempt_at`; engine moves ride the renewal. But each command reads the merchant's whole catalog up to four times under the merchant's lock, and a migration reads and moves its whole cohort in one request.

Checks:
- `EnqueueProviderObjectObserve` is called only from
  `internal/modules/webhooks/stripe_catalog.go`.
- `catalogLocked` reads `readCatalogState` before, after, and again for
  `recordCatalogEdit`.
- `move` queues the push at the period end when `effective_at` lies beyond it (`deferredPushRequired`).

### 8. Abuse protection

Catalog routes are staff routes, outside the customer limits
([limits](../standalone.md#abuse-limits)). Bodies are capped at 1 MiB
(`middleware.DefaultMaxBodyBytes`), documents at 2000 items. The assistant
spends model tokens: the `catalog-ask` bucket (10 a minute) and a capped loop.

Checks:
- `ClassifyBucket` maps `POST /v1/console/catalog/ask` to `catalog-ask`.
- `Application.Validate` and `GuardJSON` enforce the document bounds.

### 9. Destructive-action safeguards

Changing, archiving or removing an existing product, price, meter or limit
spends a unit per object (`substantiveCatalogTargets`); creating one, or editing
a display name or description, spends none. A CLI file spends the merchant's
autonomous budget (`AdmitAutonomous`, `operator_catalog`). A held command never
starts; a document is admitted after preparation, so a refused one can leave an
unlinked Solana plan, never a catalog change. Deletes are soft and refused for
anything referenced. `catalog_field_owners.actor` records who set each field.

A migration is one `destructive.CatalogChange` whose units are its moved or blocked subscribers and archived sources, reserved until its last scheduled move resolves; canceling spends nothing, and `cancel_at_period_end` cancels nothing.

Checks:
- `catalogLocked` fails a changed object with no admission.
- `ApplyOperatorCatalog` passes an origin, so `catalogCommandOnce` calls
  `AdmitAutonomous`.
- `pruneCatalogObjects` deletes only through `deleteProduct` and
  `deletePriceKey`, which check references first.
- `admitMigration` counts a target per non-skipped subscription and unarchived source; `SettlePriceMigration` finishes the command only when no move is `scheduled` (OR-J19-S05).

## Tests

Journeys:
- OR-J05-S01 to OR-J05-S05 (`TestPublicCatalog`): exact amounts, refused terms,
  replay with ownership and revision conflicts, archive keeps history, accepted
  terms survive edits.
- OR-J08-S01, OR-J08-S03: carts price from the catalog, refuse an archived price.
- OR-J38-S01, S02, S04, S05, S33, S34, S42: provider-price mapping, retired
  Stripe prices, read-only pulls, Map needing catalog manage, NMI currencies.
- OR-J15-S08, OR-J15-S12, OR-J29-S05, OR-J01-S05, OR-J01-S07, OR-J04-S02.
- OR-J19-S01 to OR-J19-S07 (`TestPriceMigrationContracts`): only eligible subscribers move, at the paid boundary; a failed NMI push retries; cancel leaves applied and ineligible ones; an admitted renewal keeps its terms; a move that no longer applies is skipped and settles; notice follows cost per unit of time and a one-time target is refused; a Stripe push keeps seats and replays a lost create.
- OR-J14-S07: a calendar subscription previews as blocked. OR-J01-S02 covers the migration routes.
- OR-J43-S01, S02, S05, S06, S07 (`TestConfigurationHistory`): an edit and its entry commit and roll back together; a document records each changed object once; the list filters by object and time; a product's revision moves with its keys, not its prices, and its entries carry it; a stale `expected_revision` is refused.

Other tests: `TestBillingIntervalUnits`; rate cards in
`internal/modules/money/rating_test.go`, OR-J24-S09, OR-J24-S10; Solana plans
are published only as setup (`CreatePrice` in `TestSolanaPlanChanges`).

Gaps:
- Nothing covers `prune: true`, entitlement replacements or a warned document.
- Nothing tests the catalog assistant or its drafting tools.
- No scenario shows a catalog edit spending a unit or held by an incident.
- No scenario crashes between a Solana plan publish and the commit, or asserts
  the re-publish guard or the `limited`/`readonly` attach.
- A migration's cohort is one read with no `LIMIT` (`ListMigratableSubscriptionsByPriceIDs`), classified and moved one transaction per subscriber inside the request; only the destructive ceiling, applied after the read, bounds it over HTTP, and nothing bounds a preview or the in-process host's own call. No scenario migrates a cohort large enough to show it.
- An NMI schedule on a named plan, or without `plan_payments`, fails its push as retryable, so it retries on the backoff instead of blocking.
- A migration archives its sources with `PriceService.SetArchived`, outside the catalog command: no catalog lock, no edit ownership.
- No scenario migrates by price key or to another product, blocks a CCBill or Solana subscriber, asserts the price-change notices, `fallback_policy` or `rail_release_required`, or interrupts `CreatePriceMigration` between admission and its last move.
