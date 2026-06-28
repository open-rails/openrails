<!-- openrails issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; section anchor for
> tooling is a line starting with `# #`. IDs are stable for an issue's whole lifecycle and
> share ONE per-repo id space across progress.md / future.md / completed.md; new issues take `next_id` below and bump it.
> CONCURRENT EDITS: only ever edit/append your own issue's section with targeted string
> replacement — never rewrite the whole file.


next_id: 591

---

# #590: Auto-register + reconcile the Stripe webhook endpoint (OpenRails owns the endpoint + signing secret)

**Completed:** partial — CORE DONE + LIVE-VALIDATED 2026-06-26. Built the
webhook-endpoint client + reconcile in internal/modules/catalog/stripe_webhooks.go
(`CreateWebhookEndpoint` returns the `whsec_`; `ListWebhookEndpoints`,
`UpdateWebhookEndpoint`, `DeleteWebhookEndpoint`; `ReconcileWebhookEndpoint` =
find-or-create by `openrails_managed` marker, in-place patch of url/events/disabled
with the secret surviving, delete+recreate on api_version drift OR lost secret,
ignores unmanaged endpoints). `enabled_events` single source = `HandledStripeEventTypes`
in internal/modules/webhooks/stripe.go (reconcile takes events as a param to avoid
the webhooks→catalog import cycle). Endpoint pinned to `stripeapi.APIVersion`.

DECISION: register a SNAPSHOT endpoint for the first cut (the mature path the
handler is built around); thin-event Destinations are a follow-up.

VALIDATED AGAINST REAL STRIPE (test account, restricted `rk_test_` key,
`-tags=stripelive`): TestLiveWebhookEndpointReconcile — real create returns a
secret + endpoint pinned to our version + our events; idempotent unchanged; URL
drift patches IN PLACE (same endpoint id, no recreate); events drift patches in
place. TestLiveWebhookDeliveryThroughTunnel — stood up a real cloudflared quick
tunnel, registered a managed endpoint at it, created a real product, and the
`product.created` webhook was ACTUALLY DELIVERED through the tunnel and its
signature VERIFIED with the auto-captured secret via `sigverify.VerifyStripe` (the
same verifier production uses). Both self-clean.

REMAINING (the integration wiring — deliberately deferred; needs deployment-mode-
aware design + dual-mode testing I can't validate solo): persisting the captured
`whsec_` to the RIGHT place is mode-dependent — multi-merchant verification reads
the per-merchant secret store (`MerchantSecretStore.Put(merchantID,
merchants.SecretStripeWebhookSigning, …)`), but standalone/config verification
reads `stripeProc.WebhookSecret` from config Rails. Get this wrong and webhook
verification silently breaks, so it's not safe to blind-wire overnight. Also
remaining: the public-URL config source, the create-at-credential-setup hook, and
the periodic reconcile River job (URL-drift self-heal). The reusable core +
ReconcileWebhookEndpoint is ready for these to call.

Proposed 2026-06-26. Follow-up to #587 (version pin) and #586 (catalog push).
Today the operator manually configures the Stripe webhook endpoint in the
dashboard per merchant: create endpoint → paste the OpenRails URL → pick event
types → set API version → reveal the signing secret → copy `whsec_` into the
merchant's stripe rail config (`WebhookSecret` / `WebhookSecretThin`, read by
`prepareStripeMultiSecret` in internal/http/handlers/webhook.go). Five fiddly,
error-prone steps; the classic silent failures are a wrong/under-selected event
list and a mistyped/stale secret. Make OpenRails do it programmatically.

GOAL: OpenRails registers + keeps-correct its own Stripe webhook endpoint, so the
operator only supplies the Stripe secret key. Closes the #587 OPS ACTION (the
endpoint's `api_version` gets pinned to `stripeapi.APIVersion` from code, no
dashboard step).

VERSION DECISION (owner, 2026-06-26, see #587): the endpoint's `api_version` is the
SAME single hardcoded `stripeapi.APIVersion` const used for outbound — not
per-merchant, not a config field. One value, both directions, bumped only by a
deliberate code change + breaking-change audit.

DESIGN DECISION (confirmed with owner 2026-06-26): OpenRails OWNS the webhook
secret lifecycle — capture the `whsec_` returned on create, store it encrypted in
the merchant's stripe rail secret(s), use it for verification, and re-capture on
any recreate. (The alternative — auto-register URL only, operator still hand-copies
the secret — keeps the two most painful/breakage-prone steps, so rejected.)

Key facts that shape it:
- The signing secret is returned ONLY on the create response (`POST
  /v1/webhook_endpoints`); it can't be fetched later. So create MUST capture +
  persist it, or verification breaks.
- Endpoint identity for find-or-create = a stable `metadata[openrails_managed]=true`
  marker, NOT the URL — the URL is the field that drifts, so it can't be the key.
- COST ASYMMETRY: `url`, `enabled_events`, `disabled` are updatable in place
  (`POST /v1/webhook_endpoints/{id}`) → patch, secret SURVIVES, cheap. `api_version`
  is NOT updatable → a version bump forces delete + recreate → secret ROTATES →
  must re-capture + re-store. So a redeploy/URL change self-heals cheaply; only a
  deliberate version bump is the expensive reconcile.
- Two endpoint flavors exist (snapshot vs thin events, separate secrets:
  `WebhookSecret` / `WebhookSecretThin`). DECIDE: register a thin-event endpoint
  (thin + our pinned hydration in `hydrateThinStripeEvent` = version-robust
  inbound — attractive), a snapshot endpoint, or both. Lean thin-first.

TWO triggers:
1. CREATE at Stripe credential setup (the merchant-adds-secret-key moment, where
   the balance/`/v1/account` check already runs) — first-time registration.
2. PERIODIC RECONCILE as a background River job (sibling to
   jobs_catalog_reconciliation), sweeping merchants with Stripe configured — this
   is what catches later config drift (URL changed on redeploy, events list grew,
   endpoint auto-disabled by Stripe). NOT inline at process boot: multi-merchant
   boot must not fan out network writes across every merchant's Stripe account.
   (Embedded single-merchant hosts like doujins MAY reconcile at startup — one
   merchant, one write.)

Reconcile (desired = our config+code, actual = the registered endpoint):
- missing → create (capture+store secret).
- `url` mismatch → update in place.
- `enabled_events` mismatch → update in place (desired = exactly the types
  `handleEvent` switches on; keep this list in one place so it can't drift).
- `disabled` → re-enable.
- `api_version` mismatch (we bumped the pin) → delete + recreate → re-capture +
  re-store secret. Comment loudly so a future bump can't silently break verify.
- secret not on hand (e.g. DB restore lost it; can't re-fetch) → delete + recreate.

Scope:
- [x] Stripe webhook-endpoints client: Create / List / Update / Delete (through the
      stripeapi choke-point — writes blocked in readonly, version header attached).
- [x] `enabled_events` single source of truth (`HandledStripeEventTypes`, kept next to the `handleEvent` switch).
- [ ] Public webhook URL from config; skip cleanly + log when absent (embedded/local/no public URL). [DEFERRED — wiring]
- [ ] Persist the returned `whsec_` to the mode-correct store (per-merchant secret store vs config Rails); wire to what `prepareStripeMultiSecret` reads. [DEFERRED — mode-aware]
- [ ] Create-at-credential-setup hook (idempotent find-or-create by `openrails_managed` marker). [DEFERRED — wiring]
- [ ] Periodic reconcile River job (drift-fix per the rules above), best-effort, never blocks boot. [DEFERRED — wiring]
- [x] Decided: SNAPSHOT endpoint for first cut (thin Event Destinations = follow-up).
- [x] Tests: mock unit tests (idempotency, url/events in-place patch, version recreate, lost-secret recreate, ignore-unmanaged) + LIVE create/reconcile/delete + LIVE delivery-through-tunnel with signature verify.

---

# #589: payment-methods listing API with derived health/status (drop failure_reason denorm)

**Completed:** no

STATUS 2026-06-27 (Claude): PLAN. Turn the saved-payment-method surface into a proper listing
API that returns *derived* health per method, and drop the `payment_methods.failure_reason`
display-only denorm in favor of query-time derivation.

## Goal
A "list payment methods" surface for two audiences:
- **user self-list** — a user lists their own saved methods.
- **admin list-for-user** — an admin lists a specific user's saved methods.
Each method returns *derived* health/status useful for UI, e.g. `last charge: 2026-06-24,
failed`, card expired, active/valid:
- `expiry_status` — derived from the card row vs now (card kind only).
- `last_charged_at` + `last_charge_outcome` (success/failed).
- `active`/`valid` — composite: not expired, not revoked, last charge not hard-failed.
- `last_failure` (reason + when) — REPLACES the `failure_reason` column.

## Why drop failure_reason
- It's a display-only denorm: only consumer is the payment-methods read path
  (`internal/http/handlers/payment_methods.go` ~:539); charge paths never read it; no path
  writes a real failure into it → vestigial + goes stale.
- Failure source of truth is the append-only stores: `external_provider_mutation_logs`
  (per-attempt: phase=failed, reason, evidence) + ClickHouse payment_events — NOT
  `provider_intents` (transient outbox, overwritten per retry, drains/expires).

## ⚠️ Design gap to resolve first: charges aren't attributed to a payment_method
`payments`, `provider_intents`, and `external_provider_mutation_logs` have **no
`payment_method_id`** — they link to a *subscription* (`subscriptions.payment_method_id`), not
the method. So "last charge for THIS method" can only be derived TRANSITIVELY (method →
subscriptions on that method → their payments), which MISSES one-off / invoice / top-up vault
charges (RunSale never records which stored method it used). DECIDE:
- (a) accept transitive (subscription-scoped) derivation — incomplete for one-offs; or
- (b) add `payment_method_id` to `payments` (and likely `provider_intents` /
  `external_provider_mutation_logs`) for direct attribution. RECOMMENDED — it's the only way to
  get accurate "last charge per method" for non-subscription charges, and it makes the listing
  derivation a simple join.

## Existing surface
`ListPaymentMethods` already exists (`internal/http/handlers/payment_methods.go:393`,
paginated via `listPaymentMethodsQuery`). Audit it for the self vs admin-for-user split and
extend the response with the derived fields above.

## Tasks
- [ ] decide attribution: transitive vs add `payment_method_id` to payments/intents/logs
      (recommended) — prerequisite for accurate per-method charge history.
- [ ] list API: confirm/add user-self + admin-for-user variants (scoped by customer).
- [ ] response: add expiry_status, last_charged_at, last_charge_outcome, active/valid, last_failure.
- [ ] derive last_failure / last_charge from the append-only stores (never provider_intents).
- [ ] drop `payment_methods.failure_reason` (migration) + struct field + UpdatePaymentMethod
      param + any setters; grep for remaining readers.

## References
- internal/http/handlers/payment_methods.go (:157 response, :393 ListPaymentMethods, :539);
  internal/db/repo/payment_method.go (UpdatePaymentMethod); internal/db/gen/payment_methods.sql.go.
- Failure source-of-truth: external_provider_mutation_logs, ClickHouse payment_events,
  payments/money_ledger. Charge→method attribution gap noted above. Related: #588.

---

# #588: rail-agnostic payment-method (instrument) model — generalize beyond vault_id/billing_id

**Completed:** no

STATUS 2026-06-27 (Claude): PLAN (greenfield design, not started). Captured so it isn't
lost. **No urgency:** today every consumer is effectively one-instrument-per-customer per
rail, so the current model is unambiguous. Act on this when onboarding a rail (or usage)
with multiple stored instruments under one customer — i.e. NMI multi-billing vaults, or a
customer with several Stripe/HyperSwitch cards.

## Problem
`payment_methods` encodes instrument identity as `(merchant, rail, vault_id)` with
`billing_id` as a secondary nullable column, and that doesn't generalize across rails:
- `vault_id` is **overloaded** — for Stripe/Spreedly/HyperSwitch it's the *instrument* token
  (`pm_…` / spreedly token / `payment_method_id`); for NMI it's the *customer container*
  (`customer_vault_id`) and the real instrument is the pair `(customer_vault_id, billing_id)`.
  Today the code already double-writes NMI's `customer_vault_id` into both
  `payment_methods.vault_id` and `rail_customers.rail_customer_id` — the tell that the model
  doesn't fit.
- `billing_id` is an NMI-ism, and the unique indexes (`uq_payment_methods_*` on
  `(merchant, rail, vault_id)`, scoped by provider_account_id) do NOT include it → only **one
  method per vault** is representable; an NMI customer vault with >1 billing record collides.
- card-centric columns (`last_four`/`card_type`/`expiry_date`) don't fit non-card rails
  (crypto/ACH/wallet).
- the recurring/MIT anchor exists only as NMI-flavored `initial_transaction_id`; there's no
  generalized network-transaction-id / mandate concept for off-session charges.

## Insight
Every rail has at most TWO opaque handles — a customer-scope one and an instrument-scope one
— plus the provider-account scope. NMI is the only one that requires BOTH on the charge call;
the others charge on the instrument token alone. Model the two slots explicitly; name nothing
after a single rail.

| rail | customer-scope | instrument-scope | charge needs |
|---|---|---|---|
| NMI | customer_vault_id | billing_id | both |
| Stripe | cus_ | pm_ | method (+customer off-session) |
| HyperSwitch/Juspay | customer_id | payment_method_id | method (+customer) |
| Spreedly | — (token-centric) | token | method |

## Proposed design
- `rail_customers` (normalize as one per customer×rail×provider_account): `rail_customer_ref`
  = cus_/customer_vault_id/customer_id; `''` for token-only rails.
  UNIQUE(merchant_id, rail, provider_account_id, rail_customer_ref).
- `payment_methods`:
  - `id uuid PK` — stable internal identity; subscriptions/payments FK to this, NEVER to a
    rail token (rails rotate/re-vault tokens).
  - `rail_customer_id` → rail_customers (nullable for token-only rails).
  - `rail_method_ref text NOT NULL` — **replaces vault_id + billing_id** (pm_/billing_id/token/
    payment_method_id). No `vault`/`billing` naming.
  - `network_transaction_id text`, `mandate_ref text` — generalized off-session/MIT anchors
    (supersede `initial_transaction_id`; add connector mandate for HyperSwitch/SEPA).
  - `kind text NOT NULL` (card|bank_account|wallet|crypto…), `fingerprint`, nullable card
    descriptors (brand/last_four/exp), `status`, `metadata jsonb`.
  - UNIQUE(merchant_id, rail, provider_account_id, rail_customer_id, rail_method_ref) —
    composite, customer-scoped. No-op for single-token rails (globally-unique method ref);
    fixes the NMI multi-card case.
- Charge adapter resolves the two slots per rail: NMI→{customer_vault_id, billing_id};
  Stripe→{payment_method, customer}; HyperSwitch→{payment_method_id, customer_id};
  Spreedly→{payment_method_token}.

## Tasks
- [ ] schema migration: restructure payment_methods (vault_id/billing_id → rail_method_ref;
      add network_transaction_id/mandate_ref/kind/fingerprint; composite unique index); add
      rail_customer_ref where missing.
- [ ] rail adapters resolve charge handles from the two-slot model (nmi/stripe/spreedly/
      hyperswitch); thread network_transaction_id/mandate_ref into off-session/MIT calls.
- [ ] backfill existing rows (vault_id→rail_method_ref or rail_customer_ref by rail;
      initial_transaction_id→network_transaction_id) — data migration, never a hard cut.
- [ ] consumer impact: doujins legacy_migrate writes vault_id/billing_id directly (target
      models + raw SQL in customers_vaults/subscriptions/wallet_transactions handlers) — bump
      together (authkit+openrails+doujins semver-sync) and update doujins after.

## Risks / compat
- Breaking schema change for any embedded host → coordinate semver bump with consumers.
- Provide a data migration for live billing data; never a hard cut.
- doujins's shipped legacy migration is one-card-per-vault → unaffected today.

## References
- migrations/postgres/001_schema.up.sql (payment_methods, rail_customers, provider_accounts,
  uq_payment_methods_*); internal/modules/payments/stripe_card.go;
  internal/integrations/nmi/{payments,subscriptions,vault}.go.
- Spreedly: developer.spreedly.com/docs/using-payment-methods, /docs/third-party-vaulting.
- HyperSwitch: docs.hyperswitch.io/integration-guide/workflows/vault, .../payment-methods-management.

---

# #587: Pin a Stripe API version (Stripe-Version header) instead of floating on the account default

**Completed:** yes — DONE 2026-06-26. Pinned `Stripe-Version: 2026-06-24.dahlia`
(Stripe's latest stable as of today) on the choke-point client. All four scope
items below are checked. Adapting our parsers to dahlia required NO code changes:
audited the known breaking changes since our implicit baseline — `billed_until`
(not read), Billing meter-event validation (we don't use meters), the invoice
`parent`/`pricing.price_details` reshaping (already handled in stripe.go), and
subscription `current_period_end` moving to items (we pass it in from OpenRails
state, never parse it from Stripe). Payouts/Treasury/Issuing/Radar/Batch-job
changes don't touch our surface. Files: internal/integrations/stripeapi/stripeapi.go
(+ test), internal/modules/webhooks/stripe.go (api_version drift warning).

Proposed 2026-06-26. OpenRails pins NO Stripe API version anywhere — no stripe-go
SDK (all Stripe HTTP is hand-rolled through the choke-point client
internal/integrations/stripeapi/stripeapi.go), and no `Stripe-Version` header on
any request. So every API response AND every webhook is served in the Stripe
ACCOUNT's default API version (whatever the dashboard is set to / the account was
last upgraded to), which can change out from under us. Because we hand-parse JSON,
a Stripe-side version roll can silently change field shapes and break parsing.
Inversion worth stating: we are NOT "hardcoded to version X" — we are pinned to
nothing and float with the account default; the fix is to pin to a known X we test
against.

Target version: **`2026-06-24.dahlia`** — Stripe's latest stable release train as
of 2026-06-26 (newer than the 2026-05-27.dahlia first noted; verified against
https://docs.stripe.com/changelog at implementation time).

DECISION (owner, 2026-06-26): SINGLE hardcoded const for BOTH the outbound header
and the webhook endpoint (#590 reads the same const). NOT per-merchant (the version
is a property of our parser code, not a merchant — per-merchant would force
multi-version parsing) and NOT a config field (a knob just lets an operator pin a
version their parsers weren't written for — pure footgun). "Latest" = latest at
pin/test time, not auto-tracking; it moves only by a deliberate code bump + the
breaking-change audit. Source-available patch is the emergency escape hatch.

Scope:
- [x] Add a `const APIVersion = "2026-06-24.dahlia"` and set `Stripe-Version: <that>` on every request from the shared `stripeapi` client — single choke-point in `guardTransport.RoundTrip` covers all outbound Stripe calls (catalog, subscriptions, checkout, invoices, portal, refunds, reconcile). Clones the request so the RoundTripper contract holds; a caller-set version is preserved.
- [x] Pin to that named constant; bump deliberately ONLY after testing a newer version — never float, never re-fetch "latest" at runtime.
- [x] Webhooks don't flow through the outbound client: added a non-fatal warning when an inbound event's `api_version` differs from the pinned version (parsers tolerate adjacent shapes, so don't reject). OPS ACTION still required: pin the webhook endpoint's API version in the Stripe dashboard to `2026-06-24.dahlia`.
- [x] Test: `TestPinsStripeVersionHeader` asserts the header is present on GET + POST and that a caller override is preserved; existing webhook parse tests still green with the new `api_version` field.

---

# #586: Sync product-feature entitlements into Stripe so the Stripe catalog mirror matches OpenRails

**Completed:** yes — DONE 2026-06-26. New Stripe Entitlements (Features) client in
internal/modules/catalog/stripe_entitlements.go: CreateFeature / ListFeatures /
ListProductFeatures / AttachFeatureToProduct / DetachProductFeature, plus
`SyncProductFeatures(stripeProductID, desiredKeys)` — the idempotent reconcile
(find-or-create a Feature per entitlement string at lookup_key = the string,
attach the missing, detach the no-longer-desired OpenRails-MANAGED ones; never
touches operator/third-party features). Wired into the PUSH path: the Stripe
adapter's AutoCreate (pkg/service/catalog_provider_stripe.go) syncs features right
after it ensures the Stripe Product during a price link, and Service.UpdateProduct
(pkg/service/service_definition_catalog.go) re-syncs on an entitlements edit
(emptying the spec detaches all managed features). Both are BEST-EFFORT (a sync
failure logs + lets drift surface on reconcile; never fails the price link / DB
write), matching the existing Stripe propagations. One-way OpenRails→Stripe; the
window ledger stays the cross-rail active-access truth. ponytail: Feature `name` =
the entitlement string (friendlier names from `entitlement_features` are a
follow-up). Added the version-pin (#587) so these calls carry Stripe-Version too.

Tests: internal/modules/catalog/stripe_entitlements_test.go drives the reconcile
against a stateful fake Stripe (create+attach, idempotent rerun, partial detach,
full detach on empty, and "never detach an unmanaged feature"). Catalog
publish/apply integration tests (StandaloneMerchantCatalog*) stay green — the new
push is dormant when no Stripe rail is configured (the harness case). Real-Stripe
validation against a sandbox is the one thing unit/integration coverage can't do
here; left for a sandbox run.

Proposed 2026-06-26. We already mirror products + prices into Stripe
(internal/modules/catalog/stripe_catalog.go: `/v1/products`, `/v1/prices` +
`openrails_*` ownership metadata). The mirror is INCOMPLETE: product entitlements
are never pushed, so a synced Stripe product shows price but no features —
inconsistent with the OpenRails definition. Close the gap so the catalog mirror is
faithful: same prices AND same entitlements as OpenRails. Sibling of #134
(stripe-meter-connector): same one-way OpenRails→Stripe mirror pattern, for the
catalog's entitlements instead of usage meters. OpenRails stays source of truth.

Entitlements are just STRINGS: `product.EntitlementsSpec` is a `map[string]*int`
whose keys are the entitlement names (`"premium"`) and whose `*int` value is an
optional grant duration in days. Purchase grants iterate those keys directly
(internal/modules/checkout/purchase_service.go), so the spec IS the product's
entitlement definition. They map 1:1 onto Stripe: each distinct key → a Stripe
Feature (`lookup_key` = the string), attached to the synced product. The
`entitlement_features` table is an OPTIONAL naming layer (gives a string a display
name/metadata); use its `name` for the Stripe Feature when present, else fall back
to the string itself (Stripe requires a `name`). The `*int` duration has no Stripe
equivalent (Stripe product-features carry no per-feature duration) — it stays
OpenRails-only, one more reason OpenRails remains the active-access truth.

Keep two levels distinct:
- CATALOG level (THIS issue) — Stripe Features + Product Features. Rail-INDEPENDENT
  catalog metadata (a product's feature list is not customer-specific). Completes
  the existing product+price mirror so Stripe == OpenRails for product definitions.
  One-way OpenRails → Stripe.
- CUSTOMER level (explicitly NOT this issue) — Stripe's per-customer computed
  active-entitlements. Once product-features exist, Stripe auto-derives these for
  Stripe customers; fine as a Stripe-native convenience, but OpenRails' entitlement-
  window ledger stays the source of truth for ACTIVE access because it spans all
  rails (Stripe/NMI/CCBill/Solana). Never read Stripe active-entitlements back as
  authority — they'd be empty for non-Stripe customers.

Scope:
- [x] `POST /v1/entitlements/features` per distinct entitlement string, idempotent via `lookup_key` (find-or-create); `name` = the string (entitlement_features friendly-name = follow-up); ownership marked via `metadata[openrails_managed]=true`.
- [x] `POST /v1/products/{stripe_id}/features` to attach the features in each product's `EntitlementsSpec`; `DELETE` to detach when a key is removed.
- [x] Wired into the PUSH path (catalog_provider_stripe.go AutoCreate + service_definition_catalog.go UpdateProduct), Stripe-rail merchants only, best-effort. NOTE: NOT the pull/drift job (jobs_catalog_reconciliation.go is alert-only by design); the push is where definitions flow to Stripe.
- [x] One-way only (OpenRails → Stripe). The window ledger stays the cross-rail truth for active access.
- [x] Doc note in code (stripe_entitlements.go header): product-features mirror the catalog; Stripe active-entitlements are Stripe-customer-only and NOT authoritative.

---

# #585: Role-agnostic default privileges in 001 (drop hardcoded `FOR ROLE admin`)

**Completed:** yes

The squashed 001 baseline carried `ALTER DEFAULT PRIVILEGES FOR ROLE admin` — a
pg_dump artifact naming the dump-source superuser. Any host running migrations
as a role NOT named `admin` either fails ("role admin does not exist") or
silently never grants future objects to openrails_app (breaking RLS-mode access
on later migrations). Dropped `FOR ROLE admin` so the clause targets current_user
(the migration runner), role-agnostic.

- [x] Drop `FOR ROLE admin` from both ALTER DEFAULT PRIVILEGES (sequences + tables).
- [x] Migration + RLS tests pass (openrails_app still gets the grants).

Note: embedded single-merchant hosts (doujins) connect as a superuser, so RLS +
these grants are inert there; this matters for non-superuser / multi-merchant
deployments. No standalone tag needed — rides the next release.

---

# #584: Migration baseline 001 self-creates the `openrails` schema

**Completed:** no

Proposed 2026-06-25 (doujins embedded-migration review).
The squashed `migrations/postgres/001_schema.up.sql` baseline is fully
schema-qualified (`openrails.*`) but never runs `CREATE SCHEMA openrails`, so the
migration FS is NOT self-contained: any consumer applying it via migratekit must
pre-create the schema first. openrails' own standalone migrator already does
`CREATE SCHEMA IF NOT EXISTS` (internal/migrate/migrator.go), but the embedded
FS-driven path (doujins) bypasses that, forcing doujins to hand-maintain a
`CREATE SCHEMA IF NOT EXISTS openrails` pre-step. Make the FS own its schema.

- [x] Prepend `CREATE SCHEMA IF NOT EXISTS openrails;` to 001 (before the first
      `openrails.`-qualified object), idempotent so already-migrated DBs skip it.
- [x] Confirm migration tests still pass (schema pre-create becomes redundant, not conflicting).
- [x] Tag + release (v0.65.1); doujins drops `openrails` from its host-side ensureBaseSchemas list.

---

# #583: Embedded-host boundary cleanup — public permissions pkg + combined MountHandler (etcd lessons)

**Completed:** yes
**Status:** DONE 2026-06-26 (Codex). OpenRails is pinned to authkit v0.72.0 with no local replace. The OpenRails-side embed boundary is complete: public `permissions` constants are the route-gate vocabulary, `embgin.MountHandler` is the combined `/me` + user/admin/webhook mount, current docs show AuthKit client-first construction plus OpenRails `merchant:*`/`customer:*` guards, and stale `openrails:self:*`, `openrails:admin`, `org:*`, and `/v1/service/*` guidance was hard-cut from active docs. Added real integration coverage: `TestEmbeddedMountHandlerEndToEnd` drives `/v1/merchant/credits/deposit` then `/v1/me/balance` through `MountHandler` against real migrated Postgres/Redis. Fixed the integration harness for authkit v0.72.0 by removing the obsolete service-JWT resource-scope API and deleting a skipped legacy org↔merchant assertion. Validation: `go test -tags=integration ./internal/integrationharness -run 'Test(EmbeddedMountHandlerEndToEnd|APIKeyCrossMerchantIsolationHTTP|RemoteApplicationSelfJWTCrossMerchantIsolationHTTP|StandaloneMerchantAdmitAccepts(DelegatedJWT|UserSession)ByPermissionHTTP|DelegatedAdminCrossMerchantIsolationHTTP|DelegatedSelfTokenSubjectIsolationHTTP|CoreDoesNotMountPlatformAdminRoutesHTTP|StandaloneMerchantCatalog(RoutesHTTP|ApplyOptionsOverHTTP|PublishHTTP)|StandaloneMerchantPaymentProviderConfigHTTP|StandaloneNoDefaultMerchantResolvesRequestScopedMerchant)' -count=1 -v`; `go test -tags=integration ./embed -run TestStandaloneMerchantControlBoundaries -count=1 -v`; `go test ./pkg/embedded/gin ./internal/controlplane ./internal/http/routes ./internal/http/middleware/ginmw ./internal/http/routes/ginroutes ./embed -count=1`; `git diff --check`.

Proposed 2026-06-25 (doujins boundary review + etcd embed-design comparison).
Embedding hosts (doujins) write more glue against the embed surface than they
should, and hardcode permission strings openrails owns. Two additive,
non-breaking changes.

PART A — public `permissions` package (perm-export). openrails' route-gate
vocabulary (`merchant:*` seller, `customer:*` treasury) lived only in
`internal/controlplane/catalog.go`, so hosts can't import it and hardcode
literals — and doujins drifted to vestigial `openrails:self:*` strings that match
no gate (`/v1/me` self-service needs no grant). New public `permissions/` package
owns the string constants + the `merchant:*`/`customer:*` owner globs;
`internal/controlplane` now references it (single source of truth, drift-proof).
Status: implemented locally, builds, values asserted unchanged.

PART B — `embgin.MountHandler` (etcd lesson #1, mount-glue absorption). Today a
host builds `SelfHandler(rt.Embedded())` AND `rt.Handler(opts)` separately and
hand-stitches them + rewrites `/api/openrails/v1/*` → `/billing/v1/*` (doujins
`newMountHandler` — the bug-prone path; carries 3 re-path comments documenting
past breaks). New `pkg/embedded/gin.MountHandler(e, MountOptions{RouteSets,
MountPrefix})` returns ONE prefix-aware handler that internally dispatches
/me+/customers to the self surface and everything else to the user surface.
Status: implemented locally, builds.

NEXT CONSUMER REPO (doujins): imports `openrails/permissions` and stamps
`permissions.CustomerAll` (self) / `permissions.MerchantAll` (admin) in
`openrailsembed/auth.go`, replacing the vestigial self-strings; replaces
`buildMount`/`newMountHandler` (~60 lines) with one `embgin.MountHandler` call.
Do this when adapting the doujins repo; the OpenRails side is complete.

NOT doing (descoped from the etcd review): unify the standalone binary onto
`embed.Runtime` (lessons #2/#3) — only worth it if standalone stays first-class;
config translation is irreducible (two products, two Config structs).

---

# #575: Convergence sweep scalability — set-based DERIVE + period-overdue index

**Completed:** yes
**Status:** DONE (in tree, uncommitted) 2026-06-24 (Claude). Set-based DERIVE landed: the 4 detections are now single customer-nullable queries — 2 new SQL anti-joins (`ListLiveGrantsMissingEffects`, `ListUnretractedTerminations`, both `SELECT g.*` → `[]OpenrailsGrant`) + 2 renamed nullable (`ListUngrantedGrantablePayments`, `ListLiveGrantsWithRefundedPayment`); `derivePass.Run` global branch now calls them ONCE with `customer=nil` (the `ListCustomerIDsWithGrants` per-customer loop is gone); dead `isMaterialized`/`effectStillLive` Go helpers removed. Migration 030 adds `idx_subscriptions_period_overdue`. Validation: `task sqlc` generate+vet green (db-prepare PREPAREs every new query against the real schema); `go build ./...` + `go vet ./...` clean; converge DERIVE/CON/LIFE integration tests + river `TestConvergeSweep` green; added `TestConverge_DeriveSweepRemediatesAllCustomers` (one merchant-scoped sweep repairs unmaterialized grants across 2 customers via the new `customer=nil` path). The existing per-customer DERIVE tests are the equivalence oracle — unchanged, still green against the new SQL. CAVEATS: (1) the empty sqlc vet DB can't prove index use via EXPLAIN (tiny tables always seq-scan) — the supporting indexes exist (`idx_entitlements_grant_id`, `idx_ledger_transfers_grant`, `idx_grants_merchant_id`/`idx_grants_customer`); confirm plans on populated staging. (2) `GrantHasLiveEntitlement` query is now unused (left in place — trivial follow-up cleanup).

## Background
The merchant-wide sweep (`Scope.IsGlobal`) runs three passes per active merchant. Two cost centers grow with population rather than with drift/due-events:
- **DERIVE** (`internal/reconcile/converge/converge_passes.go` `derivePass.Run`) enumerates every grant-holding customer via `ListCustomerIDsWithGrants` (DISTINCT over the append-only `grants` ledger — i.e. every customer who has *ever* held a grant, incl. churned) and runs **4 per-customer queries each** (`MissingEffects`, `UnretractedTerminations`, `UngrantedGrantablePayments`, `RefundedSourceGrants` in `internal/modules/grants/grants.go`). That is O(grant-holders) × 4 round-trips every 15 min, regardless of whether anything is wrong. At 1M lifetime grant-holders ≈ 4M serialized round-trips.
- **LIFE** `period_overdue` (`ListPeriodOverdueSubscriptions`) filters `status='active' AND current_period_ends_at < now`, but there is no index on `current_period_ends_at`, so it scans all active subs for the merchant. Every other LIFE lane already rides a partial index (`idx_subscriptions_grace_ends_at`, `idx_subscriptions_due_dunning`, `checkout_sessions_expires_at_idx`).

LIFE/CON already accept a nullable customer (`sqlc.narg(customer_id)`) and run merchant-wide when nil; DERIVE is the only pass still looping per-customer. This issue brings DERIVE in line and adds the one missing index.

## Proposed Changes
- **Set-based DERIVE for the merchant-wide sweep.** Make the 4 grant checks **customer-nullable single queries** (the LIFE/CON `sqlc.narg(customer_id)` pattern), used by BOTH paths: inline passes the customer (fast indexed seek), the sweep passes NULL (one merchant-wide anti-join). Prefer this over separate per-customer + merchant-wide variants. Global-scope DERIVE then issues a **constant number of queries per merchant**, not 4×N. Findings + repair closures (built per returned row) must be byte-identical to the per-customer enumeration.
- **Keep the per-customer path for inline scope.** The customer/subscription-scoped path (`AfterMutation`, `runForCustomer`) MUST stay O(1 customer) — it's on the hot path after every mutation. Only `derivePass.Run`'s global branch passes customer=NULL.
- **Add the period-overdue partial index** (migration 030) so the last LIFE lane becomes a due-time seek:
  ```sql
  CREATE INDEX idx_subscriptions_period_overdue
    ON openrails.subscriptions (current_period_ends_at) WHERE status = 'active';
  ```
  Self-pruning: the repair flips `active → past_due`, so the row leaves the partial index; a `current_period_ends_at < now` range scan over it is O(overdue-and-unprocessed) ≈ newly-due-since-last-sweep — no watermark needed (see Design notes).

## Tasks
- [x] Make the 4 DERIVE grant queries customer-nullable in `internal/db/queries/grants.sql` (one query each, `sqlc.narg(customer_id)` like LIFE/CON — serves inline AND sweep), `task sqlc`; update the `Ledger` methods in `internal/modules/grants/grants.go` to take an optional customer.
- [x] EXPLAIN each of the 4 anti-joins under the sweep (customer NULL): confirm index-backed (`idx_entitlements_grant_id`, `idx_grants_source`, `grants_payment_fk`), NO seq scan; add a covering index if the planner picks one (set-based only helps if the anti-joins don't seq-scan).
- [x] Rewrite the global-scope branch of `derivePass.Run` to call the set-based queries once instead of looping `ListCustomerIDsWithGrants`; leave the `scope.Customer != nil` branch on the per-customer path.
- [x] Migration 030: `idx_subscriptions_period_overdue` partial index.
- [x] Tests: seed a merchant with N customers, inject drift on a few (missing effect, unretracted termination, ungranted grantable payment, refunded-source grant); assert the global sweep surfaces exactly the same findings as the per-customer version AND that DERIVE query count is constant in N (not O(N)). Functional test that `period_overdue` still flags overdue active subs (index-backed).

Validation: `task sqlc`; `go test ./internal/reconcile/... ./internal/modules/grants/... ./migrations/postgres/...`; `go test -tags=integration ./internal/reconcile/converge -count=1`; `git diff --check`.

## Acceptance
- A merchant-wide convergence sweep issues a constant number of DERIVE queries regardless of grant-holder count.
- DERIVE findings (and applied repairs) for the sweep are identical to the previous per-customer enumeration.
- Inline customer-scoped convergence (`AfterMutation`) remains per-customer and behaviorally unchanged.
- `period_overdue` is served by `idx_subscriptions_period_overdue` — no full active-subscription scan.
- No change to repair behavior; only query shape + indexing.

## Design notes (decided 2026-06-24 — do not re-litigate)
- **LIFE stays index-driven, never watermarked.** Each LIFE lane's "needs action" is a *column predicate* (`status=… AND deadline < now`), so a partial index on the candidate state + a `deadline < now` range scan gives O(due) for free, self-prunes (the repair flips the row out of the index), and correctly retries failed repairs. Do NOT add a forward due-time watermark to LIFE: redundant with the partial index AND unsafe — convergence backdates deadlines (e.g. `period_overdue` sets `grace_ends_at = past period end`), and a forward cursor silently skips deadlines that land behind it. The robust per-row form already exists as dunning's `next_retry_at` (deadline column + `WHERE … IS NOT NULL` partial index).
- **DERIVE/CON need the `updated_at` watermark precisely because they can't be partial-indexed:** their "needs action" is the *absence* of a related row (anti-join — grant with no effect, entitlement with no source), which is not an indexable predicate. The watermark bounds the scan instead.
- **Scope honesty:** #575 removes the round-trip explosion and the period_overdue full-active-scan. It does NOT reduce DERIVE scan volume to O(churn) — the 4 anti-joins still index-scan O(lifetime grant-holders) per sweep. Fine to low-hundred-thousands; the watermark below is the next step when those scans dominate.

## Out of scope (separate, later issues)
- Per-merchant River fan-out of the sweep (leader currently iterates all merchants serially; same pattern in Provider Refresh).
- DERIVE/CON `updated_at` watermark — the O(population)→O(churn) fix (new watermark table + `(merchant_id, updated_at)` partial indexes). Defer until `pg_stat_statements` shows the anti-join scans dominate.
- CON orphan-scan elimination via write-path entitlement cleanup in the delete sites (provider-pull-prune + delete-by-id), retiring the two CON orphan checks (FKs can't express the needed soft-revoke).

---

# #574: Expand subscription liveness into always-on Provider Refresh

**Completed:** yes
**Status:** DONE 2026-06-24 (Codex). Provider Refresh now wraps subscription liveness, bounded provider event windows, CCBill DataLink, durable watermarks, and scoped post-refresh convergence. The scheduled job runs on startup and every 4 hours. Failed provider reads record the attempt without advancing the watermark. Existing reconcile writers backfill charges/refunds and local terminal subscription state; chargebacks remain human-review findings. Imported active subscriptions with missing local period evidence now enter the liveness lane and are corrected from provider truth instead of being skipped forever. `pull-provider` stays the manual full-batch/operator command.

## Proposed Changes
- Rename/reframe the scheduled provider-read system as **Provider Refresh**.
- Keep `ConvergeEngine` local-only. Provider Refresh performs provider reads, updates local truth, then triggers scoped convergence for touched subscriptions/customers or merchant-wide when needed.
- Keep `Subscription Liveness Refresh` as the state lane inside Provider Refresh. It handles stale/silent subscription state: provider charged, declined, still alive with future billing, gone/terminal, or unreachable.
- Add a **Provider Event Refresh** lane for missed webhook/event backfill: recent payments, refunds, chargebacks/disputes, subscription cancellations/expirations, and other provider events OpenRails can safely read.
- Run Provider Refresh on startup and every 4 hours. If a provider is unavailable, unconfigured, credential-mismatched, or temporarily down, leave local state unchanged, record/log the failure, and retry on the next pass.
- Add durable per-merchant/provider/account/domain watermarks for event refresh. Advance a watermark only after a provider/window completes successfully. Never advance on partial fetch, provider error, pagination failure, or credential/account mismatch.
- Process old watermarks in bounded windows, not one giant catch-up call. Example: `next_window_end = min(watermark + 24h, now - safety_lag)`. Successful windows advance; failed windows retry unchanged.
- Extend liveness coverage for imported legacy rows that are active but stale/unknown, including rows with missing or stale period data. The importer should preserve legacy facts; Provider Refresh should fill the days/months gap after a stale dump.
- Use provider-specific refresh strategies:
  - NMI/Stripe: per-subscription liveness probes plus event-window backfill.
  - CCBill: scheduled DataLink bulk refresh, because there is no cheap per-record liveness API.
  - Solana: keep the cranker/reconcile path as the provider-refresh equivalent for on-chain recurring state.
- Keep remote provider writes out of Provider Refresh. Outbound mutations stay in the provider intent ledger.

## Tasks
- [x] Introduce Provider Refresh naming in worker/docs/logs while preserving the existing subscription liveness behavior.
- [x] Add a Provider Refresh scheduler wrapper that runs on startup and every 4 hours, with one heartbeat summary across lanes.
- [x] Move or wrap the current `SubscriptionLivenessWorker` as the `Subscription Liveness Refresh` lane.
- [x] Add durable provider event watermarks keyed by merchant, provider, provider account, and event domain.
- [x] Implement bounded event-window refresh with no watermark advancement on failed/partial provider reads.
- [x] Backfill missed provider events into local payments/refunds/disputes/subscription terminal state using existing idempotent writers.
- [x] Extend the subscription liveness cohort to include imported active rows whose provider truth is stale or unknown, including missing/stale period evidence.
- [x] Add CCBill DataLink bulk refresh to the scheduled Provider Refresh path.
- [x] After refresh writes, run scoped convergence for affected subjects so entitlements/grants/derived state follow refreshed truth.
- [x] Update operations docs to distinguish Provider Refresh, Subscription Liveness Refresh, Provider Event Refresh, and manual `pull-provider`.
- [x] Add focused tests for startup scheduling, provider-unavailable retry, watermark non-advancement, bounded catch-up, and stale legacy import catch-up.

Validation: `task sqlc`; `go test ./internal/app ./internal/river ./internal/db/repo ./internal/migrate ./migrations/postgres`; `go test -tags=integration ./internal/river -count=1`; `git diff --check`.

## Acceptance
- A stale legacy dump imported days or months after export can be corrected automatically without an operator running `pull-provider`.
- Provider Refresh runs on startup and every 4 hours.
- Provider outages or missing credentials never mutate local billing state and never advance event watermarks.
- Missed webhook/event data catches up automatically from the last successful watermark in bounded windows.
- Subscription liveness remains read-only at the provider and can repair stale active subscriptions by reading provider truth.
- `pull-provider` remains available as the manual full-surface batch operation, but routine provider catch-up does not depend on it.
- `ConvergeEngine` remains local-only; network/provider reads live only in Provider Refresh lanes.

---

# #573: Simplify reconciliation findings ledger shape

**Completed:** yes
**Status:** DONE 2026-06-24 (Codex). The `openrails.reconciliation_findings` ledger now uses the simplified workflow/schema shape. The table no longer needs `provider='self'`, `requires_admin`, split evidence columns, `first_seen_at`, or `occurrence_count`; provider context lives in `evidence.provider` only for provider-backed findings, and old rows migrate into nested `evidence` payloads. Human-review rows use `requires_review`, terminal rows require `resolved_at` plus a resolution reason, and CLI/log/report output now uses the new review vocabulary.

## Proposed Changes
- Drop `requires_admin`; status owns workflow. Rename the human-review state to `requires_review` and index that status directly for fast queue reads.
- Drop top-level `provider` unless a concrete `pull.*` consumer still needs it. Prefer encoding provider/account identity into `subject_key` and/or `evidence` for `pull.*` findings. Internal `derive.*`, `life.*`, and `consistency.*` findings should not need a `self` sentinel.
- Collapse `local_evidence`, `remote_evidence`, `intent_evidence`, and `resolution_evidence` into one nullable `evidence jsonb`. Use nested keys (`local`, `remote`, `intent`, `resolution`) only when a finding actually has those distinct payloads.
- Drop `first_seen_at`; it duplicates `created_at` and/or `first_seen_run -> reconciliation_runs.started_at`.
- Decide `last_seen_at` based on run integrity: keep it only as a denormalized queue sort column, or drop it after making `first_seen_run` / `last_seen_run` real foreign keys and deriving dates from runs.
- Drop `occurrence_count` unless a real alert/escalation rule uses it. `first_seen_run` and `last_seen_run` are enough to tell whether the same finding reappeared across runs.
- Keep `resolved_at`, and make it mandatory-by-invariant for terminal statuses such as `auto_fixed`, `fixed`, and `ignored`. Backfill existing terminal rows.
- Review `resolution` and `notes`: keep one nullable operator-facing text field (`operator_notes`) for manual review, and keep a separate machine-readable resolution reason only if reports need multiple reasons under the same terminal status.

## Tasks
- [x] Inventory current writer/reader usage of every reconciliation finding column and identify generated sqlc/API/report structs that need a shape change.
- [x] Add a migration that backfills terminal rows with `resolved_at`, rewrites evidence into `evidence jsonb`, and removes redundant columns in one narrow pass.
- [x] Update the unique identity/indexes after removing `provider`; likely `UNIQUE (merchant_id, finding_type, subject_key)`.
- [x] Rename the human-review status (`admin_pending` / `admin_required`) to `requires_review`.
- [x] Update admin/review queue queries to filter by status only; no `requires_admin` predicate.
- [x] Add/replace the review queue partial index so `WHERE status = 'requires_review'` is fast.
- [x] Update convergence and provider-pull writers to populate the simplified evidence shape and terminal `resolved_at`.
- [x] Update CLI/report/API output labels so operators still see payment IDs, customer/product identifiers, provider details for `pull.*`, and manual notes where relevant.
- [x] Regenerate sqlc and update focused tests for migration, finding upsert/reopen behavior, terminal timestamps, review queue filtering, and evidence payloads.

Validation: `go test ./cmd/openrails ./internal/migrate ./internal/reconcile ./internal/reconcile/converge ./internal/river`; `go test -tags=integration ./internal/reconcile ./internal/reconcile/converge ./internal/river`; `task sqlc`; `git diff --check`.

## Acceptance
- No reconciliation finding row needs `provider='self'`, `requires_admin`, four separate evidence columns, `first_seen_at`, or `occurrence_count`.
- `auto_fixed`, `fixed`, and `ignored` findings always have `resolved_at`.
- Human-review findings still carry the evidence needed to act, especially duplicate-charge payment IDs, amounts, customer, product, provider/account context, and timestamps.
- Review/admin queue behavior is fully determined by `status = 'requires_review'`, with no boolean mirror.
- Existing rows migrate deterministically without losing useful evidence.

---

# #572: Harden provider-pull prune with snapshot completeness guarantees

**Completed:** yes
**Status:** DONE 2026-06-24 (Codex). `openrails pull-provider --prune` stays a single operator flag, but internally it now prunes only domains whose provider snapshot is provably complete for the relevant scope. Subscription prune requires an exhaustive provider roster. Payment prune requires a complete paginated transaction export for the exact `[since, until]` window. Unsafe domains are counted as `skipped`, and the `.log` keeps specific skip reasons such as `snapshot_not_complete`, `protected_dependents`, or `grant_ledger_entangled`.

## Proposed Changes
- Added an explicit provider snapshot completeness contract to `reconcile.RemoteSnapshot`, with per-domain coverage for subscription roster completeness, transaction window `since/until`, and completed pagination.
- `--prune` checks the contract before deleting. A domain that lacks proof is skipped, never deleted.
- Kept one `--prune` flag. Internally, subscriptions prune only when the subscription roster is exhaustive, and payments prune only when transaction export coverage is exhaustive for the same `[since, until]` window.
- Hardened NMI by paging `SearchTransactions` with `result_limit`/`page_number` and paging recurring subscription queries.
- Kept local dependency safety gates: rows absent from the provider but entangled with grants/refunds/checkout sessions remain skipped rather than hard-deleted.
- Pull-provider stdout summarizes prune `deleted` / `skipped` by table, while the `.log` records specific reasons such as `snapshot_not_complete`, `protected_dependents`, or `grant_ledger_entangled`.
- Added tests for Stripe coverage metadata, NMI paginated completeness, incomplete snapshot refusal, bounded payment-window prune, and skipped-reason logging.

## Tasks
- [x] Add per-domain snapshot coverage/completeness fields and propagate them from each provider fetcher.
- [x] Implement NMI transaction pagination with deterministic `result_limit` / `page_number` looping.
- [x] Decide and implement NMI recurring subscription pagination or a documented proof that the report is exhaustive without pagination.
- [x] Make prune refuse deletes for domains without completeness proof, while still logging skipped counts/reasons.
- [x] Preserve the single `--prune` flag and update stdout/log summaries to show `deleted` / `skipped` by table.
- [x] Add focused unit and integration coverage for completeness gates, window scoping, NMI pagination, and account-bound prune isolation.

Validation: `go test ./cmd/openrails ./internal/reconcile`; `go test -tags=integration ./internal/reconcile -run 'TestPruneProviderAccountExcess' -count=1`; `go test ./cmd/openrails ./internal/migrate ./internal/reconcile ./internal/river`; `task sqlc`; `git diff --check`.

## Acceptance
- `--prune` never deletes payments unless the provider transaction snapshot is complete for the exact local payment window being considered.
- `--prune` never deletes subscriptions unless the provider subscription snapshot is an exhaustive roster for that provider account.
- Incomplete provider coverage results in `skipped`, not deletion, with row/domain-level reasons in the `.log`.
- Stripe keeps paginated prune behavior over bounded windows; NMI prune is safe against truncated first-page exports.
- Pulls for separate provider accounts, including two Stripe accounts, remain isolated by `provider_account_id`; null-bound legacy rows are not pruned.

---

# #571: Pull-provider writes operator change-log artifact

**Completed:** yes
**Status:** DONE 2026-06-24 (Codex). `openrails pull-provider` no longer dumps the detailed audit to stdout. Each run writes a plain `.log` file under `--log-dir` (default `openrails-pull-provider-logs`) with timestamped logfmt-style events for run metadata, findings, planned local mutations, applied local mutations, prune deletes/skips, converge results, and final summary. Stdout now stays concise: run id/status, finding status counts, planned/applied row counts by table and operation, converge counts, and the log path.

## Tasks
- [x] Add a default pull-provider `.log` path with an override flag.
- [x] Include provider-pull run metadata, findings, planned local mutations, applied local mutations, prune deletions, and post-pull converge summary as one-line log events.
- [x] Make stdout concise: run id, findings counts, created/updated/deleted row counts by table, converge counts, and log path.
- [x] Cover the artifact writer / summary behavior with focused tests and run targeted validation.

Validation: `go test ./cmd/openrails ./internal/migrate ./internal/reconcile ./internal/river`; `go test -tags=integration ./internal/reconcile -run TestPruneProviderAccountExcess -count=1`; `git diff --check`.

---

# #570: Rename reconciliation finding lifecycle statuses to product workflow terms

**Completed:** yes
**Status:** DONE 2026-06-24 (Codex). User-facing finding lifecycle names now match the actual operator workflow and avoid ambiguous limbo states. Replaced the old status vocabulary:
- `resolved` -> `fixed`
- `dismissed` -> `ignored`
- `held` -> `reconcile_required`
- `admin_pending` -> `admin_required`
- eliminate `open` as an emitted standing state; findings that previously landed as `open` must be classified as either `reconcile_required` when blocked on source/provider reconciliation, or `admin_required` when a human/operator must act.

Keep `auto_fixed` as the automatic-repair status. `fixed` and `auto_fixed` must retain structured resolution evidence describing what changed or why the finding no longer applies. Existing `resolution` values should be reviewed/renamed where needed so reports can distinguish `auto_vanished`, `enforced`, admin-confirmed repair, and ignored findings without relying on vague status names.

## Tasks
- [x] Define the canonical status set and transition rules: `auto_fixed`, `reconcile_required`, `admin_required`, `fixed`, `ignored`.
- [x] Update Postgres constraints and migrations without losing existing rows; map old values to new values during migration.
- [x] Update sqlc queries/generated code and reconciliation store helpers so dismissed/ignored identities stay ignored across reruns, while fixed/auto_fixed findings reopen if the same issue reappears.
- [x] Update convergence engine status returns: confirmed-absence gate returns `reconcile_required`; human/operator findings return `admin_required`; no path should emit `open`.
- [x] Ensure `auto_fixed` and `fixed` rows carry `resolution_evidence` or equivalent structured context describing the local repair, admin repair, or vanished condition.
- [x] Update CLI/admin report filters, labels, and tests to use the new vocabulary.
- [x] Verify with focused reconciliation/convergence tests plus build/sqlc gates.

Validation: `task sqlc`; `go test ./internal/migrate ./internal/reconcile ./internal/river`; `go test -tags=integration ./internal/reconcile`; `go test -tags=integration ./internal/reconcile/converge ./internal/river`; `git diff --check`. Added a migration rewrite regression for `025_reconciliation_status_workflow_terms.up.sql` so configured Postgres schema names still relocate the canonical `openrails` qualifiers before apply.

## Acceptance
- No active code path writes `open`, `resolved`, `dismissed`, `held`, or `admin_pending` as a reconciliation finding status.
- Existing DB rows migrate deterministically to the new terms.
- Repeated sightings of an ignored finding remain ignored and increment recurrence metadata; repeated sightings of fixed/auto_fixed findings reopen with the newly computed status.
- Operators can tell from status plus resolution evidence what was automatically fixed, what needs reconcile first, what requires admin action, what was fixed, and what was intentionally ignored.

---

# #568: cycle-free entitlements wiring for embedded billing — read-client split (A) + post-construction hook (B)

**Completed:** yes
**Status:** DONE 2026-06-22 (Claude) via Option B. The authkit dependency shipped as authkit #112 (`SetEntitlementsProvider`, authkit v0.48.0). The holder is now removed from BOTH hosts with NO openrails-package change: each builds auth → builds the embedded engine → `authSvc.SetEntitlementsProvider(engine.EntitlementsProvider())` before serving. doujins dropped its `deferredEntitlements` holder; hentai0 dropped the `SetClient`/atomic late-bind (its `OpenRailsEntitlementsProvider` now takes the client at construction). Option A (split an auth-free `openrailsembed.NewClient` read-client from `NewEngine`) and the optional openrails-side `AttachEntitlements` sugar were NOT needed and remain PARKED — open a fresh issue if a host ever wants pure-constructor wiring instead of the sanctioned post-construction setter.

## Background: the embedding cycle
Embedding OpenRails billing in a host creates a bidirectional dependency:
- **Billing needs auth:** `openrailsembed.New` builds the runtime via `embed.New(...)` with the host's `Verifier` + `AuthKitCore` as the request `Authenticator` / `DelegatedAuthenticator` — every billing request authenticates through the host's authkit.
- **Auth needs billing:** the host's authkit stamps minted tokens with entitlements from the engine's `EntitlementsProvider()` (which wraps `rt.Client()`).

So `auth → engine → entitlements → auth`: neither is fully buildable before the other. Under authkit ≤v0.46 the host broke it with post-construction `svc.WithEntitlements(provider)`. authkit v0.47.0 made construction options-only, so doujins (`internal/server/server.go`) and hentai0 (`internal/infra/authkit.go`) now carry a `deferredEntitlements` holder — an empty box passed at construction, `.Set()` after the engine is built, before first mint. Correct (set-before-mint invariant) but a host-side mutable seam we want to delete.

## Option A — split the read-client from the auth-bearing engine (openrails-only; no authkit change)
The entitlements provider needs only a DB read client, never Verifier/Core. Expose that half standalone so construction is a straight line:
```go
billing := openrailsembed.NewClient(ctx, cfg, pg)               // DB reads only; no auth
svc := authhttp.NewServer(cfg, pg,
    authhttp.WithEntitlements(billing.EntitlementsProvider()))  // real provider at construction
ors := openrailsembed.NewEngine(ctx, cfg, billing,              // request engine, now with auth
    Deps{Verifier: svc.Verifier(), AuthKitCore: svc.Core()})
```
Cycle gone: entitlements half is auth-free; only request-handling needs auth. Pure constructor wiring, no mutation.

## Option B — post-construction entitlements hook (PREFERRED; needs an authkit addition)
Keep the natural order (auth → engine), then attach the engine's entitlements to the already-built auth service through a SANCTIONED authkit seam:
```go
svc := authhttp.NewServer(cfg, pg)                              // no entitlements yet
ors := openrailsembed.New(ctx, cfg, Deps{Verifier: svc.Verifier(), AuthKitCore: svc.Core()})
ors.AttachEntitlements(svc)                                     // openrails hook -> svc.SetEntitlementsProvider(...)
```
authkit exposes ONE blessed post-construction setter (`(*core.Service).SetEntitlementsProvider` + `authhttp` equivalent) — the single deliberate exception to #108's options-only rule, justified by the inherent cycle and the fact authkit reads the provider LAZILY at mint time. openrails exposes a thin `(*Service).AttachEntitlements(sink)` that installs `EntitlementsProvider()` into it. Replaces the host-side holder with a library one-liner.

## Why it matters
- Deletes the `deferredEntitlements` holder (~30 lines) from BOTH doujins and hentai0 — the "no hacks" win.
- Makes the cycle library-supported and explicit instead of a host-owned mutable box guarded by a "set before mint" comment.
- A and B are complementary, not either/or: A suits hosts wanting pure constructor wiring; B suits hosts preferring the natural auth→engine order with one explicit attach call.

## Tasks
- [ ] Option A: add `openrailsembed.NewClient(ctx, cfg, pg)` (DB read client + `EntitlementsProvider()`, no auth) and `NewEngine(ctx, cfg, client, Deps{Verifier, AuthKitCore})`; refactor `New(...)` into `NewClient` → `NewEngine` (keep `New` as a back-compat wrapper).
- [ ] Option B (openrails side): add `(*Service).AttachEntitlements(sink)` that installs `EntitlementsProvider()` via the authkit setter; define the minimal sink interface openrails depends on.
- [ ] Option B (authkit dependency — file a separate authkit issue): add the sanctioned post-construction entitlements setter + sink interface; document it as the one cyclic-dependency exception to #108. Bump+tag authkit before openrails ships B.
- [ ] Migrate doujins (`internal/server/server.go`): drop `deferredEntitlements`; adopt the chosen option (B per preference).
- [ ] Migrate hentai0 (`internal/infra/authkit.go`): same.
- [ ] Tests: minted tokens carry entitlements after wiring under both options; no token is mintable before entitlements are attached (B); 3-repo build green; bump+tag openrails + consumer bumps.
- [ ] Docs: embedding guide / README shows both wiring options.

---

# #567: adopt authkit permission-group model (authkit #111) — merchant + customer personas (no org)

**Completed:** yes
**Status:** COMPLETED 2026-06-24 (Codex). OpenRails is on authkit v0.62.0 plus a local AuthKit workspace change exposing `WithPermissionGroupAuthorizer` for generated group routes. The merchant side, no-`org` route model, permission catalogs, group-scoped auth checks, permission-group credential identity, #569 resource-scope removal, lazy customer group materialization, generated customer management routes, and root/operator boundary proof are implemented. This pass enabled customer remote-application management in the declared AuthKit `customer` profile, added idempotent `EnsureCustomerPermissionGroup(customerID, ownerSubject)`, mounts AuthKit's generated permission-group routes in standalone, and lazily materializes the customer group both on standalone customer spend-delegation writes and on AuthKit-generated self-addressed customer management routes (`members`, `api-keys`, `remote-applications`). Bootstrap and integration harness API-key fixture minting were also updated for AuthKit v0.62's actor-required API-key minting: OpenRails creates/reuses real passwordless actors, grants them merchant owner, and passes `CreatedBy` when minting. `openrails.customers` remains the payable-row source of truth; AuthKit group state is created only once the customer chooses to delegate/manage credentials. Tensorhub owns its account UX/API that attaches `cozy.art` (or another remote app) to a Tensorhub account and syncs that policy into OpenRails (tracked as tensorhub #501).

**Re-path + DB migration DONE 2026-06-22 (Claude).** The two deferred items are complete (build/vet incl. `-tags=integration` + unit tests green):
- **Org-treasury → customer surface re-path (BREAKING, no alias).** Every `/v1/orgs/:org_id/*` route is now `/v1/customers/:customer_id/*`. `OrgRoutePrefix`(`/orgs`)→`CustomerRoutePrefix`(`/customers`); `RegisterOrgTreasuryRoutes`→`RegisterCustomerTreasuryRoutes`; middleware `OrgTreasuryScopeRequired`/`OrgIDMatchesDelegated`→`CustomerScopeRequired`/`CustomerIDMatchesDelegated` (rebind logic kept; `org_scope_mismatch`→`customer_scope_mismatch`); handlers `Get/PutOrgSpendDelegations`→`Get/PutCustomerSpendDelegations`. Files renamed: `ginmw/org_treasury.go`→`customer_treasury.go`, `handlers/org_spend_delegations.go`→`customer_spend_delegations.go`, the two `tests/org_treasury_*`→`customer_treasury_*`. Both transports (`routes_self.go`, `pkg/embedded/gin/self.go`) + the embedded mount (`/billing/v1/customers/*`) re-pathed. SDK re-pathed too: `PolicySyncClient.SetOrgSpendDelegations`→`SetCustomerSpendDelegations` (param `orgID`→`customerID`), `remote.go` hits `/v1/customers/:id/spend-delegations`, `embed/client.go` localClient renamed. The customer persona is UNIVERSAL — no "personal balances are never delegable" guard remained (it was structural: spend-delegations only ever existed on this surface; the body `customer_id` is still rejected, scope is forced from the path). README + catalog comments updated.
- **DB migration `023_merchant_permission_group_id.up.sql`** DROPs `uq_merchants_owner_org_id` (the #527 1:1 index), RENAMEs `merchants.owner_org_id`→`merchants.permission_group_id` (now the merchant's OWN group id, not a parent org) + renames `idx_merchants_owner_org_id`→`idx_merchants_permission_group_id` + refreshes the column comment. Go side renamed in lockstep: raw SQL strings (`controlplane/{api_key,bootstrap,issuer_registry}.go`, `merchants/lifecycle.go`, `bootstrap/merchant_manifest.go`, `integrationharness/harness.go`) `owner_org_id`→`permission_group_id`; Go identifiers `Merchant.OwnerOrgID`/`ProvisionRequest.OwnerOrgID`(json `permission_group_id`)/`ErrOwnerOrgRequired`→`ErrPermissionGroupRequired`/`merchantForOwnerOrg`→`merchantForGroupID`/`AuthorizeMerchant` param. The sqlc-gen `OpenrailsMerchant.OwnerOrgID`→`PermissionGroupID` hand-edited to match a future regen (sqlc `db-prepare` needs a live PG; no query SELECTs the column so it's struct-only). INFRA-GATED: sqlc regen + all `//go:build integration` DB/Redis tests (incl. the re-pathed `customer_treasury_*` tests, migration apply, merchants/controlplane/bootstrap integration) need a live Postgres to actually RUN — verified compile + vet only here.

## Principle
OpenRails has **NO `org` persona at all** (the old org↔merchant coupling is gone — supersedes #527's `owner_org_id` UNIQUE). Per the `<persona>:<resource>:<action>` naming (authkit #111, persona ≡ type ≡ namespace), OpenRails has exactly two personas — `merchant` and `customer` — both FLAT top-level types under `root`:
- **`merchant`** — a merchant IS a top-level permission-group (`type=merchant`, child of `root`). Creating a merchant creates the group directly — no parent org, no `owner_org_id`. Staff roles owner/support/viewer; `merchant:*` lives here; `/v1/merchant/*` gates resolve here.
- **`customer`** — a payer with an API balance. **Every customer is the same `customer` persona — there is NO 'org customer' vs 'individual customer' distinction; ALL customers can delegate the spending of their balance** (configure invoker/member spenders + budget windows). Holds `customer:*` (the #566 payer/treasury perms + spend-delegations). A lone customer is just the `owner`; a co-managed/shared balance adds members. `/v1/me/*` is a customer acting on their own balance; the #566 treasury + delegation routes are the `customer` surface.

So OpenRails' tree is `root → { merchant groups, customer groups }` — flat, no nesting, and no `org`. (`org` is a *tensorhub* persona, not OpenRails'; doujins/hentai0 have no persona beyond `root`.) `root` is reached only for platform moderation.

**Reach ≠ capability** (authkit #111): `root` can delete/moderate orgs+merchants but cannot run their internals, and a merchant cannot impersonate its customers — enforced by the disjoint per-type catalogs + the no-`*` namespace-anchored-glob rule, not by structural superset.

OpenRails uses **app-defined role catalogs, NOT custom roles** (`AllowCustomRoles=false` on every type; custom roles + deep hierarchy are tensorhub's domain). Two catalogs to declare:

**`merchant` type** — `owner` + 2, kept minimal:
- `owner` *(required)* — all `merchant:*`; the only role that touches config (`settings`, `payment-providers`, `catalog`) and the machine path (`admissions`).
- `support` — customer-facing ops: `merchant:customer-settings:read|update`, `merchant:payments:read|refund`, `merchant:subscriptions:read|update`, `merchant:usage:read`, `merchant:repair-alerts:read`. (Asymmetry in action: `subscriptions:update` lets support CANCEL a customer's subscription, but the #554 catalog has NO `merchant:subscriptions:create`, so neither support nor owner can create one as the customer.)
- `viewer` — read-only: every `merchant:*:read`. Finance / audit / analyst.

**`customer` type (universal — every payer, lazily materialized in AuthKit)** — holds `customer:*` ONLY (does NOT own merchants, so no `merchant:*`):
- `owner` *(required)* — the customer; full `customer:*` (manage the balance, payment methods, invoices, spend-delegations, and any co-managers/members). The AuthKit group is created on first customer-owned group/delegation action, not on every `openrails.customers` row creation.
- `member` — a delegated spender. Membership/credentials say who may be associated with the customer balance; OpenRails spend-delegation windows say how much they may spend. Budget enforcement stays OpenRails-side.
- `remote_application` delegates are a HARD REQUIREMENT: a customer account must be able to attach a remote application to its OWN customer group, grant that remote app the spender/member role, and attach OpenRails budget windows for that remote-app principal. OpenRails must support this as a host-agnostic customer delegate primitive; Tensorhub wires the concrete Cozy Art flow where a Tensorhub account attaches the `cozy.art` remote application so the site can spend Tensorhub API balance on that account's behalf.

`AllowCustomRoles=false`. Every customer is eligible for this persona, but the AuthKit group is lazily created when needed. A lone customer is just the `owner`; a shared/co-managed balance adds members/API keys/remote apps. Spend budget windows remain OpenRails policy, not AuthKit role data.

## Tasks
- [x] Bump authkit to the #111+ release; adopt the group-scoped authorize path. DONE: OpenRails is on authkit v0.62.0; `HasAdminPermission` is now a compatibility seam over `Core().Can(ctx, user, user-kind, merchant, merchant-ref, perm)`, and `merchantActionPermissionMW` resolves user sessions to the live merchant group instead of trusting an org claim.
- [x] Merchant creation = create a `merchant` permission-group directly (child of `root`, NO parent org, NO `owner_org_id`); the creator/operator becomes the merchant `owner`; mint the admin api-key nested under the merchant group. Drop the `uq_merchants_owner_org_id` 1:1 index + the org-ownership coupling (#527). **DB migration 023 DONE** — drops `uq_merchants_owner_org_id`, renames `owner_org_id`→`permission_group_id` (merchant's own group id).
- [x] Customer persona (universal, lazy): a payer gets a `type=customer` permission-group (child of `root`) only when it first manages delegation/credentials. `owner` = the customer, `member` = delegated spender. NOT an opt-in 'org' — a lone customer can remain just an `openrails.customers` payable row until it delegates. DONE: the `customer` type/catalog and `/v1/customers/:customer_id/*` treasury surface exist, `RemoteAppRegistration` is enabled on `CustomerType`, every customer can delegate spend via OpenRails spend-delegation windows, standalone `PUT /v1/customers/:customer_id/spend-delegations` lazily materializes the customer group with the delegated subject as owner, and AuthKit-generated customer `members`, `api-keys`, and `remote-applications` routes can be the first materializer when the authenticated user addresses their own customer group. Remote-app support is generic OpenRails behavior; the Tensorhub/Cozy Art account-attachment and policy-sync flow is tracked in tensorhub #501. Do not add an eager backfill unless a migration later needs it.
- [x] **Collapse the org-treasury surface into the customer surface — the API gets SMALLER (one persona, not org+customer).** DONE: `/v1/orgs/:org_id/*` retired; the customer payer surface is `/v1/me/*` (self) + `/v1/customers/:customer_id/*` (co-managed/shared). The FULL #566 payer subset — balance, transactions, settings, payment-methods, checkout, invoices — PLUS spend-delegations all live on the ONE customer surface as `customer:*` perms; no separate org bucket. `OrgTreasuryScopeRequired`→`CustomerScopeRequired` keeps the payer-rebind verbatim; only the path + scope-param name changed (`:org_id`→`:customer_id`, `org_scope_mismatch`→`customer_scope_mismatch`). No "personal balances never delegable" guard existed to remove (it was structural — spend-delegations only ever lived on this surface).
- [x] **Re-path the openrails wire (BREAKING, no alias) — openrails side DONE.** `GET/PUT /v1/orgs/:org_id/spend-delegations` + the embedded `/billing/v1/orgs/*` mount are now `/v1/customers/:customer_id/*`; openrails SDK (`PolicySyncClient.SetOrgSpendDelegations`→`SetCustomerSpendDelegations`, `remote.go`, `embed/client.go`) re-pathed. **Tensorhub + doujins consumer re-wire is the still-open downstream half** (tensorhub `openrailsclient` `embeddedOrgsPrefix` bump → **tensorhub #499**; doujins' embedded `/billing/v1/orgs/*` mount + clients). Version-bumped hard cut, no alias.
- [x] Declare BOTH fixed type catalogs to authkit (`AllowCustomRoles=false`): `merchant` = `owner`/`support`/`viewer`; `customer` = `owner`/`member`. DONE in `internal/controlplane/catalog.go` `Groups()`.
- [x] `/v1/merchant/*` gates resolve at the **merchant** group (`merchant:*`); the customer balance/delegation surface gates on `customer:*`. DONE for the implemented surfaces: merchant browser sessions resolve group membership live; service credentials/delegated JWTs are group-bound; customer treasury routes use `CustomerScopeRequired` + `RequirePermission`.
- [x] Re-nest remote_applications + api-keys under the permission-group (was org); confirm `ResolveRemoteApplicationAuthority` still resolves authority via the group + parent walk (it feeds the delegated/service-JWT verifier — keep the #564 bound intact). DONE with #569: credential identity is `PermissionGroupID`, no resource scopes.
- [x] Drop `OwnerOwnsAppResources=true` (set in `service.go` today for the org→merchant coupling, authkit #100). DONE: `internal/controlplane/service.go` declares `RBAC.Groups` and does not set `OwnerOwnsAppResources`.
- [x] Root (operator) boundary: the old `platform:` plane is AuthKit `root:` under #111; root owners can hold `root:merchants:*` moderation authority, but root authority cannot run merchant internals and merchant owners cannot reach root moderation. DONE: `TestRootOperatorBoundary_ReachNotMerchantCapability`.
- [x] Tests: merchant + customer gates pass/deny correctly under the group model; owner auto-holds merchant:*/customer:*; cross-merchant isolation holds; root/operator boundary is explicit; the #564 uniform-auth parity tests stay green. DONE: catalog/bootstrap/API-key/customer-treasury/delegation coverage exists; added catalog coverage for customer remote-app route exposure, Postgres-backed coverage for lazy customer group creation/idempotence/owner perms, generated-route first-write coverage proving `/customer/{user_id}/remote-applications` creates the customer group and registers `cozy.art` under it, and root boundary coverage.
- [x] Update embed + standalone bootstrap. DONE for merchant/bootstrap, `/billing/v1/customers/*` mounting, customer permission-group lazy materialization, and AuthKit v0.62 actor-required API-key minting.

## Acceptance
- OpenRails consumes the permission-group API; org-scoped calls are gone. OpenRails has NO `org` persona — only `merchant` + `customer`.
- A `merchant` is a top-level permission-group (`owner`/`support`/`viewer`, `merchant:*`); `customer` is a SEPARATE top-level persona (`owner`/`member`, `customer:*`) lazily materialized when a payer delegates/manages customer credentials; fixed catalogs, `AllowCustomRoles=false`; owner auto-holds; customer remote applications are supported as budgeted delegated spenders.
- Every customer (lone or shared) can delegate spend of their balance — no org-vs-individual distinction. No 'org' owns a merchant (supersedes #527; drops `owner_org_id`/`uq_merchants_owner_org_id`). Tree: `root → { merchant, customer }`, flat, no nesting.
- All existing merchant/customer/admin auth tests + #564 parity stay green against the new authkit.

## Note
OpenRails is the shallow/simple adopter (fixed catalogs, no custom roles, two flat top-level personas — `merchant` + `customer` — under `root`, no nesting, no `org`). It proves authkit #111 works for the flat case and that there's no built-in `org`; the deep features (nested per-resource groups, custom roles, the `org` persona) land in tensorhub.

**Two route surfaces per merchant (don't conflate):** (1) authkit AUTO-GENERATES the staff/credential MANAGEMENT routes from the `merchant`/`customer` profiles — `/merchant/:id/{members,api-keys,remote-applications}`, `/customer/:id/{members,api-keys,…}` (disabled capabilities aren't generated). OpenRails mounts these and builds none of them. (2) OpenRails' OWN DOMAIN routes — `/v1/merchant/*` (catalog/payments/admissions, the #564 unified-auth surface) + the customer balance/checkout/spend-delegations surface. Both gate on the SAME `merchant:*`/`customer:*` perms (persona ≡ namespace), so a `support`/`owner` role works across both.

---

# #566: org-as-payer treasury routes — extend /v1/orgs/:org_id/* to the full payer subset of the customer surface

**Superseded by #567 (2026-06-22):** the implementation (shared `/me` money handlers + the scope-rebind middleware) is REUSED, but re-pathed off `/v1/orgs/:org_id/*` onto the universal `customer` persona surface (`/v1/customers/:id/*`). `org` naming is retired and the payer surface is no longer "org-only" — EVERY customer gets it. **RE-PATH LANDED 2026-06-22 (Claude):** routes/middleware/handlers/SDK all `org`→`customer` (`org_scope_mismatch`→`customer_scope_mismatch`); build/vet/unit green (DB integration tests infra-gated). The remaining tensorhub/doujins consumer wire bump is tracked under #567 (tensorhub #499). The design below is historical (org framing); the handlers/perms survive.

**Completed:** yes
**Status:** DONE 2026-06-22 (Claude). Follow-on to #557. Builds on the #554 `customer:*` catalog. Implemented as a group-level `ginmw.OrgTreasuryScopeRequired` middleware that scope-checks `:org_id` and REBINDS the acting payer subject to the org's payable subject (the merchant/org uuid), so the SHARED `/v1/me` money handlers operate on the org balance unchanged — perms stay on the resolved principal (`PrincipalContextKey`), so the rebind never widens authority. Both transports inherit the routes (both call `RegisterOrgTreasuryRoutes`). 5 integration tests green over HTTP against the real DB + Redis.

**Two implementation decisions (deviations from the literal target sketch, grounded in the principle):**
- **`status` NOT mounted.** The only `/me` status handler (`GetMyBillingStatus`) returns subscriptions + entitlements — consumer concepts the org does not own (and the EXCLUDED list names). The payer's account state is already exposed by `balance` + `settings`. (`status` was in the target sketch but NOT the acceptance criteria.)
- **Billing-mode (prepaid/arrears) is merchant-granted, NOT customer self-service.** The shared `SetMyCreditAccountSettings` deliberately refuses platform-owned policy fields (the existing `/me` invariant; only the service/merchant `serviceAccountSettingsRequest` carries `billing_mode`). So `PUT /v1/orgs/:org_id/settings` sets self-imposed caps + auto-topup; the org's arrears tab is granted by the merchant via `merchant:customer-settings:update`. Honors "reuse handlers, no new handler logic".

## Principle
An org is a **payer**, not a **consumer**. It holds an API-credit balance, pre-pays, owes in arrears, has a billing mode, gets invoices, and sets payment methods — but it does NOT itself own catalog products, entitlements, or personal subscriptions (its *members* consume; the org *funds*). So the org surface should mirror the **payer subset** of `/v1/me/*`, scoped to the org's payable subject, and expose NONE of the consumer routes.

This is modeled as **separate resource-scoped routes** under `/v1/orgs/:org_id/*` — NOT by shoehorning an org-target into the personal `/v1/me/*` routes. Rationale: (1) the payer SUBJECT differs (org payable subject vs token `delegated_sub`); `/v1/me` is deliberately self-only with no `customer_id` param, and adding an "act-as-org" param would break that invariant and invite confused-deputy bugs; (2) the AUTHZ differs (`/v1/me` needs no permission — authenticated self; the org surface needs `customer:*` because the org balance is a SHARED resource); (3) the org gets a strict SUBSET (no products/entitlements/subscriptions) which would mean conditionally disabling routes if folded into `/v1/me`. The route path NAMES the scope (`/v1/orgs/:org_id`), which is exactly the scoped-RBAC model and generalizes to future `/v1/<resource>/:id` surfaces. Handlers are SHARED (same money logic, payer resolved from the scope), so this is route separation, not handler duplication.

## Target surface (additive to the existing `spend-delegations`)
```
GET   /v1/orgs/:org_id/balance | transactions | status | usage | payments
GET   /v1/orgs/:org_id/invoices | /invoices/:id
PUT   /v1/orgs/:org_id/settings                 # billing mode (prepaid|arrears), caps
GET.. /v1/orgs/:org_id/payment-methods[/:id]    # list/create/update/delete
POST  /v1/orgs/:org_id/checkout | /checkout/:id | /checkout/:id/confirm   # pre-pay / load credits
POST  /v1/orgs/:org_id/stripe/portal
GET/PUT /v1/orgs/:org_id/spend-delegations      # done in #557
```
EXCLUDED (consumer-only, stay on `/v1/me/*`): `products`, `products/:id/access`, `entitlements/active`, `subscriptions/*`, `notifications`.

## Tasks
- [x] Factor the payer-subject resolution so each `/v1/me/*` money handler works for BOTH surfaces. DONE differently/better than planned: instead of editing each handler, `ginmw.OrgTreasuryScopeRequired` rebinds the acting `UserContext.UserID` to the org payable subject (`resolved.MerchantID.UUID()`) — so `selfAccountPayer`/`r.GetUser()` resolve the org payer with ZERO handler changes. `CustomerIDFromString(merchantUUID)` == the customer id the spend-delegations store keys on. The scope check is the shared `ginmw.OrgIDMatchesDelegated` (403 `org_scope_mismatch`); the handler's `orgIDMatchesResolved` now delegates to it (one impl).
- [x] Mount the payer-subset routes under `/v1/orgs/:org_id/*` on BOTH transports — added to `RegisterOrgTreasuryRoutes`, which BOTH the standalone gin (`routes_self.go`) and the embedded (`pkg/embedded/gin/self.go` `SelfHandler`) already call. Reuses the existing money handlers, no new handler logic.
- [x] Do NOT mount the consumer routes — only the payer subset is mounted; `TestOrgTreasuryPayerSurface_NoConsumerRoutes` asserts products/products/:id/access/entitlements/active/subscriptions/notifications all 404 on the org surface.
- [x] Permission split — added `customer:balance:read` (balance/transactions/usage/payments/invoices), `customer:billing:update` (settings/caps), `customer:payment-methods:update` (payment-methods CRUD + stripe portal), `customer:checkout:create` (checkout) to the catalog; each route gated accordingly. (Note: `status` not mounted — see decision above.)
- [x] Seed the new `customer:*` perms in the catalog; org owner auto-holds them — `catalogEntries` extended; `OperatorRolePermissions() == CatalogNames()` so the owner/operator role and `OwnerOwnsAppResources` cover the new perms. `TestOperatorRolePermissions_AreOrgCatalog` + `TestCatalogPermissionsCoveredByOwnerGrant` green.
- [x] Reject targeting a personal/individual balance through the org surface — there is no `customer_id` path/body param; the payer is FORCED from `:org_id` via the rebind. The spend-delegations handlers still reject a body `customer_id` (unchanged).
- [x] Tests — `tests/org_treasury_payer_surface_test.go` (//go:build integration, real DB+Redis over HTTP): full payer loop (load credits→balance→transactions→settings→usage/payments/invoices/payment-methods→spend-delegations, all scoped to the org payable subject); permission split (balance:read token → reads OK, every write/spend 403); merchant-only token → 403 (namespace separation); cross-org → 403 `org_scope_mismatch`; no consumer routes (404).
- [x] Tensorhub use-case E2E — `TestOrgTreasuryPayer_DelegatedDrawDownE2E`: org loads credits → reads balance over HTTP → grants invoker a per-day window via `PUT spend-delegations` over HTTP → the spendgate-backed admitter (real PG+Redis, the production path) lets the invoker draw the ORG balance down within the window, blocks the breach, and an ungranted second invoker is denied (per-invoker isolation, #563). Billing-mode-via-settings replaced per the decision above (merchant-granted, not self-service).
- [x] Update hosts — no host change required: the routes are ADDITIVE and tensorhub already consumes `spend-delegations`. Tensorhub adopting org-`checkout`/`settings`/`balance` is an optional future enhancement on the tensorhub side (it currently funds org balance via the admin-funding client), not a break.

## Acceptance
- An org has the full **payer** surface at `/v1/orgs/:org_id/*` (balance, pre-pay, arrears/billing-mode, invoices, payment-methods), scoped to the org's payable subject, gated by `customer:*`.
- The org surface exposes NO consumer routes (products/entitlements/subscriptions).
- Handlers are shared with `/v1/me/*` (payer resolved from the scope), not duplicated; nothing is shoehorned into `/v1/me/*`.
- Merchant-only and cross-org credentials are rejected by permission + scope.

---

# #565: finish #564 uniform auth — glob-aware perms for ALL credential types + embedded in-process host-principal path on /v1/merchant

**Completed:** yes
**Status:** COMPLETED 2026-06-22. Uniform merchant auth is finished. `ResolvedDelegated` and `ResolvedServiceCredential` use AuthKit's namespace-glob matcher; remote-application verifier authority is fed with raw role grant tokens so stored `merchant:*` survives to OpenRails' route gate; and embedded `/v1/merchant/*` route sets accept the in-process `billingauth.DelegatedAuthenticator` host principal, including host-only `RouteSetMerchantAPI`.

**(1) Glob perms were silently restricted.** `ResolvedDelegated.HasPermission` did an EXACT string compare (and `ResolvedServiceCredential.HasPermission` likewise), so a token/key carrying a namespace glob like `merchant:*` matched NO concrete route perm and silently 403'd everywhere — even though AuthKit's verifier already lets a signer MINT a glob within its authority. DECISION (maintainer): treat every auth method equally; the MINTER chooses exact or glob (accepting a glob may expose more than strictly necessary — that is the minter's call, not a gate-enforced rule). Both `HasPermission` impls use `authbase.PermissionTokenCovers` (namespace-anchored glob: `merchant:*` covers `merchant:catalog:update`; exact still matches exact; bare `*` never matches). No credential type is special.

**(2) Embedded in-process host-principal can't reach `/v1/merchant/*`.** The neutral `merchantActionPermissionMW` branches are service-credential / control-plane `DelegatedResolver` / user-session (`AdminPermissionChecker`) — all control-plane-backed, all nil for an embedded no-control-plane host (doujins/hentai0). The gin self surface already trusts a host `billingauth.DelegatedAuthenticator` principal (`DelegatedPrincipalRequired` → `resolvedFromHostPrincipal`; #564 "in-process host is trusted, perms authoritative"), but the merchant routes don't. ADD that path so the SAME `/v1/merchant/*` routes serve API keys, control-plane JWTs, AND in-process host admins — no separate route, per #564.

## Tasks
- [x] `ResolvedDelegated.HasPermission` + `ResolvedServiceCredential.HasPermission` → `authbase.PermissionTokenCovers` (glob-aware); update doc comments (drop "exact-match / no broad grant" wording).
- [x] Preserve raw remote-application role grant tokens for the delegated/service verifier authority source so `merchant:*` is not pre-expanded away before OpenRails' glob-aware route gate.
- [x] Add shared `controlplane.ResolvedDelegatedFromHostPrincipal`; refactor ginmw `resolvedFromHostPrincipal` to use it (one impl).
- [x] `routes.Options.DelegatedAuthenticator` + an in-process host-principal branch in `merchantActionPermissionMW` (validate via the shared converter, gate on `resolved.HasPermission`).
- [x] Thread `app.DelegatedAuthenticator` through `embedhttp` (`FromApp` + `RouteSetMerchantAdmin`/`RouteSetMerchantSettings`/`RouteSetMerchantAPI` mounts) into merchant `Options`.
- [x] Tests: a host principal with `merchant:*` (glob) passes a concrete merchant route; lacking it → 403; an exact perm still works; a browser delegated token carrying a glob now passes too.
- [x] Build/vet/unit/integration validation completed. Commit/tag/push intentionally left to the release flow.

## Validation (2026-06-22)
- `go test ./internal/controlplane ./internal/http/routes ./internal/http/embedhttp -run 'TestHasPermissionGlobUniformAcrossCredentials|TestMerchantActionRoutesHostPrincipalGated|TestMerchantActionRoutesDelegatedTokenGated|TestServiceRoutesDelegatedAdmitGatedByPermission|TestEmbedded|TestHTTP' -count=1`
- `go vet ./internal/controlplane ./internal/http/routes ./internal/http/embedhttp`
- `go test -tags=integration ./tests -run 'TestHTTPHandlerOptions_MerchantRoutesAcceptHostPrincipalPermissions|TestHTTPHandlerOptions_RouteSetPresetsOverHTTPServer' -count=1 -v`
- `go test -tags=integration ./internal/integrationharness -run 'TestStandaloneMerchantAdmitAcceptsDelegatedJWTByPermissionHTTP' -count=1 -v`

## Acceptance
- A minter may put exact OR glob perms on ANY credential (delegated JWT, API key, in-process host principal); all matched identically via `PermissionTokenCovers`. No credential type silently drops globs.
- Embedded no-control-plane hosts reach `/v1/merchant/*` with their `DelegatedAuthenticator` principal, gated purely by permission.

---

# #564: unify /v1/merchant auth (no source discrimination) + least-privilege live-DB subset for remote-app-signed JWTs; retire #259 allowlist

**Completed:** yes
**Status:** COMPLETED 2026-06-21. SECURITY-CRITICAL auth half of #555/#552. `/v1/merchant/*` route access is now permission-based, not credential-source-based: API keys / remote-application service JWTs, delegated JWTs, and logged-in user access tokens all flow through `merchantActionPermissionMW`; service-route handlers no longer require a pinned service credential after the route gate has authorized a merchant principal. AuthKit, not OpenRails, performs the live stored-authority bound for remote-app-signed delegated JWTs and rejects over-claims (`permission_not_granted`). Validation: `go test ./internal/controlplane ./internal/http/middleware/ginmw ./internal/http/routes ./internal/http/routes/ginroutes ./internal/http/handlers ./pkg/catalog`; `go test -tags=integration ./internal/integrationharness -run 'Test(APIKeyCrossMerchantIsolationHTTP|RemoteApplicationSelfJWTCrossMerchantIsolationHTTP|StandaloneMerchantAdmitAccepts(DelegatedJWT|UserSession)ByPermissionHTTP|DelegatedAdminCrossMerchantIsolationHTTP|DelegatedSelfTokenSubjectIsolationHTTP|CoreDoesNotMountPlatformAdminRoutesHTTP|StandaloneMerchantCatalog(RoutesHTTP|ApplyOptionsOverHTTP|PublishHTTP)|StandaloneMerchantPaymentProviderConfigHTTP|StandaloneNoDefaultMerchantResolvesRequestScopedMerchant)' -count=1 -v`; `go test -tags=integration ./tests -run 'TestServiceAdmit_HTTP_EndToEnd|TestServiceFacadeParity|TestUnifiedBillingE2E|TestEmbeddedMerchantControlBoundary|TestAdmin(Payments|Metrics|EntitlementsSource|OffChannelPayments|ProductAccess|Subscription|UserDetail)' -count=1 -v`; `git diff --check`.

**KEY CORRECTION (2026-06-21):** the per-signer subset check does NOT need wiring in OpenRails — AuthKit's verifier ALREADY enforces it for delegated tokens (`authkit/http/verifier.go` `permissionsWithinAuthority`): it bounds the `permissions` claim to the signing remote-app's stored authority, glob-aware, wired in prod via `newDelegatedVerifier` → `WithService(Core())`. So #564 reduced to deleting the OpenRails-specific `#259` browser-safe allowlist that sat ON TOP of AuthKit's bound. **SEMANTICS = REJECT, not DROP:** AuthKit rejects an over-claiming token (`permission_not_granted`, fail-loud) rather than silently dropping excess. An OpenRails-side intersection in `ResolveDelegated` was tried then REVERTED as redundant (it re-did AuthKit's work + added a per-request DB call).

## Principle (the auth model)
Every `/v1/merchant/*` request follows ONE path, regardless of credential type:
1. The presented credential is validated — access-token (signed by OpenRails'/host's own key), API key, delegated-user JWT, or self-service JWT (the last two signed by a **remote application**).
2. The credential's permissions are determined by a **live DB check**.
3. Those perms are compared to the permission the route requires.
4. `next()` or reject.
**The credential TYPE never gates a route — only the permission does.** Making routes discriminate by auth source is wrong. There is no "browser-safe" ceiling; a browser holding any token has the same blast radius (whatever perms that token carries), and that is governed by the signer's granted authority (below).

Permission source per credential type:
- **Access token** (OpenRails/host-signed): perms = the logged-in user's live org-role perms. Cannot over-claim (we sign it).
- **API key**: perms = its stored role, resolved live (already correct today).
- **Delegated-user / self-service JWT** (remote-app-signed): perms = the JWT claim **bounded to the signing remote-app's STORED authority by AuthKit's verifier** (live DB, glob-aware). A remote-app can only put ≤ its own current authority on a JWT it signs; an over-claim **REJECTS the whole token** (`errPermissionNotGranted`, fail-loud — NOT a silent drop). Least-privilege: a leaked/stolen token can never exceed the signer's authority. Enforced by AuthKit, NOT OpenRails; #564 only removes the extra #259 browser-safe allowlist OpenRails layered on top.

## Findings / what was actually done (2026-06-21)
- **The subset bound lives in AuthKit, at verify time.** `authkit/http/verifier.go:890–900`: for a delegated token with permissions, `Verify()` resolves the signer's stored authority (`remoteApplicationAuthority` via `fedSource`/`enrich`) and runs `permissionsWithinAuthority(claim, authority)` — a claimed perm not covered by any stored grant **rejects the whole token** (`errPermissionNotGranted`). Glob-aware (`PermissionTokenCovers`): a signer holding `merchant:*` covers `merchant:catalog:update`. Wired in prod (`newDelegatedVerifier` → `WithService(authSvc.Core())`), so `principal.Permissions` reaching `ResolveDelegated` is ALREADY the authority-bounded subset.
- **OpenRails intersection reverted.** Mirroring `service_jwt.go` BND-C1 in `ResolveDelegated` was redundant (AuthKit already bounds; the extra `ResolveRemoteApplicationAuthority` call ran every permissioned request and never saw an over-claim). Reverted to `Permissions: append(nil, principal.Permissions...)`.
- **#259 allowlist deleted.** `IsDelegatedPermission` + `merchantCatalog` + `IsMerchantPermission` + `MerchantCatalogNames` removed from `catalog.go` (the latter two had NO non-test caller — the "browser-token mint set" warning did not apply). `WithPermissions(IsDelegatedPermission)` removed from `newDelegatedVerifier`. Three gate callers fixed: `ginmw/delegated.go` (host-principal path now trusts the in-process host's perms), `ginmw/principal.go` (`can` = `HasPermission`), `routes/routes.go` delegated branch (`!HasPermission(perm)`).
- **Route auth unified (parallel work on master).** `RegisterServiceRoutes` now gates every machine route via `merchantActionPermissionMW` (service-cred → delegated → user-session) + per-route perm + `MerchantDBConnMW`; the `serviceCredentialRequiredMW`/`servicePermissionMW` service-only split is deleted. The user-session branch resolves the merchant for the caller's org (`merchantOrgResolver.ResolveMerchantForOrg`) so DB scoping works for logged-in admins too.

## Tasks
**A. Per-signer subset bound — [x] DONE (via AuthKit, not OpenRails):**
- [x] Confirmed AuthKit's verifier already bounds the delegated `permissions` claim to the signer's stored authority (`verifier.go:890–900`), REJECT-on-over-claim. No OpenRails wiring needed.
- [x] Removed `WithPermissions(IsDelegatedPermission)` from `newDelegatedVerifier`.
- [x] Reverted the redundant OpenRails-side `ResolveDelegated` intersection.
- [x] Self-service tokens use the SAME `ResolveDelegated` path — covered.

**B. Delete the #259 hardcoded allowlist — [x] DONE:**
- [x] Deleted `IsDelegatedPermission` + `merchantCatalog` + `IsMerchantPermission` + `MerchantCatalogNames` from `catalog.go`; removed unused `strings` import. `MerchantCatalogNames`/`IsMerchantPermission` had NO non-test caller — warning N/A.
- [x] Fixed 3 gate callers: `ginmw/delegated.go` (host-principal path trusts in-process host perms), `ginmw/principal.go` (`can` = `HasPermission`), `routes/routes.go` delegated branch (`!HasPermission(perm)`).

**C. Unify route auth — [x] DONE (parallel work on master):**
- [x] `RegisterServiceRoutes` gates via `merchantActionPermissionMW` (service-cred → delegated → user-session) + `MerchantDBConnMW`; `serviceCredentialRequiredMW`/`servicePermissionMW` deleted.
- [x] ONE shared auth+perm path (`merchantActionPermissionMW`) for service + action routes; user-session branch resolves merchant via `ResolveMerchantForOrg`.
- [x] `/admit` etc. now reachable by any credential type holding `merchant:admissions:create` (gated by perm, not source).

**D. Tests — [x] DONE:**
- [x] Rewrote `catalog_delegated_test.go` + `delegated_test.go`: dropped the allowlist tests; assert (1) a delegated token CARRIES an in-authority perm incl. the formerly-blocked `merchant:admissions:create`, (2) an over-claim beyond the signer's authority REJECTS the whole token (reject, not drop). `intersectPermissions` stays unit-tested in `bnd_c1_permission_intersection_test.go` (service-JWT path).
- [x] Added real standalone integration coverage for delegated JWT and user-session access tokens hitting `/v1/merchant/admit` by permission: holders of `merchant:admissions:create` pass; tokens lacking it get 403.
- [x] Re-ran focused `internal/integrationharness` + `tests/` suites under the unified surface (cross-merchant isolation, remote_application service JWT, delegated admin/self isolation, catalog publish, payment-provider config, no-default-merchant, and service-admit HTTP).

**E. Ship:**
- [x] Focused unit + integrationharness + tests/ merchant auth/admission slices green; `git diff --check` green. Full `go build ./...` / `go vet ./...` were not rerun in this final auth pass.
- [x] Updated `docs/principal-boundary-audit.md` to the uniform model (credential → live/stored perms → route gate; AuthKit bounds/rejects remote-app-signed claims, no #259 allowlist).
- [x] Verified integration harness grants remote-app authority before delegated tokens carry merchant permissions. Signing merchant-frontend remote-apps MUST carry the right org-role/direct authority, or AuthKit rejects permission over-claims.

## Acceptance
- No `/v1/merchant` route inspects credential type; access is purely permission-based. ✓
- A remote-app-signed JWT's effective perms ≤ the signer's stored authority; an over-claim REJECTS the token (AuthKit, fail-loud). ✓
- `IsDelegatedPermission` + the #259 browser-safe allowlist are gone. ✓
- #259 tests assert the subset/reject model. ✓ Integration proves real delegated JWT and user-session tokens pass a `/v1/merchant` route by permission. ✓
- All credential types reach all `/v1/merchant` routes, gated only by permission. ✓

---

# #563: spend-delegation budgets meter PER-INVOKER, never pooled — fix role-scope spendgate key + lock the delegated-invoker identity model

**Completed:** yes
**Status:** COMPLETED 2026-06-20. Role-scope spend-cap windows now meter per concrete invoker instead of sharing one Redis counter across every delegated user carrying the same role UUID. Validation: `go test ./internal/modules/admission/spendgate ./internal/modules/admission`; `go test -tags=integration ./internal/modules/admission/spendgate ./internal/modules/admission -run 'TestGate_RoleWindowIsPerInvoker|TestGate_DelegatedScopesPerInvokerPayerScopeAggregate|TestAdmitter_DelegatedInvokerWindowEnforced' -count=1 -v`.

## Problem

`resolvedWindow.identity(base)` builds the Redis counter key `{merchant:customer}:w:<scope>:<scopeID>:<key>` (`spendgate/policy.go`). For `scope=role`, `scopeID` is the role UUID and the invoker is absent from the key, so every invoker holding that role under one payer increments ONE counter. A single heavy user drains the whole role's window and starves the rest. Only `invoker` (scopeID = the specific invoker) and `invoker_tier` (which folds `req.Invoker` into ScopeID, key-prefixed `it:<tier>:`) meter per user today; `role` does not.

## Decision

- ALL delegated spend scopes meter per invoker. The scope is only the SELECTOR for which invokers a window applies to — never the granularity:
  - `invoker` — window targets one named delegated invoker.
  - `role` — window applies to each invoker holding the role UUID; each holder gets an INDEPENDENT meter.
  - `invoker_tier` — window applies to each invoker at the matching tier; each gets an INDEPENDENT meter.
- Only the `payer` scope is aggregate (the payer org's own balance-velocity cap). There is NO pooled/shared-across-users meter anywhere, and pooled role metering is NOT configurable — it is removed, not optional.
- A role budget of "$50 / 5h" means "$50 / 5h for EACH delegated user holding the role", never "$50 / 5h shared by the role".

## Delegated-invoker identity (what the per-invoker meter hangs on)

Per-invoker metering is only correct if the invoker key is stable across requests. Lock the model:

- The invoker is the HOST's stable principal id, surfaced as the canonical invoker string (`<issuer>:<sub>`, `user:<id>`, or `service-token:<key_id>`).
- For host-delegated users (e.g. Cozy Art), `<sub>` is the host's own IMMUTABLE user UUID — Cozy Art uses its uuidv7 user id. These are NOT OpenRails/Tensorhub users: OpenRails never materializes a user/customer row for an invoker; the invoker string is an opaque, stable counter key only.
- Roles are likewise host-owned immutable UUIDs (e.g. `cozyart.roles.id`), opaque to OpenRails. The role UUID is the single join key across host role catalog → host policy `role_budgets[].role_id` → delegated-token `attributes.roles[]` → OpenRails role-scope window.
- OpenRails must not assume invoker or role identifiers are AuthKit-owned or resolvable; it treats both as stable opaque strings supplied per admit.

## Implementation notes

- Fold the invoker into the role-scope counter key: `{merchant:customer}:w:role:<roleuuid>:<invoker>:<windowkey>`. Keep the hash tag `{merchant:customer}` so all of a payer's keys stay on one Cluster slot; the invoker is just another scrubbed key segment.
- Carry `req.Invoker` onto role `resolvedWindow`s in `EffectiveWindows` (mirror the `invoker_tier` pattern in the loader) and append it in `identity()`; `scrub()` already neutralizes `:` in the invoker string.
- payer-scope windows and the affordability inputs are unchanged.
- Rolling windows are TTL'd ephemeral counters, so the key change just starts fresh per-invoker buckets — no Redis migration. Caps run slightly conservative until the next window reset (existing estimate-based behavior).
- A role grant still authorizes a delegated invoker (`hasDelegatedGrant` unchanged in `policy.go`); only the metering granularity changes.

## Tasks

- [x] Fold the invoker into the role-scope window identity so the Redis counter is per (merchant, payer, invoker, role, window-key).
- [x] Add a spendgate test: two invokers, same payer + same role budget — each gets the full window independently; exhausting one does not block the other.
- [x] Add a regression pinning delegated invoker-scope and role-scope windows as per-invoker, and `payer` as aggregate. `invoker_tier` remains represented by the loader as an invoker-scoped window with an `it:<tier>:` key prefix.
- [x] Update the spendgate package doc comment + `ScopeRole` doc to state per-invoker semantics; drop any "shared"/"role the invoker holds … pooled" wording.
- [x] Document the delegated-invoker identity invariant (host-owned stable UUID invoker; opaque host-owned role UUID; OpenRails never materializes invoker rows) in the admission package docs.
- [x] Fix the #552 spend-delegations doc role-scope semantics (done in this batch) and carry the per-invoker rule into #557 when the route lands.
- [x] Coordinate Tensorhub #496 + Cozy Art #144 doc/test updates (their "shared by role UUID" wording is the same accident).

## Acceptance

- No spend-cap meter is shared across delegated users; role budgets are enforced per invoker.
- A role budget caps each holder independently; one user cannot consume another holder's role window.
- The meter key uses the host's stable principal id as the invoker and survives across that user's requests.
- Pooled/shared role metering cannot be configured anywhere in OpenRails.
- `payer` scope remains the only aggregate (whole-balance velocity) meter.

---

# #554: define final OpenRails permission catalog for public, org-treasury, and merchant routes

**Superseded by #567 (2026-06-22) — model only:** the `merchant:*` + `customer:*` permission catalog below is STILL accurate and stands. What #567 reverses is the *route model*: the "org-customer treasury vs personal customer self" split is gone (one universal `customer` persona), `/v1/orgs/:org_id/*` collapses into `/v1/customers/:id/*`, and the "personal/individual balances are never delegable" rule (below) is REVERSED — every customer can delegate. Read the catalog as current; read the org-vs-individual route framing as historical.

**Completed:** yes
**Status:** COMPLETED 2026-06-21: HARD CUT. OpenRails defines `merchant:*` permissions plus the customer-treasury permissions `customer:spend-delegations:read|update` (renamed from `org:spend-delegations:*` on 2026-06-21 so the buyer hat is a namespace DISJOINT from `merchant:*` and AuthKit-native `org:*`; OpenRails now defines ZERO `org:` permission of its own). The old pre-554 OpenRails `org:*` route permissions (`org:credits:*`, `org:billing:*`, `org:entitlements:*`, `org:catalog:*`, etc.) and old `merchant:customers:*` names are not aliases, are not cataloged, and must not satisfy any gate. AuthKit still owns its native `org:*`/`platform:*` model; OpenRails adds ONLY its app-defined `merchant:*` and `customer:*` namespaces. The `customer:` rename needed NO AuthKit change: `ownerGrantTokens()` derives owner namespaces from `Config.Permissions`, so the org owner auto-holds `customer:*` exactly as it holds `merchant:*` (authkit v0.44.0 `OwnerOwnsAppResources`). Current code state: `internal/controlplane/catalog.go` defines `merchant:customer-settings:read|update`; route gates/tests use the new constants only. Deprecated source-compat permission aliases were deleted. Validation: `go test ./...`; `go test -tags=integration ./internal/integrationharness -run TestStandaloneMerchantCustomerLookupClientHTTP -count=1 -v`; `git diff --check`. Treasury route implementation and its route-level tests remain under #557, not this permission-catalog issue.

## Model

- OpenRails core has four route buckets: public, personal customer self, org-customer treasury, and merchant.
- Public routes require no OpenRails RBAC permission.
- Individual customer self routes are authenticated self-access by subject; personal user balances are not delegable.
- Org customer treasury routes are explicit `/v1/orgs/:org_id/*` routes and AuthKit-org scoped. This covers "I am acting for this org customer/payer, and I want to read billing or share this org balance."
- Merchant routes are AuthKit-org scoped where the AuthKit org owns exactly one OpenRails merchant.
- OpenRails core should not define platform-admin permissions for the merchant/customer surface. OpenRails SaaS tracks platform operator permissions separately (`platform:merchants:read/delete/restore`).
- Permission names describe the OpenRails resource. Merchant-resource permissions use `merchant:<resource>:<action>` (seller hat) and customer-treasury permissions use `customer:<resource>:<action>` (buyer hat) — both granted/evaluated in the owning org scope, both disjoint from AuthKit-native `org:*`.

## Public Routes

No permissions.

## Personal Customer Self Permissions

None. Authenticated self-subject is enough for `/v1/me/*`.

## Customer Treasury Permissions

Only define permissions for the org-customer treasury routes that actually exist. These live in the OpenRails `customer:` namespace (org acting AS a buyer/payer over its OWN balance) — disjoint from `merchant:*` (seller) and AuthKit-native `org:*` (membership):

```text
customer:spend-delegations:read     # read org balance-sharing policy
customer:spend-delegations:update   # replace org balance-sharing policy
```

`customer:spend-delegations:*` only applies to the org named in `/v1/orgs/:org_id/spend-delegations`. Do not allow an individual/personal customer to delegate their personal balance.

## Merchant Permissions

Scoped to the AuthKit org that owns the OpenRails merchant:

```text
merchant:settings:read             # GET /v1/merchant/settings
merchant:settings:update           # PUT /v1/merchant/settings

merchant:payment-providers:read    # GET /v1/merchant/payment-providers*
merchant:payment-providers:update  # PUT/DELETE payment provider config

merchant:catalog:read              # GET catalog products, prices, drift
merchant:catalog:update            # product/price writes, drift refresh, catalog publish

merchant:customer-settings:read   # customer support profile/overview, balance, transactions, entitlements/product access, saved payment-method metadata
merchant:customer-settings:update # customer support writes: profile fields, entitlement/product-access grants, balance adjustments, credit limits

merchant:payments:read             # search/read merchant payments and customer payment history
merchant:payments:refund           # refund a payment

merchant:subscriptions:read        # search/read subscriptions
merchant:subscriptions:update      # cancel/update a subscription, including assigning an existing customer payment method

merchant:admissions:create         # create, capture, release admissions; report wasted spend
merchant:usage:read                # POST /v1/merchant/usage/*

merchant:repair-alerts:read        # GET /v1/merchant/repair-alerts
```

Merged permission mapping:

- `merchant:payment-providers:update` covers provider update and delete/disable.
- `merchant:catalog:update` covers product/price writes, drift refresh/repair,
  and `POST /v1/merchant/catalog/publish`.
- `merchant:customer-settings:read` covers the customer support overview: customer
  profile/status/trust-tier, per-currency balance, customer ledger transactions,
  entitlement/product-access reads, and saved payment-method metadata such as
  brand/last4/expiry. It does NOT cover full payment/subscription ledgers except
  summary fields embedded in the support overview.
- `merchant:customer-settings:update` covers customer-record/support mutations: profile
  fields, manual entitlement/product-access grants/revokes, balance adjustments,
  and credit-limit changes.
- Merchant admins may read saved customer payment methods but may NOT add,
  update, or delete them. Customers manage saved payment methods under `/v1/me/*`.
- `merchant:payments:read` covers payment history/search/detail, including
  customer payment history.
- `merchant:payments:refund` covers payment refunds. Off-channel/manual payment
  recording belongs with payment event/write work when that route lands; do not
  hide it under `merchant:customer-settings:update`.
- `merchant:subscriptions:update` covers cancellation/update workflows, including
  reassigning a subscription to another existing saved payment method belonging
  to the same merchant/customer. It must not create or delete payment methods.
- `merchant:admissions:create` covers the whole admission lifecycle: create,
  capture, release, and wasted-spend report.

## Tasks

- [x] Replace #552's rough initial permission list with this catalog. DONE: `internal/controlplane/catalog.go` `catalogEntries` is now exactly the 17-perm `merchant:*` + `customer:spend-delegations:*` set.
- [x] Rename the customer-treasury namespace `org:spend-delegations:*` → `customer:spend-delegations:*` (2026-06-21): catalog.go constants/entries + `catalog_test.go`; buyer hat is now disjoint from `merchant:*` and AuthKit `org:*`, and OpenRails defines ZERO `org:` permission. No AuthKit change — owner namespaces derive from `Config.Permissions` (`ownerGrantTokens()`), so the owner auto-holds `customer:*`. `go vet ./internal/controlplane` + catalog tests green.
- [x] Add these permission definitions to the OpenRails AuthKit permission catalog. DONE: seeded via `Catalog()` -> `core.Config.Permissions`; bootstrap admin/operator role gets them via `OperatorRolePermissions()`; org owner auto-holds `merchant:*` via authkit#100.
- [x] Delete old planned permissions that have no route: no org-customer `billing`/`checkout`/`payment-methods`/`subscriptions` perms exist; the only customer-treasury perms are `customer:spend-delegations:read|update`.
- [x] Rename merchant-resource permission constants/docs/tests from old OpenRails `org:*` route permissions to canonical `merchant:*`, while keeping the AuthKit scope check tied to the owning org. HARD CUT: no deprecated source-compat permission aliases remain.
- [x] **AUTHZ-CRITICAL — found + RESOLVED 2026-06-20 (Claude):** AuthKit's prebuilt `owner` role holds `OrgOwnerGrant = "org:*"`, which is namespace-anchored (`permMatches`): `org:*` covers `org:<resource>:<action>` ONLY and can NEVER reach `merchant:*`. So renaming merchant perms to `merchant:*` without also granting the owner the `merchant:` namespace would **silently lock every merchant owner out of all merchant operations**. RESOLUTION: fixed in AuthKit as opt-in **#100** — new `Config.OwnerOwnsAppResources bool` makes the prebuilt `owner` (and the owner-role-minted bootstrap admin) auto-own every app-declared namespace (`merchant:*`; future TensorHub `endpoint:*`/`repo:*`/`dataset:*`) in addition to `org:*`; `EnsureOwnerGrants` reconciles pre-existing orgs. OpenRails sets the flag true in `internal/controlplane/service.go` (consumed via a gitignored `go.work` -> local `/home/fidika/authkit`). Proven end-to-end: authkit `TestOwnerHoldsAppNamespaceEndToEnd` (owner holds `merchant:*`, still cannot reach `platform:`) + full authkit `core` suite + OpenRails cross-merchant-isolation & auth-boundary integration suites, all green against local authkit. Guard `TestCatalogPermissionsCoveredByOwnerGrant` relaxed to the surviving invariant: every catalog perm must be namespaced and must never be `platform:`.
- [x] Collapse old fine-grained merchant permissions into the smaller set above. HARD CUT: gates now use the coarse `merchant:*` constants directly; old `credits`/`entitlements`/`product_access`/`secrets`/`configuration`/`metrics`/`billing` permission names are not accepted or aliased.
- [x] Hard-cut rename `merchant:customers:read|update` to `merchant:customer-settings:read|update` in catalog constants, route gates, tests, docs, and downstream consumers; no aliases. DONE 2026-06-21: old tokens are regression-tested as excluded from the catalog.
- [x] Map every planned `/v1/me/*` route to authenticated personal self-access and every planned `/v1/orgs/:org_id/*` route to one of the org customer permissions above. DONE for catalog: `/v1/me/*` has no OpenRails permission; `/v1/orgs/:org_id/spend-delegations` maps to `customer:spend-delegations:*` when #557 implements it.
- [x] Map every planned `/v1/merchant/*` route to one merchant permission above. DONE for catalog; per-route implementation continues in child route issues.
- [x] Add tests proving individual self-access does not require org permissions but org-scoped treasury access does. DEFERRED to #557 because the treasury route does not exist yet.
- [x] Add tests proving personal/individual customers cannot use `spend-delegations`. DEFERRED to #557 because the route does not exist yet.
- [x] Add tests proving merchant permissions are scoped to the merchant-owner org and do not apply to customer/payer orgs unless it is the same org. DEFERRED to #557/#564 route-auth work; the catalog namespaces are disjoint now.
- [x] Add docs showing OpenRails SaaS platform operator permissions live outside this core catalog. DONE: this issue records that core defines no platform-admin permissions and OpenRails SaaS tracks `platform:merchants:*`.

## Acceptance

- OpenRails has one concrete permission catalog for all non-public core routes.
- No core merchant/customer route depends on a fake platform org or platform-admin permission.
- Every defined OpenRails core permission binds to at least one planned route.
- Merchant-resource permission strings use `merchant:*`, not `org:*`; org scope is part of the AuthKit grant/check context.
- Merchant permissions are coarse enough to match real roles; fine-grained splits are only added when a concrete admin persona needs them.
- `customer:spend-delegations:*` is org-customer-only and cannot delegate personal user balances.
- Route tests cover public/no-permission, personal self-access, org-scoped treasury access, and merchant org access.

---

# #553: rename payer tier to trust-tier

**Completed:** yes
**Status:** COMPLETE 2026-06-20: trust-tier wire/API rename is implemented for the live admission/service surfaces. Canonical graduated-tier route is `/v1/service/trust-tier`; `/v1/service/tier` and old `tier` request fields remain compatibility aliases because Tensorhub still uses the current Go client `Tier` fields. Validation: `go test . ./internal/http/handlers ./internal/http/routes/ginroutes ./pkg/service ./embed`; `go test ./internal/modules/admission/...`; `git diff --check`. NOTE: the surviving `/v1/service/tier` route + `tier` field compat aliases are temporary; their hard-cut removal is tracked in #555 (route) and #558 (Go-client field) when Tensorhub bumps.

## Scope

- Rename API response fields from `tier` to `trust_tier` when the value is the customer's auto-maintained trust/spend level.
- Rename route/docs wording from "payer tier" or "customer tier" to "trust tier" where that is the actual meaning.
- Keep product/catalog terminology unchanged: product tiers, tier groups, prices, and subscription plans are not trust tiers.
- Keep internal database column renames optional unless the current names leak into generated APIs or make code misleading. Prefer API/docs clarity over churn.
- Preserve compatibility aliases only if downstreams still read `tier`; otherwise hard-cut before the new merchant route surface ships.

## Tasks

- [x] Audit OpenRails for payer/customer `tier` usages and classify each as trust-tier vs product/catalog tier.
- [x] Update merchant customer profile responses to return `trust_tier` instead of ambiguous `tier`. DONE 2026-06-20: no live merchant-customer profile handler exists yet; the live graduated-tier read response now returns `trust_tier`.
- [x] Update admission/settings policy docs so trust-tier language is explicit where it refers to spend/trust classification.
- [x] Update Go DTOs/client names if they expose payer/customer tier publicly.
- [x] Update tests and fixtures to use `trust_tier`.
- [x] Verify catalog/product tier terminology remains unchanged.

## Acceptance

- Merchant/customer APIs no longer expose ambiguous `tier` for payer trust classification.
- Product/catalog tier names are untouched.
- Tests cover the renamed customer profile/admission DTO fields.

---

# #552: merchant-api-surface-recut (/v1/service → /v1/merchant) [EPIC — children #554–#562]

**Completed:** yes
**Status:** COMPLETED 2026-06-22. OpenRails-side route/principal recut is implemented and validated across children #554, #555, #556, #557, #558, #559, #560, #561, and #562. First-party Tensorhub was hard-cut in lockstep with no legacy routes/shims: it wraps batch-native admission through the Go client seam, pushes merchant admission policy through one `SetMerchantSettings` document, and syncs org delegated-spend grants through `SetOrgSpendDelegations` / `/v1/orgs/:org_id/spend-delegations` rather than old merchant-admin policy setters. Validation: OpenRails `go test . ./embed ./pkg/service ./internal/http/handlers ./internal/http/routes ./internal/http/routes/ginroutes ./pkg/embedded/gin`; OpenRails `go test -tags=integration ./tests -run TestOrgTreasurySpendDelegationsHTTPFullReplacement -count=1 -v`; Tensorhub normal module `GOWORK=off go test ./internal/billing/openrailsclient ./internal/api ./pkg/platformpolicy ./cmd`; Tensorhub against local OpenRails via temporary `/tmp/openrails-tensorhub-552.go.work` (no committed replace): `go test ./internal/billing/openrailsclient ./internal/api ./pkg/platformpolicy`; Tensorhub Docker-backed `go test -tags=integration ./internal/billing/openrailsclient -run 'TestPlatformTierLadderEnforcement|TestBudgetPolicySyncEnforcement|TestEmbeddedModeBootsEngine|TestEmbeddedSelfSurfaceHostIdentity' -count=1 -v`.

**Fresh compose + HTTP revalidation 2026-06-22:** started a clean isolated Docker Compose stack (`COMPOSE_PROJECT_NAME=openrails552`, alternate host ports) through Postgres, Redis, ClickHouse, migrations, and the real `openrails server`; `/health/live` returned `{"service":"billing","status":"ok"}` and unauthenticated `POST /v1/merchant/admissions` returned the expected 401 from the live route. Fixed two compose-start blockers found during that proof: ClickHouse bootstrap now waits for `analytics_user` before grants, and OpenRails rewrites AuthKit v0.45.0's API-key role migration to backfill legacy `service_tokens` before `role NOT NULL`. Post-fix validation passed: `go test ./internal/migrate`; `docker compose --profile all config --quiet`; clean compose `up -d --build --wait openrails`; `go test -tags=integration ./internal/integrationharness -run 'Test(APIKeyCrossMerchantIsolationHTTP|RemoteApplicationSelfJWTCrossMerchantIsolationHTTP|StandaloneMerchantAdmitAccepts(DelegatedJWT|UserSession)ByPermissionHTTP|DelegatedAdminCrossMerchantIsolationHTTP|DelegatedSelfTokenSubjectIsolationHTTP|CoreDoesNotMountPlatformAdminRoutesHTTP|StandaloneMerchantCatalog(RoutesHTTP|ApplyOptionsOverHTTP|PublishHTTP)|StandaloneMerchantPaymentProviderConfigHTTP|StandaloneNoDefaultMerchantResolvesRequestScopedMerchant)' -count=1 -v`; `go test -tags=integration ./tests -run 'Test(ServiceAdmit_HTTP_EndToEnd|OrgTreasurySpendDelegationsHTTPFullReplacement)' -count=1 -v`; Tensorhub `GOWORK=/tmp/openrails-tensorhub-552.go.work go test -tags=integration ./internal/billing/openrailsclient -run 'TestPlatformTierLadderEnforcement|TestBudgetPolicySyncEnforcement|TestEmbeddedModeBootsEngine|TestEmbeddedSelfSurfaceHostIdentity' -count=1 -v`.

HARD CUT — no backwards compatibility, no data migration, no aliases. `/v1/service/*` and `/v1/self/*` are deleted, not aliased. All consumers (Doujins, Tensorhub, Cozy Art) are first-party and bump in lockstep.

`/v1/service/*` describes the credential type, not the resource boundary. Merchant-owned operations move under `/v1/merchant/*` and accept any credential that resolves to the owning org with the needed permission: regular logged-in user, delegated JWT, self-service/browser JWT with org authority, or API key.

## Metadata

- Category: auth
- Status: completed
- Passes: true

## Decisions

- There is no OpenRails "platform org".
- Do not use AuthKit org membership or org roles to authorize OpenRails platform routes.
- Do not model platform admins as merchant admins on a special merchant.
- OpenRails owns the permission names and route gates; AuthKit owns the single RBAC model, role assignment, and effective-permission resolution.
- Platform permissions use `platform:<resource>:<action>` and are global to the OpenRails installation.
- Org-scoped OpenRails permissions use the AuthKit org RBAC plane but the permission string names the OpenRails resource, e.g. `merchant:payments:refund` for a merchant route scoped to the merchant-owner org.
- Merchant route authorization is tied to the AuthKit org that owns the OpenRails merchant.
- Route paths should describe the resource boundary, not the auth mechanism: use `/v1/merchant/*`, not `/v1/service/*`, for merchant-owned operations.
- Do not add `/admin` inside merchant routes. `/v1/merchant/*` already means "authorized actor operating inside the authenticated merchant/org"; admin-ness comes from the permission gate, not the path.
- API keys, delegated JWTs, self-service/browser JWTs with org authority, and regular logged-in users should all normalize into the same org/merchant principal check on merchant routes.
- Core merchant lookup APIs should be customer-forward: list a customer's entitlements, check one entitlement, list a customer's product access, check one product access, and read a customer's balance.
- Reverse lookups such as "which customers have entitlement X" are directory/filter APIs, not the common embedded host path.
- Drop `issuer` from merchant entitlement lookup request bodies once customer identity is `(merchant_id, subject)`; merchant/org comes from auth, subject comes from the request.
- Merchant HTTP routes should be classified by deployment need: standalone/remote needs HTTP APIs; embedded hosts should prefer the Go interface into OpenRails and direct DB access through OpenRails services.
- Embedded mode should mount browser-facing/customer/admin/webhook routes as needed, but should not require internal merchant lookup HTTP routes just to let the host call back into OpenRails.
- OpenRails routes should fall into four product buckets in the core product:
  public routes, personal customer self routes, org-customer treasury routes,
  and merchant routes. Platform/operator routes belong to OpenRails SaaS, not
  the core merchant/customer surface.
- `platform:*` must never imply any org-local OpenRails permission inside a merchant/org.
- Org-local OpenRails permissions must never imply any `platform:*` permission.
- Standalone mode uses OpenRails' bundled AuthKit control plane for RBAC.
- Embedded mode uses the host application's AuthKit RBAC/principal mapping; OpenRails still checks the same permission names.

## Child Issues

This epic is the design source of truth; the master checklist under "## Tasks" is decomposed into landable child issues. Each child carries its own detailed `- [ ]` task list and acceptance, and is a hard cut (no aliases).

- [x] #554 — final OpenRails permission catalog (public / personal-self / org-treasury / merchant; `merchant:*` naming). DONE 2026-06-21.
- [x] #555 — `/v1/service` → `/v1/merchant` rename + one merchant/org principal resolver; drop `issuer`; customer-forward vs directory split. DONE 2026-06-21.
- [x] #556 — embedded route-set presets and honest route-group split (replace `IncludeUser` / `IncludeAdmin` / `IncludeWebhooks` booleans; split checkout/customer/merchant admin/merchant settings/merchant API). DONE 2026-06-22.
- [x] #557 — customer self `/v1/me/*` + org-treasury `/v1/orgs/:org_id/*` (spend-delegations); delete `/v1/self/*`. Role-scope spend-delegation windows meter per-invoker per #563 (not a shared role pool). DONE 2026-06-22.
- [x] #558 — Tensorhub client recut into Admission/PolicySync/AdminFunding Go interfaces + settings/policy split; batch-native admission. DONE 2026-06-22; Tensorhub consumer hard-cut and integration validation completed.
- [x] #559 — merchant payment-provider config API (replace flat secret-name CRUD; atomic validate-then-store). DONE 2026-06-21: moved to `agents/completed.md`; integration coverage passes.
- [x] #560 — merchant catalog publish/drift over HTTP (HTTP form of `push-merchant-catalog`; plan-only default). DONE 2026-06-21: moved to `agents/completed.md`; integration coverage passes.
- [x] #561 — merchant customer-support + payments/subscriptions admin surface (resource-named, grant-ledger audited). DONE 2026-06-22.
- [x] #562 — delete dead platform-org wiring from core (empty-slug mount switch + `PermPlatformSuperadmin` alias).

**Build order (critical path):** #554 (permission catalog) and #555 (principal resolver + `/v1/merchant` base) land first. Everything that mounts under `/v1/merchant/*` — #557, #558, #559, #560, #561 — depends on #555's resolver and must not start before it lands. #556 (route-sets) and #562 (dead-wiring cleanup) are independent and can land any time.

## Permission Catalog

Use #554 as the core OpenRails permission catalog. This issue should not keep a
second rough list. OpenRails core currently plans only:

- org-customer treasury permissions for `/v1/orgs/:org_id/spend-delegations`.
- `merchant:*` permissions for actual `/v1/merchant/*` routes, scoped to the
  AuthKit org that owns the merchant.

OpenRails SaaS tracks platform/operator permissions separately.

## Current Smells To Remove

- #562 removed `controlPlane.PlatformOrgSlug()` as a mount switch for `/v1/platform/*`.
- #562 removed `PermPlatformSuperadmin` as a single fake route gate in core.
- #562 removed route comments that implied platform administration is tied to an AuthKit org.
- #562 removed test setup that granted platform route access by creating or referencing an org-like authority.
- Any route-specific merchant permission that still uses the misleading `org:*` prefix. Merchant-resource permissions should be `merchant:*` and scoped by the AuthKit org grant context.
- `/v1/service/*` as the primary path for merchant-owned APIs.
- `POST /v1/service/customers/by-external-subject/entitlements`: badly named, carries obsolete `issuer`, and mixes standalone remote identity lookup with the simpler merchant/customer model.
- Auth middleware split by credential type instead of normalizing credentials into one org/merchant principal.

## Proposed Merchant Lookup API

Core HTTP surface:

```text
GET  /v1/merchant/customers/:customer_id/entitlements
POST /v1/merchant/customers/entitlements:batch
GET  /v1/merchant/customers/:customer_id/entitlements/:name
GET  /v1/merchant/customers/:customer_id/products
GET  /v1/merchant/customers/:customer_id/products/:product_id/access
GET  /v1/merchant/customers/:customer_id/balance?currency=USD
```

Batch entitlements request after dropping issuer:

```json
{
  "subjects": ["user-uuid-1", "user-uuid-2"],
  "at": "2026-06-20T12:00:00Z"
}
```

Response remains keyed by subject, with unknown subjects returning `[]`.

Directory/filter API, if still needed:

```text
GET /v1/merchant/entitlements/:name/customers
```

Go library surface should stay as close as possible to HTTP:

```go
ListEntitlements(ctx, customerID)
HasEntitlement(ctx, customerID, name)
ListEntitlementsBatch(ctx, []customerID)
ListProductAccess(ctx, customerID)
HasProductAccess(ctx, customerID, productID)
GetBalance(ctx, customerID, currency)
ListCustomersWithEntitlement(ctx, name, page) // directory/filter only
```

## Route Mounting By Deployment Mode

Standalone/remote mode should mount merchant HTTP routes because the host calls OpenRails over the network:

```text
GET  /v1/merchant/customers/:customer_id/entitlements
POST /v1/merchant/customers/entitlements:batch
GET  /v1/merchant/customers/:customer_id/entitlements/:name
GET  /v1/merchant/customers/:customer_id/products
GET  /v1/merchant/customers/:customer_id/products/:product_id/access
GET  /v1/merchant/customers/:customer_id/balance?currency=USD
GET  /v1/merchant/entitlements/:name/customers
```

Embedded mode should prefer the Go interface for those same operations:

```go
ListEntitlements(...)
HasEntitlement(...)
ListEntitlementsBatch(...)
ListProductAccess(...)
HasProductAccess(...)
GetBalance(...)
ListCustomersWithEntitlement(...)
```

Embedded mode still mounts HTTP for browser/user flows, delegated merchant-admin UI flows, and webhooks. It should not need HTTP routes for host-internal lookups when the host can call OpenRails in-process.

## Embedded Route Group API

Host applications should not have to infer route groups from booleans like
`IncludeUser`, `IncludeAdmin`, and `IncludeWebhooks`. Expose boring named route
sets and presets instead:

```go
type RouteSet string

const (
	RouteSetCheckout         RouteSet = "checkout"          // buyer-facing products/prices/config + checkout/pay flows
	RouteSetCustomer         RouteSet = "customer"          // /me/* browser self-service
	RouteSetMerchantAdmin    RouteSet = "merchant_admin"    // human admin customer/support/payment/subscription operations
	RouteSetMerchantSettings RouteSet = "merchant_settings" // provider secrets, catalog pushes, merchant config/settings
	RouteSetMerchantAPI      RouteSet = "merchant_api"      // machine-to-machine API; embedded opt-in only
	RouteSetWebhooks         RouteSet = "webhooks"          // processor callbacks
)

var EmbeddedDefaultRouteSets = []RouteSet{
	RouteSetCheckout,
	RouteSetCustomer,
	RouteSetMerchantAdmin,
	RouteSetWebhooks,
}

var StandaloneDefaultRouteSets = []RouteSet{
	RouteSetCheckout,
	RouteSetCustomer,
	RouteSetMerchantAdmin,
	RouteSetMerchantSettings,
	RouteSetMerchantAPI,
	RouteSetWebhooks,
}
```

Deployment rule: standalone mounts every route set. Embedded mounts the normal
browser/admin/webhook route sets and skips `RouteSetMerchantSettings` plus
`RouteSetMerchantAPI` by default. Hosts may opt into `merchant_settings` when
they intentionally want HTTP-accessible provider/catalog/config management, and
may opt into `merchant_api` when they intentionally want HTTP loopback parity or
remote-compatible machine API access. Do not keep `public_catalog` as a separate
route set: product/price/config discovery is part of the buyer-facing checkout
surface.

## Customer Self-Service Route Recut

Customer routes are browser/delegated self-service for the authenticated
customer/payer. They do not need RBAC permissions for ordinary own-account
operations; the auth subject is the customer.

Core customer self routes:

```text
GET    /v1/me/balance?currency=USD
GET    /v1/me/transactions?currency=USD&limit=&offset=
GET    /v1/me/settings
PUT    /v1/me/settings
GET    /v1/me/payment-methods
POST   /v1/me/payment-methods
DELETE /v1/me/payment-methods/:id
POST   /v1/me/checkout
GET    /v1/me/subscriptions
GET    /v1/me/subscriptions/:id
POST   /v1/me/subscriptions/:id/cancel
GET    /v1/me/payments
GET    /v1/me/invoices
GET    /v1/me/invoices/:id
GET    /v1/me/usage?currency=USD&from=&to=
```

Org-customer delegated spend sharing:

```text
GET    /v1/orgs/:org_id/spend-delegations
PUT    /v1/orgs/:org_id/spend-delegations
```

`spend-delegations` is the self-service shape for "I am acting as this org
customer, and I want selected users/roles to spend this org's balance, subject
to budget windows." It is org-customer policy only: personal/individual customer
balances cannot be delegated. `:org_id` is the payer/customer org whose balance
is being shared. The caller must have AuthKit authority in that org and hold the
org permission that allows reading or changing spend delegation. It is not a
merchant-admin support override. The request should be a full replacement document
with boring scopes. This is a view over the same delegated-spend limit records
OpenRails admission enforces; do not create a second delegation system:

```json
{
  "currency": "USD",
  "delegations": [
    {
      "scope": "invoker_tier",
      "scope_id": "tier_1",
      "windows": [
        {"key": "5h", "window_seconds": 18000, "amount": 5000000}
      ]
    },
    {
      "scope": "role",
      "scope_id": "role-uuid",
      "windows": [
        {"key": "1w", "window_seconds": 604800, "amount": 35000000}
      ]
    }
  ]
}
```

Scope semantics — every delegated scope meters PER INVOKER (see #563); the scope
is only the SELECTOR for which invokers a window applies to, never a shared pool:

- `invoker`: a meter for one named delegated invoker.
- `invoker_tier`: a per-invoker meter applied to each delegated user at the
  selected tier.
- `role`: a per-invoker meter applied to each delegated user carrying the role
  UUID; each holder gets an INDEPENDENT window (NOT a pool shared by the role).
- `amount` is the currency-native integer amount. For USD, it is micro-dollars.
- Only the `payer` scope (the org's own balance velocity) is aggregate. No
  per-user custom override until a real UI/workflow needs it.

For embedded Tensorhub/Cozy Art, the host can keep using its Go/policy path:
Cozy Art authors role/tier budgets, Tensorhub checks AuthKit `billing:spend`
and passes payer/invoker/role/tier to OpenRails admission. These `/v1/me`
routes are only needed when OpenRails itself owns the customer-facing UI for
sharing a balance.

## Tensorhub Merchant API Recut

Tensorhub is the only real consumer of the broad `/v1/service/*` credit/admit
surface. Its production path does not need a generic service-account grab bag.
It needs three boring OpenRails client surfaces:

```go
type AdmissionClient interface {
	Admit(ctx, batchReq)
	Capture(ctx, requestID, capturedAmount, usage)
	Release(ctx, requestID)
	ReportWastedSpend(ctx, report)
	GetTier(ctx, customerID, currency)
}

type PolicySyncClient interface {
	SetTierSchedule(ctx, currency, schedule)
	SetTierSpendLimits(ctx, currency, tier, limits)
	SetDelegatedInvokerWastedSpendLimits(ctx, windows)
}

type AdminFundingClient interface {
	DepositCredits(ctx, request)
	SetCreditLimit(ctx, customerID, currency, limit)
	UsageRollup(ctx, customerID, currency, from, to, groupBy)
	ResourceRevenueDaily(ctx, resourceID, currency, from, to)
}
```

Tensorhub owns request pricing, endpoint/resource identity, endpoint access,
capacity/scheduler decisions, local platform-abuse event display, and the UI
meaning of its tiers/roles. OpenRails owns the money ledger, balances,
holds/captures/releases, arrears credit-limit enforcement, spend counters,
wasted-spend counters, tier graduation from paid spend, usage rollups from
captured ledger events, payments, subscriptions, and invoicing.

Remote/standalone Tensorhub should only need this merchant HTTP surface:

```text
POST /v1/merchant/admissions                    # always batch-shaped; one item is still an array
POST /v1/merchant/admissions/:request_id/capture
POST /v1/merchant/admissions/:request_id/release
POST /v1/merchant/wasted-spend
GET  /v1/merchant/settings
PUT  /v1/merchant/settings

POST /v1/merchant/customers/:customer_id/balance-adjustments
PUT  /v1/merchant/customers/:customer_id/credit-limit
POST /v1/merchant/customers/:customer_id/usage/rollup
POST /v1/merchant/usage/resource-revenue
```

Routes to cut from the Tensorhub-required surface unless another downstream
proves a real need:

```text
GET  /v1/service/budget
GET  /v1/service/merchant-configuration
GET  /v1/service/abuse-usage
GET  /v1/service/credit-limit
GET  /v1/service/credits/balance
POST /v1/service/credits/withdraw
GET  /v1/service/credits/transactions/lookup
PUT  /v1/service/credits/account-settings
GET  /v1/service/credits/account-settings
GET  /v1/service/credits/transactions
```

Reasoning:

- Tensorhub's hot path is `Admit` before work, `Capture` on success, `Release`
  on failure/cancel, and `ReportWastedSpend` for rejected or failed provider
  spend. Admit is always batch-shaped; a single request is just a one-item batch.
  Direct `withdraw` bypasses that lifecycle.
- Tensorhub reads local abuse/platform event tables for its abuse UI; it does
  not need OpenRails' `abuse-usage` endpoint.
- Tensorhub's previous admin proxy for OpenRails account settings and
  transaction history was already removed; customers/orgs should use the
  OpenRails customer/merchant surfaces directly.
- OpenRails invoicing remains internal to OpenRails. Tensorhub should not need
  routes to crank invoices, charge arrears, or inspect processor internals.
- Embedded Tensorhub should use the same Go interfaces directly and should not
  mount these merchant HTTP routes unless it wants remote parity.

## Merchant Admin Frontend Surface

There are two different admin surfaces and they should not blur together:

- Platform admin: cross-merchant OpenRails operations such as creating,
  disabling, exporting, or deleting merchants.
- Merchant admin: actions by an admin of exactly one merchant/org, usually from
  a host frontend or OpenRails standalone merchant dashboard.

Merchant admins should not manage `owner_org_id`, platform status, platform
exports, or hard deletes. Those stay under `platform:*`.

### Merchant Route Contracts

Keep merchant settings boring: display/support metadata, checkout/webhook URLs,
and merchant-owned billing policy. Payment-provider routing and credentials live
under `/v1/merchant/payment-providers/*`. Do not put platform lifecycle fields
here.

Settings and payment providers:

| Route | Purpose | Request | Response |
|---|---|---|---|
| `GET /v1/merchant/payment-providers` | List configured payment providers. | `?provider=&environment=&status=` | `{data:[payment_provider]}` |
| `GET /v1/merchant/payment-providers/:provider` | Read one payment provider's status/config. | optional `?environment=live` | `{payment_provider}` |
| `PUT /v1/merchant/payment-providers/:provider` | Create/update one provider config as a block. Supplied credentials are validated before storage. | `{environment,enabled,account_id?,public_config?,credentials?}` | `{payment_provider}` |
| `DELETE /v1/merchant/payment-providers/:provider` | Disable/remove one payment provider from future use. | optional `{environment,reason}` | `{payment_provider}` |

`payment_provider` responses expose metadata and configured-field status, never
secret plaintext:

```json
{
  "id": "uuid",
  "provider_type": "stripe",
  "environment": "live",
  "account_id": "acct_...",
  "role": "primary",
  "status": "enabled",
  "public_config": {
    "publishable_key": "pk_..."
  },
  "credentials": {
    "secret_key": {"configured": true, "updated_at": "..."},
    "webhook_signing_secret": {"configured": true, "updated_at": "..."}
  }
}
```

Current merchant credential/config fields by provider:

- Stripe: `secret_key`, `webhook_signing_secret`,
  `webhook_signing_secret_thin`, and optional public `publishable_key`.
- NMI/Mobius: `production_key`, `tokenization_key`, `tokenization_url`, and
  `webhook_signing_secret`. `tokenization_key`/URL are browser-facing config,
  but still belong on the provider account.
- CCBill: `account_config` for now, until it is split into typed fields.
- Solana: `private_key` exists but is OpenRails/platform-owned, not
  merchant-admin writable.

Credential updates are atomic: validate first, store only if validation passes.
There is no separate `/validate` route in the primary API. Direct
`/v1/merchant/secrets/*name` CRUD should be retired or compatibility-only.
The storage layer may still store values under provider-account-scoped secret
names like `provider_accounts/{provider}/{environment}/{account_id}/{key}`.

Catalog:

| Route | Purpose | Request | Response |
|---|---|---|---|
| `GET /v1/merchant/catalog/products` | List products for admin UI. | query filters/page | `{data:[product],page}` |
| `POST /v1/merchant/catalog/products` | Create product. | product fields | `{product}` |
| `GET /v1/merchant/catalog/products/:id` | Read product. | none | `{product}` |
| `PATCH /v1/merchant/catalog/products/:id` | Update product fields. | partial product fields | `{product}` |
| `POST /v1/merchant/catalog/products/:id/activate` | Make product sellable/visible. | optional `{reason}` | `{product}` |
| `POST /v1/merchant/catalog/products/:id/deactivate` | Stop new sales without deleting history. | optional `{reason}` | `{product}` |
| `GET /v1/merchant/catalog/prices` | List prices. | query filters/page | `{data:[price],page}` |
| `POST /v1/merchant/catalog/prices` | Create price. | price fields | `{price}` |
| `GET /v1/merchant/catalog/prices/:id` | Read price. | none | `{price}` |
| `PATCH /v1/merchant/catalog/prices/:id` | Update price fields. | partial price fields | `{price}` |
| `POST /v1/merchant/catalog/prices/:id/activate` | Make price available for new purchases. | optional `{reason}` | `{price}` |
| `POST /v1/merchant/catalog/prices/:id/deactivate` | Retire price for new purchases. | optional `{reason}` | `{price}` |
| `POST /v1/merchant/catalog/publish` | Plan/apply the catalog-as-code manifest, mirroring `openrails push-merchant-catalog`. | `{catalog:{version,default_providers?,tier_groups},insert?,overwrite?,prune?,plan_only?}` | `{plan,result?,extras?}` |
| `GET /v1/merchant/catalog/drift` | List provider/catalog drift findings, including provider-side orphans. | `?provider=&kind=&resource_type=&limit=&offset=` | `{data:[finding],page}` |
| `POST /v1/merchant/catalog/drift/refresh` | Run drift detection now. | optional `{provider}` | `{summary}` |

Product/price writes should enqueue provider sync automatically where a provider
mirror is needed. Do not require a merchant admin UI to call per-object
`reconcile` routes after saving. Keep drift reads as ops visibility; provider
orphans are just `GET /v1/merchant/catalog/drift?kind=orphan`, not a separate
route. If manual repair is needed later, add one boring bulk repair endpoint
rather than per-product/per-price buttons.

`POST /v1/merchant/catalog/publish` is the HTTP form of the existing
`openrails push-merchant-catalog` CLI. The route is merchant-scoped from auth, so
the body should contain one catalog entry, not the CLI file's multi-merchant
`catalogs[]` wrapper. The `catalog` object is the same desired-state shape used
inside `config/catalog.example.yaml`: `default_providers`, `tier_groups`,
products, prices, and `provider_links`. Default behavior should be plan-only;
mutation requires explicit `insert`, `overwrite`, or `prune`, matching the CLI.

Customer support:

| Route | Purpose | Request | Response |
|---|---|---|---|
| `GET /v1/merchant/customers` | Search/list merchant customers. | query filters/page | `{data:[customer],page}` |
| `GET /v1/merchant/customers/:customer_id` | Customer support profile/overview, including trust tier/status and summaries. | optional `?currency=USD` | `{customer,balance_summary,trust_tier,status,active_entitlements}` |
| `GET /v1/merchant/customers/:customer_id/balance` | Per-currency balance. | `?currency=USD` | `{currency,balance}` |
| `GET /v1/merchant/customers/:customer_id/transactions` | Customer ledger history. | `?currency=&limit=&offset=` | `{data:[transaction],page}` |
| `GET /v1/merchant/customers/:customer_id/entitlements` | List active/history entitlement grants. | query filters/page | `{data:[entitlement],page}` |
| `GET /v1/merchant/customers/:customer_id/entitlements/:name` | Check one entitlement. | optional `?at=` | `{active,entitlement?}` |
| `POST /v1/merchant/customers/:customer_id/entitlements` | Manual entitlement grant. | `{entitlement,starts_at?,ends_at?,reason}` | `{grant,entitlements}` |
| `DELETE /v1/merchant/customers/:customer_id/entitlements/:grant_id` | Revoke manual grant. | optional `{reason,refund?}` | `{grant,revoked:true}` |
| `GET /v1/merchant/customers/:customer_id/products` | List owned/product access. | query filters/page | `{data:[product_access],page}` |
| `GET /v1/merchant/customers/:customer_id/products/:product_id/access` | Check one product. | none | `{has_access,access?}` |
| `POST /v1/merchant/customers/:customer_id/product-access` | Manual product ownership grant. | `{product_id,starts_at?,ends_at?,reason}` | `{grant,product_access}` |
| `DELETE /v1/merchant/customers/:customer_id/product-access/:grant_id` | Revoke manual product grant. | optional `{reason}` | `{grant,revoked:true}` |
| `GET /v1/merchant/customers/:customer_id/payment-methods` | List saved payment-method metadata for support (brand/last4/expiry/status only); no detail route because there is no extra merchant-visible sensitive detail. | query filters/page | `{data:[payment_method],page}` |
| `POST /v1/merchant/customers/:customer_id/balance-adjustments` | Append-only support adjustment to the customer's prepaid balance; for comp credits, corrections, or migrations, not normal purchases. | `{currency,amount,reason,idempotency_key?}` | `{transaction}` |
| `PUT /v1/merchant/customers/:customer_id/credit-limit` | Set platform/merchant-approved arrears exposure for this customer. | `{currency,limit,reason}` | `{currency,limit}` |

Merchant-wide payments and subscriptions:

| Route | Purpose | Request | Response |
|---|---|---|---|
| `GET /v1/merchant/payments` | Search merchant payments. | query filters/page | `{data:[payment],page}` |
| `GET /v1/merchant/payments/:id` | Read payment detail. | none | `{payment}` |
| `GET /v1/merchant/customers/:customer_id/payments` | Customer payment history, same payment resource filtered to one customer. | query filters/page | `{data:[payment],page}` |
| `POST /v1/merchant/customers/:customer_id/payments/off-channel` | Record manual/off-channel payment for an existing price and run the normal purchase side effects. | `{price_id,transaction_id,amount?,currency?,purchased_at?,discount_code?,discount_reason?,discount_metadata?}` | `{payment_id,entitlements,delayed_start,eligibility}` |
| `POST /v1/merchant/payments/:id/refunds` | Refund payment through provider/ledger. | `{amount?,reason,revoke_access?}` | `{refund,payment}` |
| `GET /v1/merchant/subscriptions` | Search merchant subscriptions. | query filters/page | `{data:[subscription],page}` |
| `GET /v1/merchant/subscriptions/:id` | Read subscription detail. | none | `{subscription}` |
| `POST /v1/merchant/subscriptions/:id/cancel` | Stop future rebilling. | `{reason?,revoke_access?,effective_at?}` | `{subscription}` |
| `PUT /v1/merchant/subscriptions/:id/payment-method` | Reassign subscription to another existing saved payment method for the same merchant/customer. | `{payment_method_id}` | `{subscription}` |
| `GET /v1/merchant/customers/:customer_id/subscriptions` | Customer subscriptions, current and historical, same subscription resource filtered to one customer. | `?status=&limit=&offset=` | `{data:[subscription],page}` |

Usage/admission and policy:

| Route | Purpose | Request | Response |
|---|---|---|---|
| `POST /v1/merchant/admissions` | Batch authorize work and create holds. | `{items:[{payer,invoker?,resource?,currency,estimated_amount,idempotency_key?}]}` | `{items:[{request_id,admitted,reason?,hold?}]}` |
| `POST /v1/merchant/admissions/:request_id/capture` | Capture an admitted hold after work succeeds. | `{amount,usage?}` | `{transaction}` |
| `POST /v1/merchant/admissions/:request_id/release` | Release an admitted hold after failure/cancel. | optional `{reason}` | `{released:true}` |
| `POST /v1/merchant/wasted-spend` | Report provider spend that produced no billable result. | `{payer,invoker?,currency,amount,reason,resource?}` | `{recorded:true}` |
| `POST /v1/merchant/usage/rollup` | Usage/spend rollup for analytics/support. | `{customer_id?,currency,from,to,group_by}` | `{data:[row]}` |
| `POST /v1/merchant/usage/resource-revenue` | Revenue rollup by resource. | `{currency,from,to,resource?}` | `{data:[row]}` |
| `GET /v1/merchant/settings` | Read merchant-owned settings, including admission policy. | none | `{settings}` |
| `PUT /v1/merchant/settings` | Update merchant-owned settings as one document. | `{display?,checkout?,admission_policy?}` | `{settings}` |

Policy split:

- `settings.admission_policy.tier_schedule` is merchant-wide. Tensorhub uses it
  to declare the cumulative paid-spend ladder once; OpenRails auto-graduates each
  payer.
- `settings.admission_policy.tier_spend_limits` is merchant-wide default policy
  for a resolved payer tier: in-flight/held spend caps, single-charge caps, and
  payer wasted-spend windows.
- `settings.admission_policy.delegated_invoker_wasted_spend_limits` is
  merchant-wide. OpenRails can provide defaults, but multi-merchant deployments
  need a per-merchant override.
- Delegated spend authority is host/org policy, not a merchant-admin override.
  Tensorhub-style org delegation should stay in the host app and reach
  OpenRails through the embedded Go API or a future host-internal sync path only
  if standalone deployment needs it.
- If OpenRails owns the customer-facing UI for this, expose it as
  `/v1/orgs/:org_id/spend-delegations`, because the route must explicitly name
  the payer org/customer whose spend-sharing policy is changing. Do not allow
  individual/personal customer balances to be delegated.
- OpenRails should still trivially accommodate Tensorhub org-delegated spend:
  admission already has the right generic shape — payer/customer id, invoker,
  invoker type, tier, role UUIDs, estimated amount, request id, and resource.
  Tensorhub decides whether the invoker may spend the org's balance through
  AuthKit + Tensorhub policy; OpenRails meters the resulting windows and money.
- Merchant-wide invoker safety policy lives in settings, but payer authorization
  still lives on the payer/customer. Do not add a merchant-level "invoker may
  spend anyone's balance" grant.

Admission policy shape:

```json
{
  "admission_policy": {
    "currency": "USD",
    "tier_schedule": [
      {"tier": "free", "min_cumulative_paid_amount": 0},
      {"tier": "pro", "min_cumulative_paid_amount": 100000000}
    ],
    "tier_spend_limits": [
      {
        "tier": "free",
        "windows": [],
        "wasted_spend_windows": []
      }
    ],
    "delegated_invoker_wasted_spend_limits": []
  }
}
```

Ops reads:

| Route | Purpose | Request | Response |
|---|---|---|---|
| `GET /v1/merchant/repair-alerts` | Ledger/provider repair alerts. | query filters/page | `{data:[alert],page}` |

Routes to skip until a real caller needs them:

```text
POST /v1/merchant/customers/entitlements:batch
GET  /v1/merchant/entitlements/:name/customers
```

The first can come back as `POST /v1/merchant/entitlements:batch` if JWT
enrichment over HTTP needs it. The second is a directory/filter query, not a
core customer support workflow.

Rules:

- Customer rows should still be created naturally by checkout/usage/auth flows;
  admin routes may upsert by subject only when recording an off-channel payment
  or manual grant.
- Manual entitlements and product access should be recorded through the grant
  ledger with `source_type=admin`, explicit `starts_at`, optional `ends_at`,
  reason, and acting admin for audit.
- Off-channel payments are for payments tied to an existing catalog price. They
  use the same purchase registration path as checkout/webhooks so entitlements,
  product access, and idempotency stay consistent. Arbitrary free access uses
  the manual entitlement/product-access grant routes instead.
- Customer subscriptions should include current and historical subscriptions plus
  lifecycle dates the admin UI needs: started, current period start/end, cancel
  time, next retry/renewal where available, status, price/product, and processor
  references.
- Customer profile reads include the customer's auto-maintained trust/spend tier;
  do not keep a separate `GET /v1/merchant/customers/:customer_id/tier` route.
  Customer balance checks use `GET /v1/merchant/customers/:customer_id/balance`.
  `balance-adjustments` is append-only support/migration/correction credit,
  not a purchase path. `credit-limit` is explicit arrears exposure, not a
  billing-mode toggle. Tier is read-only and auto-maintained from spend/trust
  rules; do not add a merchant-admin tier override unless a real support
  workflow proves it is needed.
- Usage rollups are reporting/analytics, not core customer support CRUD, so keep
  them under `/v1/merchant/usage/*`.
- Refunds should not automatically revoke access. Revoking access is a separate
  admin decision unless the refund workflow explicitly asks for `revoke_access`.
- Cancel subscription means "do not rebill"; whether existing access is revoked
  immediately or ends at period end must be an explicit request flag.
- Deleting a payment method is support cleanup only; it must be scoped to the
  same merchant/customer and must not delete historical payment records.
- Payment-provider credentials are managed as payment-provider fields, not as a
  flat secret-name CRUD API. Admin UI can list configured status, update supplied
  fields, and delete/disable accounts; it never reads plaintext. Supplied
  credentials are validated immediately and are not stored if validation fails.

## Tasks

- [ ] Define/register only the OpenRails core permissions from #554 during bootstrap/config sync.
- [ ] Rely on AuthKit's single RBAC model to reject invalid permission/scope combinations: non-`platform:` permissions in platform roles and `platform:` permissions in org roles.
- [x] Delete core `/v1/platform/*` mounting instead of replacing it with another core platform gate; OpenRails SaaS owns platform operator routes when needed.
- [x] Delete core `/v1/admin/merchants*` / cross-merchant admin route wiring instead of keeping a fake superadmin surface.
- [x] Remove `PermPlatformSuperadmin`; do not replace it in core.
- [x] Ensure delegated/browser merchant admin tokens cannot reach any removed platform/cross-merchant surface.
- [x] Ensure any future SaaS platform principal cannot satisfy org-local merchant gates without explicit org-scoped authority for that merchant/org.
- [x] Rename merchant-local permission constants/routes/docs to the chosen org-local OpenRails permission names where needed. DONE under #554/#561.
- [x] Ensure standalone mode checks the bundled AuthKit control plane for org-local OpenRails merchant permissions. DONE under #564/#561.
- [x] Ensure embedded mode checks the same permission names from the host/AuthKit principal mapping. DONE under #556/#557/#561.
- [x] Move merchant-owned `/v1/service/*` routes to `/v1/merchant/*`; hard cut the old paths, no compatibility aliases. DONE under #555 / OpenRails v0.48.0; remaining auth unification is tracked in #564.
- [x] Make `/v1/merchant/*` routes accept every supported credential type that resolves to the owning org/merchant and required permission: logged-in user, delegated JWT, self-service/browser JWT with org authority, and API key. DONE under #564: `merchantActionPermissionMW` now gates the former service routes too; integration proves delegated JWT and user-session `/v1/merchant/admit` access by permission.
- [x] Replace route-level "service credential required" assumptions with a shared principal resolver for merchant routes. DONE under #564: `serviceCredentialRequiredMW` / `servicePermissionMW` removed; service handlers keep API-key customer-resource checks only when the caller is actually a service credential.
- [x] Remove nested `/admin` naming from merchant-owned routes; use resource paths such as `/v1/merchant/subscriptions/:id`, with permissions deciding who may call them. DONE under #561: the delegated billing-admin Gin bundle and tests moved to `/v1/merchant/*`.
- [x] Normalize personal customer self-service routes under `/v1/me/*` and add org-customer-owned `GET/PUT /v1/orgs/:org_id/spend-delegations` for org balance sharing; personal customer balances must not be delegable. DONE under #557.
- [x] Replace `POST /v1/service/customers/by-external-subject/entitlements` with `POST /v1/merchant/customers/entitlements:batch`; remove `issuer` from the request once customer identity is `(merchant_id, subject)`. DONE under #555 / OpenRails v0.48.0.
- [x] Split customer-forward lookups from directory/filter reverse lookups in both HTTP and Go APIs. DONE under #555 / OpenRails v0.48.0; no reverse lookup route is in the primary merchant surface.
- [x] Keep the Go library API and HTTP API names/shapes aligned so embedded and standalone hosts use the same concepts. DONE under #558; Tensorhub consumer hard-cut validated.
- [x] Separate merchant route registration by deployment mode: standalone mounts merchant HTTP lookup APIs; embedded exposes the same operations primarily through the Go interface and avoids unnecessary host-internal HTTP routes. DONE under #556/#558.
- [x] Replace/augment embedded `IncludeUser`, `IncludeAdmin`, `IncludeWebhooks` booleans with explicit named route sets or presets so hosts can see what should be mounted. DONE under #556.
- [x] Make embedded defaults exclude merchant host-internal HTTP lookup routes; provide an opt-in route set for HTTP parity. DONE under #556.
- [x] Update Doujins, Tensorhub, and Cozy Art in lockstep for the hard cut; expected common needs are account balance, check/list entitlements for JWT/auth, check/list product access, and Tensorhub `Admit`. DONE: Tensorhub and Cozy Art are on OpenRails v0.49.0; stale old SDK/route references are gone from Tensorhub's active Go code.
- [x] Recut Tensorhub's OpenRails dependency into explicit admission, policy-sync, and admin-funding/reporting Go interfaces; keep HTTP only for standalone/remote mode. DONE under #558; Tensorhub uses the Go client seam directly.
- [x] Make admission batch-native: one `POST /v1/merchant/admissions` endpoint and one Go method that both accept an item array; no separate single-admit route. DONE under #558.
- [x] Replace Tensorhub's broad `/v1/service/*` expectations with the smaller `/v1/merchant/*` route set above. DONE under #558; Tensorhub consumer adoption validated.
- [x] Remove or compatibility-gate Tensorhub-unused service routes: `budget`, merchant configuration read, `abuse-usage`, credit-limit read, direct credit withdraw, transaction lookup, account settings, and generic credit transactions. DONE as a hard cut in OpenRails under #558.
- [x] Keep OpenRails invoicing, payment processor state, and arrears charging internal to OpenRails; Tensorhub should only configure limits/policies and consume ledger/admission results. DONE on the OpenRails surface under #558.
- [x] Fold the existing delegated `/v1/admin/*` merchant-admin routes into the resource-named `/v1/merchant/*` surface; hard cut the old admin paths. DONE under #561.
- [x] Keep platform merchant provisioning routes separate from merchant self-admin routes: platform can create/delete/export/disable merchants; merchant admins can only manage their own merchant settings, payment providers, catalog, and customers. DONE under #561/#562.
- [x] Replace direct merchant secret-key CRUD with payment-provider configuration routes; store individual credentials as write-only fields under the provider account internally. DONE under #559; direct `/v1/merchant/secrets/*` and legacy delegated `/v1/admin/secrets/*` routes are hard-cut.
- [x] Cut generic `/v1/merchant/configuration`; use `/v1/merchant/settings` for merchant-owned settings and `/v1/merchant/payment-providers/*` for provider configuration. DONE 2026-06-21: removed stale `/merchant-configuration` mounts from the router `/v1/merchant` surface and delegated `/v1/admin` gin surface; route tests assert 404.
- [x] Implement `/v1/merchant/payment-providers` list/read/update/delete using provider names in the path and `{environment}` in query/body; keep `provider_accounts` as internal storage only. DONE under #559.
- [x] Make payment-provider update atomic: validate supplied credentials against the provider first, then store config/credentials only on success. DONE under #559; integration proves invalid NMI config is not persisted.
- [x] Return payment-provider credential status as redacted field metadata (`configured`, `updated_at`, `last_validated_at` if available), never plaintext. DONE under #559.
- [x] Support test/live provider environments explicitly; enforce one active config per `{merchant, provider, environment}` until multiple active accounts are actually needed. DONE under #559.
- [x] Expose catalog-as-code publish/apply over HTTP with the same plan/apply engine as `openrails push-merchant-catalog`; HTTP is single-merchant because the merchant comes from auth. DONE under #560.
- [x] Add `POST /v1/merchant/catalog/publish` that accepts the inner single-merchant catalog manifest shape from `config/catalog.example.yaml`, not the CLI `catalogs[]` wrapper. DONE under #560.
- [x] Keep catalog publish plan-only by default; require explicit `insert`, `overwrite`, or `prune` for mutation, matching `openrails push-merchant-catalog`. DONE under #560.
- [x] Remove product/price per-object reconcile routes from the planned primary surface; product/price writes and catalog publish should enqueue safe provider sync automatically. DONE under #560; primary surface has no per-object reconcile routes.
- [x] Fold catalog orphan listing into `GET /v1/merchant/catalog/drift?kind=orphan`; do not keep a separate `/orphans` route. DONE under #560; no `/orphans` aliases remain on the primary surface.
- [x] Add or normalize customer-management routes for merchant admins: customer settings/profile read, saved payment-method metadata read-only, manual entitlement grant/revoke, product-access grant/revoke, off-channel payment record, payment refund, subscription payment-method reassignment, and subscription cancel. DONE under #561.
- [x] Keep product access write routes named `/product-access`, not `/products`, because the written resource is the customer's access grant, not a catalog product. DONE under #561.
- [x] Make off-channel payment recording require `price_id` and `transaction_id`, then route through the existing normal purchase registration path for entitlements/product access/idempotency. DONE under #561.
- [x] Do not add merchant routes to create/update/delete saved payment methods; merchant admins may only read saved payment-method metadata and may reassign a subscription to an existing method belonging to the same customer/merchant. DONE under #561.
- [x] Ensure customer subscription list returns current and historical subscriptions with lifecycle dates, status, product/price, processor refs, renewal/retry data where available, and pagination/status filtering. DONE under #561.
- [x] Move usage rollup out of customer CRUD into `/v1/merchant/usage/rollup` with optional `customer_id`. DONE under #561/#558.
- [x] Keep `balance-adjustments` and `credit-limit` as explicit money admin writes, not balance reads; customer balance stays `GET /v1/merchant/customers/:customer_id/balance`. DONE under #561.
- [x] Make manual grants go through the grant ledger with acting-admin audit fields; do not write raw entitlement/product-access rows from HTTP handlers. DONE under #561.
- [x] Make refund and subscription-cancel workflows explicit about access revocation instead of silently coupling money reversal to entitlement revocation. DONE under #561.
- [x] Replace separate merchant policy routes with `GET/PUT /v1/merchant/settings`, containing merchant-owned admission policy as one document. DONE under #558.
- [x] Rename/update Go library policy methods to prefer one merchant settings call, e.g. `GetMerchantSettings` and `SetMerchantSettings`; do not add customer delegated-spend methods unless a real standalone host sync path requires them. DONE under #558.
- [x] Keep broad merchant-level "invoker can spend" grants out of the model; invoker/role spend authority must be attached to the payer/customer whose balance is being spent. DONE under #557/#558.
- [x] Preserve OpenRails admission support for Tensorhub org-delegated spend budgets with generic fields: payer/customer id, invoker, invoker type, tier, role UUIDs, estimated amount, request id, and resource. DONE under #558 and validated by Tensorhub integration.
- [x] Do not expose Tensorhub org delegated-spend budgets as OpenRails merchant-admin routes; org-customer-owned sharing belongs under `/v1/orgs/:org_id/spend-delegations`, while embedded Tensorhub/Cozy Art can keep their host-owned policy sync path. DONE under #557/#558 and validated by Tensorhub keeping `platform_budget_sync.go` out of merchant settings.
- [x] Keep `ResourceRevenueDaily` / `/v1/merchant/usage/resource-revenue` as reporting-only endpoint analytics, not admission settlement. DONE under #558; Tensorhub keeps this behind its reporting client seam.
- [x] Cut planned `GET /v1/merchant/manual-rebill-attempts`; rebill attempts are dunning/provider-intent events, so surface failures needing attention through `repair-alerts` and defer aggregate dashboard counts/history drill-downs to the future admin dashboard work. DONE 2026-06-21: removed the old delegated `/v1/admin/manual-rebill-attempts` mount; route test asserts 404.
- [x] Add integration coverage proving core no longer mounts platform/cross-merchant admin routes and merchant permissions cannot act as platform authority. DONE under #562/#564.
- [x] Add integration coverage for payment-provider credential validation failure proving invalid credentials are not persisted. DONE under #559: `TestStandaloneMerchantPaymentProviderConfigHTTP`.
- [x] Add integration coverage for `POST /v1/merchant/catalog/publish` plan-only and mutating modes against the same catalog engine as the CLI. DONE under #560: `TestStandaloneMerchantCatalogPublishHTTP`.
- [x] Add integration coverage for remote merchant admission batch shape, capture, release, and wasted-spend against live HTTP server + DB/Redis stack. DONE under #558.
- [x] Add integration coverage for customer manual grants, off-channel payment registration, refund with explicit revoke flag behavior, and subscription cancel with explicit access behavior. DONE under #561.
- [x] Add integration coverage for settings/policy split: Tensorhub-style merchant settings install tier schedule, tier spend limits, and delegated-invoker wasted-spend defaults without exposing customer delegated-spend overrides as merchant-admin routes. DONE under #558 and Tensorhub #495.
- [x] Update docs and comments to use "platform RBAC" and "org-local merchant RBAC", not "platform org".

## Acceptance

- No OpenRails route requires or references a "platform org".
- Core has no platform admin routes; OpenRails SaaS owns any future platform operator surface.
- Merchant admin routes are protected by OpenRails-defined AuthKit org-local permissions.
- Merchant-owned routes are exposed under `/v1/merchant/*`; `/v1/service/*` is deleted (hard cut, no alias).
- The same merchant permission gate works for regular users, delegated JWTs, self-service/browser JWTs with org authority, and API keys.
- Batch entitlement lookup no longer accepts or requires `issuer`; merchant/org is resolved from auth.
- Customer-forward lookup APIs are simple and mirrored between HTTP and Go.
- Standalone/remote merchant lookup routes are available over HTTP; embedded hosts can use the Go API without routing through HTTP.
- Tensorhub's hot path uses only admission/capture/release/wasted-spend/tier reads, with policy sync and admin funding/reporting kept separate.
- Tensorhub/Cozy Art org-delegated spend budgets can be enforced by OpenRails admission without adding merchant-admin delegated-spend override routes; if OpenRails owns the UI, it is org-customer self-service via `/v1/orgs/:org_id/spend-delegations`.
- Admission has one batch-shaped API; a one-item admission request uses the same route and response shape.
- Routes removed from the Tensorhub-required surface either have no downstream caller or are replaced by the OpenRails Go interface/customer/merchant surface.
- Merchant admin routes are resource-named under `/v1/merchant/*`, not hidden under credential-shaped `/v1/admin/*` or `/v1/service/*` paths.
- Merchant admins cannot create, delete, export, disable, or reassign merchants; those stay platform-only.
- Merchant customer-management actions are audited with the acting admin and scoped to the authenticated merchant/org.
- Merchant provider credentials are configured through payment-provider routes with redacted field status; direct secret-name routes are deleted (hard cut).
- Catalog publish/apply is available through both CLI and HTTP, using one shared engine and the same plan-only-by-default semantics.
- A platform role cannot contain org-local permissions, bare `*`, or negated permissions.
- A merchant role/token cannot grant `platform:*`.
- Integration tests prove the platform and merchant permission planes are disjoint.

---

# #555: service-to-merchant-route-rename-and-unified-principal

**Completed:** yes
**Status:** COMPLETED 2026-06-21. Child of #552. DONE + shipped in **OpenRails v0.48.0**: `/v1/service`→`/v1/merchant` hard cut + gin-duplicate deletion, `issuer` drop (`POST /customers/entitlements:batch`), customer-forward vs directory split, SDK (`remote.go`) move; consumer bump landed (Tensorhub @ v0.48.0). Final #555 follow-up completed 2026-06-21: public `openrails.Client` now mirrors customer-forward lookups with `ListEntitlements`, `HasEntitlement`, `ListActiveEntitlements` (batch), `ListProductAccess`, `HasProductAccess`, and existing balance APIs over the live `/v1/merchant` HTTP surface. Validation: `go test ./...`; `go test -tags=integration ./internal/integrationharness -run TestStandaloneMerchantCustomerLookupClientHTTP -count=1 -v`; `git diff --check`. The FULL uniform-auth model — all credential types on ALL `/v1/merchant` routes + live-DB-subset perms + retire the `#259` `IsDelegatedPermission` allowlist — is carved out to **#564**. The buyer-hat/seller-hat namespace-separation guard is deferred to **#557**, which builds the `/v1/orgs/:org_id/*` treasury surface it needs.

Delete `/v1/service/*` and mount merchant-owned operations under resource-named `/v1/merchant/*`. One shared principal resolver normalizes every credential type into an `(org, merchant, permissions)` principal so the same permission gate serves all callers. Customer identity is `(merchant_id, subject)`, so `issuer` is dropped. See parent #552 "Proposed Merchant Lookup API" and "Route Mounting By Deployment Mode"; permissions come from #554.

## Tasks

- [~] Add a shared merchant-principal resolver that maps logged-in user / delegated JWT / browser-org JWT / API key to `(org, merchant, permissions)`. SUPERSEDED: the first cut was a gin `MerchantPrincipalRequired` (commit f4a70655), but that was the WRONG abstraction (the live `/v1/merchant` is router-based, not gin) and is now DELETED (commit 9677a9dc). The canonical unified resolver is the router `merchantActionPermissionMW` (`internal/http/routes/routes.go`), which normalizes service-cred + delegated + user-session on the ACTION surface. The FULL uniform model (all credential types on ALL routes incl. the machine routes, + live-DB-subset perms) is tracked in **#564**.
- [~] DEFERRED to #557 (needs the treasury surface that #557 builds): keep the seller hat and buyer hat on disjoint namespaces. Since org↔merchant is 1:1 (#527), one org id is both a **merchant-owner** (`merchant:*`, merchant routes) and a **paying customer** over its own balance (`customer:*`, treasury routes). The route's required permission IS the context: gate merchant routes on `merchant:*`, treasury routes on `customer:*`, never a bare `org:*`. Disjoint namespaces (owner-owns-resources grants them as separate globs, neither covering the other) mean neither token satisfies the other's gate — no special mechanism, just the permission check. Regression test proving both directions 403 lives in #557.
- [x] Delete the surviving #553 compatibility alias as part of this cut: removed the `/v1/service/tier` route alias (only `/v1/merchant/trust-tier` remains). The matching `tier` Go-client field removal stays coordinated with #558/Tensorhub.
- [x] Remove the credential-type-split auth middleware; gate `/v1/merchant/*` on the resolved principal. DONE under #564: the router `merchantActionPermissionMW` now covers both action and former service routes (`/admit`, `/credits/*`, catalog, payment providers), accepts service credentials, delegated JWTs, and user sessions by permission, and the #259 `IsDelegatedPermission` browser-safe allowlist is gone. AuthKit bounds remote-app-signed permission claims to stored authority and rejects over-claims.
- [x] Move every merchant-owned `/v1/service/*` route to `/v1/merchant/*`; delete the old paths (hard cut, no alias). DONE: the canonical surface is the router-based `routes.RegisterServiceRoutes` mounted at `/v1/merchant` (standalone via `registerMerchantActionRoutesAt`, embedded via `RouteSetMerchantAPI`); the gin `ginroutes.RegisterServiceRoutes` + `routes_service.go` + `ServiceRoutePrefix` are deleted; SDK (`remote.go`) + all integration tests moved to `/v1/merchant`. Validated green: unit (72 pkgs), integrationharness, tests/ service+route-set suites, embed boundary.
- [x] Implement the customer-forward lookup API over HTTP: list/check entitlements, batch entitlements, list/check product access, get balance. DONE: entitlements batch route, customer entitlement list, product-access list/check route with `?product_id=`, and credit-account balance route are live under `/v1/merchant`.
- [x] Mirror those as Go library methods (`ListEntitlements`, `HasEntitlement`, `ListActiveEntitlements` batch, `ListProductAccess`, `HasProductAccess`, existing `Balance`/`GetCreditAccount`) with HTTP-aligned names/shapes. DONE 2026-06-21.
- [x] Replace `POST /v1/service/customers/by-external-subject/entitlements` with `POST /v1/merchant/customers/entitlements:batch`; drop `issuer` (merchant/org from auth, subjects from body). DONE under v0.48.0; the new SDK continues to call only `/v1/merchant/customers/entitlements:batch`.
- [x] Split customer-forward lookups from directory/filter reverse lookups (`GET /v1/merchant/entitlements/:name/customers`) in both HTTP and Go. DONE: forward methods are subject/customer-facing; reverse lookup remains `ListCustomersWithEntitlement`.
- [~] Update Doujins / Tensorhub / Cozy Art call sites to the new paths/principal; bump in lockstep. ASSESSED 2026-06-21: all three use the OpenRails Go SDK (`openrails.NewRemote`), NOT raw `/v1/service` paths — the `/v1/service`->`/v1/merchant` move is internal to `remote.go`, so no path edits in consumers. The only breaking SDK signature is `Client.ListActiveEntitlements` (dropped `issuer`). **Tensorhub** calls Admit/Capture/Release/GetTier/Deposit/... (all unchanged) and does NOT call `ListActiveEntitlements` -> ZERO code changes, a clean `go.mod` bump only. **Doujins/Cozy-Art** must drop the `issuer` arg IFF they call `ListActiveEntitlements`. RELEASED 2026-06-21: tagged + pushed **OpenRails v0.48.0** (on authkit v0.44.0). **Tensorhub** bumped to v0.48.0 and VALIDATED green (build + openrailsclient/api tests; its test mock servers moved `/v1/service`->`/v1/merchant`; no production code change) — staged in the tensorhub tree, not pushed (its remote diverged + carries unrelated WIP). Doujins/Cozy-Art `go.mod` bumps remain (mechanical) when those repos are next touched.
- [x] Tests: unknown subjects return `[]`; HTTP and Go return matching customer-forward shapes. DONE 2026-06-21: `TestStandaloneMerchantCustomerLookupClientHTTP` exercises live standalone HTTP + real Postgres/Redis for batch/single/check entitlements, product-access list/check, and balance. Credential-type parity is owned by #564.

## Acceptance

- `/v1/service/*` is gone; merchant-owned ops live under `/v1/merchant/*`.
- Uniform credential parity is owned by #564; #555's route rename/customer-lookup scope is complete.
- `issuer` is no longer accepted or required.
- Customer-forward lookups are mirrored between HTTP and Go.

---

# #556: embedded-route-set-presets

**Completed:** yes
**Status:** COMPLETED 2026-06-22: route-set presets now use the deployment names `checkout`, `customer`, `merchant_admin`, `merchant_settings`, `merchant_api`, and `webhooks`. `public_catalog` is gone. `merchant_admin` mounts merchant support/admin routes; `merchant_settings` separately mounts catalog/payment-provider management; `merchant_api` remains machine-to-machine. Embedded defaults mount `checkout`, `customer`, `merchant_admin`, and `webhooks`, and exclude both `merchant_settings` and `merchant_api`. Standalone mounts every route set. The browser `/me/*` route-table recut remains owned by #557; the existing embedded self-service handler is still `pkg/embedded/gin.SelfHandler`.

Replace the embedded `IncludeUser` / `IncludeAdmin` / `IncludeWebhooks` booleans with named `RouteSet` values + presets so hosts can see exactly what mounts. Embedded defaults exclude host-internal machine HTTP routes (the host calls the Go service directly); standalone includes them. See parent #552 "Embedded Route Group API".

Host contract: a normal embedded host mounts the zero-value/default handler and
gets checkout, customer self-service, merchant admin, and webhooks. It opts into
`merchant_settings` only when it wants provider/catalog/config management
available over HTTP, and into `merchant_api` only when it intentionally wants
machine-to-machine HTTP/API-key compatibility.

## Tasks

- [x] Define a `RouteSet` type and replace the old boolean handler options.
- [x] Replace the current canonical sets with: `checkout`, `customer`, `merchant_admin`, `merchant_settings`, `merchant_api`, and `webhooks`.
- [x] Collapse old `public_catalog` into `checkout`; `checkout` owns products/prices/config discovery plus checkout/pay flows.
- [x] Rename self-service route-set grouping to `customer`; the full `/me/*` route recut stays in #557.
- [x] Add `merchant_admin` for human admin customer/support/payment/subscription operations.
- [x] Add `merchant_settings` for HTTP-accessible provider secrets, catalog pushes, and merchant config/settings.
- [x] Keep `merchant_settings` out of embedded defaults so hosts like Doujins can manage provider/catalog/settings through CLI or internal calls instead of exposing them over HTTP.
- [x] Keep `merchant_api` machine-to-machine only; embedded default excludes it, standalone mounts it.
- [x] Do not fold `/me/*` into #556; #557 owns the neutral customer self-service route recut. Existing embedded self-service remains available via `pkg/embedded/gin.SelfHandler`.
- [x] Define `EmbeddedDefaultRouteSets` as `checkout`, `customer`, `merchant_admin`, and `webhooks`.
- [x] Define `StandaloneDefaultRouteSets` as embedded default plus `merchant_settings` and `merchant_api`.
- [x] Mount route groups by RouteSet; let hosts opt into `RouteSetMerchantSettings` for HTTP settings/catalog/provider management and `RouteSetMerchantAPI` for HTTP loopback parity.
- [x] Update public docs/examples to show the host-facing rule: zero-value embedded options for the normal host; append `RouteSetMerchantSettings` only for explicit HTTP settings/catalog/provider management; append `RouteSetMerchantAPI` only for explicit machine HTTP compatibility.
- [x] Migrate embedded hosts (Cozy Art, Tensorhub) off the boolean flags (hard cut); delete the booleans.
- [x] Tests: embedded default mounts checkout/customer/merchant-admin/webhooks; embedded default does not mount merchant settings or machine API; standalone mounts both; opt-in adds each route set.
- [x] Integration test through a real `httptest.NewServer` HTTP client/server proving embedded route-set defaults and opt-in behavior. PASS 2026-06-22: `go test -tags=integration ./tests -run TestHTTPHandlerOptions_RouteSetPresetsOverHTTPServer -count=1 -v`.

## Acceptance

- Hosts declare route sets, not booleans.
- Route-set names map to product surfaces, not implementation leftovers.
- Host apps can choose correctly from names alone: normal embedded default, explicit `merchant_settings` opt-in, and explicit `merchant_api` opt-in.
- Embedded default exposes real OpenRails checkout/merchant-admin/webhook routes directly, with no host proxy routes needed; `/me/*` customer self-service remains the #557 route-table recut and is currently available through `pkg/embedded/gin.SelfHandler`.
- Standalone mounts every route set; embedded default skips HTTP settings/config/catalog management and host-internal machine HTTP routes.
- `public_catalog` is gone as a standalone route set; buyer catalog discovery lives under `checkout`.
- Provider secrets, catalog pushes, and merchant config/settings live under `merchant_settings`, not `merchant_admin`.

---

# #557: customer-self-and-org-treasury-route-recut

**Superseded by #567 (2026-06-22):** the `/v1/self/*`→`/v1/me/*` normalization stands. But the `/v1/orgs/:org_id/*` treasury bucket and the "personal balances are never delegable" rule are retired — they fold into the universal `customer` persona surface (`/v1/me/*` + `/v1/customers/:id/*`), where every customer can delegate. The org-treasury parts below are historical.

**Completed:** yes
**Status:** COMPLETED 2026-06-22. Child of #552. `/v1/me/*` is the only personal customer self-service surface; `/v1/self/*` has no current source/docs references. `/v1/orgs/:org_id/spend-delegations` is live in standalone and embedded Gin self handler, backed by the existing payer-owned invoker spend-limit store, with the payer forced to the resolved org/merchant id. The route gates on `customer:spend-delegations:read|update` only; merchant permissions do not satisfy treasury routes and customer permissions do not satisfy merchant routes. Doujins was updated in lockstep from `/v1/self/*` to `/v1/me/*` and routes `/v1/orgs/*` to the embedded self/org handler. Validation: `go test ./internal/http/handlers ./internal/http/routes/ginroutes ./internal/http/routes ./pkg/embedded/gin`; `go test -tags=integration ./tests -run TestOrgTreasurySpendDelegationsHTTPFullReplacement -count=1 -v`; Doujins `go test ./internal/billing/openrailsembed`; Doujins `go build ./...`; Doujins frontend `pnpm build`; Doujins real docker-compose stack targeted Playwright proof: `E2E_API_BASE_URL=http://localhost:25252 pnpm -C frontend exec playwright test e2e/openrails/billing-live.spec.ts -g "billing route recut is live on the compose stack" --project=chromium` after starting the stack with `task e2e-test -- openrails-contract` and isolated compose ports; `git diff --check` in both repos.

Normalize personal customer self routes under `/v1/me/*` and delete `/v1/self/*`. Add the org-customer treasury bucket at `/v1/orgs/:org_id/*`, including `GET/PUT /v1/orgs/:org_id/spend-delegations` for sharing an org balance under budget windows, gated by `customer:spend-delegations:*` (#554). Personal/individual balances are never delegable. See parent #552 "Customer Self-Service Route Recut".

## Tasks

- [x] Move all personal customer self routes to `/v1/me/*`; delete `/v1/self/*` (hard cut, no alias). `/v1/me/*` needs no OpenRails permission beyond authenticated self-subject. Verified current Gin/server route registration uses `/v1/me/*`; remaining `/v1/self/*` references are stale docs/tracker notes only.
- [x] Add the `/v1/orgs/:org_id/*` treasury bucket, AuthKit-org scoped. DONE 2026-06-22: standalone mounts it under `/v1/orgs`; embedded `embgin.SelfHandler` mounts the same real route table under `/billing/v1/orgs`.
- [x] Keep the buyer hat and seller hat on separate namespaces. The same org id owns its merchant (`merchant:*`, seller) AND is a customer over its own balance (`customer:*`, buyer) — org↔merchant is 1:1 (#527). Gate the treasury route on `customer:spend-delegations:*` ONLY; never on `merchant:*` or a bare `org:*` wildcard. Because the namespaces are disjoint (and owner-owns-resources grants `merchant:*` and `customer:*` as separate globs, not one covering the other), a merchant-only token can't reach the treasury and a treasury-only token can't reach merchant routes — by permission, no special mechanism. DONE 2026-06-22: route tests prove both directions 403.
- [x] Implement `GET/PUT /v1/orgs/:org_id/spend-delegations` as a full-replacement document; gate read on `customer:spend-delegations:read`, write on `customer:spend-delegations:update`. DONE 2026-06-22: backed by the existing payer-owned `invoker_spend_limits` store, with payer forced to the org/merchant id.
- [x] Reject spend-delegation requests against personal/individual customer balances. DONE 2026-06-22: the route has no customer-id path, the payer is forced from the resolved org, and `customer_id` in the document or delegation rows is rejected.
- [x] Update host call sites (Doujins) off `/v1/self/*`; bump in lockstep. DONE 2026-06-22: Doujins embedded mount dispatch, payments clients, and E2E direct paths now use `/v1/me/*`; `/v1/orgs/*` is routed to the embedded self/org handler.
- [x] Tests: personal self-access needs no org permission; org-treasury access requires the org permission; personal balances cannot be delegated. DONE 2026-06-22: route tests cover auth/permission boundaries and bad personal `customer_id`; integration test exercises PUT/GET/replace over a real HTTP server + test DB.

## Acceptance

- `/v1/self/*` is gone; personal customer self-service is `/v1/me/*`.
- Org balance sharing is `/v1/orgs/:org_id/spend-delegations`, org-customer-only, not a merchant-admin override.

---

# #558: tensorhub-client-recut-and-policy-split

**Completed:** yes
**Status:** COMPLETED 2026-06-22. Child of #552. OpenRails exposes the hard-cut batch-native merchant admission/settings/usage surface and the public SDK is split into `AdmissionClient`, `PolicySyncClient`, `AdminFundingClient`, and `CustomerLookupClient`. Legacy SDK shims/routes are not kept: no registered `/v1/merchant/admit`, `/admit/batch`, `/credits/holds/*`, `/credits/usage/*`, `/budget`, `/abuse-usage`, credit-limit read, direct withdraw, transaction lookup/list, or account-settings routes remain in Go. Tensorhub is cut to `AdmitBatch`, `SetMerchantSettings`, and `SetOrgSpendDelegations` on released OpenRails `v0.50.0` with no local replace; source grep has no active old OpenRails route/method references. Validation: OpenRails packages and org-treasury integration listed under #552; Tensorhub `go test ./internal/billing/openrailsclient ./internal/api ./internal/orchestrator/...`; Tensorhub `go test ./...`; Tensorhub Docker-backed `go test -tags=integration ./internal/billing/openrailsclient -count=1 -v`.

Replace Tensorhub's broad `/v1/service/*` dependency with three narrow OpenRails Go interfaces (`AdmissionClient`, `PolicySyncClient`, `AdminFundingClient`) plus a minimal `/v1/merchant/*` HTTP surface for standalone/remote. Admission is batch-native. Merchant-wide admission policy lives in `/v1/merchant/settings`; payer authorization stays on the payer. See parent #552 "Tensorhub Merchant API Recut" and the admission/policy split under "Merchant Admin Frontend Surface".

## Tasks

- [x] Define `AdmissionClient` (batch admit / capture / release / wasted-spend / get-trust-tier), `PolicySyncClient` (single merchant settings document), `AdminFundingClient` (deposit, credit-limit, usage rollup, resource-revenue), and `CustomerLookupClient` Go interfaces.
- [x] Make admission batch-native: one `POST /v1/merchant/admissions` + one Go method taking an item array (a single request is a one-item batch); no separate single-admit route. Added `/v1/merchant/admissions/:request_id/capture`, `/v1/merchant/admissions/:request_id/release`, and `POST /v1/merchant/wasted-spend`.
- [x] Move merchant-wide policy (`tier_schedules`, `tier_spend_limits`, `delegated_invoker_wasted_spend_limits`) into `GET/PUT /v1/merchant/settings` as one document; added Go `GetMerchantSettings` / `SetMerchantSettings`.
- [x] Keep invoker/role spend authority attached to the payer; no merchant-level "invoker may spend anyone's balance" grant. Preserved generic admission fields (payer id, invoker, invoker type, trust tier, role UUIDs, estimated amount, request id, resource).
- [x] Delete Tensorhub-unused service routes (hard cut): `budget`, merchant-configuration read, `abuse-usage`, credit-limit read, direct credit `withdraw`, transaction lookup, account-settings read/write, generic credit transactions are no longer registered under `/v1/merchant/*`.
- [x] Remove the surviving #553 route/client compatibility alias from the OpenRails public SDK surface for this cut: no public `Client.Admit`, `CaptureHold`, `ReleaseHold`, `WithdrawCredits`, config setter shim, or old merchant policy route remains. NOTE: request-field `tier` aliases still exist on DTOs until downstream source stops constructing them; the registered route/API surface is hard-cut.
- [x] Keep OpenRails invoicing, processor state, and arrears charging internal; Tensorhub only configures limits/policy and consumes ledger/admission results.
- [x] Embedded Tensorhub uses the Go interfaces directly (no merchant HTTP mount); standalone uses the HTTP surface. DONE: Tensorhub pins OpenRails v0.50.0 with no local replace; Tensorhub source grep has no old service/self route or removed SDK references.
- [x] Tests: hot path admit/capture/release; one-item batch path via conformance; settings install policy without exposing customer delegated-spend as merchant-admin routes. See validation in Status.

## Acceptance

- Tensorhub hot path uses only admit/capture/release/wasted-spend/trust-tier read; policy sync and admin funding/reporting are separate interfaces.
- Admission has one batch-shaped API; a one-item request uses the same route/shape.
- Removed service routes have no remaining first-party caller.

# #561: merchant-customer-support-admin-surface

**Completed:** yes
**Status:** COMPLETED 2026-06-22. Child of #552. The resource-named `/v1/merchant/*` support/admin surface is mounted, permission-gated, and covered by route-permission plus Docker-backed integration tests. The old delegated billing-admin Gin bundle is removed rather than aliased; first-party admin integration tests now call `/v1/merchant/*` with JWT-shaped delegated test credentials. Final hard cut also removed the stale embedded/self `/v1/admin/*` mounts and tests. Validation: `go test ./...`; `go test -tags=integration ./tests -run 'TestAdmin|TestHTTPHandlerOptions_RouteSetPresetsOverHTTPServer' -count=1 -v`; `go test -tags=integration ./internal/integrationharness -run 'TestStandaloneMerchantAdmitAcceptsDelegatedJWTByPermissionHTTP|TestStandaloneMerchantAdmitAcceptsUserSessionByPermissionHTTP|TestDelegatedAdminCrossMerchantIsolationHTTP|TestDelegatedSelfTokenSubjectIsolationHTTP' -count=1 -v`; `rg -n "RegisterAdminRoutes|AdminRoutePrefix|/v1/admin|billing/v1/admin|/billing/v1/admin" internal pkg tests embed -g'*.go'` returns no Go references; `git diff --check`.

Resource-named `/v1/merchant/*` admin surface for one merchant's support staff: customer profile (incl. `trust_tier`), balance/transactions, saved payment-method metadata read-only, entitlement and product-access grant/revoke, off-channel payments, refunds, subscription cancel/payment-method reassignment, merchant-wide payments/subscriptions, usage rollups, balance-adjustments, credit-limit, and repair-alerts. Gated by `merchant:customer-settings:*` / `merchant:payments:*` / `merchant:subscriptions:*` / `merchant:usage:read` / `merchant:repair-alerts:read` (#554). Manual grants go through the grant ledger with acting-admin audit; refund/cancel make access-revocation explicit. Merchant admins cannot touch platform lifecycle or create/update/delete customer saved payment methods. See parent #552 "Merchant Admin Frontend Surface".

## Tasks

- [x] Mount resource-named customer support reads under `/v1/merchant/customers/:customer_id`: profile includes `trust_tier`, current balance sections, entitlements, product access, payments, and saved payment-method metadata.
- [x] Add read-only saved payment-method metadata list: `GET /v1/merchant/customers/:customer_id/payment-methods`; no merchant route creates/updates/deletes customer payment methods.
- [x] Mount manual entitlement grant/revoke + product-access grant/revoke under `/customers/:customer_id/{entitlements,product-access}` using the existing grant-ledger handlers. Keep write routes named `/product-access`, not `/products`.
- [x] Mount off-channel payment at `POST /v1/merchant/customers/:customer_id/payments/off-channel`; it requires `price_id` + `transaction_id` and routes through `CheckoutService.RegisterPurchase`.
- [x] Add explicit `revoke_access` handling to refund and subscription-cancel request bodies; do not silently couple money reversal/cancel to entitlement revocation. Refund revokes one-off entitlements/product access by payment only when requested; admin subscription cancel revokes subscription/grace entitlements only when requested.
- [x] Mount merchant-wide payments/subscriptions search/read and customer-filtered payment history; mount subscription cancel/resume/payment-method reassignment. Reassignment validates the new payment method against the subscription's own customer/merchant.
- [x] Decide whether `balance-adjustments`/`credit-limit` need new resource-named customer routes: no alias for now. Existing `/v1/merchant/credits/{deposit,withdraw}` + `/v1/merchant/credit-limit` remain until a real admin UI caller needs a friendlier route.
- [x] Decide whether usage rollup needs `POST /v1/merchant/usage/rollup`: no alias for now. Existing `/v1/merchant/credits/usage/rollup` remains until a real caller needs it moved.
- [x] Mount `GET /v1/merchant/repair-alerts`; keep `manual-rebill-attempts` cut.
- [x] Tests: route-permission unit coverage for support routes; live Docker-backed standalone coverage for delegated admin profile isolation and self-token denial on `/v1/merchant/customers/:customer_id`.
- [x] Final hard cut: remove the old delegated `/v1/admin/*` mount/tests after first-party tests and clients are moved to `/v1/merchant/*`.

## Acceptance

- Customer-support surface is resource-named under `/v1/merchant/*`; no credential-shaped `/v1/admin/*`.
- Manual grants are grant-ledger audited with the acting admin.
- Refund and cancel access behavior is explicit.
- Merchant admins cannot create/delete/export/disable/reassign merchants.

---

# #562: delete-dead-platform-org-wiring

**Completed:** yes
**Status:** COMPLETED 2026-06-20. Deleted the dead core platform-org/platform-superadmin wiring. Validation: `go test ./...`, `go build ./...`, `go vet ./...`, and `go test -tags=integration ./internal/integrationharness -run TestCoreDoesNotMountPlatformAdminRoutesHTTP -count=1 -v` pass.

The `platform:*` and org planes already exist and are already separate, and platform/operator permissions live in OpenRails SaaS (not core). This removed the vestigial core wiring: a mount switch keyed on an always-empty org slug, and a single coarse gate aliased to one platform permission.

## Tasks

- [x] Delete `internal/http/routes_platform.go` core route registration entirely, including the empty-slug `PlatformOrgSlug() == ""` mount switch.
- [x] Delete core cross-merchant admin route wiring that depends on `PermPlatformSuperadmin` (including `/v1/admin/merchants*` style routes).
- [x] Remove the `PermPlatformSuperadmin` coarse alias in `internal/controlplane/catalog.go` and all references.
- [x] Remove `PlatformOrgSlug()` / `HasPlatformSuperadmin` from the core control-plane authority.
- [x] Update route comments/docs: drop remaining "platform org" language; state the platform/operator surface lives in OpenRails SaaS.
- [x] Build/tests: `go build ./...` + `go vet ./...` clean; no core route is gated by a fake platform org.

## Acceptance

- No dead platform-org wiring remains in core; `/v1/platform/*` and core cross-merchant admin routes are gone.
- The `PermPlatformSuperadmin` coarse alias is gone.
- Docs/comments use "platform RBAC" / "org-local merchant RBAC", not "platform org".

---

# #330: nmi-immediate-subscription-checkout-stuck-pending

**Completed:** no
**Status:** IN_PROGRESS 2026-06-08: immediate NMI checkout activation patch is implemented; package tests and focused mock-provider regression pass; configured-account NMI recurring test has been added and compiles/skips locally because `NMI_SANDBOX_SECURITY_KEY` is not set. Remaining work: run real NMI credential test and repair/replay Paul2 after fixed code is deployed.

Fix the NMI subscription checkout path where an immediately approved recurring checkout is persisted as pending, leaving host apps such as Doujins stuck on the pending-subscription screen even though NMI accepted the transaction.

## Metadata

- Category: bug
- Status: in_progress
- Passes: false

## Live findings from Doujins/Paul2 on 2026-06-08

- OpenRails received the checkout request and `POST /v1/self/checkout` returned HTTP 200.
- NMI accepted the card vault and transaction: payment vault `314825442`, provider transaction `12162933364`, provider subscription `12162933429`.
- Local checkout session `019ea96b-221b-7274-a341-8cdc85cb72d6` was marked `succeeded` with processor `mobius` and amount `2300 usd`.
- Local subscription `019ea96b-223b-75b1-926b-35956d10eba5` stayed `pending`, with no current period start/end timestamps.
- Local payments only had a pending attempt row keyed as `nmi_sub_attempt:sub_cb204b034d2c3ec46be93b0470ff44df`; there was no completed payment row keyed by the real NMI transaction id.
- No billing entitlement rows were created for the user, so Doujins correctly kept showing the pending-subscription state.

## Root cause

`processNMISubscription` called `AddRecurringSubscription` successfully, but `completeNMISubscriptionRegistration` intentionally created a local pending subscription and returned a pending checkout response. The initial NMI response was not used to synchronously activate an immediate subscription, set the current billing period, create a completed payment, or grant entitlements. Webhooks should not be required for the initial happy path when NMI immediately approves the transaction; delayed/future starts can remain pending.

## Desired behavior

For immediate NMI subscription approvals, OpenRails should finish the local checkout atomically from the direct provider response: mark the subscription active, set current period timestamps, record the completed payment against the real provider transaction id, grant entitlements, and return a succeeded checkout response. Delayed-start subscriptions and genuinely asynchronous provider states should remain pending and rely on follow-up provider events/reconciliation.

**Tasks:**
- [x] Capture live-stack evidence for the Paul2 failure: NMI accepted the transaction, checkout succeeded, subscription stayed pending, payment stayed as a pending attempt, and no entitlement was granted.
- [x] Identify root cause in the NMI checkout finalization path.
- [x] Patch immediate NMI subscription finalization to activate the subscription, set period dates, create the completed payment, and grant entitlements without waiting for a webhook.
- [x] Preserve pending behavior for delayed/future-start NMI subscriptions.
- [x] Fix stale integration-test helpers that still query billing tables by `user_id` instead of `tenant_subject_id`.
- [x] Add/validate focused integration coverage proving an immediate NMI subscription checkout grants the premium entitlement synchronously.
- [x] Add an actual NMI test-account integration path so the live provider contract is exercised, guarded by NMI test credentials.
- [ ] Run the actual NMI configured-account recurring-subscription integration with `NMI_SANDBOX_SECURITY_KEY` set.
- [x] Run focused OpenRails checkout/module tests and NMI regression tests.
- [ ] After deployment/restart, repair or replay the affected live pending subscription row for Paul2 if it still exists.

---

# #328: robinhood-coinbase-usdc-funding-sessions

**Completed:** no
**Status:** PARTIAL 2026-06-08: Implemented Solana-only USDC funding session APIs, persistence, config, Coinbase hosted-session adapter with CDP JWT auth, Coinbase Hook0-signed Onramp webhook/status ingestion, Robinhood launch-template handoff, provider eligibility gates, self-service routes, idempotency, structured insufficient-USDC funding context on checkout errors, backend Solana USDC balance verification, focused tests, and DB-backed self-service API tests for create/get, merchant/user isolation, idempotency, and unsupported provider/network rejection. Retained for future provider integration work; current Doujins UX uses manual Robinhood/Coinbase links plus connected-wallet balance checks instead of OpenRails provider sessions. Remaining: real Robinhood partner adapter/status docs and access.

Plan and implement OpenRails-owned USDC funding sessions for host apps that need users to buy or transfer USDC into their own live Solana wallet before completing a Solana wallet checkout.

## Metadata

- Category: feature
- Status: partial
- Passes: false

## Goal

- OpenRails should expose a provider-backed funding-session API for Robinhood and Coinbase only. Host apps such as Doujins can ask OpenRails for a funding URL, send the user to the provider in a new tab or popup, then resume checkout after OpenRails and/or the host app verifies that the user's live Solana wallet has enough USDC.

## Product Behavior

- The user already has or creates a Robinhood/Coinbase account on the provider site.
- OpenRails does not custody funds and does not collect provider KYC; the provider handles account login, payment method, buy/transfer, KYC, and compliance.
- Provider redirect/return means the provider flow ended; it is not proof that the wallet is funded.
- Completion must be based on provider status/webhooks when available plus Solana wallet-balance verification.
- Only offer a provider when it can fund USDC on Solana. Coinbase/Base and all EVM chains are out of scope.

## Scope

- Implement Robinhood and Coinbase integration surfaces only for Solana USDC.
- Do not implement Ramp, Transak, MoonPay, PayPal, Venmo, Base, Ethereum, Polygon, Arbitrum, Optimism, or bridge paths for this issue.
- Keep provider abstraction narrow but extensible enough that more providers could be added later without changing the host-app contract.

**Tasks:**
- DESIGN:
- [x] Define the OpenRails funding-session contract for browser self-service callers: provider preference, wallet address, asset, network, requested amount, checkout_session_id, return_url, and idempotency key.
- [x] Define provider statuses and normalize them into OpenRails statuses such as created, opened, pending_provider, pending_settlement, funded, failed, expired, and cancelled.
- [x] Define Solana-only compatibility rules for USDC funding.
- [x] Decide whether funding amount comes from the checkout-session shortfall, an explicit requested amount, or both with server-side validation. Implemented explicit requested amount with optional checkout_session_id context.
- [x] Decide how provider ranking is configured per merchant: Robinhood preferred, Coinbase fallback. Implemented default provider order with Robinhood first and Coinbase second.
-
- DATA MODEL:
- [x] Add a funding/onramp session table with tenant_id, user_id, checkout_session_id, provider, wallet_address, asset, network, requested_amount, provider_session_id, provider_url, status, return_url, idempotency key, timestamps, and provider metadata.
- [x] Add indexes for tenant/user lookup, checkout_session_id lookup, provider_session_id lookup, and idempotency.
- [x] Store provider secrets/config in OpenRails config, never in host apps.
-
- API:
- [x] Add `POST /v1/self/usdc-funding-sessions` to create a Robinhood/Coinbase funding session for the authenticated browser user.
- [x] Add `GET /v1/self/usdc-funding-sessions/:id` to return normalized funding status and provider URL/status details safe for frontend polling.
- [x] Add `GET /v1/self/usdc-funding-options` to list eligible Robinhood/Coinbase options for wallet, network, asset, amount, and optional checkout_session_id.
- [x] Add provider webhook/status callback endpoints where Coinbase supports them. Implemented signed Coinbase Onramp webhook ingestion on the existing provider webhook route; Robinhood remains blocked on partner docs/access.
- [x] Enforce self-service auth, merchant boundaries, and idempotency on funding-session create/read routes.
-
- PROVIDERS:
- [x] Implement a Coinbase provider adapter that creates a hosted onramp URL/session with destination wallet, network, asset, amount, return URL, and partner/user reference, including short-lived CDP JWT bearer generation from Coinbase secret API keys.
- [x] Implement Coinbase status/webhook handling and map provider lifecycle into OpenRails funding-session status. Coinbase success maps to pending_settlement; only live Solana wallet-balance verification can mark funded.
- [ ] Implement a Robinhood provider adapter after partner docs/access are available, supporting external handoff into Robinhood Connect and funding into the user's live wallet.
- [ ] Implement Robinhood status/webhook handling if exposed by partner API; otherwise rely on return handling plus on-chain wallet-balance verification.
- [x] Add provider availability checks so unsupported network/asset combinations are hidden rather than offered.
-
- WALLET VERIFICATION:
- [x] Reuse existing Solana USDC balance-checking code where possible to verify the funded wallet before marking a session funded for Solana checkout.
- [x] Do not add Base/EVM balance verification for this issue; Solana is the only supported chain.
- [x] Ensure returning from a provider only triggers polling/checking; it must not mark the session funded by itself.
-
- CHECKOUT INTEGRATION:
- [x] Allow a funding session to reference the checkout session that produced an insufficient-USDC state.
- [x] Ensure insufficient-USDC API errors expose enough structured amount/network/wallet context for host apps to create a funding session. Added `error.metadata.usdc_funding` with Solana network, USDC asset, wallet, decimal amount/balance/shortfall, and base-unit values.
- [x] Keep final subscription/payment creation in the existing checkout confirmation path after the wallet is funded.
-
- VERIFY:
- [x] Add unit tests for provider eligibility and network compatibility gates.
- [x] Add API tests for create/get funding session, merchant isolation, idempotency, and unsupported-provider/network rejection.
- [x] Add provider adapter tests with mocked Coinbase responses.
- [x] Add wallet-balance verification tests proving redirect alone is insufficient through status semantics and frontend polling contract.
- [x] Document the host-app integration contract for Doujins in config.example.yaml and the tracker issue.

---

# #111: admin-rate-limiting

**Completed:** no

Add rate limiting to admin endpoints to limit blast radius of compromised JWT

## Metadata

- Category: security
- Status: not_started
- Passes: false

**Tasks:**
- STEPS:
- [ ] PROBLEM: If admin JWT is leaked, attacker could cancel/refund thousands of users before detection
- [ ] Implement per-admin-user rate limiting using Redis (keyed by admin user ID from JWT)
- [ ] Define rate limits for destructive operations:
-     - Cancellations: 5/minute, 10/hour, 50/day
-     - Refunds: 5/minute, 10/hour, 50/day
-     - Entitlement revocations: 5/minute, 10/hour, 50/day
- [ ] Define rate limits for bulk/expensive operations:
-     - Extend operations: 3/minute
-     - Off-channel payments: 10/minute
-     - Admin grants: 10/minute
- [ ] On rate limit exceeded: lock out admin for extended period (e.g., 1 hour) and alert
- [ ] Add alerting/notification when rate limits are approached (e.g., 80% threshold)
- [ ] Create admin rate limit middleware that wraps destructive endpoints
- [ ] Allow super-admin or manual override to unlock rate-limited admin if legitimate
- [ ] Log all rate limit events for security audit
- BENEFIT: Limits blast radius - attacker can only affect ~5-10 users before getting locked out

---

# #126: test-architecture-improvements

**Completed:** no

Tests should use real structs and functions from production code, not invent test-specific abstractions

## Metadata

- Category: testing
- Status: not_started
- Passes: false

## Details

- current_problems: ["SubscriptionOptions helper struct uses different field names than models.Subscription (PeriodStart vs CurrentPeriodStartsAt)","Tests can pass while using wrong field names because helpers translate between them","When a model field is renamed/removed, tests may not break because helper abstracts it away","Developers get confused about which struct to use and what fields are available"]
- example_bad: {"code":"suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{PeriodStart: now})","problem":"SubscriptionOptions.PeriodStart doesn't exist on models.Subscription"}
- example_good: {"code":"sub := &models.Subscription{CurrentPeriodStartsAt: now, ...}; suite.CreateTestSubscription(sub)","benefit":"Uses real model, compiler catches field name errors"}
- philosophy: {"principle":"Tests should verify actual production code behavior, not test-specific wrappers","goal":"When production code changes, tests should break if they're testing the affected behavior","anti_pattern":"Test helper structs that diverge from production models hide API changes and create maintenance burden"}
- recommendations: ["Use models.Subscription directly in tests instead of SubscriptionOptions","Use api.ListResponse[T] for parsing API responses instead of map[string]interface{}","Import and use the actual request/response structs from handlers","If a helper is needed, it should take the real model struct as input, not a parallel struct","Test assertions should use strongly-typed response parsing"]

**Tasks:**
- REFACTORING:
- [ ] Audit all test helper structs (SubscriptionOptions, PaymentMethodOptions, etc.)
- [ ] Replace helper structs with direct model usage where possible
- [ ] Update CreateTestSubscriptionWithOptions to accept *models.Subscription
- [ ] Use json.Unmarshal with actual API response types instead of map[string]interface{}
- [ ] Add linter rule or CI check to discourage map[string]interface{} in tests

---

# #158: ccbill-dunning-grace-entitlements

**Completed:** no

Adjust CCBill subscription entitlement logic to model paid term + retries (dunning) + end-of-term expiration. Use CCBill webhook fields like nextRetryDate/nextRenewalDate to drive subscription status and finite entitlement windows, and cap any grace extensions to avoid unlimited free access.

**Tasks:**
- SPEC:
- [ ] Verify CCBill webhook sequence in sandbox: cancel mid-term -> Expiration at end-of-term (and timing)
- [ ] Decide grace policy during retries: disabled vs enabled; cap strategy (extend-to-nextRetryDate vs fixed extra days)
- [ ] Decide policy for CCBill `Cancellation.source=failedRB`: treat as terminal end (no more retries) vs still paid-through end-of-term
-
- DATA MODEL:
- [x] Ensure we can store current_period_ends_at (from nextRenewalDate) for CCBill subscriptions
- [x] Store next_retry_at (from nextRetryDate) and optional grace_ends_at (policy derived)
- [x] Decide whether to reuse existing `subscriptions.next_retry_at` fields for CCBill (recommended) vs add CCBill-specific columns
-
- WEBHOOK -> STATE MACHINE:
- [x] NewSaleSuccess: set paid-term end; grant entitlements for [now, paid_term_end)
- [x] RenewalSuccess: extend paid-term end; extend entitlement windows
- [x] RenewalFailure: set past_due/dunning; persist next_retry_at from webhook; optionally extend entitlements within grace cap
- [x] Cancellation: mark cancelled but keep access until paid_term_end (do NOT revoke immediately)
- [x] Expiration: mark ended/expired and end/revoke entitlements
-
- CODE CHANGES (expected touch points):
- [x] `internal/services/webhook_ccbill.go`: parse and apply `nextRenewalDate` on `NewSaleSuccess` and `RenewalSuccess` (don’t rely solely on `price.billing_cycle_days`)
- [x] `internal/services/webhook_ccbill.go`: on `RenewalFailure`, parse `nextRetryDate` and update `subscriptions.status=past_due` + `subscriptions.next_retry_at` (avoid calling `SubscriptionLifecycleService.FailMembership`, which schedules NMI-style retries)
- [x] `internal/services/webhook_ccbill.go`: on `Cancellation`, call `CancelMembership` with `RevokeAccess=false` (today it passes true)
- [x] `internal/services/lifecycle_service.go`: update `CreateMembership` entitlement grant behavior for CCBill to create finite windows ending at `current_period_ends_at` (instead of indefinite `end_at=NULL`)
- [x] `internal/services/lifecycle_service.go`: update `RenewMembership` for CCBill to extend entitlements to match the new `current_period_ends_at` (today renewal doesn’t extend entitlements at all unless a downgrade changes entitlement set)
- [x] `internal/db/repo/entitlement.go`: make `IsEntitled` / “active entitlement” queries explicitly exclude `deleted_at` (don’t rely on implicit soft-delete behavior)
- [x] `config/config.go`: add a feature flag / config for CCBill grace behavior (enable/disable + max cap)
-
- ANALYTICS:
- [x] Ensure ClickHouse subscription_events reflect dunning/cancelled/expired transitions for CCBill
-
- TESTS:
- [x] Add integration test: NewSaleSuccess -> RenewalFailure(nextRetryDate) keeps access until paid_term_end (+ optional grace) -> RenewalSuccess extends window
- [x] Add integration test: Cancellation mid-term does NOT revoke immediately; entitlement ends at paid_term_end; verify Expiration handling if CCBill sends it
- [x] Add unit test for `EntitlementRepo.IsEntitled` to ensure deleted rows are excluded (regression guard)

---

# #164: cloudflared-managed-dev-tunnel

**Completed:** no

Make `Config.Cloudflared` an actual supported dev feature: OpenRails can (optionally) run and manage a Cloudflare Tunnel for local/dev, so a full local stack (e.g., multiple host apps + billing) can be accessed from an external device (phone) over HTTPS.

## Metadata

- Category: devx
- Status: planned
- Passes: false

## Goal

- With `cloudflared.tunnel_token` configured, a developer can start OpenRails and get a stable public hostname that routes to the local billing instance, usable from a phone browser/app.

## Non-goals

- Do not require Cloudflared in production.
- Do not make Cloudflared a hard dependency for boot.

## Notes

- This is for local/dev convenience (public ingress), not webhook signature bypass.
- Prefer explicit opt-in (config flag or separate command) so production never spawns subprocesses unexpectedly.

**Tasks:**
- DESIGN:
- [ ] Decide opt-in mechanism: (A) standalone CLI subcommand `openrails cloudflared up` that supervises both OpenRails + cloudflared, or (B) OpenRails spawns cloudflared when `cloudflared.tunnel_token` is set and `env=dev`
- [ ] Decide what constitutes “success”: tunnel established + /health/ready reachable via public hostname
-
- IMPLEMENTATION (if spawning subprocess):
- [ ] Add a small `pkg/cloudflared` supervisor that starts `cloudflared tunnel run` with the configured token and captures stdout/stderr into structured logs
- [ ] Ensure clean shutdown: propagate SIGINT/SIGTERM, kill child process group, avoid zombies
- [ ] Add a readiness check that probes the configured `cloudflared.public_hostname` (or local route) and reports status
-
- CONFIG / DOCS:
- [ ] Clarify config semantics in config.example.yaml (token is secret; hostname is non-secret; tunnel name optional)
- [ ] Document a phone-access workflow in docs (prereqs: cloudflared installed or image available; set APIURL/CORS origins appropriately)
-
- SECURITY / SAFETY:
- [ ] Ensure the service API (port 8060) is not accidentally exposed publicly by default; only expose the public API unless explicitly configured
- [ ] Ensure `api_key` and auth verification are still required for protected routes
-
- VERIFY:
- [ ] Add a lightweight unit test for supervisor command construction (no subprocess exec)
- [ ] Manual dev verification: start stack + tunnel and hit /health/ready from an external device

---

# #288: processor-routing-and-fallback-policy

**Completed:** no

Add deterministic processor selection and fallback policy so checkout can choose the best available rail for a merchant/product/tier/user context before creating a one-processor checkout session.

## Metadata

- Category: feature
- Status: planned
- Passes: false

## Motivation

- Hyperswitch wins on payment orchestration. OpenRails should not chase broad smart-routing, but customers do need basic processor redundancy and product/merchant-aware routing, especially high-risk merchants with Stripe plus NMI/CCBill/Solana.
- Keep checkout sessions one-processor-at-a-time; route before session creation.

## Non-goals

- No ML routing.
- No 200-connector orchestration platform.
- No automatic retry to a second processor after a successful authorization attempt unless the processor contract makes that safe.

**Tasks:**
- DESIGN:
- [ ] Define routing inputs: tenant_id, product_id, price_id, tier_group, amount, currency, billing cycle, user country/state when known, processor availability, processor capability metadata (#291), and explicit client preference.
- [ ] Define routing outputs: selected processor, fallback candidates, reason, and policy version.
- [ ] Decide precedence: explicit price/provider config > merchant policy > product/tier_group policy > global default.
- [ ] Decide failure classes that can trigger fallback before checkout finalization: processor unavailable, unsupported capability, credential missing, sandbox/live mismatch, hard validation failure. Do not fallback after a successful charge.
-
- DATA MODEL / CONFIG:
- [ ] Add routing policy representation in DB or catalog manifest: allowed processors, preferred order, disabled processors, and optional per-tier overrides.
- [ ] Extend catalog-as-code manifest only if needed; prefer using existing provider lists as the first version.
- [ ] Record selected processor and routing reason on checkout_sessions for auditability.
-
- IMPLEMENTATION:
- [ ] Add a ProcessorRouter service used before checkout session creation and legacy checkout flows.
- [ ] Integrate with processor health/capability metadata so unsupported processors are filtered with explicit errors.
- [ ] Add dry-run endpoint/CLI to explain routing for a price/user/merchant without creating a checkout.
- [ ] Ensure idempotency keys bind to the resolved processor/policy so retries do not unexpectedly switch rails.
-
- VERIFY:
- [ ] Unit-test policy precedence and fallback filtering.
- [ ] Integration-test Stripe primary -> NMI fallback when Stripe is disabled/unavailable before charge.
- [ ] Integration-test product constrained to NMI does not route to Stripe even if NMI is configured.

---

# #290: provider-certification-matrix

**Completed:** no

Publish and maintain a provider certification matrix for Stripe, NMI, CCBill, and Solana that records exactly which customer-visible flows are supported, sandbox/devnet tested, live/test-mode tested, and known-limited.

## Metadata

- Category: product
- Status: planned
- Passes: false

## Motivation

- OpenRails' strongest differentiator is deep support for real non-Stripe rails, especially NMI-compatible high-risk gateways. Customers need confidence that the specific flows they care about actually work.
- This should become both documentation and an executable certification harness where practical.

**Tasks:**
- MATRIX DESIGN:
- [ ] Define provider capabilities and certification statuses: not_supported, manual_only, unit_tested, integration_tested, sandbox_certified, live_test_mode_certified, devnet_certified, production_certified.
- [ ] Define flows to track: catalog/product push, price/recurring-plan push, one-time checkout, recurring checkout, vault/tokenization, rebill, cancellation, deferred cancellation, refund, dispute/chargeback, webhook handling, subscription sync/backfill, catalog drift detection.
- [ ] Include processor-specific notes: NMI product/prices are local while recurring prices push as NMI recurring plans; CCBill catalog actions may be manual; Solana recurring requires on-chain readback/devnet certification.
-
- DOCS:
- [ ] Add docs/providers.md or equivalent with the current matrix and exact tested commands.
- [ ] Add NMI-compatible gateway guidance: required security_key, Collect.js/tokenization key if needed, direct/query endpoints, test_mode behavior, and how white-label NMI accounts map to the same interface.
- [ ] Add troubleshooting for common provider failures: bad endpoint, key belongs to different gateway user, sandbox URL not supported, recurring plan query mismatch, webhook signature failure.
-
- EXECUTABLE CERTIFICATION:
- [ ] Add or formalize focused integration tests for NMI sale, vault, recurring plan create/readback, and query API.
- [ ] Add Stripe test-mode catalog command + subscription sync certification steps.
- [ ] Add Solana devnet read-back certification for plan accounts and recurring lifecycle.
- [ ] Add CCBill manual-action verification path so unsupported remote catalog operations surface as pending_manual_actions rather than errors.
-
- PROCESS:
- [ ] Make certification matrix updates part of provider-related PRs.
- [ ] Record last verified date, environment, and command for each provider flow without exposing secrets.

---

# #291: processor-capability-metadata

**Completed:** no

Expose processor capability metadata in code, APIs, catalog planning, checkout validation, and admin/provider status so OpenRails can explain what each rail supports instead of relying on scattered processor-specific conditionals.

## Metadata

- Category: architecture
- Status: planned
- Passes: false

## Motivation

- Stripe, NMI, CCBill, and Solana have different lifecycle semantics. Customers need predictable errors and routing decisions. OpenRails also needs a shared capability source for routing/fallback, catalog-as-code, checkout validation, and the provider certification matrix.

**Tasks:**
- CAPABILITY MODEL:
- [ ] Define ProcessorCapabilities with booleans/enums for recurring, one_time, vault/tokenization, hosted checkout, redirect checkout, direct sale, catalog push, recurring plan push, refund, dispute, cancel immediate, cancel deferred, remote subscription listing, remote dedup check, webhooks, drift enumeration, and manual actions.
- [ ] Add capability details for current processors: stripe, NMI-backed (`mobius`), ccbill, solana.
- [ ] Distinguish processor class capabilities (NMI-backed) from named provider overrides (Mobius).
-
- INTEGRATION POINTS:
- [ ] Use capabilities in checkout validation instead of hard-coded processor switches where practical.
- [ ] Use capabilities in catalog planning/reconciliation to decide provider actions and pending_manual_actions.
- [ ] Use capabilities in routing/fallback policy (#288) so unsupported rails are filtered before checkout.
- [ ] Surface capabilities through admin/provider status endpoints and docs.
-
- ERRORS / UX:
- [ ] Return structured unsupported-capability errors with processor, capability, and suggested alternative when possible.
- [ ] Ensure user-facing errors are clean while admin/debug surfaces retain processor detail.
-
- VERIFY:
- [ ] Unit-test capability metadata for each supported processor.
- [ ] Regression-test known special cases: CCBill manual catalog actions, NMI recurring plan push, Stripe remote dedup check, Solana one-off/recurring distinction.

---

# #320: Add Hyperswitch payment vault support for cloud and self-hosted deployments

**Completed:** no

Add Hyperswitch as an optional OpenRails payment provider and payment-method vault integration, covering both Hyperswitch Cloud and self-hosted Hyperswitch. This is payment vaulting/tokenization, not HashiCorp Vault merchant-secret storage. OpenRails should store only opaque Hyperswitch customer/payment-method identifiers plus non-sensitive metadata, while PAN/card collection stays in Hyperswitch-hosted/client-side tokenization flows or equivalent PCI-scoped Hyperswitch surfaces.

This issue must reconcile with future issue #297 (`deplatforming-resilient-card-vault`): Hyperswitch should not be positioned as the default adult/high-risk deplatforming-resilient vault until its contractual/export/compliance posture is explicitly certified. Hyperswitch Cloud may still be useful for lower-risk deployments or as an optional provider, and self-hosted Hyperswitch may be a break-glass/advanced deployment path if the operator accepts and satisfies the PCI requirements.

This should integrate with the provider capability and routing work in #288, #290, and #291: Hyperswitch can be selected as a processor/vault provider when the merchant/provider config says it is available, and OpenRails can route checkout/setup flows through it without treating it as a generic secret vault.

**Tasks:**
- [ ] Research the current Hyperswitch Cloud and self-hosted API surfaces needed for customers, payment methods, payment/setup intents, saved payment methods, refunds/voids, webhooks, connector routing, and vault/tokenization modes; record any version assumptions in docs.
- [ ] Reconcile the implementation plan with future issue #297: document whether Hyperswitch is an optional provider, a lower-risk deployment choice, or a PCI-heavy break-glass/self-hosted vault path; do not make it the default portable adult/high-risk vault without explicit certification.
- [ ] Define the OpenRails provider config shape for `hyperswitch`: cloud vs self-hosted mode, API base URL, optional vault/base URL split if Hyperswitch requires it, merchant/profile/account identifiers, API key secret reference, webhook secret reference, return/callback URLs, and test/live mode.
- [ ] Store Hyperswitch credentials in the existing merchant secret store path; do not store processor API keys in bootstrap YAML, database rows, logs, or generated frontend config.
- [ ] Extend provider capability metadata (#291) so Hyperswitch can advertise supported flows: payment-method vault/tokenization, one-time checkout, recurring/setup/mandate behavior if supported, refunds, webhooks, processor-side routing, remote payment-method listing, and manual certification status.
- [ ] Add a provider adapter/client abstraction that lets NMI customer-vault IDs and Hyperswitch payment-method IDs fit the same OpenRails payment-method model without hard-coding NMI semantics into checkout/subscription flows.
- [ ] Implement Hyperswitch customer/payment-method setup flow using an opaque token or hosted/client-side Hyperswitch collection result; persist only customer, provider, external customer ID, external payment method ID, brand/last4/expiry metadata, and status.
- [ ] Implement checkout/subscription charge paths that can use a saved Hyperswitch payment method and reconcile the resulting payment/subscription state back into OpenRails records.
- [ ] Implement webhook verification and event handling for successful payment, failed payment, refund/void, payment-method updates/deletes, and any recurring/mandate lifecycle events OpenRails relies on.
- [ ] Add self-hosted operational docs: required Hyperswitch services, base URL/TLS requirements, webhook reachability, secret injection, health checks, and local compose/dev smoke path if practical.
- [ ] Add cloud operational docs: required Hyperswitch Cloud credentials, webhook setup, provider certification checklist entry (#290), and merchant bootstrap/provider-link examples.
- [ ] Add tests with a mocked Hyperswitch server/client for tokenization/setup, saved-method charge, failure mapping, webhook signature verification, idempotency, merchant isolation, and no-sensitive-card-data persistence/logging.
- [ ] Validate with focused Go tests for provider/checkout/vault/webhook code, compile-only full package coverage, `task build`, and an optional live sandbox/self-hosted smoke test when credentials or local Hyperswitch are available.

# #333: admin-e2e-suite-needs-control-plane-wiring

**Completed:** no
**Status:** OPEN 2026-06-10 (Claude): diagnosed during the #332 failure review; not started. All other e2e failures from that review are fixed (see completed #332 + commits 9fe72d4 openrails, 68cb6a1 tensorhub, b72f61e8 cozy-art).

The admin e2e tests (tests/admin_payments_test.go, admin_metrics_test.go, admin_entitlements_source_test.go, admin_offchannel_payments_test.go) fail with 500 {'message':'authorization unavailable'} — the nil-admin-checker fail-closed path. DIAGNOSIS (2026-06-10): pre-existing #312 bit-rot, documented in the suite itself (testcontainer_suite.go Auth config comment: 'Admin-route integration tests therefore require the embedded control plane wired with the test admin granted openrails:admin'). The #312 hard-cut moved admin authority from JWT claims (tests still mint operatorAdminClaims() tokens via test_helpers.go) to the LIVE control-plane permission check (routes_admin.go only sets AdminPermissionChecker when s.controlPlane != nil; the suite never enables Auth.ControlPlane -> checker nil -> admin_neutral.go/ginmw admin gates 500 fail-closed).

WHAT THE FIX NEEDS:
1. suite.Config.Auth.ControlPlane = &config.ControlPlaneConfig{Enabled: true} (ginboot embcp.Attach then builds it; authkit profiles schema is already applied by migrate.RunPostgres).
2. The admin test users must EXIST in authkit profiles.users with ids equal to the JWT subs the suite mints, then be made members + granted the admin role (controlplane Bootstrap/AddMember/AssignRole or authkit core APIs — NOT raw SQL into roles). authkit core.CreateUser(email, username) generates its own id, so either (a) resolve the verifier's user mapping (issuer+sub -> user) and mint tokens for created users' real ids, or (b) add a test-only authkit user-provisioning hook.
3. operatorAdminClaims()/CreateAdminToken in tests/test_helpers.go are then vestigial — admin authority comes from the role grant, not claims.

NOTE: the admin-METRICS tests' 'Table test_analytics.daily_metrics does not exist' failures were a STALE TEST ENV artifact (migratekit records the ClickHouse ledger in postgres; reusing a postgres DB across runs against fresh CH containers skips re-apply). On a fresh env CH applies fine and those tests fail at the same admin-auth gate instead. Squashed CH baseline (cb9200b) is NOT at fault (daily_metrics present; migrations/clickhouse schema_test passes).

**Tasks:**
- [ ] Enable Auth.ControlPlane in testcontainer_suite config and verify embcp.Attach builds it in ginboot.
- [ ] Provision admin test users in authkit profiles.users matching the JWT subs (or mint tokens from created users' ids); AddMember + AssignRole openrails:admin via control plane / authkit core APIs.
- [ ] Re-run tests/admin_*.go on a fresh env; remove vestigial operatorAdminClaims if green.
- [ ] (env hygiene) consider failing loudly or re-validating when the migratekit CH ledger says applied but the CH database lacks the tables (stale-ledger detection).

# #569: API-key / remote-application minting authz — identity is the permission group; drop resource-scope hook; rename authkit APIKeyResource.Kind → Persona

**Completed:** yes

## Trigger

openrails was bumped to **authkit v0.57.0**. It compiles and unit tests pass, but ~30
integration tests fail at harness setup with:

```
controlplane: mint initial admin API key: invalid_resource
```

Root cause: v0.57.0 shipped authkit's #121 hardening. `MintAPIKeyWithOptions` now calls
`AuthorizeAPIKeyResources`, which **returns `invalid_resource` when a mint requests resource
scopes but no `ResourceScopeAuthorizer` is registered** (fail-closed). openrails' control-plane
bootstrap (and the `mint-merchant-api-key` CLI, and the test harness) mint **merchant-scoped**
keys (`Resources: [MerchantResource(id)]`) without registering an authorizer → every mint is
rejected → bootstrap fails → the standalone-surface tests cascade-fail.

This issue records the model we want and the plan to get there. It spans **both** repos.

## The model (the part that was muddied — stating it cleanly)

There are TWO independent authorization dimensions when minting an API key. They were
conflated; they are not the same thing.

### 1. Actor authorization (role + permission) — authkit already does this NATIVELY. No hook.

The HTTP mint route (`POST /<persona>/<slug>/api-keys`) runs, before minting
(`authkit http/permission_group_routes.go`):

```
svc.Can(caller, "user", persona, resource_slug, route.Perm)
```

That answers "is the caller a member of this group with a role that holds the required
permission?" — exactly the check we want, with no openrails logic and no hook. The permissions
(authkit built-ins, auto-generated because the persona's `ManagementProfile` enables the family):

- **Create / revoke a merchant API key:** `merchant:api-keys:manage`
- **Register / delete a merchant remote application:** `merchant:remote-applications:manage`
- (List-only: `merchant:api-keys:read` / `merchant:remote-applications:read`.)
- Generic form: `<persona>:<family>:manage`. Held only by the merchant `owner` role
  (`merchant:*`, auto-seeded); `support` / `viewer` do NOT include `:api-keys:manage`.

Role no-escalation is also native: a minted key must reference a **catalog role** of the group,
and its permissions are resolved FROM that role at verify time — the key can't exceed the role.

**Conclusion: nothing about the actor/role/permission decision needs a hook.**

### 2. Resource scope `APIKeyResource{Kind, ID}` — the ONLY thing the hook was ever for.

authkit treats a resource scope's `{Kind, ID}` as **opaque strings** ("AuthKit treats resource
kinds and IDs as opaque and never interprets their semantics itself"). Because it won't interpret
them, it can't authorize them — hence the optional `ResourceScopeAuthorizer` hook.

## The OpenRails reality (what the scopes actually are)

- An API key is a way to **verify yourself when performing an action**.
- A **merchant** key → performs merchant actions (cancel a customer's subscription, push a
  catalog update). Its identity is the **merchant**, which under #567 **IS a permission group**
  (`persona=merchant`, slug=merchant-slug).
- A **customer** group does exactly ONE thing: let other principals **spend the customer's API
  balance, bounded by budget windows**. The delegates are whatever the customer adds to its group
  — **members (users), remote applications, and API keys**. Every delegate's identity is the
  **customer** (`persona=customer`, id=customer-uuid), derived from the group; the **budget
  window** is an OpenRails concept attached to the delegation and enforced OpenRails-side — it is
  NOT an authkit resource scope.
- **"A merchant API key scoped to a customer" does not exist and has no meaning.**

So openrails' `{openrails.customer, <uuid>}` scope carried on a merchant credential
(`AllowsCustomer`, `CustomerResource`, the customer branch of `validateAPIKeyResources`) models a
concept that does not exist. It is exercised only by tests; **no production path ever mints a
customer-scoped key**, so `AllowsCustomer` already returns true in production today.

## The insight that de-muddies everything

A "resource scope" in openrails was always just a **permission-group instance**:
`{openrails.merchant, id}` = the merchant group; `{openrails.customer, id}` = the customer group.
But a key is **already minted under** a permission group — so the resource scope **duplicates the
`permission_group_id` the key already carries.** The principal's identity (merchant or customer)
can be **derived from the group**, never re-stated as an opaque scope.

This is also why authkit's `APIKeyResource.Kind` should be renamed **`Persona`**: a scope's
"kind" is really a **persona** (the same vocabulary authkit already uses for permission-group
types, e.g. `ResourceScopeAuthorizationRequest.Persona`). `{Persona, ID}` is a permission-group
reference. Naming it `Kind` hid that and made the redundancy invisible.

## Decision

**Guiding principle (the architecture this issue locks in): authkit authorization is
CONFIG-DRIVEN, never code-driven.** The host application (openrails, tensorhub, …) configures
authkit with personas, per-persona configuration, and permission definitions; authkit derives
every authorization decision from that static config at runtime. There are **NO host callbacks /
hooks**. If config ever cannot express a needed relationship we re-evaluate then — but the
default is that static per-persona config is sufficient. The `ResourceScopeAuthorizer` is a code
hook and violates this principle, so it is removed.

- **No hook.** openrails will not register a `ResourceScopeAuthorizer`.
- **Identity comes from the permission group**, not a resource scope.
- **Remove the meaningless merchant-key-scoped-to-customer machinery.**
- **Remove the resource-scope hook from authkit entirely** (`ResourceScopeAuthorizer`,
  `AuthorizeAPIKeyResources`, `WithResourceScopeAuthorizer`, the `MintAPIKeyWithOptions` call,
  `ResourceScopeAuthorizationRequest`). A principal's scope IS its permission group, so nothing
  opaque is left to authorize and the #121 concern becomes moot. The `APIKeyResource{Kind,ID}`
  concept is renamed to `{Persona,ID}` on its way out (a permission-group reference) or removed
  outright — either way **no host callback survives in either repo**.

## Required-to-unblock vs. model-cleanup (keep these separate)

**Required to make openrails green against v0.57.0 (openrails-only — authkit needs NO change for
this):** just stop passing resource scopes on mint and resolve identity from the group. A
zero-resource mint already passes authkit's check; the `invalid_resource` error only fires when
resources are present without an authorizer.

**Model cleanup (clarity; can be staged):** the `Kind → Persona` rename in authkit, removing the
dead customer-on-merchant machinery in openrails, and deciding the long-term fate of the
authkit resource-scope/hook concept.

## Plan

### authkit (decisive hook removal; NOT required to unblock openrails)
- REMOVE the resource-scope authorizer hook: `ResourceScopeAuthorizer`,
  `AuthorizeAPIKeyResources`, `WithResourceScopeAuthorizer`, the call in `MintAPIKeyWithOptions`,
  and `ResourceScopeAuthorizationRequest`. No host callback remains.
- The opaque `APIKeyResource` scope concept goes with it (a scope was always a permission-group
  reference): rename `{Kind,ID}` → `{Persona,ID}` and authorize natively, or drop the resource
  field outright. Removing it makes #121 moot (no scope to escalate).
- CAUTION: this reworks a DB-backed subsystem (`service_token_resources`) in a SHARED library and
  reverts the shipped v0.57.0 security mechanism — verify no other consumer relies on resource
  scopes before deleting, cut a new authkit version, and bump openrails' pin. Because of that
  blast radius it is sequenced AFTER the openrails-only unblock (which already removes every
  hook/scope from openrails and gets the suite green on v0.57.0 as-is).

### openrails (required to unblock + cleanup)
- Stop passing `Resources` on every mint: `internal/controlplane/bootstrap.go`,
  `cmd/openrails/mint_merchant_api_key.go`, `internal/integrationharness/harness.go` (`MintAPIKey`).
- `internal/controlplane/api_key.go` `ResolveAPIKey`: derive the merchant from
  `resolved.PermissionGroupID` via the existing `merchantForGroupID(...)`, not from
  `merchantFromResources(...)`.
- Remove the merchant-scoped-to-customer machinery: `CustomerResource`, the customer branch of
  `AllowsCustomer`, customer kinds in `validateAPIKeyResources`. Simplify `AllowsCustomer` to
  "a merchant credential may act for any subject within its merchant" (behavior-preserving — prod
  never minted a customer scope). Call sites: `internal/http/handlers/service_credits.go`,
  `internal/http/middleware/ginmw/service_credential.go`.
- Update/remove the tests that mint resource-scoped keys or assert customer-scope denial via a
  minted key — at least: `internal/controlplane/api_key_scope_test.go`,
  `internal/http/middleware/ginmw/service_credential_test.go`,
  `tests/service_admit_http_integration_test.go`, `tests/service_facade_parity_test.go`,
  `internal/integrationharness/cross_merchant_isolation_test.go`,
  `internal/integrationharness/merchant_payment_providers_http_test.go`. (Several `MintAPIKey`
  callers already pass `nil` resources — those are unaffected.)
- Confirm the legitimate **customer** API-key path end-to-end: a customer key is minted under the
  customer permission group (`customer:api-keys:manage`), resolves its customer identity from the
  group, and its spend is bounded by the budget-window/spend-delegation concept (separate from
  authkit resource scopes). Add coverage if missing.

## Open questions to resolve while implementing
- Confirm WHERE OpenRails enforces the spend "budget window" on a customer delegation (member /
  remote-app / API key). It is OpenRails-side and orthogonal to authkit identity — wire
  identity-from-group and leave budget enforcement where it already lives.
- Cross-merchant isolation tests currently lean on the merchant resource scope to prove isolation
  — confirm that resolving the merchant from the permission group preserves the same isolation
  guarantees (it should: the group IS the merchant) and rewrite those assertions accordingly.
- Final authkit decision: rename-only, or also remove the opaque-resource/hook concept?

## Status 2026-06-23 (Claude) — COMPLETE
End state: **NO host hook in either repo**, config-driven authorization throughout, full
integration suite GREEN.
- **authkit v0.58.0**: `ResourceScopeAuthorizer` hook removed entirely (no host callbacks).
  build/vet/full-suite green; committed `133a78e`, tagged `v0.58.0`, pushed.
- **openrails**: resource-scope concept hard-removed (identity = permission group); full
  integration suite GREEN (7/7 incl. cross-merchant isolation). Landed master + tag `v0.58.0`,
  then bumped to authkit **v0.59.0** + added customer-delegation coverage → tag `v0.59.0`.
- **authkit v0.59.0**: `APIKeyResource.Kind` → `Persona` ({Persona,ID} = a permission-group
  reference); no consumer used the field. build/vet/full-suite green; tag `v0.59.0`, pushed.

The 6 formerly-failing integration tests were PRE-EXISTING (failed identically on clean v0.56.2)
and are now fixed: (1) river job-queue helpers queried a stale `openrails.river_job` — River
moved to the `public` schema (#545), helpers now use `config.RiverSchema`; (2) self-invoices test
read the wrong list-envelope key (`data` → `invoices`); (3) embed boundary exposed a REAL product
bug — service-JWT `intersectPermissions` used exact-match, so an owner's `merchant:*` grant didn't
cover a concrete claimed perm → now uses `authbase.PermMatches` glob coverage (strict
down-scoping preserved).

## Tasks
- [x] openrails: drop Resources from mints; resolve merchant from the permission group (API key / service-JWT / remote-app); remove merchant-scoped-to-customer machinery; `AllowsCustomer` merchant-wide; tests updated (cross-merchant isolation preserved + green).
- [x] authkit: REMOVE the `ResourceScopeAuthorizer` hook (config-driven authz, no host callbacks); build/vet/full-suite green; tag v0.58.0.
- [x] openrails: bump to authkit v0.58.0; full integration suite green; land on master + tag v0.58.0.
- [x] fix the 6 pre-existing integration failures (river schema ref; self-invoices envelope key; service-JWT glob coverage).
- [x] customer delegation path: confirmed implemented end-to-end (catalog persona → delegated.go/customer.go identity → customer_spend_delegations.go budget windows → admission/spendgate enforcement). Added `tests/customer_delegation_spend_http_integration_test.go` (`TestCustomerDelegationSpend_HTTP_EndToEnd`): customer sets a $1000/day window via treasury HTTP, delegate's spend within-window allowed, over-window denied (403 `budget_exceeded`), ungranted delegate denied (`delegated_spend_not_allowed`). PASS.
- [x] authkit: renamed `APIKeyResource.Kind` → `Persona` (a `{Persona,ID}` permission-group reference); confirmed no consumer (doujins/hentai0/openrails) uses the field; tag **v0.59.0**. openrails bumped to authkit v0.59.0.

---

# #582: rename `processor` → `rail` across notes + codebase (terminology consolidation; breaking DB + API allowed)

**Completed:** no

Decided 2026-06-24 (Paul + Claude). On-brand consolidation: **"rail" becomes THE term for a payment channel/lane.** `processor` and `rail` are synonyms — the `models.Processor` enum values (`mobius`/`ccbill`/`stripe`/`solana`/…) literally ARE the rails. `provider` is a **different** concept (a credentialed *account on* a rail) and **stays**. Breaking changes to DB + API are authorized now (pre-production; **no migrations** — edit the schema in place and `task sqlc`). Behavior does not change; this is cosmetic/brand only.

## Decision / scope boundary (read first)
- **RENAME:** everything meaning the payment lane/type currently called `processor` → `rail`.
- **KEEP `provider`** (account-level concept): `provider_accounts`, `provider_account_id` (71-use FK), `provider_intents`, `provider_refresh_watermarks`, `external_provider_mutation_logs`. A rail is the lane; a provider account is an account on the lane (the schema already says "multiple provider accounts per provider rail").
- **KEEP wire/string VALUES:** `"mobius"`, `"ccbill"`, `"stripe"`, `"solana"`, `"paypal"`, `"admin"`, `"manual"` are rail *names*, not the word "processor" — they don't change.
- **No blanket-sed:** rename ONLY the payment-rail sense of "processor". Audit for any unrelated "processor" (generic data/event/webhook processors) and leave those.

## Target naming

**Go**
- `internal/db/models/processor.go` → `rail.go`; `type Processor string` → `type Rail string`; `ProcessorMobius/CCBill/Solana/Stripe/Paypal/Admin/Manual` → `RailMobius/...` (values unchanged).
- `internal/db/models/processor_customer.go` → `rail_customer.go`; `ProcessorCustomer` → `RailCustomer`.
- `internal/modules/payments/processors/` package → `internal/modules/payments/rails/`; `IsNMIBackedProcessor`→`IsNMIBackedRail`, `NMIBackedProcessors`→`NMIBackedRails`, `SameProcessor`→`SameRail`.
- reconcile `ProcessorFetcher` → `RailFetcher`; webhooks `WebhookHandler.Processor()` → `.Rail()`; intents handler `processor` refs; all fields/params/locals named `processor` → `rail`.
- `pkg/` exported API (`pkg/service`, `pkg/embedded`, `pkg/api`): exported `Processor` → `Rail` (**BREAKING** for embedded consumers).

**DB (breaking, no migration — edit `migrations/postgres/*.sql` in place)**
- column `processor` → `rail` everywhere (payments, subscriptions, etc.); the named enum type (if a `processor` enum type exists) → `rail`; table `processor_customers` → `rail_customers`.
- `internal/db/queries/*.sql`: update column refs; `task sqlc` to regenerate `internal/db/gen` + vet against the real schema.

**JSON / HTTP / CLI**
- request/response fields named `processor` → `rail` (**BREAKING** API change — audit `pkg/api`, handlers, `docs/api/endpoints.md`).
- CLI: `pull-provider --provider=nmi,ccbill,stripe,solana` currently takes **rail** names → this is the provider/rail overload below; resolve there.

**Docs/notes**
- README.md, docs/*.md, agents/*.md: "processor" (payment sense) → "rail".
- Add a billing glossary: **rail** = the lane/type (former `Processor`); **provider account** = a credentialed account on a rail; **integration** = the client code under `internal/integrations/`.

## Provider-overload cleanup (decide as part of this)
Today "provider" is overloaded: it means the ACCOUNT (`provider_accounts`) AND sometimes the TYPE (the `provider` column in `provider_intents`/`provider_refresh_watermarks` holds `"stripe"`/`"nmi"`; CLI `--provider=nmi,…`). Renaming processor→rail makes this glaring. Options:
- **(A) Minimal:** rename only `processor`→`rail`; leave every `provider` as-is (accept that some `provider` columns/flags hold a rail value). Lowest churn.
- **(B) Disambiguate (recommended):** also rename the *type-holding* `provider` columns/flags → `rail` (e.g. `provider_intents.provider`→`.rail`, `--provider`→`--rail`), while KEEPING `provider_accounts`/`provider_account_id` as the true account concept. Cleanest end state.
- Either way **KEEP the convergence vocabulary "provider-observed truth"** — the `pull.*` plane is about a provider *account's* observed facts, so "provider" is correct there. Pick A or B before starting.

## Findings (audit DONE 2026-06-24)
**"processor" is overloaded — three senses:**
1. **OUR rail** (`models.Processor`, columns `processor`/`processor_subscription_id`/`processor_customer_id`/`processor_transaction_id`/`processor_fields`/`processor_state`/`processors`, enum `processor_type`, table `processor_customers`, ~283 Go files) → **rename**.
2. **NMI's *acquirer* "processor"** in `internal/integrations/nmi/` + `internal/modules/webhooks/` — `json:"processor_id"`, `json:"processor_response_text"`, the `Processor` acquirer object (`body.Processor.ID`), and decline strings `transaction_was_declined_by_processor` / `transaction_error_returned_by_processor`. These mirror **NMI's actual webhook payload** — renaming breaks parsing. **PRESERVE** (NMI's external wire format; we don't own it, so pre-launch status is irrelevant here).
3. **Value strings** in CHECK constraints: ledger `processor_clearing`, blocklist `processor_customer` (singular, quoted). **PRESERVE**.

**Coupling:** DB column `processor` → sqlc-gen field `.Processor` → referenced everywhere ⇒ this is ONE coordinated transform (Go + SQL schema + queries + `task sqlc` regen), not a Go-only first pass.

**Method:** sentinel-protect the value strings (#3); EXCLUDE the NMI/webhook wire dirs from the *lowercase* sweep (preserves their acquirer wire tags #2 — their CamelCase fields still rename to `Rail*` but the lowercase `json:` tags survive, so wire stays correct); CamelCase `Processor`→`Rail` + lowercase `processor`→`rail` everywhere else; `git mv` package `processors`→`rails`; `task sqlc` regen; `go build`/`vet`/test (compose stack is up: openrails-postgres `:5434`, garnet, clickhouse).

**Pre-launch:** breaking DB/JSON/API changes are fine (Paul, 2026-06-24) — except NMI's external wire (#2). **LOC:** take trivial simplifications opportunistically, but keep the diff reviewable as a rename.

## Tasks
- [x] Audit: enumerate every payment-sense "processor" (Go idents, DB schema, `*.sql`, JSON, CLI, docs); separate from unrelated "processor" uses. **DONE 2026-06-24** — see Findings above.
- [x] Go rename: types/consts/files (`Processor*`→`Rail*`), `processors` pkg→`rails`, fetcher/handler/field renames. **DONE** — 811+56 `*.go` swept; `git mv` package + files; `go build`/`go vet` green; gofmt clean.
- [x] DB rename (breaking, no migration): `processor` col→`rail`, enum `processor_type`→`rail_type`, `processor_customers`→`rail_customers`; queries + `processor_customers.sql`→`rail_customers.sql`; `task sqlc` regen + vet. **DONE** — gen flipped (0 residual `Processor`).
- [x] JSON/HTTP/CLI + **ClickHouse** field rename (breaking). **DONE** — `json:"processor"`→`json:"rail"` (21 of-ours fields), env `PROCESSORS_*`→`RAILS_*`, example config/env, `migrations/clickhouse/*.sql` (caught by `TestAdminMetricsFolded`), `scripts/e2e_dump_local.sh`, `pkg/embedded/README.md`. No `--processor` CLI flag (only `--provider`, kept).
- [x] Provider-overload decision. **DECIDED A** — renamed only `processor`→`rail`; kept ALL `provider` (incl. type-holding `provider_intents.provider` col + `--provider` CLI, which still hold a rail value) + `provider_accounts`/`provider_account_id` + "provider-observed truth". B (rename those too) is an optional follow-up.
- [x] Docs/notes sweep + glossary. **DONE** — README + `docs/*.md` + `docs/api/*.md`; new `docs/glossary-rails.md`. Historical `agents/*.md` left as point-in-time records.
- [x] Grep gate. **DONE** — every residual is an intentional preservation (NMI acquirer wire / `processor_clearing` / `processor_customer`) or out-of-scope (historical notes, other-repo paths, skill-doc examples).

## Validation
- [x] `go build ./...` + `go vet ./...` clean. **PASS** (+ `gofmt -l` clean).
- [x] `task sqlc` generate + vet (PREPAREs every query vs schema). **PASS**. (`task sqlc-check` fails ONLY because gen differs from git HEAD — i.e. the whole uncommitted rename; passes once committed.)
- [~] DB-backed tests. Unit: **71/72 packages green** (the 1 fail, `internal/migrate TestRewriteMigrationsSchema`, is PRE-EXISTING — missing `025/026` migration files, untracked in git). Integration: `./tests` (incl. analytics/CH after the fix), `./pkg/service`, `./embed` **green**; targeted NMI-webhook (wire preserved) + checkout (API/DB) **green**. `./internal/river` 13 liveness/dunning fails are **PROVEN PRE-EXISTING** — identical `products_merchant_fk` on stashed HEAD (a test-harness merchant-seeding/ordering gap; my diff to those files is a pure rename, `internal/dbtest` untouched).
- [x] `rg -i '\bprocessor'` over go/sql/ch returns only intentional/unrelated hits. **PASS**.

## STATUS 2026-06-24 (Claude)
Rename **complete** across Go (`models.Rail`, `payments/rails` pkg, all idents), Postgres schema + queries + sqlc gen, ClickHouse schema, JSON/HTTP fields, env contract (`RAILS_*`), example config, scripts, and docs (+ new `docs/glossary-rails.md`). Three concepts kept distinct: **rail** (renamed from processor), **provider account** (`provider_*`, unchanged), **NMI acquirer "processor"** (wire fields preserved). Behavior unchanged — pure rename. **Uncommitted** in the working tree; not yet committed/tagged, and consumer bump (Cross-repo) not yet done. The one regression introduced (ClickHouse schema lag) was caught + fixed; all other test failures predate this change.

## Cross-repo
- The breaking embedded-API + JSON `processor` rename affects any consumer of openrails' Go API or HTTP that references `processor` (openrails-saas; doujins/hentai0 only if they read a `processor` field). Enumerate consumers, coordinate a version bump, adopt after this lands.

## Notes
- Pure rename, no behavior change — land as a reviewable rename (a Go-commit + a DB/sqlc-commit pair reads cleanest).
- Out of scope (separate, already-flagged decisions): `mobius` as the NMI rail id (rename to `nmi`?); keeping "provider-observed truth" in the convergence taxonomy (yes, keep).

---
