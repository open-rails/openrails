# Catalog

What a merchant sells: products, prices, meters, rate cards and usage limits. It
changes by documents (`catalog.Application`) an operator applies and by edits
from the console, the catalog routes and `Client` methods; starting OpenRails
never applies one (`internal/engine/engine.go`). Guides:
[catalog](../catalog.md), [catalog export](../catalog-export.md).

## Concepts

- **Product**: key, display fields, entitlements, tier group and rank,
  ownership rule, credit grant, rate cards and limit capacities.
- **Price**: an immutable financial version of a price key, id `product:key:vN`
  (`billing.PriceID`); `Price.Revision` is the key's concurrency counter.
- **Provider price**: mirrors an NMI plan, Stripe price or CCBill FlexForm; read,
  never written. Its **binding** (`price_psp_bindings`) names the object.
- **Manager**: who last set a field, `apply` (a document) or `edit`
  (`catalog_field_owners`).
- **Catalog assistant**: a model that answers questions and drafts changes.

## How it works

**One write path.** Every edit and application is a catalog command
(`catalogCommand`, `internal/service/catalog_admission.go`):

1. Plan under the catalog lock, writing nothing (`catalogReadLocked`,
   `readCatalogState`): list the existing objects it changes.
2. Admit them, if any, as one destructive command (`destructive.CatalogChange`).
3. Commit in one merchant transaction (`catalogMutation`): lock the revision
   (`LockCatalogRevision`), record edit ownership (`recordCatalogEdit`), refuse
   a new recurring benefit overlap (`catalog_benefit_overlap`), finish the
   admission.
4. If the catalog moved since planning, or a changed object was not admitted
   (`errCatalogSnapshotMoved`), release the admission as no-effect and plan
   again, up to three times, then answer `409 catalog_revision_conflict`.

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

`internal/catalogrules` validates charge models and rates usage in integer
micros ([usage billing](usage-billing.md)). Price migrations are
[subscriptions](subscriptions.md); `StripeCatalogService` (webhook endpoints) is
[payment providers](payment-providers.md).

Tables: `products`, `product_entitlements`, `prices`, `price_keys`,
`price_key_movements`, `price_psp_bindings`, `catalog_meters`,
`catalog_rate_cards`, `catalog_limits`, `catalog_product_limits`,
`catalog_field_owners`, `catalog_applications`, `catalog_restore_receipts`;
read-only here, `provider_catalog_objects`.

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
- CLI: `openrails catalog apply FILE --merchant NAME [--force-conflicts]`,
  `openrails catalog export|import`, `openrails dump-merchant-catalog`.
- Configuration: `llm.api_key`, `llm.catalog_copilot_enabled`,
  `llm.catalog_drafting_enabled`.

## Guarantees

### 1. Exactly-once provider calls

The catalog writes nothing to NMI, Stripe or CCBill. Its one provider write, a
Solana plan, is published before the price row commits: in a document's
preparation, or in `createPriceLinks` for one price (inside the inline product's
transaction when `ProductData` is used). The address derives from price, mint
and treasury, so a publish whose commit failed is reattached, not repeated.

Checks:
- `internal/service/catalog_*.go` and `providerobjects` only read NMI and Stripe
  (`ObserveByID` with `providerobjects.ViaRead`).
- `PublishPlan`'s re-publish guard returns a matching plan and refuses others.
- With `RemoteWritesDisabled`, `publishSolanaPlan` only reads the chain.

### 2. Agreement with providers

A provider price keeps the terms observed in `provider_catalog_objects`, where
pulls, webhooks and by-id reads keep the latest observation. New terms become a
new version only at the next apply. A lost Stripe event is caught by the next
event or a pull ([imports](imports.md)); `GET /v1/catalog/prices/{id}?verify=true`
re-reads the provider (`VerifyPublicPriceSync`).

Checks:
- `observedTerms` keeps a deleted object's last terms and refuses one with no
  fixed amount.
- OR-J38-S05 and OR-J15-S08 cover the Stripe observation path.

### 3. Exact money

`parseAmount` reads `9.99 USD` into the registry's integer scale, refusing extra
decimals and int64 overflow; `currency.ForPrice` refuses a stablecoin price. NMI
and Stripe decimals convert through `moneyutil.DecimalToRailMinor` and
`RailMinorToNative`; a Solana plan amount uses the mint's on-chain decimals, and
`PublishPlan` refuses any other scale. No catalog path converts FX.

Checks:
- OR-J05-S01 and OR-J05-S02 pin exact amounts and refuse bad ones.
- OR-J38-S42 pins NMI decimals per currency.
- `immutable_price_terms` refuses an update to `amount` or `currency`.

### 4. Access control and tenant isolation

Catalog routes are `Group: CatalogAdmin, Auth: AuthMerchant`; their level maps
to the read or manage pair (`internal/http/routes/permissions.go`). The service
takes the merchant from the context (`merchant.Require`) and writes in
`MerchantTx`; tables key on `(merchant_id, …)`. The CLI requires `--merchant`.

Checks:
- OR-J01-S05 and OR-J01-S07 send catalog calls across merchants.
- OR-J04-S02 keeps cached catalog reads bound to their merchant.
- Every query in `products.sql` and `prices.sql` names `merchant_id`.

### 5. Contracts match the code

The `catalog` package, `billing` catalog types and `Client` methods are in
`api/go.txt`, the routes in `api/openapi.json`, the tables in `api/schema.txt`;
documents carry `schema_version` (`catalog.ApplicationSchemaVersion`). The
in-process `Client` reaches the same handlers (`internal/http/inprocess`), and
`ApplyOperatorCatalog` normalizes its document through the JSON wire form.

Checks:
- `go run ./scripts/contracts` passes with no catalog line removed.
- A replay answers `replayed: true` with the first `application_id` (OR-J05-S03).

### 6. Data lifecycle

`products`,
`product_entitlements`, `prices`, `price_key_movements`, `catalog_applications`
and `catalog_restore_receipts` are permanent, the rest state; none is
partitioned. Products and prices soft-delete.

Checks:
- `internal/retention/tables.go` lists every catalog table (by hand: nothing
  checks it against the schema).
- `no_product_delete`, `immutable_price_terms` and
  `immutable_catalog_application_receipt` triggers exist.

### 7. Work scales with activity

No catalog job is periodic (`internal/river/periodic.go`); the only background
work is a `ProviderObjectObserveWorker` job per Stripe product or price an event
names, unique per object and debounced, so a burst is one read. But each command
reads the merchant's whole catalog up to four times under the merchant's lock.

Checks:
- `EnqueueProviderObjectObserve` is called only from
  `internal/modules/webhooks/stripe_catalog.go`.
- `catalogLocked` reads `readCatalogState` before, after, and again for
  `recordCatalogEdit`.

### 8. Abuse protection

Catalog routes are staff routes, outside the customer limits
([limits](../limits.md)). Bodies are capped at 1 MiB
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

Checks:
- `catalogLocked` fails a changed object with no admission.
- `ApplyOperatorCatalog` passes an origin, so `catalogCommandOnce` calls
  `AdmitAutonomous`.
- `pruneCatalogObjects` deletes only through `deleteProduct` and
  `deletePriceKey`, which check references first.

## Tests

Journeys:
- OR-J05-S01 to OR-J05-S05 (`TestPublicCatalog`): exact amounts, refused terms,
  replay with ownership and revision conflicts, archive keeps history, accepted
  terms survive edits.
- OR-J08-S01, OR-J08-S03: carts price from the catalog, refuse an archived price.
- OR-J38-S01, S02, S04, S05, S33, S34, S42: provider-price mapping, retired
  Stripe prices, read-only pulls, Map needing catalog manage, NMI currencies.
- OR-J15-S08, OR-J15-S12, OR-J29-S05, OR-J01-S05, OR-J01-S07, OR-J04-S02.

Other tests: `TestBillingIntervalUnits`; rate cards in
`internal/modules/money/rating_test.go`, OR-J24-S09, OR-J24-S10; Solana plans
are published only as setup (`CreatePrice` in `TestSolanaPlanChanges`).

Gaps:
- Nothing covers `prune: true`, entitlement replacements or a warned document.
- Nothing tests the catalog assistant or its drafting tools.
- No scenario shows a catalog edit spending a unit or held by an incident.
- No scenario crashes between a Solana plan publish and the commit, or asserts
  the re-publish guard or the `limited`/`readonly` attach.
