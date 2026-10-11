# OpenRails route design audit — Final

**Status: Final.** Incorporates our previous review and agreed refinements.

Reviewed against fetched `master` **`440cda618c231a16cbacda210fd2b85b4988762e`** on 11 October 2026. Code references link to the inspected commit snapshots. The 181-route inventory and recommendations apply to that reviewed commit. Later changes on master, including the payment-operation removal and purchase-flow updates, are outside this review.

We’d keep the main audience split and the unified order model. The biggest improvement would be making the contracts more explicit: what a request authorizes, what has actually finished, what is still pending, and what happens when the response is lost. Those details matter more than making every URL look alike.

This document brings together our review of **all 181 routes**, including [duplication and overlap across routes, contracts and callers](#duplication-and-overlap). We’ve worked through the route organization, request and response structures, permissions and recovery behavior. Each entry explains what the code does today, what we’d keep or change, and why, with links to the evidence. These are our recommendations for discussion; nothing has been implemented.

**Verdicts:** 86 keep, 89 refine, 6 redesign. “Refine” often means an additive field, clearer SDK behavior or better documentation; it does not mean the route is broken. “High/medium/low” below means our suggested priority, not a formal security severity. Any confirmed defects are pre-existing at the reviewed commit and remain unmodified.

Optional design ideas are separate from confirmed problems. The findings below are grounded in the code; we haven’t established whether they have occurred in production.

## What we’d change first

### 1. Make retry identity match the actual operation

Keep stable idempotency keys, but make their meaning consistent. The app replay fingerprint currently includes the route template, query and body, **not the concrete path IDs**. Reusing a key/body on two different provider operations, captures or order cancellations can return the first resource’s response. Bind the fingerprint to the resolved target. Authentication and merchant permission checks already run before replay; this finding is not, by itself, evidence of an authorization bypass. [App replay implementation](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/app_requests.go#L55) [Middleware ordering](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/mount.go#L215)

There is also a two-level batch contract to explain: an HTTP 200 can contain failed items, and the whole response is then cached. Retrying that same envelope key does not retry an item-level 500. Keep successful item identities stable and define an explicit failed-item retry flow; adding correlation IDs to error results would help. Product-access grant batches need a complete request fingerprint, and the console should retain a refund key across transport retries instead of minting a fresh one each submit. [Batch admission](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admissions.go#L118) [Grant batch identity](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/product_access.go#L206) [Refund caller](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/web/admin/src/lib/api/endpoints.ts#L272)

### 2. Give pending work a clear, readable outcome

Use the existing durable operations and resource reads to distinguish accepted, pending, completed, refused and uncertain results. A card deletion returns 202 but its shipped hook removes the row immediately; a provider card swap can return generic conflict while work continues. Findings already record a queued refund and its ID, and product archives already expose refund_pending outcomes separately from processing completion. Surface those existing details clearly in callers before proposing new status machinery. [Card deletion hook](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/sdk/billing-ui/src/react/hooks.ts#L307) [Card swap handler](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/update_subscription_payment_method.go#L37) [Queued finding refund](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_findings_actions.go#L396)

Consider read-only operation status where a caller needs polling and currently uses POST replay. Existing resource reads and replay already cover many recovery paths; add a read only where it simplifies a concrete caller workflow. For card edits, preserve an unknown provider outcome instead of returning a message that the card is unchanged after any provider error. [Card edit error handling](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/payment_method_edit.go#L120)

### 3. Make large migrations resumable and export cutover explicit

Price-migration creation writes the migration before iterating its subscribers. A failure can leave a partly accepted cohort, and the existing re-driver does not visit subscribers never recorded. Persist the accepted cohort and progress, return a stable migration identity, and resume that work. Cancellation also needs durable outstanding provider-release instructions; currently a retry can lose the list returned by the first call. [Migration creation](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/subscriptions/price_migration.go#L390) [Migration cancellation](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/subscriptions/price_migration.go#L715)

Billing archive GET deliberately puts the source merchant into provider-readonly posture. This blocks provider mutations; local writes and webhook ingestion remain possible, and the documented procedure still requires stopping writers. We would prefer an explicit POST for cutover/export, preserving the fence; a separate download GET is useful only if artifacts will be retained. The restore receipt could report actual current posture: a new restore needs operator arming, while an exact replay does not re-fence a destination already armed. Current restore includes running charges and rebuilds their balance watches. These are method/receipt design preferences with SDK/CLI migration cost, not an undocumented shutdown or a missing safety mechanism. [Intentional export fence](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/merchantarchive/write_posture.go#L25) [Archive transport](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/merchant_archive.go#L19)

### 4. Make request intent expressible without guessing

Usage ingestion currently chooses catalog pricing for a successful zero amount and host pricing for a positive amount. We’d use an explicit pricing mode so “host-priced free” and “rate this event from the catalog during ingestion” are different requests. This is a proposal to support a more explicit input contract, not proof that the current deterministic rule mischarges events. For order payment, use a tagged union or generated oneOf constraints for the already-supported alternatives; clients should not have to discover valid combinations of optional fields through errors. [Usage pricing selection](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/money/usage.go#L136) [Order payment inputs](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/billing/orders.go#L77)

Merchant profile PATCH would benefit from an explicit clear operation. Currently empty/null strings preserve a field, whitespace can reach a different result, and the form omits cleared values. Use keep/set/clear semantics for nullable profile fields. Preserve the collections whose merge or replacement behavior is already deliberate; a blanket “all PATCH arrays replace” rule would be wrong here. [Configuration merge](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/merchant_configuration.go#L162) [Settings caller](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/web/admin/src/pages/settings/index.tsx#L235)

### 5. Preserve the boundary between purchases and existing debt

The unified order model is worth keeping, but its allowed actions must depend on origin. Customer cancellation currently excludes subscription-origin orders yet allows an owned open usage order into the generic canceled transition. That stops collection paths without going through the money module’s debt treatment. Restrict ordinary cancellation to purchase origins; keep void/write-off decisions on the existing merchant money actions. This is a confirmed source-level boundary problem, not merely a preferred route name. [Order close transition](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/orders/create.go#L328) [Close SQL](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/db/queries/orders.sql#L181) [Debt query](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/db/queries/usage_orders.sql#L291)

### 6. Specify credentials for consent and secret-bearing actions

Standalone card save/confirm and recurring-card changes do not consistently use the credential checks used by order payment and payment-operation authentication. Define which actions can run under automation and which need the existing interactive/payer check or a stronger recent-sign-in requirement. Keep status-only reads separate from issuing a provider client secret. Be precise: the current Interactive helper excludes API keys and signed tokens but **does allow device keys**; adding it is not proof of a fresh human interaction. [Credential classification](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/billingauth/contract.go#L64) [Recurring verification](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/app/recurring_verifier.go#L23) [Separate recent-sign-in check](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/gate.go#L217)

The current self-customer ownership checks remain in force. This is a request to decide and document consent-credential policy; the different admission rules alone do not establish unauthorized access. Tightening them could remove intentional automation capabilities and needs an owner decision.

### 7. Make the generated contract as precise as runtime validation

The OpenAPI generator marks required properties only for output objects. It also emits Idempotency-Key and requestBody without required flags, even where runtime requires them. Add explicit input/route metadata for required fields, bodies and headers, plus mutually exclusive alternatives, bounds and enum queries. Do not infer “required” from every non-omitempty Go field: defaults, PATCH and conditional fields need different treatment. Some keys are intentionally optional today. [Required property generation](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/contract/openapi.go#L132) [Header/body generation](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/contract/openapi.go#L200)

### 8. Make incomplete data and estimates obvious

Capped customer search should not look like a complete export; true continuation requires extending the directory contract. Metrics results could label their requested top-N scope more clearly, although the limit is already explicit input. Price reads already mark unverified state unknown and expose per-provider verification failures; preserve the currently swallowed top-level verification error when a requested check could not run. Subscription-change results could make their documented Stripe estimate semantics machine-readable, so clients can distinguish estimates from confirmed amounts even after an operation succeeds. Keep useful partial/local data where appropriate, with an explicit availability marker; never silently turn an outage into “not found” or an empty value. [Bounded search](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/customers.go#L137) [Metrics truncation](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/metrics/service.go#L216) [Estimated change amount](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/checkout/stripe_tier_change_intent.go#L765)

## Routes organization

We’d organize the documentation and SDK around audience first, then resource/workflow. Most of the URLs can stay. Put adjacent steps together in the docs even when their permissions differ: dashboard read/save, order preview/create/read, provider-operation write/investigation, and payment-method save/confirm/delete.

| Surface | What belongs there | Our recommendation |
|---|---|---|
| `/me` | The current customer’s account and purchases | Keep customer identity implicit; document secret-bearing versus status-only operations. |
| `/app` | Backend integration commands and narrowly permitted reads | Keep dedicated app permissions. Consider status reads that complete a real integration workflow. |
| `/admin` | Merchant support, catalog and configuration | Keep resource groups and permission distinctions; permitted applications can already use these routes. |
| `/network` | A native account acting across its owned links | Keep the active link as the grant; share domain handlers with `/me`. |
| Public checkout/config | Storefront lookup and wallet/bootstrap entry points | Keep small public projections and protocol-specific shapes. |
| SCIM | Directory provisioning | Keep its own envelopes, capitalization and pagination. |
| Webhooks | Provider-to-server delivery | Keep account resolution and rail-specific authentication at ingress. |

**Provider operations should stay under `/app` for programmatic writes.** Their admin reads/close are already available to appropriately permitted applications as well as people. If a writer needs fresh state without broad AdminRead, a reduced app read with a specific permission is a useful addition. If evidence is intentionally operator-only, preserve that and expose only the status needed by the integration. Keep the broad name: narrowing it to “cost” would prematurely constrain the resource.

**App checkout uses orders with dedicated Orders permission.** We’d focus on fresh app reads and explicitly documented recovery of the one-time hosted checkout URL. Retained HTTP response replay already recovers the original successful response; the stored order alone cannot reconstruct the secret after that replay is unavailable. [Current app order routes](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/orders.go#L48) [One-time checkout URL](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/orders.go#L836)

One specific organization gap remains in network card saving: it exposes save but not the owned confirm-by-method-ID continuation available under `/me`. Stripe callers retaining the original token can repeat save to confirm today. Add the linked confirm route so a reloaded portal can continue from the saved method ID; this does not require copying every customer endpoint into network. [Network route selection](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/customer_network.go#L38) [Existing same-token continuation](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/checkout/stripe_method_setup.go#L92)

## Request and response conventions we’d keep

- Exact decimal strings for money, typed IDs, and bounded cursor collections. Avoid floating-point money and a blanket switch to offsets.
- Explicit commands such as cancel, capture, resume and revoke when they enforce a real transition; a generic status PATCH would hide those rules.
- One order resource across purchase/payment recovery, with next_action where the customer needs to act.
- Immutable price terms, catalog revisions, field ownership and conflict receipts. Repricing and migrating subscribers should remain separate decisions.
- Per-item outcomes for deliberately independent batches; atomicity for batches that promise all-or-nothing. Document which contract each route uses.
- Existing clear semantics where implemented: subscription payment_method_id=null follows the default; customer settings already have deliberate tri-state and per-currency behavior.
- Resource responses and focused projections. Avoid adding a universal success wrapper or putting every operation through a generic job API.
- SCIM, wallet and webhook contracts that serve different clients; consistency should not erase useful protocol differences.

For rollout, start with additive fields, typed SDK helpers, accurate schema metadata and caller fixes. Changes to required keys, clearing semantics, request unions or HTTP methods need explicit compatibility handling. The deeper migration/export work should be separate, reviewable changes; this audit does not authorize implementing them.

## Duplication and overlap

We’d consolidate repeated contract definitions and pricing decisions before trying to reduce the route count. **There are no duplicate method/path entries in the 181-route catalogue.** The catalogue also rejects an exact duplicate when it initializes. Ten customer/network pairs deliberately share handlers through generated declarations; that is useful reuse, not ten accidental copies. Similar URLs, identical JSON fields and calls into the same service are starting points for review, not sufficient reasons to delete an API. [internal/http/routes/route.go:344](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/http/routes/route.go#L344) [internal/http/routes/customer_network.go:38](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/http/routes/customer_network.go#L38)

We separate three decisions: **consolidate** definitions that describe the same thing, **simplify** overlapping caller choices, and **keep** entry points whose authority or lifecycle differs. The findings below supplement the individual route verdicts; they do not add routes to the inventory or authorize implementation.

### D1. Consolidate the browser’s payment-action contracts — high priority

**Affected:** order create/pay/confirm, payment-method save/confirm, subscription reads and subscription change, including linked network equivalents.

The server already has one `billing.NextAction`. The browser maintains three parsers: `nextActionSchema`, an inline payment-method schema, and `orderNextActionSchema`. They have drifted: the subscription parser expects `redirect_to_url.url`, while the current CCBill change response sends top-level `url`. The order parser understands that current shape. The subscription parser does not preserve the URL delivered by the server. This is concrete evidence that the duplicate definitions are costing us consistency; no live CCBill journey was run. [billing/commerce.go:11](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/billing/commerce.go#L11) [sdk/billing-ui/src/client/types.ts:145](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/sdk/billing-ui/src/client/types.ts#L145) [sdk/billing-ui/src/client/types.ts:243](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/sdk/billing-ui/src/client/types.ts#L243) [sdk/billing-ui/src/client/types.ts:293](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/sdk/billing-ui/src/client/types.ts#L293) [internal/modules/checkout/service.go:1233](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/modules/checkout/service.go#L1233)

**What we’d change:** one canonical runtime parser matching the wire contract, reused by these responses, with action-specific variants where they help callers. Keep route-specific resource envelopes. The generated TypeScript wire type alone is not enough while runtime parsers independently discard fields. This needs an SDK correction and consideration for consumers of the old exported type; it does not need a URL migration.

There is related overlap between order confirmation and payment-operation authentication. Stripe order confirmation calls the same authentication service, but also performs order follow-up; it handles other rails too. Operation authentication also serves subscription changes. **Keep both addresses**, make order confirmation the documented order workflow, and share action/status interpretation. The browser’s operation-confirm method currently discards the returned operation; returning that state would help callers distinguish authentication completion from payment completion. The exported helper was inspected, but no current internal production caller was found, so this is not a claim of an observed customer failure. [internal/modules/checkout/order_pay.go:620](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/modules/checkout/order_pay.go#L620) [internal/modules/checkout/engine_change.go:515](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/modules/checkout/engine_change.go#L515) [sdk/billing-ui/src/client/client.ts:616](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/sdk/billing-ui/src/client/client.ts#L616) [sdk/billing-ui/src/authenticate.ts:10](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/sdk/billing-ui/src/authenticate.ts#L10)

### D2. Share pricing policy and preserve price terms in callers — medium priority

**Affected:** `POST /v1/admin/catalog/ask`, `POST /v1/admin/catalog/prices`, and price-migration preview/create workflows.

The copilot duplicates the console’s migration defaults and even its review text. Its proposed increase date uses a hardcoded 30-day fallback, while the console reads the merchant’s `reprice_notice_window_days`. These can propose different dates for the same merchant. The migration engine still evaluates the configured notice policy and can skip affected subscriptions unless the short notice is explicitly acknowledged; this is proposal drift, not a way around enforcement. [internal/modules/copilot/tools_draft.go:23](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/modules/copilot/tools_draft.go#L23) [web/admin/src/pages/catalog/price-wizard.tsx:92](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/web/admin/src/pages/catalog/price-wizard.tsx#L92) [internal/modules/subscriptions/price_migration.go:334](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/modules/subscriptions/price_migration.go#L334)

The console also rebuilds a replacement-price request by selecting fields from the existing price. That mapping copies cadence, trial and provider choices but omits `quantity`, even though the wizard permits quantity prices and quantity bounds are part of the server’s immutable price identity. Catalog applications already preserve the existing quantity unless changed. This is another reason to avoid separately maintained approximations of the same pricing terms. The omission is confirmed in source; its downstream customer impact was not exercised. [web/admin/src/pages/catalog/price-wizard.tsx:109](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/web/admin/src/pages/catalog/price-wizard.tsx#L109) [web/admin/src/pages/catalog/price-wizard.tsx:189](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/web/admin/src/pages/catalog/price-wizard.tsx#L189) [internal/service/service_definition_catalog.go:479](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/service/service_definition_catalog.go#L479) [internal/service/catalog_application.go:495](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/service/catalog_application.go#L495)

**What we’d change:** share the existing merchant-aware notice/default calculation where proposals are built, and use an explicit reviewed mapping that preserves the existing financial terms intended to remain unchanged. A structured preview extension is an option if current callers need it, not a prerequisite. Preserve distinct price creation, migration choice and migration execution; adding a planning endpoint or framework is not justified by this finding.

### D3. Pick one ordinary cart-preview path — low priority

**Overlap:** `GET /v1/me/cart` and `POST /v1/me/orders/preview` with `{ "cart": true }` both load and price the current persisted cart. The GET additionally carries cart timestamps, accepts an empty cart and preserves missing price IDs on refused lines; the POST rejects an empty cart and returns `OrderPreview`. They are overlapping calculations with different projections, not duplicate durable purchases. [internal/service/carts.go:28](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/service/carts.go#L28) [internal/service/carts.go:90](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/service/carts.go#L90) [internal/service/orders.go:82](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/service/orders.go#L82)

**What we’d change:** treat GET cart as the normal cart-preview path and keep POST preview for proposed explicit lines. The shipped cart UI already uses cart reads. If we later simplify the public contract, consider deprecating only the `cart:true` preview option after checking external consumers. Removing an accepted option is breaking; this is not urgent enough to justify immediate removal. Preserve the shared pricing implementation and the useful empty-cart behavior.

### D4. Remove small duplicated wire definitions — low priority

Two pairs describe the same payload independently:

| Definitions | Where they meet | What we’d do |
|---|---|---|
| `AdmissionVerdict` and `AdmissionResult` | Admit batches versus release/extend batches: both contain `status`, `admission`, `error`. | Use one wire definition, with meaningful names or helpers where needed. Preserve the different questions asked by `Allowed()` and `Err()`; absence of an error alone is not proof of an active admission. |
| `EntitlementChange` and `ProductEntitlementsChangedEvent` | Catalog application receipt versus the host event announcing the same edit. The service copies the same five fields. | Reuse one entitlement-change value/definition inside the distinct receipt and event envelopes. Preserve empty-array normalization and event delivery semantics. |

These are maintenance simplifications, not reasons to merge admission operations or replace host events with catalog responses. Prefer a shared concrete definition or alias over a general-purpose abstraction. Preserve exported names where compatibility matters. [billing/admissions.go:113](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/billing/admissions.go#L113) [billing/admissions.go:199](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/billing/admissions.go#L199) [billing/catalog_application.go:80](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/billing/catalog_application.go#L80) [billing/host_events.go:73](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/billing/host_events.go#L73) [internal/service/catalog_entitlements.go:178](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/service/catalog_entitlements.go#L178)

### D5. Similar routes we’d keep

The table spells out why each overlap is useful. Paths below are relative to `/v1`; a bracketed suffix denotes the corresponding separate routes, not a literal registered path.

| Overlapping surface | Actual distinction and recommendation |
|---|---|
| `/me` and `/network/links/{link_id}`: order GET/pay/confirm; payment-method list/save/delete; subscription GET/cancel/resume/payment-method | **Keep all ten pairs.** Merchant-customer identity and native-account-plus-active-link authority differ. Their declarations and handlers are already shared. Add any justified missing continuation through that same generator, rather than copying a second lifecycle. |
| `POST /me/orders[/preview]` and `POST /app/orders[/preview]` | **Keep audience wrappers and shared line/preview types.** Customer create can consume a cart and pay; app create selects the customer and hosted checkout terms but never charges. There is no admin order-create route to consolidate here. |
| `POST /me/orders` with payment and `POST /me/orders/{id}/pay` | **Keep both.** One-call purchase and resuming an existing unpaid order have different retry boundaries. Document creation-key versus payment-attempt-key scope instead of making clients create another order after a failed charge. |
| `POST /me/orders/{id}/pay`, `POST /admin/orders/{id}/retry-collection`, `POST /admin/payments` | **Keep all three.** Customer payment and merchant collection differ in authority and eligibility. Admin payments records an already-received external remittance; it does not initiate another provider charge. Give SDK callers a clear “record external payment” name. |
| `POST /me/subscriptions/{id}/retry-now` and order pay | **Keep.** Subscription retry refuses engine-owned subscriptions and points them to their recovery order. Provider-scheduled renewals follow another recovery lifecycle. Unifying them would require a domain migration, not deleting a duplicate URL. |
| `PUT /me/default-payment-methods/{currency}` and `PUT /me/subscriptions/{id}/payment-method` | **Keep.** One changes the inherited currency default; the other sets an explicit override, or clears it to follow the default. Assigning the same card today can produce different future behavior. |
| Customer/admin subscription payment-method PUT | **Keep audience routes, share implementation.** Both already call the same update helper. A clearer pending-swap result should be shared too; do not make staff impersonate customer presence. |
| Admission capture with `usage` and `POST /app/usage-events` | **Keep, with one clearly documented reporting choice.** Capture can settle the hold and attach usage in the same transaction. Standalone usage handles events outside that cycle, including failures. Do not routinely call both for one event. Stable source coordinates and replay rules matter, so calling both is not automatically a second charge. Existing integration docs already explain the distinction; keep that guidance prominent in SDK examples. |
| `/app/admissions` lifecycle and `/app/provider-operations` lifecycle | **Keep their distinct contracts and permissions.** Admissions govern requests and caller-specified capture amounts. Provider operations bind capacity and provider evidence that OpenRails qualifies and settles. A generic operation DTO full of optional fields would obscure who is authoritative. Keep the broader provider-operation name. |
| Entitlement reads/check, product-access grants and subscriptions | **Keep.** Effective entitlement keys, underlying access grants and billing agreements are different resources. Share the existing effective-access calculation, rather than making callers infer access from subscriptions alone. |
| `POST /admin/catalog/applications` versus individual product/price/meter writes | **Keep declarative and interactive editing.** Applications have declared ownership, content replay, partial conflict receipts and optional pruning; individual edits record edit ownership and have different provider actions. They already reuse product/price services. Share invariants, but do not funnel all edits through application semantics. |
| `POST /admin/catalog/products` versus `POST /admin/catalog/prices` with `product_data` | **Keep atomic convenience and full product creation.** Inline data creates or reuses a minimal product without editing an existing one. It also has local-only/conflict constraints that differ from creating a new immutable price version by product ID/key. Surface those differences beside the selectors in generated docs; dedicated docs already explain them. |
| `POST /admin/catalog/product-archives` versus product PATCH `{archived:true}` | **Keep.** PATCH retires offers. The command additionally applies an idempotent refund/review policy to historical purchases and explicitly rejects archive-only requests. Keep the shared archive primitive; avoid hiding a financial workflow in ordinary PATCH. |
| Public/app/admin catalog product GET routes | **Keep query and disclosure boundaries.** Public reads require named keys; app offers also support entitlement discovery; admin reads expose operator filters/provider detail. Public/app already share offer filtering and buyer-price projection. Share that projection, without opening admin enumeration to public callers. |
| SCIM User writes versus `PATCH /admin/customers/{customer_id}` | **Keep directory contact and billing settings separate.** Invoice recipients can differ from directory contacts. There is no second admin writer for the same SCIM contact fields here; automatic synchronization would require a product decision. |
| `POST /admin/billing-archive` versus `POST /admin/billing-import` | **Keep restore and historical-fact import separate.** Archive restore uses an OpenRails artifact, destination constraints and readonly posture. Import reconciles external billing facts and returns per-source outcomes. Combining them would obscure very different validation and retry contracts. |
| `POST /admin/catalog/ask`, `POST /admin/metrics/ask`, `POST /admin/dashboard/widgets/generate` | **Keep typed task boundaries.** Catalog answers/drafts, executed metric evidence and an unsaved widget candidate have different capabilities and outputs. Model transport/error machinery is already shared. Consolidate repeated pricing policy as in D2; do not replace these with a universal ask endpoint. |

**Evidence for audience/payment distinctions:** [internal/http/routes/customer_network.go:52](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/http/routes/customer_network.go#L52) [internal/http/routes/gate.go:585](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/http/routes/gate.go#L585) [billing/orders.go:102](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/billing/orders.go#L102) [internal/service/orders.go:727](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/service/orders.go#L727) [internal/http/handlers/update_subscription_payment_method.go:22](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/http/handlers/update_subscription_payment_method.go#L22) [internal/http/handlers/admin_payments.go:542](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/http/handlers/admin_payments.go#L542) [internal/service/customer_payment.go:30](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/service/customer_payment.go#L30) [internal/modules/subscriptions/default_payment_method.go:28](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/modules/subscriptions/default_payment_method.go#L28)

**Evidence for usage/access distinctions:** [internal/modules/money/admission_capture.go:23](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/modules/money/admission_capture.go#L23) [internal/modules/money/usage.go:83](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/modules/money/usage.go#L83) [docs/embedded-integration.md:438](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/docs/embedded-integration.md#L438) [billing/provider_obligations.go:47](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/billing/provider_obligations.go#L47) [internal/modules/money/provider_billing.go:125](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/modules/money/provider_billing.go#L125) [internal/http/handlers/entitlements.go:46](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/http/handlers/entitlements.go#L46)

**Evidence for catalog/configuration distinctions:** [internal/service/catalog_apply_plan.go:39](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/service/catalog_apply_plan.go#L39) [internal/service/catalog_application.go:104](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/service/catalog_application.go#L104) [internal/service/catalog_inline_product.go:26](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/service/catalog_inline_product.go#L26) [docs/catalog-client.md:21](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/docs/catalog-client.md#L21) [internal/http/handlers/product_archive.go:86](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/http/handlers/product_archive.go#L86) [internal/http/handlers/catalog.go:14](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/http/handlers/catalog.go#L14) [billing/customers.go:9](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/billing/customers.go#L9) [billing/customer_settings.go:5](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/billing/customer_settings.go#L5) [internal/http/handlers/merchant_archive.go:19](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/http/handlers/merchant_archive.go#L19) [internal/http/handlers/billing_import.go:14](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/http/handlers/billing_import.go#L14) [internal/modules/copilot/ask.go:15](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/modules/copilot/ask.go#L15) [internal/modules/dashboard/ask.go:23](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/modules/dashboard/ask.go#L23) [internal/modules/dashboard/generate.go:45](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/modules/dashboard/generate.go#L45)

For these retained boundaries, better paired documentation and shared implementations have low migration cost. Merging URLs would change caller authority, ownership or workflow semantics and would require a stronger product reason than reducing the count. The narrower buyer DTO mentioned in the individual audit remains optional; current projection is not evidence of a secret leak.

### D6. Identical JSON fields do not always warrant a shared type

The generated schema contains other exact-looking object pairs: catalog/metrics questions both have a question; credit/product-access revocations both have a reason; retry-subscription, retry-collection and set-default requests each have a payment-method ID. **Keep the semantic request names.** Their permissions, validation, optionality and effects differ. In particular, an optional retry-subscription method must match the currently selected method; collection selects a charge method, while setting a default initiates no immediate charge. Generated shape equality can also hide required-field differences already discussed in recommendation 7. Do not create a vague `PaymentMethodRequest` just to deduplicate one property. [billing/payment_recovery.go:55](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/billing/payment_recovery.go#L55) [internal/contract/openapi.go:132](https://github.com/open-rails/openrails/blob/c431f3cf231028806cb4e056cb3de2ccfee0e9de/internal/contract/openapi.go#L132)


## Coverage and how to read the entries

The count is **181 canonical route templates**, not 181 routes guaranteed to be mounted in every deployment: 171 explicit declarations plus 10 network variants generated from customer routes. The OpenAPI document contains 169 operations; the remaining 12 are SCIM. Feature flags and embedded/standalone configuration affect mounting. Host-supplied auth routes, deployment-only operator/control-plane surfaces, static assets and implicit HEAD/OPTIONS behavior are outside this canonical billing-route inventory.

| Catalog group | Routes |
|---|---:|
| Customer | 36 |
| Checkout | 3 |
| App | 33 |
| CatalogAdmin | 24 |
| Meta | 3 |
| Admin | 40 |
| Network | 15 |
| MerchantConfig | 20 |
| Access | 1 |
| Metrics | 5 |
| Webhooks | 1 |
| **Total** | **181** |

Each numbered entry is independently assessed. “Keep” permits small documentation suggestions without proposing a route redesign. “Refine” preserves the resource/workflow. “Redesign” proposes changing a meaningful lifecycle or request contract. All current-behavior descriptions are source-traced; they are not claims that every route was exercised against a live server.

Route sections: [Customer routes — 36](#customer-routes--36) · [Application routes — 17](#application-routes--17) · [Network routes — 15](#network-routes--15) · [Catalog routes — 24](#catalog-routes--24) · [Administrative billing routes — 40](#administrative-billing-routes--40) · [Merchant configuration routes — 20](#merchant-configuration-routes--20) · [Metrics and administrative access — 6](#metrics-and-administrative-access--6) · [Public checkout and process routes — 6](#public-checkout-and-process-routes--6) · [SCIM provisioning routes — 12](#scim-provisioning-routes--12) · [Provider webhook route — 1](#provider-webhook-route--1) · [Running-charge routes — 4](#running-charge-routes--4)

## Customer routes — 36

### 001. `GET /v1/me/cart`

**Keep · No change needed**

**Current:** The customer gate binds one merchant and self-acting customer. The service loads the persisted cart, treats expired carts as empty, reprices its lines and returns per-line refusals and currently compatible payment options. Empty carts have empty arrays and null change/expiry timestamps; the handler sends no-store.

**Our recommendation:** Keep this live preview model and its explicit refusal per line. A cart should remain recoverable when a price becomes unavailable, as this implementation allows.

**Why / tradeoff:** This gives merchant sites one shared selection without pretending that adding a line reserves availability or locks a price. Freezing prices belongs to order creation; no separate checkout-session resource is needed.

**Code:** [internal/http/handlers/carts.go:12](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/carts.go#L12) · [internal/service/carts.go:90](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/carts.go#L90) · [internal/modules/orders/cart.go:53](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/orders/cart.go#L53) · [ci/subscriptions/cart_test.go:29](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/ci/subscriptions/cart_test.go#L29) · [internal/http/routes/carts.go:11](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/carts.go#L11)

**Overlap review:** See D3 in [Duplication and overlap](#duplication-and-overlap): use cart GET as the ordinary cart-preview path; any removal of the overlapping cart:true preview option needs consumer review and deprecation.


### 002. `PUT /v1/me/cart/lines/{price_id}`

**Refine · Low priority**

**Current:** The customer-owned mutation runs under the cart row lock. Quantity zero removes a line; another price of the same product replaces that product’s existing line, even though the URL names a different price. Invalid business selections may remain in the cart with a refusal in the returned preview.

**Our recommendation:** Keep absolute PUT quantities and refusal-bearing cart responses, but make the product-replacement rule prominent in the generated contract and expose the replaced selection to clients. An optional cart revision would let multi-tab callers reject stale edits.

**Why / tradeoff:** The current transactional update avoids lost database writes, but clients cannot infer cross-price replacement from the resource URL alone. A revision adds conflict handling and should be optional initially; changing line identity would be a breaking change.

**Code:** [internal/modules/orders/cart.go:83](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/orders/cart.go#L83) · [internal/modules/orders/cart.go:177](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/orders/cart.go#L177) · [billing/carts.go:28](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/billing/carts.go#L28) · [sdk/billing-ui/src/cart.tsx:118](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/sdk/billing-ui/src/cart.tsx#L118) · [internal/http/routes/carts.go:13](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/carts.go#L13)

### 003. `DELETE /v1/me/cart`

**Keep · No change needed**

**Current:** The authenticated customer’s cart is cleared transactionally; an absent cart is already empty. The handler returns 204 and the SDK’s cart view rereads the cart afterward. This does not cancel an order that previously consumed those lines.

**Our recommendation:** Keep the idempotent delete and 204 response. Document that the singleton belongs to the customer across the merchant’s sites, so this is a shared-cart action.

**Why / tradeoff:** There is no useful deleted resource representation to return. Keeping cart clearing separate from order cancellation preserves the purchase lifecycle and lets clients safely retry an uncertain clear.

**Code:** [internal/http/handlers/carts.go:54](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/carts.go#L54) · [internal/modules/orders/cart.go:141](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/orders/cart.go#L141) · [sdk/billing-ui/src/cart.tsx:186](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/sdk/billing-ui/src/cart.tsx#L186) · [internal/http/routes/carts.go:15](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/carts.go#L15)

### 004. `GET /v1/me`

**Keep · No change needed**

**Current:** The self-customer scope selects the account; clients cannot supply another customer ID. GetCustomerAccount collects money and defaults per currency plus unread notification count. A customer never billed receives empty financial lists rather than a missing-resource error.

**Our recommendation:** Keep a small account summary at /me, with detailed history in its existing collections. Do not grow it into a checkout or subscription aggregate.

**Why / tradeoff:** The response answers the account screen’s immediate questions without demanding currency selection first. Separating the potentially large ledger, payments and subscriptions keeps this entry point predictable; these independently read values are a current summary, not an atomic financial statement.

**Code:** [internal/http/handlers/self_account.go:29](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/self_account.go#L29) · [internal/service/customers.go:188](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/customers.go#L188) · [sdk/billing-ui/src/client/client.ts:671](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/sdk/billing-ui/src/client/client.ts#L671) · [internal/http/routes/credits.go:33](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/credits.go#L33)

### 005. `GET /v1/me/balance/transactions`

**Keep · No change needed**

**Current:** The handler fixes customer identity from the gate and binds currency and pagination. The service requires a supported currency; the money repository uses merchant/customer/currency predicates and a created-at plus ID cursor to return newest movements first.

**Our recommendation:** Keep the currency-specific, cursor-paged ledger alongside the multi-currency account summary. Retain the separate distinction from payment records.

**Why / tradeoff:** A ledger movement explains posted credit and debt changes, whereas a payment explains provider money movement. Live holds and unposted running-charge accrual are reflected separately in the balance projection; the ledger list is not a hold history. Keeping these concepts distinct helps reconciliation.

**Code:** [internal/http/handlers/self_account.go:46](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/self_account.go#L46) · [internal/http/handlers/credits.go:229](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/credits.go#L229) · [internal/service/credit_grants.go:138](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/credit_grants.go#L138) · [internal/modules/money/money_service.go:307](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/money/money_service.go#L307) · [internal/http/routes/credits.go:35](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/credits.go#L35) · [billing/credits.go:190](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/billing/credits.go#L190)

### 006. `POST /v1/me/network-link/challenges`

**Keep · No change needed**

**Current:** The feature-gated handler derives merchant/customer proof from the customer scope, distinguishing the hosted checkout’s order secret from a normal customer token. The network service creates a random challenge and nonce with five-minute expiry, storing the proof binding for redemption.

**Our recommendation:** Keep the separate challenge resource and return its nonce and expiry. The consumer should restart explicitly after expiry or a spent challenge instead of silently reusing proof.

**Why / tradeoff:** The two-stage exchange proves both sides of the link without trusting matching email addresses. It introduces an extra request, but that cost buys a narrowly bound sign-in exchange and avoids accepting an unrelated old account token.

**Uncertainty:** Challenge persistence and AuthKit verification were source-traced at their call boundaries; no live identity-provider exchange was exercised.

**Code:** [internal/http/handlers/customer_network.go:18](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/customer_network.go#L18) · [internal/http/handlers/customer_network.go:36](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/customer_network.go#L36) · [server/internal/network/network.go:108](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/server/internal/network/network.go#L108) · [internal/http/routes/customer_network.go:19](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/customer_network.go#L19)

### 007. `POST /v1/me/network-link`

**Refine · Medium priority**

**Current:** The service takes the one-use challenge before checking the ID token, nonce, authentication time, allowed client/group, consent and terms. It transactionally inserts the link and host event, returning 201 or an existing same-account link with 200. Reusing the same spent challenge fails even if its first redemption committed.

**Our recommendation:** Keep the one-use proof exchange and document recovery by credential: a native account can inspect GET /v1/network/links, and an authorized merchant backend can use the existing network-account customer lookup. The hosted checkout callback has only its checkout credential and the linking ID token, so a same-credential success receipt or link-state read would be an optional refinement for that lost-response case. Do not infer general POST replay from the existing 200 branch.

**Why / tradeoff:** A committed link already has authenticated ways to inspect it, while its credential exchange is deliberately single-use. Make those existing paths discoverable before adding another endpoint. Any checkout-specific receipt must remain bound to the original proof and cannot turn a spent challenge into a reusable linking credential.

**Code:** [internal/http/handlers/customer_network.go:51](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/customer_network.go#L51) · [server/internal/network/network.go:130](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/server/internal/network/network.go#L130) · [server/internal/network/network.go:201](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/server/internal/network/network.go#L201) · [internal/http/routes/customer_network.go:21](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/customer_network.go#L21) · [internal/http/handlers/network.go:52](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/http/handlers/network.go#L52) · [internal/http/handlers/customer_network.go:76](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/http/handlers/customer_network.go#L76)

### 008. `GET /v1/me/entitlements`

**Keep · No change needed**

**Current:** The customer ID comes from the verified self scope. Prefix and optional time narrow the read, while an opaque customer/key cursor pages results. The entitlement service reads a database snapshot and derives current access windows or uses a time-valid cache; the response exposes held keys and quantities.

**Our recommendation:** Keep this dedicated effective-access projection. Document that at selects the access evaluation instant, rather than treating subscription status as access.

**Why / tradeoff:** This makes authorization consumers independent of cancellation, prepaid access and subscription billing states. The current compact shape is appropriate; exposing internal grants and every source on this customer route would complicate a simple access question.

**Code:** [internal/http/handlers/entitlements.go:61](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/entitlements.go#L61) · [internal/modules/entitlements/resource_checks.go:104](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/entitlements/resource_checks.go#L104) · [internal/http/handlers/entitlements.go:166](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/entitlements.go#L166) · [internal/http/routes/entitlements.go:22](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/entitlements.go#L22)

### 009. `GET /v1/me/usage`

**Refine · Medium priority**

**Current:** The self scope fixes customer ownership. Currency is required; from/to accept dates or RFC3339 and default to the previous month through now. Grouping is restricted to event_type, resource or invoker, but any valid window is accepted and all aggregate groups are returned in one response.

**Our recommendation:** Define a maximum synchronous window and a bounded strategy for high-cardinality resource/invoker groups, such as cursor-paged groups with a separately stated total. Include the applied bounds in the contract.

**Why / tradeoff:** The existing summary is convenient for a small dashboard. A customer with many resources can otherwise turn the same endpoint into an unbounded report. Adding bounds changes unusually large requests, so introduce documented limits without silently truncating existing totals.

**Code:** [internal/http/handlers/self_usage_invoices.go:35](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/self_usage_invoices.go#L35) · [internal/service/spend.go:54](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/spend.go#L54) · [internal/modules/money/service_usage.go:39](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/money/service_usage.go#L39) · [internal/modules/money/usage.go:386](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/money/usage.go#L386) · [internal/http/routes/metering.go:31](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/metering.go#L31)

### 010. `GET /v1/me/notifications`

**Keep · No change needed**

**Current:** The route binds the merchant/customer from verified scope, accepts optional seen filtering, and returns a newest-first cursor page. SQL restricts recipient kind to customer and uses created-at plus ID ordering. Reading does not mark messages read.

**Our recommendation:** Keep the read-only collection and separate acknowledgement command, including the nullable seen filter for all/read/unread.

**Why / tradeoff:** This lets notification badges, inboxes and background refreshes share one route without accidental acknowledgements. Stable ordering and tenant/customer predicates are present; adding offset pagination or read side effects would weaken this straightforward contract.

**Code:** [internal/http/handlers/notifications.go:33](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/notifications.go#L33) · [internal/http/handlers/notifications.go:114](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/notifications.go#L114) · [internal/db/queries/notifications.sql:117](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/db/queries/notifications.sql#L117) · [internal/http/routes/ops.go:39](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/ops.go#L39)

### 011. `POST /v1/me/notifications/read`

**Refine · Low priority**

**Current:** The body chooses IDs or all; both together are rejected. Selected acknowledgements return an ID-keyed lookup with null for unknown/foreign notifications and preserve existing read timestamps. all marks every currently unread notification at execution time and returns an empty lookup.

**Our recommendation:** Keep the ID-based batch shape. For mark-all, accept an optional seen-through timestamp or cursor and return an affected count, so clients can acknowledge the inbox snapshot the person actually reviewed.

**Why / tradeoff:** The current all command is legitimate if it means all messages at execution, but it can acknowledge a notification arriving after the UI rendered. A cutoff costs one optional field and should preserve today’s all behavior for existing clients.

**Code:** [internal/http/handlers/notifications.go:62](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/notifications.go#L62) · [internal/db/queries/notifications.sql:128](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/db/queries/notifications.sql#L128) · [internal/db/queries/notifications.sql:136](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/db/queries/notifications.sql#L136) · [internal/http/routes/ops.go:41](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/ops.go#L41)

### 012. `POST /v1/me/orders/preview`

**Keep · No change needed**

**Current:** The authenticated customer sends either lines or cart=true. The service rejects both/neither, resolves current prices and ownership, and returns totals, per-line refusals and compatible PSP options without creating an order or moving money.

**Our recommendation:** Keep POST for the structured preview and the same line vocabulary as creation. Preserve refusal details rather than failing the entire preview on the first unavailable line.

**Why / tradeoff:** A useful preview explains what the customer can repair before buying. The subsequent expected_total check already protects price changes; previews should remain advisory rather than imply a reservation or require a new quote-resource lifecycle.

**Code:** [internal/http/handlers/orders.go:114](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/orders.go#L114) · [internal/service/carts.go:16](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/carts.go#L16) · [internal/service/orders.go:82](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/orders.go#L82) · [internal/modules/orders/create.go:24](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/orders/create.go#L24) · [internal/http/routes/orders.go:27](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/orders.go#L27)

**Overlap review:** See D3 in [Duplication and overlap](#duplication-and-overlap): use cart GET as the ordinary cart-preview path; any removal of the overlapping cart:true preview option needs consumer review and deprecation.


### 013. `POST /v1/me/orders`

**Refine · Medium priority**

**Current:** The route creates an open order or performs one-call payment; payment requires the Present credential policy and expected_total. Keys are scoped to the customer and compare request digests. Cart lines are consumed in the order transaction; tokenized new cards are saved by the charge itself. Success returns the order, and a decline returns 402 carrying that same order.

**Our recommendation:** Keep this unified purchase resource. Add an optional expected cart revision or line fingerprint when cart=true; expected_total alone does not establish which same-priced selection the buyer approved.

**Why / tradeoff:** Two sites can change a shared cart between display and buy without changing its total. A composition precondition protects that intent while preserving the current one-call flow. Introduce it additively; explicit lines already state the desired composition.

**Code:** [internal/http/handlers/orders.go:134](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/orders.go#L134) · [internal/service/orders.go:157](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/orders.go#L157) · [internal/modules/orders/create.go:52](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/orders/create.go#L52) · [ci/subscriptions/order_payment_test.go:47](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/ci/subscriptions/order_payment_test.go#L47) · [internal/http/routes/orders.go:29](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/orders.go#L29)

**Overlap review:** See D1 in [Duplication and overlap](#duplication-and-overlap): share the payment-action parser and status interpretation while retaining the distinct resource workflows.


### 014. `GET /v1/me/orders`

**Refine · Low priority**

**Current:** The service overwrites customer_id with the authenticated customer. Status/origin/overdue and subscription filters are validated and the query uses keyset pagination. List entries reuse Order but deliberately omit next_action and payment_options, while preserving payment state and billing origin.

**Our recommendation:** Document the list projection explicitly, or introduce a named OrderSummary schema with the same financial fields and a detail link. Do not let null next_action imply that no action is required.

**Why / tradeoff:** Keeping list reads free of provider secrets and routing work is sensible. A distinct documented projection prevents SDK users from treating a requires_action list entry as a complete checkout representation; a new wire type is optional and should not require reshaping existing JSON.

**Code:** [internal/http/handlers/orders.go:441](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/orders.go#L441) · [internal/service/orders.go:454](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/orders.go#L454) · [internal/service/orders.go:487](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/orders.go#L487) · [internal/modules/orders/create.go:215](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/orders/create.go#L215) · [internal/http/routes/orders.go:31](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/orders.go#L31)

### 015. `GET /v1/me/orders/{id}`

**Refine · Medium priority**

**Current:** The service enforces customer ownership and returns frozen order data plus current payment options. For an action-required Stripe order, it also asks the provider for authentication data, which requires a payer passing the existing Present credential policy; provider failure or a self API-key/signed-token credential can therefore fail the whole detail read. Hosted checkout credentials are additionally bound to this order.

**Our recommendation:** Keep the order as the recovery handle, but separate guaranteed local status retrieval from optional secret-bearing action retrieval, or return an explicit action-unavailable reason while withholding the secret.

**Why / tradeoff:** A customer should be able to see persisted order status during provider recovery without confusing absence of a secret with absence of an action. This is a resilience refinement, not proof orders are unrecoverable; list reads and retries by permitted credentials already provide recovery paths.

**Code:** [internal/service/orders.go:546](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/orders.go#L546) · [internal/modules/checkout/order_pay.go:587](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/checkout/order_pay.go#L587) · [internal/modules/checkout/stripe_engine_authentication.go:24](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/checkout/stripe_engine_authentication.go#L24) · [internal/http/routes/mount.go:426](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/mount.go#L426) · [internal/http/routes/orders.go:33](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/orders.go#L33)

### 016. `POST /v1/me/orders/{id}/pay`

**Refine · Medium priority**

**Current:** The handler requires the Present credential policy, ownership, expected_total and Idempotency-Key, with checkout-secret restrictions on saved cards. Purchases accept saved card, new token, Solana token symbol or hosted return URL; renewal/usage orders use their own saved-card collection paths. Successful and declined responses carry the order.

**Our recommendation:** Keep one pay endpoint and durable attempts. Express payment inputs as an explicit tagged union in generated clients/schema, and document which origin/payment_options combinations are admissible instead of implying every order accepts all four forms.

**Why / tradeoff:** The server already enforces exactly one source; flat optional fields force clients to rediscover those rules and allow invalid combinations at compile time. Add typed constructors or oneOf first, retaining the existing wire shape until a versioned change is justified.

**Code:** [internal/http/handlers/orders.go:157](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/orders.go#L157) · [internal/service/orders.go:117](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/orders.go#L117) · [internal/service/orders.go:322](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/orders.go#L322) · [internal/service/usage_orders.go:39](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/usage_orders.go#L39) · [internal/http/routes/orders.go:35](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/orders.go#L35)

**Overlap review:** See D1 in [Duplication and overlap](#duplication-and-overlap): share the payment-action parser and status interpretation while retaining the distinct resource workflows.


### 017. `POST /v1/me/orders/{id}/confirm`

**Keep · No change needed**

**Current:** The handler requires a customer passing the Present credential policy and owned order. Confirmation follows the existing attempt identity, reading Solana/hosted/Stripe or NMI evidence rather than trusting a browser result. A settled or actionless order is a no-op read; a failed attempt becomes the standard 402 order response.

**Our recommendation:** Keep this explicit reconciliation command without a new charge key or client-supplied success assertion. Document that a 200 can still carry processing or requires_action.

**Why / tradeoff:** The command can be safely repeated to converge an existing payment and cannot silently select another payment instrument. The resource state, not the HTTP success code, is the completion signal; that distinction already fits OrderCheckout’s handling of asynchronous payment.

**Code:** [internal/http/handlers/orders.go:188](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/orders.go#L188) · [internal/service/orders.go:403](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/orders.go#L403) · [internal/modules/checkout/order_pay.go:620](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/checkout/order_pay.go#L620) · [sdk/billing-ui/src/order-checkout.tsx:126](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/sdk/billing-ui/src/order-checkout.tsx#L126) · [internal/http/routes/orders.go:37](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/orders.go#L37)

**Overlap review:** See D1 in [Duplication and overlap](#duplication-and-overlap): share the payment-action parser and status interpretation while retaining the distinct resource workflows.


### 018. `POST /v1/me/orders/{id}/cancel`

**Redesign · High priority**

**Current:** The service checks ownership and open status, but close rejects only subscription-origin orders. An open usage-origin order therefore reaches CloseOrder, becomes canceled, releases claims and emits an event without the money module’s debt transition. SQL and migration constraints permit this; collection and repayment queries exclude canceled usage orders.

**Our recommendation:** Restrict customer cancellation to purchase origins customer/merchant at the order domain and SQL mutation boundary. Keep usage debt changes behind the existing merchant money actions, and add a focused test that an owned open usage order is refused.

**Why / tradeoff:** This is a source-confirmed action-eligibility defect: an authenticated owner can cancel an open usage order through a purchase-style transition, causing collection/repayment queries to exclude it without the money module’s debt adjustment. Ownership checks still work; this is not cross-customer access or a demonstrated general authorization bypass.

**Uncertainty:** Source-confirmed path and constraints; no database scenario was executed, and production occurrence is unknown.

**Code:** [internal/http/handlers/orders.go:207](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/orders.go#L207) · [internal/modules/orders/create.go:329](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/orders/create.go#L329) · [internal/db/queries/orders.sql:181](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/db/queries/orders.sql#L181) · [internal/db/queries/usage_orders.sql:291](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/db/queries/usage_orders.sql#L291) · [internal/http/routes/orders.go:39](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/orders.go#L39)

### 019. `PUT /v1/me/default-payment-methods/{currency}`

**Refine · High priority**

**Current:** The self-customer handler validates currency/card and invokes SetDefaultPaymentMethod. It validates followers, can perform customer-present recurring verification, commits default/mandates plus durable NMI swap intents, and returns 200 even when inline swap execution fails and remains pending. The handler does not require scope.Present.

**Our recommendation:** Make the credential policy for establishing a new customer-initiated mandate explicit and consistent with payment authentication. Return the committed default together with pending follower migrations or an operation reference, rather than presenting the entire multi-subscription move as one completed assignment.

**Why / tradeoff:** Ownership is enforced, but self API-key and signed-token credentials are not excluded here as they are by the existing Present policy. That policy itself admits device keys and does not require a recent sign-in. The response should distinguish a saved preference from provider convergence. Changing admitted credentials may affect automation; adding state fields is backward-compatible.

**Code:** [internal/http/handlers/payment_methods.go:369](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/payment_methods.go#L369) · [internal/modules/subscriptions/default_payment_method.go:172](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/subscriptions/default_payment_method.go#L172) · [internal/app/recurring_verifier.go:23](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/app/recurring_verifier.go#L23) · [internal/http/routes/gate_test.go:412](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/gate_test.go#L412) · [internal/billingauth/contract.go:64](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/billingauth/contract.go#L64) · [internal/http/routes/payment_methods.go:15](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/payment_methods.go#L15)

### 020. `GET /v1/me/payment-methods`

**Refine · Low priority**

**Current:** The scoped cursor list enriches each card with expiry/latest-charge health, mandates, subscriptions and default currencies. Hosted checkout further filters to reusable saved cards after paging. Setup next_action is only attached by save/confirm, so a listed requires_action method has no resumable action in this response.

**Our recommendation:** Keep the bounded card list and the hosted checkout filter, but document its summary nature and provide a read-only detail/action retrieval path for a requires_action card. Distinguish an unpopulated action from a card needing no action.

**Why / tradeoff:** A person reopening account settings can recover a pending save without knowing that POST confirm doubles as action retrieval. This is a usability refinement; existing confirm can already resume it. A new read endpoint should state its secret-specific credential policy.

**Code:** [internal/http/handlers/payment_methods.go:78](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/payment_methods.go#L78) · [internal/http/handlers/payment_methods.go:420](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/payment_methods.go#L420) · [billing/payment_methods.go:42](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/billing/payment_methods.go#L42) · [internal/modules/checkout/stripe_method_setup.go:157](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/checkout/stripe_method_setup.go#L157) · [internal/http/routes/payment_methods.go:17](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/payment_methods.go#L17)

### 021. `POST /v1/me/payment-methods`

**Refine · Medium priority**

**Current:** The owned handler accepts a PSP token or admitted server-entry card, with card-abuse checks. Stripe derives a stable method/setup identity from the same token. NMI token entry creates a new vault; raw-card entry uses a durable vault intent. Both NMI entry paths verify before saving locally and attempt remote cleanup if verification or persistence fails. The public save route has no client-keyed replay contract, and an ambiguous result asks callers to refresh the list.

**Our recommendation:** Keep standalone saving and first align its documented retry promise with the implemented rail behavior. Preserve Stripe’s same-token continuation and NMI’s existing cleanup/intent machinery. If account-settings callers need exact replay after a lost NMI response, extend that existing machinery with a scoped client key or recovery receipt; an empty local list is not proof that no remote vault was created. Review consent-credential admission separately as a product policy.

**Why / tradeoff:** The remaining issue is reliable public retry identity, not absence of every recovery mechanism or demonstrated duplicate charging. A client key could reduce repeated remote saves, but adds persistence and compatibility work. Best-effort cleanup meaningfully limits fallout without guaranteeing success; no live duplicate or orphan scenario was reproduced.

**Code:** [internal/http/handlers/payment_methods.go:102](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/payment_methods.go#L102) · [internal/modules/paymentmethods/rail_payment_method_service.go:250](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/paymentmethods/rail_payment_method_service.go#L250) · [internal/modules/checkout/stripe_method_setup.go:86](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/checkout/stripe_method_setup.go#L86) · [internal/http/handlers/payment_methods.go:281](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/payment_methods.go#L281) · [internal/billingauth/contract.go:64](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/billingauth/contract.go#L64) · [internal/http/routes/payment_methods.go:19](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/payment_methods.go#L19) · [internal/modules/paymentmethods/rail_payment_method_service.go:250](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/modules/paymentmethods/rail_payment_method_service.go#L250) · [internal/modules/paymentmethods/rail_payment_method_service.go:425](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/modules/paymentmethods/rail_payment_method_service.go#L425) · [docs/frontend-integration.md:319](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/docs/frontend-integration.md#L319)

**Overlap review:** See D1 in [Duplication and overlap](#duplication-and-overlap): share the payment-action parser and status interpretation while retaining the distinct resource workflows.


### 022. `PATCH /v1/me/payment-methods/{id}`

**Redesign · High priority**

**Current:** The handler checks ownership through the service and validates expiry pairs. EditPaymentMethod pushes edits to the provider before a local transaction. pushCardEdit wraps every NMI/Stripe update error as ErrPaymentMethodEditRefused with the underlying error rendered via %v; the HTTP branch then says the card is unchanged, including for an uncertain transport result.

**Our recommendation:** Preserve typed ambiguous provider errors, expose an unresolved update state and reconcile the existing remote card before reporting refusal or completion. Define the local-versus-provider recovery path when the remote write succeeds but local persistence fails.

**Why / tradeoff:** An expiry/address update should not falsely promise unchanged provider state. A durable update or reconciliation marker adds implementation work, but gives retries and account screens an honest outcome. Keep the existing narrow edit fields; no general payment-method redesign is needed.

**Code:** [internal/http/handlers/payment_method_edit.go:25](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/payment_method_edit.go#L25) · [internal/service/payment_method_edit.go:42](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/payment_method_edit.go#L42) · [internal/service/payment_method_edit.go:120](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/payment_method_edit.go#L120) · [internal/http/handlers/payment_method_edit.go:78](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/payment_method_edit.go#L78) · [internal/http/routes/payment_methods.go:21](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/payment_methods.go#L21)

### 023. `DELETE /v1/me/payment-methods/{id}`

**Refine · High priority**

**Current:** Ownership is checked and live subscription use/unsafe deletion are refused. Successful convergence returns 204; a durable deletion still converging returns an empty 202. The SDK correctly maps 202 to pending, but usePaymentMethods.remove ignores that value, removes the row and emits payment_method.removed immediately.

**Our recommendation:** Honor pending in the shipped hook: retain a deleting state and refresh until removal is confirmed. Add a deletion operation/status link or an explicit card deletion status so consumers can observe eventual failure as well as completion.

**Why / tradeoff:** The HTTP distinction is already sound; the end-to-end contract loses it in the caller. Fixing the hook is a narrow first step. A richer status response is additive but must not suggest that a 202 guarantees provider deletion.

**Code:** [internal/http/handlers/payment_methods.go:299](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/payment_methods.go#L299) · [internal/http/handlers/payment_methods.go:332](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/payment_methods.go#L332) · [sdk/billing-ui/src/client/client.ts:624](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/sdk/billing-ui/src/client/client.ts#L624) · [sdk/billing-ui/src/react/hooks.ts:308](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/sdk/billing-ui/src/react/hooks.ts#L308) · [internal/http/routes/payment_methods.go:23](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/payment_methods.go#L23)

### 024. `POST /v1/me/payment-methods/{id}/confirm`

**Refine · Medium priority**

**Current:** The handler accepts an authenticated self customer, checks the card belongs to that customer in ConfirmStripeCard, then reads the original SetupIntent. It returns the card, its next action while waiting, or records setup success; unsuccessful setup is abandoned. Unlike payment-operation authentication, this path does not require the Present credential policy before exposing its client secret.

**Our recommendation:** Apply a consistent, explicit credential policy when returning a setup client secret. Reusing payment authentication’s Present check would exclude API-key and signed-token credentials but still admit device keys; a fresh-sign-in policy would be a separate product decision. Keep provider-truth confirmation and harmless rereads of already-active cards.

**Why / tradeoff:** The ownership check prevents cross-customer access, but the self-customer gate admits credential kinds the payment-authentication endpoint excludes. Secret issuance should have an intentional contract rather than depend on the UI being the usual caller. Automation that only reads status can use a secret-free read; tightening credential admission requires caller migration.

**Code:** [internal/http/handlers/payment_methods.go:228](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/payment_methods.go#L228) · [internal/modules/checkout/stripe_method_setup.go:157](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/checkout/stripe_method_setup.go#L157) · [internal/modules/checkout/stripe_method_setup.go:226](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/checkout/stripe_method_setup.go#L226) · [internal/modules/checkout/stripe_engine_authentication.go:24](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/checkout/stripe_engine_authentication.go#L24) · [internal/billingauth/contract.go:64](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/billingauth/contract.go#L64) · [internal/http/routes/payment_methods.go:25](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/payment_methods.go#L25)

**Overlap review:** See D1 in [Duplication and overlap](#duplication-and-overlap): share the payment-action parser and status interpretation while retaining the distinct resource workflows.


### 025. `POST /v1/me/stripe/billing-portal-sessions`

**Refine · Low priority**

**Current:** The feature-gated route verifies a customer, checks an armed Stripe configuration, resolves its Stripe customer and creates a portal session. Return destinations are origin-allowlisted, but the path is always /account, derived from the request origin or first configured allowed origin.

**Our recommendation:** Keep the explicitly Stripe-named capability. Allow a configured account return path, or a validated return URL constrained to the existing allowlist, instead of assuming every host’s account screen is /account.

**Why / tradeoff:** The provider-specific resource is accurate and the allowlist prevents arbitrary destinations. A configurable path supports embedded hosts with different navigation; adding a free-form redirect without the current origin checks would lose that protection.

**Code:** [internal/http/handlers/stripe_portal.go:23](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/stripe_portal.go#L23) · [internal/http/handlers/stripe_portal.go:74](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/stripe_portal.go#L74) · [internal/modules/subscriptions/stripe_portal.go:27](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/subscriptions/stripe_portal.go#L27) · [internal/http/routes/payment_methods.go:27](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/payment_methods.go#L27)

### 026. `GET /v1/me/payment-operations/{id}/authentication`

**Refine · Medium priority**

**Current:** The service validates the payer under the existing Present credential policy, merchant binding and immutable operation ownership before resolving Stripe credentials. Terminal operations return status only; unresolved ones recover the original provider intent reference and obtain its client secret. The handler sends no-store.

**Our recommendation:** Keep this privileged authentication read, and add a separate owner-scoped, secret-free GET /v1/me/payment-operations/{id} for uniform status across operation kinds. Make provider-specific authentication payloads explicitly tagged.

**Why / tradeoff:** Orders and subscriptions already expose recovery and replay can resume work, so the missing read is not a demonstrated unrecoverable-payment defect. It would simplify polling without requesting secrets or replaying a mutation. A new read requires an explicit customer-safe projection of each supported operation kind.

**Code:** [internal/http/handlers/stripe_engine_authentication.go:12](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/stripe_engine_authentication.go#L12) · [internal/modules/checkout/stripe_engine_authentication.go:24](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/checkout/stripe_engine_authentication.go#L24) · [internal/modules/checkout/stripe_engine_authentication.go:49](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/checkout/stripe_engine_authentication.go#L49) · [internal/service/customer_payment.go:78](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/customer_payment.go#L78) · [internal/http/routes/payments.go:36](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/payments.go#L36)

**Overlap review:** See D1 in [Duplication and overlap](#duplication-and-overlap): share the payment-action parser and status interpretation while retaining the distinct resource workflows.


### 027. `POST /v1/me/payment-operations/{id}/authentication/confirm`

**Refine · Medium priority**

**Current:** The same Present-policy and ownership gate selects the immutable operation and VerifyByID drives provider reconciliation. The response is PaymentOperation with current status, not a browser-supplied success result. The billing UI client currently discards this response and returns Promise<void>.

**Our recommendation:** Keep confirmation as a repeatable command on the existing operation, but have the SDK return the operation it receives and use a secret-free status read for subsequent polling.

**Why / tradeoff:** This preserves the strong provider-truth boundary while letting a caller distinguish completed, failed and still-unresolved confirmation immediately. Returning the body from the SDK is additive; a uniform read avoids conflating a successful HTTP request with a settled payment.

**Code:** [internal/modules/checkout/stripe_engine_authentication.go:95](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/checkout/stripe_engine_authentication.go#L95) · [internal/http/handlers/stripe_engine_authentication.go:36](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/stripe_engine_authentication.go#L36) · [sdk/billing-ui/src/client/client.ts:615](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/sdk/billing-ui/src/client/client.ts#L615) · [internal/http/routes/payments.go:38](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/payments.go#L38)

**Overlap review:** See D1 in [Duplication and overlap](#duplication-and-overlap): share the payment-action parser and status interpretation while retaining the distinct resource workflows.


### 028. `GET /v1/me/payments`

**Refine · Low priority**

**Current:** The handler forces customer ownership, validates status/kind and typed order/subscription IDs, then pages newest-first payments with refund totals. The underlying repository applies merchant/customer predicates. The billing-ui client exposes only rail plus cursor options even though the route supports richer filters.

**Our recommendation:** Keep this financial history collection, and expose the existing order_id, subscription_id, status and kind filters in the customer SDK rather than inventing extra endpoints for filtered histories.

**Why / tradeoff:** Customers can inspect a particular order’s money movement without downloading unrelated pages or building their own HTTP layer. This is a small SDK parity refinement; do not turn payment history into an operation-polling API, since pending intent state is a different concern.

**Code:** [internal/http/handlers/admin_payments.go:447](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_payments.go#L447) · [internal/http/handlers/admin_payments.go:499](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_payments.go#L499) · [internal/modules/payments/payment_repo.go:545](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/payments/payment_repo.go#L545) · [sdk/billing-ui/src/client/client.ts:657](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/sdk/billing-ui/src/client/client.ts#L657) · [internal/http/routes/payments.go:40](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/payments.go#L40)

### 029. `POST /v1/me/subscriptions/{id}/cancel`

**Refine · Medium priority**

**Current:** The handler requires an owned subscription and a 4–500-character reason. Card rails cancel inline or record durable provider cancellation; Solana first returns transactions, then accepts a signature that the service verifies. The result remains a subscription with optional next_action and access/recovery state.

**Our recommendation:** Keep the lifecycle command and provider-specific next action, but make cancellation feedback optional or accept a structured reason with optional text. Clarify that accepted scheduled cancellation can still have provider convergence outstanding.

**Why / tradeoff:** Forcing prose couples a core account control to a merchant feedback policy and complicates accessible or automated cancellation clients. Optional feedback loses some analytics but preserves the customer’s ability to cancel. Do not replace the deliberate end-of-period behavior with immediate access revocation.

**Code:** [internal/http/handlers/subscription_lifecycle.go:45](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/subscription_lifecycle.go#L45) · [internal/http/handlers/subscription_lifecycle.go:108](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/subscription_lifecycle.go#L108) · [internal/modules/subscriptions/user_service.go:265](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/subscriptions/user_service.go#L265) · [internal/http/routes/subscriptions.go:41](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/subscriptions.go#L41)

### 030. `POST /v1/me/subscriptions/{id}/resume`

**Refine · Low priority**

**Current:** The customer must own the subscription and Resumable must approve its rail/status/window. Stripe can update its provider then local lifecycle; NMI supersedes deferred deletion and restores local subscription/access. Calling resume again after success currently returns invalid_param because the subscription is no longer canceled.

**Our recommendation:** Keep the restricted resume operation, but treat an already-resumed matching subscription as a successful no-op, or return a dedicated already_active code with its current resource.

**Why / tradeoff:** A lost success response should not make a safe retry look like a new failure. This change needs a precise boundary: it must not reactivate an expired, terminal or differently canceled subscription. The existing provider/window checks should remain authoritative.

**Code:** [internal/http/handlers/subscription_lifecycle.go:139](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/subscription_lifecycle.go#L139) · [internal/http/handlers/subscription_lifecycle.go:190](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/subscription_lifecycle.go#L190) · [internal/http/handlers/subscription_lifecycle.go:237](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/subscription_lifecycle.go#L237) · [internal/http/routes/subscriptions.go:43](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/subscriptions.go#L43)

### 031. `PUT /v1/me/subscriptions/{id}/payment-method`

**Refine · High priority**

**Current:** The route owns-checks the subscription; null clears its explicit card. Engine and NMI paths verify the target’s custody/account and may create recurring lineage. NMI changes use a durable swap intent, but unresolved work returns generic resource_conflict rather than the changed resource or operation; no Present credential-policy check precedes customer-present verification.

**Our recommendation:** Define consent credential admission explicitly and consistently with payment authentication, and return a specific pending swap state/reference for accepted unresolved work. The current Present policy excludes API-key and signed-token credentials, admits device keys, and is distinct from recent sign-in. Preserve null as clear-to-default and the strict provider-account checks.

**Why / tradeoff:** Callers need to distinguish a rejected choice from an accepted swap still converging, especially after a timeout. A structured pending result makes retries safer without weakening rail custody rules. Existing clients treating every 409 as refusal will need a documented transition.

**Code:** [internal/http/handlers/update_subscription_payment_method.go:37](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/update_subscription_payment_method.go#L37) · [internal/http/handlers/update_subscription_payment_method.go:166](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/update_subscription_payment_method.go#L166) · [internal/http/handlers/update_subscription_payment_method.go:194](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/update_subscription_payment_method.go#L194) · [internal/app/recurring_verifier.go:23](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/app/recurring_verifier.go#L23) · [internal/billingauth/contract.go:64](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/billingauth/contract.go#L64) · [internal/http/routes/subscriptions.go:45](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/subscriptions.go#L45)

### 032. `GET /v1/me/subscriptions`

**Refine · Low priority**

**Current:** The customer gate fixes identity; status or all filters a cursor page. UserSubscriptionService batch-enriches plans, products and live access windows, and response helpers add mandates. The handler accepts any other status string, which reaches repository filtering rather than being rejected as a misspelled enum.

**Our recommendation:** Validate status against the subscription statuses plus the intentional all alias, and expose that vocabulary in generated query schemas. Keep the batched enrichment and bounded page.

**Why / tradeoff:** An empty result should mean no matching subscriptions, not silently mask a spelling mistake in a commonly used filter. This is a small behavior tightening for previously invalid query values; valid statuses and existing page shape can stay unchanged.

**Code:** [internal/http/handlers/user_subscriptions.go:18](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/user_subscriptions.go#L18) · [internal/modules/subscriptions/user_service.go:174](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/subscriptions/user_service.go#L174) · [internal/modules/subscriptions/subscription_repo.go:649](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/subscriptions/subscription_repo.go#L649) · [internal/http/routes/subscriptions.go:47](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/subscriptions.go#L47)

**Overlap review:** See D1 in [Duplication and overlap](#duplication-and-overlap): share the payment-action parser and status interpretation while retaining the distinct resource workflows.


### 033. `GET /v1/me/subscriptions/{id}`

**Keep · No change needed**

**Current:** ownSubscription hides another customer’s subscription as missing. The detail read includes the current/scheduled plan, independently derived access, mandates and PaymentRecovery. Engine renewals point to their order; provider-scheduled subscriptions expose retryability, blocked reason and an unresolved operation where relevant.

**Our recommendation:** Keep this resource-oriented recovery read and its distinction between billing state and access. Make its order-based recovery instructions the primary path for engine renewals.

**Why / tradeoff:** The current detail already answers how a customer should recover a missed renewal, so a generic operation read would supplement it rather than replace a missing workflow. Avoid embedding provider secrets or re-running a charge during a normal subscription read.

**Code:** [internal/http/handlers/subscription_lifecycle.go:291](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/subscription_lifecycle.go#L291) · [internal/http/handlers/subscription_lifecycle.go:331](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/subscription_lifecycle.go#L331) · [internal/service/customer_payment.go:78](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/customer_payment.go#L78) · [internal/modules/subscriptions/user_service.go:195](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/subscriptions/user_service.go#L195) · [internal/http/routes/subscriptions.go:49](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/subscriptions.go#L49)

**Overlap review:** See D1 in [Duplication and overlap](#duplication-and-overlap): share the payment-action parser and status interpretation while retaining the distinct resource workflows.


### 034. `POST /v1/me/subscriptions/{id}/retry-now`

**Refine · Medium priority**

**Current:** The handler requires the Present credential policy and an Idempotency-Key, but its route declaration does not mark that header as required. The service refuses engine-owned subscriptions in favor of their renewal order, enqueues/replays a provider rebill and returns subscription plus operation, with 202 while unresolved.

**Our recommendation:** Keep this provider-scheduled recovery command and declare its required idempotency header in the route contract. Document the existing subscription GET recovery.operation status and exact-key replay before adding a generic operation read. Such a read is an optional convenience if callers need to track a completed historical operation independently of the subscription. Direct engine renewals to recovery.order_id.

**Why / tradeoff:** The narrowed responsibility is coherent after renewals became orders. Making the header and applicability machine-readable prevents clients discovering requirements only through runtime errors. Do not unify it by reintroducing a second payment path for engine renewals.

**Code:** [internal/http/routes/subscriptions.go:51](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/subscriptions.go#L51) · [internal/http/handlers/customer_payment.go:55](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/customer_payment.go#L55) · [internal/service/customer_payment.go:30](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/customer_payment.go#L30) · [internal/service/customer_payment.go:78](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/service/customer_payment.go#L78)

### 035. `POST /v1/me/subscriptions/{id}/change`

**Refine · Medium priority**

**Current:** The handler requires a customer passing the Present credential policy; service resolution enforces ownership and compatible targets. Immediate durable changes return processing, action-required or successful results and can be recovered by key replay. Stripe provider-change responses retain the locally estimated AmountDueNow after success, as the Go contract explicitly documents; unlike preview, the final JSON has no is_estimate field.

**Our recommendation:** Keep the durable command and its documented estimate semantics. Make that distinction machine-readable in the final wire response, for example with an estimate qualifier; expose a confirmed amount only when supported by matched provider evidence. A uniform operation-status read is optional because keyed replay and existing resource reads already provide recovery.

**Why / tradeoff:** A wire qualifier would help clients avoid treating successful completion as proof that amount_due_now is a receipt amount. This is a contract-clarity improvement over an already documented estimate, not evidence that the service charged the wrong amount. Add fields compatibly before renaming amount_due_now; replay remains valid and may resume work.

**Code:** [internal/http/handlers/subscription_change.go:71](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/subscription_change.go#L71) · [internal/modules/checkout/subscription_change.go:107](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/checkout/subscription_change.go#L107) · [internal/modules/checkout/stripe_tier_change_intent.go:765](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/checkout/stripe_tier_change_intent.go#L765) · [billing/subscription_change.go:22](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/billing/subscription_change.go#L22) · [internal/http/routes/subscriptions.go:53](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/subscriptions.go#L53) · [billing/subscription_change.go:37](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/billing/subscription_change.go#L37)

**Overlap review:** See D1 in [Duplication and overlap](#duplication-and-overlap): share the payment-action parser and status interpretation while retaining the distinct resource workflows.


### 036. `POST /v1/me/subscriptions/{id}/change/preview`

**Refine · Medium priority**

**Current:** The preview checks owned subscription and target eligibility using the same resolution as change, without mutating it. It returns effective timing, due-now/next-renewal amounts and is_estimate for Stripe provider upgrades. The human message nevertheless says “You’ll be charged” even when that estimate flag is true.

**Our recommendation:** Keep the side-effect-free preview, but make estimate wording agree with the structured flag and carry quote assumptions or an accepted maximum into the eventual charge where the customer requires a firm bound.

**Why / tradeoff:** The preview is useful and more honest than the final change result today. Clear estimate wording is a small refinement; a binding quote or amount ceiling is a larger product choice because proration and provider-finalized amounts can move between preview and commit.

**Code:** [internal/http/handlers/subscription_change.go:124](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/subscription_change.go#L124) · [internal/modules/checkout/subscription_change.go:217](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/checkout/subscription_change.go#L217) · [internal/modules/checkout/subscription_change.go:280](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/checkout/subscription_change.go#L280) · [billing/subscription_change.go:55](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/billing/subscription_change.go#L55) · [internal/http/routes/subscriptions.go:55](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/subscriptions.go#L55)

## Application routes — 17

### 037. `GET /v1/app/catalog/products`

**Refine · Low priority**

**Current:** An application with Offers permission searches saleable products by keys or entitlement, with paging. It shares the public offer projection rather than the editable admin representation.

**Our recommendation:** Keep the audience split and shared offer view. The SDK already describes the keys-or-entitlement requirement; carry that constraint into generated validation metadata and group this beside app order preview/create in documentation.

**Why / tradeoff:** The boundary expresses what the backend is buying without granting catalog editing. Documentation organization is sufficient here; a URL rename would add migration work without improving the workflow.

**Code:** [internal/http/handlers/catalog.go:42](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/catalog.go#L42) · [internal/http/routes/catalog.go:33](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/catalog.go#L33) · [resources_catalog.go:47](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/resources_catalog.go#L47)

### 038. `GET /v1/app/customers`

**Keep · No change needed**

**Current:** Applications with Customers permission can resolve a required network_account_id into the linked customer in the current merchant. Despite the collection name and page shape, this lookup returns at most one customer.

**Our recommendation:** Keep this filtered lookup. The SDK already calls it ListNetworkCustomers, documents a page of at most one and requires network_account_id; the network guide describes the same contract. Generated required-query metadata can improve alongside recommendation 7, without a route or SDK rename.

**Why / tradeoff:** A collection-shaped filtered lookup is consistent with the existing client and is already clearly named and documented. General customer search would be a different product requirement.

**Code:** [internal/http/handlers/customer_network.go:74](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/customer_network.go#L74) · [internal/http/routes/customer_network.go:24](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/customer_network.go#L24) · [customers.go:27](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/customers.go#L27) · [docs/customer-network.md:100](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/docs/customer-network.md#L100)

### 039. `POST /v1/app/entitlements/check`

**Keep · No change needed**

**Current:** A read-level application permission gates exact entitlement checks and bounded prefix expansion for one customer. The response has parallel entitlements/quantities maps and held-prefix results, explicitly marking prefix truncation.

**Our recommendation:** Keep POST for the structured read, the parallel maps and explicit prefix truncation. One per-key object is an alternative design preference, not a demonstrated problem worth a breaking response change. Explain that this checks access, not spending capacity.

**Why / tradeoff:** The implementation populates both exact-key maps together, including false/null for keys not held. Preserve bounded queries and the application permission at the merchant. The named customer is the lookup target; the merchant helper does not enforce a separate per-customer application grant.

**Code:** [internal/modules/entitlements/resource_checks.go:234](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/entitlements/resource_checks.go#L234) · [internal/modules/entitlements/resource_checks.go:269](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/entitlements/resource_checks.go#L269) · [internal/http/routes/entitlements.go:14](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/entitlements.go#L14) · [internal/http/handlers/credits.go:25](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/http/handlers/credits.go#L25) · [internal/http/routes/gate.go:863](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/http/routes/gate.go#L863)

### 040. `POST /v1/app/admissions`

**Refine · Medium priority**

**Current:** Applications with Usage permission submit up to 1,000 independently evaluated items. Request IDs identify durable admissions; results preserve input order and carry per-item status, denial, or error while the HTTP envelope is 200. Estimated holds require an explicit deadline.

**Our recommendation:** Keep batch admission, explicit deadlines and ordered results. Add an HTTP example for retrying a failed subset under a new envelope key while retaining each logical request_id. Echoed item IDs and a permitted state read are optional recovery ergonomics; existing domain replay already protects repeated holds.

**Why / tradeoff:** The HTTP replay layer stores a 200 even when one item has a transient 500, so the same envelope returns the recorded item failure. Positional correlation already works; an identifier makes extracted/logged outcomes easier to interpret. A future read could reuse GetAdmission, but its absence does not make ordinary retries unrecoverable.

**Code:** [internal/http/handlers/admissions.go:118](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admissions.go#L118) · [internal/service/service.go:100](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/service.go#L100) · [internal/http/routes/app_requests.go:79](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/app_requests.go#L79) · [internal/http/routes/metering.go:20](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/metering.go#L20)

**Overlap review:** See D4 in [Duplication and overlap](#duplication-and-overlap): the batch item wire definitions can be shared while preserving distinct admission and error semantics.


### 041. `POST /v1/app/admissions/{request_id}/capture`

**Refine · High priority**

**Current:** Capture resolves the durable admission inside the merchant and requires the authorized merchant principal before returning a settlement receipt. Amount is explicitly required, including an intentional zero; the first capture fixes amount and optional usage terms.

**Our recommendation:** Keep this command and the explicit-zero contract. Bind HTTP replay identity to the concrete request_id. A receipt read is optional if callers need fresh observation beyond the existing replay contract.

**Why / tradeoff:** The domain already prevents changed capture terms from silently replaying. The outer HTTP fingerprint currently omits path values, so it can answer another admission’s receipt before the handler runs. Repair that shared boundary; do not replace capture with a generic PATCH.

**Code:** [internal/http/handlers/admissions.go:160](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admissions.go#L160) · [billing/admissions.go:151](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/billing/admissions.go#L151) · [internal/http/routes/app_requests.go:55](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/app_requests.go#L55) · [internal/http/routes/metering.go:22](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/metering.go#L22) · [internal/http/handlers/credits.go:25](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/http/handlers/credits.go#L25)

### 042. `POST /v1/app/admissions/release`

**Refine · Medium priority**

**Current:** Accepts request_ids and releases each authorized hold independently. Already released admissions can be returned again; captured admissions produce a per-item refusal. The response uses ordered items under HTTP 200.

**Our recommendation:** Keep batch release and ordered per-item outcomes. An echoed request_id on error results would improve standalone logs; document subset retries with a new envelope key. SDK callers can already distinguish success from refusal using each result.

**Why / tradeoff:** Independent cleanup is useful and should not become all-or-nothing. Input position already correlates the outcome, so identifiers are an additive convenience. The shared HTTP-envelope retry rule is the substantive clarification.

**Code:** [internal/http/handlers/admissions.go:232](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admissions.go#L232) · [billing/admissions.go:199](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/billing/admissions.go#L199) · [internal/http/routes/metering.go:25](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/metering.go#L25)

**Overlap review:** See D4 in [Duplication and overlap](#duplication-and-overlap): the batch item wire definitions can be shared while preserving distinct admission and error semantics.


### 043. `POST /v1/app/admissions/extend`

**Refine · Low priority**

**Current:** Accepts independent request_id/expires_at items, resolves each admission within the authorized merchant, and only moves open hold deadlines later. Expired, captured, or otherwise unavailable holds are refused rather than resurrected.

**Our recommendation:** Keep an absolute expires_at and the dedicated command. Link the existing documented maximum lifetime and no-shortening rules from the generated route reference; add an HTTP subset-retry example. Echoed item identifiers would be optional convenience.

**Why / tradeoff:** An absolute target is safer to retry than a relative extension. The lifecycle rules are already documented; the remaining suggestion is discoverability and the distinction between domain replay and HTTP envelope replay. Do not add unrestricted state edits or a job service.

**Code:** [internal/http/handlers/admissions.go:254](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admissions.go#L254) · [billing/admissions.go:187](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/billing/admissions.go#L187) · [internal/http/routes/metering.go:27](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/metering.go#L27) · [docs/admission-operations.md:16](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/docs/admission-operations.md#L16) · [internal/http/handlers/credits.go:25](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/http/handlers/credits.go#L25)

**Overlap review:** See D4 in [Duplication and overlap](#duplication-and-overlap): the batch item wire definitions can be shared while preserving distinct admission and error semantics.


### 044. `POST /v1/app/usage-events`

**Refine · Medium priority**

**Current:** Records up to 1,000 independent events with stable source/source_id/event_type identity. Successful zero-amount events select catalog pricing, positive amounts select host pricing, and failed events use host cost/grace logic. Amount is a decimal JSON string.

**Our recommendation:** If successful host-priced zero-cost events are a required integration use case, add an explicit catalog-versus-host pricing mode rather than inferring it from zero. Keep outcome separate. Optionally echo source identity on errors and document subset retries independently of HTTP envelope replay.

**Why / tradeoff:** The current zero/positive rule is deliberate and deterministic; no wrong charge is demonstrated here. An explicit mode would express a free host-priced success separately from an event rated by the catalog during ingestion. That is a versioned request-design proposal, not an urgent defect.

**Code:** [billing/usage.go:48](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/billing/usage.go#L48) · [internal/modules/money/usage.go:136](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/money/usage.go#L136) · [internal/http/handlers/admissions.go:299](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admissions.go#L299) · [internal/http/routes/metering.go:29](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/metering.go#L29)

### 045. `POST /v1/app/provider-operations`

**Refine · Medium priority**

**Current:** An application with Costs permission opens a durable capacity reservation using its operation_id and authorization evidence. New operations return 201; domain replays return the same operation representation. Humans do not gain this route merely by holding admin rights.

**Our recommendation:** Keep the broad provider-operations name and its app placement. Consider a narrow application read permission only if backend callers need fresh state without AdminRead; retain administrative investigation and closure. The Go DTO already specifies evidence byte bounds, which generated documentation should preserve.

**Why / tradeoff:** Programmatic writes and support intervention are different jobs. A backend may need fresh state without broad AdminRead, but if only operators should read evidence, return a reduced app status view. This is a permission/product decision, not proof that the existing split is wrong.

**Code:** [internal/http/routes/metering.go:33](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/metering.go#L33) · [internal/service/operation_authorization.go:63](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/operation_authorization.go#L63) · [billing/provider_obligations.go:85](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/billing/provider_obligations.go#L85)

### 046. `POST /v1/app/provider-operations/{operation_id}/increment`

**Refine · High priority**

**Current:** A Costs-authorized application requests hold growth with a contiguous ordinal, desired amount, and minimum acceptable amount. The returned operation includes last_increment.granted_amount, which can differ from the requested amount.

**Our recommendation:** Keep the bounded-growth command and ordinal identity. Fix concrete-path replay binding and emphasize granted_amount in the SDK/result documentation; expose fresh state under a narrow read permission if app recovery is intended.

**Why / tradeoff:** The caller must act on the actual grant, not the requested maximum. Renaming this to a generic update would hide that distinction. Concrete-path replay is a shared high-priority correction, while the permission addition is a separate design choice.

**Code:** [billing/provider_obligations.go:117](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/billing/provider_obligations.go#L117) · [internal/service/operation_authorization.go:104](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/operation_authorization.go#L104) · [internal/http/routes/app_requests.go:55](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/app_requests.go#L55) · [internal/http/routes/metering.go:41](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/metering.go#L41)

### 047. `POST /v1/app/provider-operations/{operation_id}/release`

**Refine · High priority**

**Current:** A Costs-authorized application supplies a release reference as proof that the upstream operation did not happen. Billing evidence or a refusal prevents ordinary release; manual resolution is a separate admin operation.

**Our recommendation:** Keep release distinct from administrative close. Bind replay to the concrete operation_id and document the evidence requirements plus the operator handoff when release is refused.

**Why / tradeoff:** This separation protects settlement responsibility and should survive any future expansion of provider operations. A generic DELETE would conceal why some holds need resolution rather than release. The HTTP path identity correction is shared.

**Code:** [billing/provider_obligations.go:138](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/billing/provider_obligations.go#L138) · [internal/service/operation_authorization.go:119](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/operation_authorization.go#L119) · [internal/http/routes/app_requests.go:55](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/app_requests.go#L55) · [internal/http/routes/metering.go:44](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/metering.go#L44)

### 048. `POST /v1/app/provider-operations/{operation_id}/observations`

**Refine · High priority**

**Current:** Appends immutable billing evidence identified by observation_id; changed terms on a replay are refused. The payload carries raw bytes, normalized query/lifecycle facts, and records or a refusal. OpenRails qualifies and settles inside the transaction; callers do not submit their own settlement amount.

**Our recommendation:** Keep observations as a subresource and keep rating ownership in OpenRails. Publish examples for evidence versus refusal and the possible qualification states. Fix the shared concrete-path replay boundary; consider a compact app status representation if evidence reads remain admin-only.

**Why / tradeoff:** This is a meaningful domain boundary, not needless route proliferation. Better schema alternatives and examples help clients build a valid observation without weakening settlement rules. Do not rename the whole resource around today’s cost-only implementation.

**Code:** [billing/provider_obligations.go:239](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/billing/provider_obligations.go#L239) · [internal/service/operation_authorization.go:136](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/operation_authorization.go#L136) · [internal/http/routes/app_requests.go:55](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/app_requests.go#L55) · [internal/http/routes/metering.go:47](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/metering.go#L47)

### 049. `GET /v1/app/host-events`

**Refine · Low priority**

**Current:** Events permission opens a cursor-paged merchant event feed, defaulting to unacknowledged entries, with type/payment filters and bounded ID lookup. Current master also includes balance.low and balance.depleted. Acknowledgement is merchant-wide, not an offset for each application consumer.

**Our recommendation:** Keep the feed and its existing durable-before-ack/redelivery guidance. Clarify that consumers of the same event share one acknowledgement; distinct type-filtered consumers are already supported. Only introduce consumer-specific delivery state if independent same-event fan-out is required.

**Why / tradeoff:** The docs already show durable processing before acknowledgement, and the SDK explains draining from the start rather than treating a cursor as a high-water mark. The narrower design question is independent delivery to multiple consumers of the same event, not missing basic delivery guidance.

**Code:** [internal/service/host_events.go:22](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/host_events.go#L22) · [internal/db/queries/host_events.sql:24](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/db/queries/host_events.sql#L24) · [internal/http/routes/ops.go:11](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/ops.go#L11) · [docs/embedded-integration.md:515](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/docs/embedded-integration.md#L515) · [billing/host_events.go:158](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/billing/host_events.go#L158)

**Overlap review:** See D4 in [Duplication and overlap](#duplication-and-overlap): reuse the entitlement-change payload across the receipt and host event; keep their envelopes and delivery contracts separate.


### 050. `POST /v1/app/host-events/acknowledge`

**Keep · No change needed**

**Current:** Accepts a bounded list of host_event_ids, acknowledges them merchant-wide, and returns an ID-keyed event lookup with null for absent IDs. Repeated acknowledgement preserves the stored delivery time.

**Our recommendation:** Keep the batch and ID-keyed response, including the documented null for a nonexistent event. Preserve the existing guidance to acknowledge only durably completed work and the shared merchant-level acknowledgement model.

**Why / tradeoff:** The shape already avoids positional ambiguity and supports repeated acknowledgement. Clear caller semantics are more valuable than changing this into per-event URLs or adding a second delivery system.

**Code:** [internal/service/host_events.go:186](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/host_events.go#L186) · [internal/http/handlers/service_host_events.go:57](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/service_host_events.go#L57) · [internal/http/routes/ops.go:13](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/ops.go#L13)

### 051. `POST /v1/app/orders/preview`

**Keep · No change needed**

**Current:** A read-level Orders permission previews typed lines for a specified customer through the same pricing/availability path as customer order preview. It does not create a durable order or collect money.

**Our recommendation:** Keep the preview alongside app order creation. Describe it as a current calculation, and keep final creation/payment validation authoritative rather than implying that preview reserves price or availability.

**Why / tradeoff:** Structured POST is reasonable for multi-line input, and the dedicated Orders permission fits this app workflow. A persisted quote is only needed if the product promises quote validity.

**Code:** [internal/http/routes/orders.go:48](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/orders.go#L48) · [internal/service/orders.go:729](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/orders.go#L729)

### 052. `POST /v1/app/orders`

**Refine · Medium priority**

**Current:** An Orders-authorized application creates an unpaid merchant-origin order for one customer, optionally attaching hosted checkout. The order has stable identity and expiry. Order/checkout storage retains the URL secret’s hash; HTTP replay separately retains the full successful response, including its URL.

**Our recommendation:** Keep the unified order resource. Consider narrowly permitted app reads for backend tracking. Document URL recovery through retained HTTP replay and define recovery/rotation for commit-before-response-record failure or later replay expiry.

**Why / tradeoff:** Ordinary lost responses already have a recovery path while the HTTP record exists. The narrower gap is that domain replay alone cannot reconstruct the secret. Any reissue policy should be deliberate; do not make checkout secrets generally readable merely to simplify reads.

**Code:** [internal/service/orders.go:739](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/orders.go#L739) · [internal/service/orders.go:836](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/orders.go#L836) · [internal/http/routes/app_requests.go:70](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/app_requests.go#L70) · [internal/http/routes/orders.go:50](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/orders.go#L50)

### 053. `POST /v1/app/orders/{id}/cancel`

**Refine · High priority**

**Current:** An Orders-authorized application withdraws an open merchant-created order and receives the order representation. This is distinct from admin debt-voiding and from customer-origin order cancellation.

**Our recommendation:** Keep this command and its origin restriction. Bind request replay to the concrete order ID, and document the terminal/in-progress refusals plus how to read the order after an uncertain response.

**Why / tradeoff:** Cancellation is a state transition with provider cleanup, not deletion of history. The route belongs in the backend integration surface; it should not inherit the broader abilities of an operator debt-voiding command.

**Code:** [internal/http/handlers/orders.go:383](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/orders.go#L383) · [internal/service/orders.go:866](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/orders.go#L866) · [internal/http/routes/app_requests.go:55](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/app_requests.go#L55) · [internal/http/routes/orders.go:53](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/orders.go#L53)

## Network routes — 15

### 054. `GET /v1/network/links`

**Keep · No change needed**

**Current:** A native self-acting network account lists its active customer links across merchants, with keyset paging and public merchant enrichment. Responses are no-store; merchant-bound application credentials do not enter this audience.

**Our recommendation:** Keep links as the authority boundary and keep the account-facing directory separate from /me. Document that a link identifies a merchant customer relationship, not a transferable merchant credential.

**Why / tradeoff:** The existing path models the cross-merchant account naturally. Flattening this into a merchant_id query would obscure the link grant and tempt callers to treat merchant selection as authorization.

**Code:** [internal/http/routes/gate.go:585](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/gate.go#L585) · [internal/http/handlers/network.go:52](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/network.go#L52) · [internal/http/routes/customer_network.go:40](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/customer_network.go#L40)

### 055. `GET /v1/network/subscriptions`

**Refine · Low priority**

**Current:** Lists subscriptions across the native account’s active links, using a compound cursor and per-merchant hydration. Each item includes its merchant and link context, and responses are no-store.

**Our recommendation:** Keep this account-wide overview and link context. If one merchant cannot be hydrated, define whether the portal intentionally fails the whole page or may show explicit partial availability; never silently drop that merchant.

**Why / tradeoff:** Cross-merchant views have a different failure boundary from a single-merchant list. The current whole-page failure is consistent and defensible; partial results should be an explicit product choice, not a generic empty fallback.

**Code:** [internal/http/handlers/network.go:115](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/network.go#L115) · [internal/http/routes/customer_network.go:42](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/customer_network.go#L42)

### 056. `GET /v1/network/orders`

**Keep · No change needed**

**Current:** Lists linked-customer orders across merchants with compound keyset pagination, link/merchant context and no-store. It is an overview; order actions run through a particular active link.

**Our recommendation:** Keep the overview/action split. Document that subsequent payment and confirmation use link_id from the row, and keep any future filters consistent with customer order filters rather than inventing a second status vocabulary.

**Why / tradeoff:** An account-wide collection makes the portal useful without broad merchant permissions. No evidence justifies collapsing it into /me or adding unrelated cross-merchant operations.

**Code:** [internal/http/handlers/network.go:171](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/network.go#L171) · [internal/http/routes/customer_network.go:38](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/customer_network.go#L38) · [internal/http/routes/customer_network.go:44](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/customer_network.go#L44)

### 057. `DELETE /v1/network/links/{link_id}`

**Refine · Low priority**

**Current:** A native account with recent sign-in can unlink its own relationship. Live subscriptions block unlinking. Successful unlinking revokes merchant-client consents and refresh tokens; it does not delete the merchant customer or their billing history, or establish immediate invalidation of every existing access token.

**Our recommendation:** Keep unlink distinct from canceling subscriptions and revoking payment authority. Give the in-use refusal a clear next action or summary of blockers so the account can resolve it without guessing.

**Why / tradeoff:** This is a sensible removal of an association. A destructive cascade would change business intent; an additive blocker response improves the existing flow while preserving retryable cleanup of auth grants.

**Code:** [internal/http/handlers/network.go:229](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/network.go#L229) · [server/internal/network/accounts.go:62](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/server/internal/network/accounts.go#L62) · [internal/http/routes/customer_network.go:46](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/customer_network.go#L46)

### 058. `POST /v1/network/links/{link_id}/revoke-authority`

**Keep · No route change needed**

**Current:** A recently signed-in native account acts through an active link to revoke that customer’s payment mandates. The receipt identifies revoked mandates; this does not itself cancel subscriptions or remove the link.

**Our recommendation:** Keep this separate command and its existing documented distinction from cancellation and unlinking. Reuse the network guide’s explanation in any portal confirmation: local mandates end while subscriptions and the link remain. A more prominent caller explanation is optional; the command does not cancel external provider schedules.

**Why / tradeoff:** Revoking local payment authority is distinct from ending a subscription or unlinking an account. Those consequences are already documented; no shipped network-portal misunderstanding was established. Do not add another lifecycle endpoint merely to repeat this explanation.

**Code:** [internal/http/handlers/network.go:249](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/network.go#L249) · [internal/http/routes/customer_network.go:48](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/customer_network.go#L48) · [docs/customer-network.md:155](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/docs/customer-network.md#L155)

### 059. `GET /v1/network/links/{link_id}/subscriptions/{id}`

**Keep · No change needed**

**Current:** Resolves an owned active link into merchant/customer scope, then runs the ordinary owned-subscription read. A foreign or inactive link is hidden as not found; it does not grant general merchant reads.

**Our recommendation:** Keep the shared handler and link-scoped URL. Ensure portal callers retain link_id alongside the subscription ID and use the same subscription model as /me.

**Why / tradeoff:** This reuses lifecycle semantics while changing the authorization entry point deliberately. A separate network subscription representation would create unnecessary drift.

**Code:** [internal/http/routes/gate.go:585](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/gate.go#L585) · [internal/http/handlers/user_subscriptions.go:46](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/user_subscriptions.go#L46) · [internal/http/routes/customer_network.go:53](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/customer_network.go#L53)

**Overlap review:** See D1 in [Duplication and overlap](#duplication-and-overlap): share the payment-action parser and status interpretation while retaining the distinct resource workflows.


### 060. `POST /v1/network/links/{link_id}/subscriptions/{id}/cancel`

**Refine · Medium priority**

**Current:** Requires native self-action, recent sign-in and an active owned link before ordinary period-end cancellation. A 4–500 character reason is mandatory; Solana can return next_action and require resubmission with a signature.

**Our recommendation:** Keep shared period-end semantics and the wallet continuation. Make free-text feedback optional or provide an explicit decline-to-answer choice, and expose pending provider cancellation distinctly from final completion.

**Why / tradeoff:** Ending a subscription should not depend on writing a sufficiently long explanation. Relaxing the feedback requirement is backward-compatible; preserving next_action avoids creating a separate cancellation API per rail.

**Code:** [internal/http/routes/gate.go:585](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/gate.go#L585) · [internal/http/handlers/subscription_lifecycle.go:45](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/subscription_lifecycle.go#L45) · [internal/http/routes/customer_network.go:53](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/customer_network.go#L53)

### 061. `POST /v1/network/links/{link_id}/subscriptions/{id}/resume`

**Refine · Low priority**

**Current:** With recent sign-in and an active link, undoes an owned scheduled cancellation only while the subscription and rail remain resumable. It returns the current subscription.

**Our recommendation:** Keep the command; publish distinct machine-readable refusal reasons for unsupported rail, elapsed period, and wrong state instead of relying on different invalid_param messages.

**Why / tradeoff:** The portal can then present a useful next step without parsing English. This is an additive error-contract improvement and should be shared with /me rather than implemented independently for network accounts.

**Code:** [internal/http/routes/gate.go:585](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/gate.go#L585) · [internal/http/handlers/subscription_lifecycle.go:190](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/subscription_lifecycle.go#L190) · [internal/http/routes/customer_network.go:53](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/customer_network.go#L53)

### 062. `PUT /v1/network/links/{link_id}/subscriptions/{id}/payment-method`

**Refine · Medium priority**

**Current:** A recent native sign-in and active link establish the owned customer. The body selects a saved payment method; null follows the customer default. Provider-backed moves can continue through durable intents beyond the initial request.

**Our recommendation:** Keep PUT on the subscription’s payment-method selection and preserve null-as-follow-default. Return an explicit pending move/state reference when work continues, instead of making the caller infer it from a generic conflict.

**Why / tradeoff:** The requested selection and the provider’s confirmed selection are not always simultaneous. Modeling both avoids a UI that claims either success or failure prematurely; use the existing move intent rather than inventing another job system.

**Code:** [internal/http/routes/gate.go:585](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/gate.go#L585) · [internal/http/handlers/update_subscription_payment_method.go:37](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/update_subscription_payment_method.go#L37) · [internal/http/routes/customer_network.go:53](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/customer_network.go#L53)

### 063. `GET /v1/network/links/{link_id}/payment-methods`

**Keep · No change needed**

**Current:** Uses the active link’s customer identity to return a cursor page of that merchant customer’s saved methods. It does not build a global wallet spanning all linked merchants.

**Our recommendation:** Keep methods scoped to the link and reuse the customer payment-method view. Make the merchant context visible in the portal so a card’s availability is not mistaken for permission to use it at another merchant.

**Why / tradeoff:** A shared network login is not a shared PSP vault or payment mandate. The current scoped collection correctly preserves that distinction, with no reason for a route rename.

**Code:** [internal/http/routes/gate.go:585](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/gate.go#L585) · [internal/http/handlers/payment_methods.go:40](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/payment_methods.go#L40) · [internal/http/routes/customer_network.go:53](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/customer_network.go#L53)

### 064. `POST /v1/network/links/{link_id}/payment-methods`

**Refine · Medium priority**

**Current:** Recent native sign-in and the active link admit a saved-card creation for that merchant customer. Input names a PSP and either a token or admitted raw-card data; rail-specific validation and unknown-provider-outcome handling remain in the shared handler.

**Our recommendation:** Keep creation link-scoped and add the owned payment-method confirm continuation to the network route family. Until then, document the existing Stripe continuation by repeating POST with the same retained token. Express token-versus-card alternatives in the generated schema.

**Why / tradeoff:** Stripe save is possible today, but a reloaded portal holding only the method ID lacks the /me confirm equivalent: list returns no setup action and does not reconcile it. This is a specific workflow gap, not a reason to copy every customer route into network. Retain recent-sign-in checks.

**Code:** [internal/http/routes/gate.go:585](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/gate.go#L585) · [internal/http/handlers/payment_methods.go:102](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/payment_methods.go#L102) · [internal/modules/checkout/stripe_method_setup.go:92](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/checkout/stripe_method_setup.go#L92) · [internal/http/routes/customer_network.go:53](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/customer_network.go#L53)

**Overlap review:** See D1 in [Duplication and overlap](#duplication-and-overlap): share the payment-action parser and status interpretation while retaining the distinct resource workflows.


### 065. `DELETE /v1/network/links/{link_id}/payment-methods/{id}`

**Keep · No route change needed**

**Current:** Checks recent native sign-in, active-link ownership and method ownership. The shared delete answers 204 when finished, 202 when deletion is converging, and refuses methods that remain in use or unsafe to remove.

**Our recommendation:** Keep the ownership/in-use protections and existing 204/202 distinction. Callers should retain pending state after 202 and refresh the existing linked payment-method list, as the integration guide already recommends. Add a richer receipt or operation reference only if a concrete caller needs failure/history detail beyond that flow; no current network-portal misuse was verified.

**Why / tradeoff:** Accepted deletion is already observable through existing reads. The confirmed customer SDK hook issue in entry 023 does not establish a separate network-client defect. A richer server receipt is optional and need not precede correcting that hook.

**Code:** [internal/http/routes/gate.go:585](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/gate.go#L585) · [internal/http/handlers/payment_methods.go:332](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/payment_methods.go#L332) · [internal/http/routes/customer_network.go:53](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/customer_network.go#L53) · [docs/frontend-integration.md:324](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/docs/frontend-integration.md#L324)

### 066. `GET /v1/network/links/{link_id}/orders/{id}`

**Refine · Medium priority**

**Current:** Resolves an active owned link and invokes the normal customer order recovery read. The service controls what customer next_action is visible; the merchant/admin projection is not reused.

**Our recommendation:** Keep the recovery address, but apply the /me detail-read refinement: separate guaranteed local status from optional provider-backed action retrieval, and explain the latter’s interactive-presence requirement.

**Why / tradeoff:** The link identifies whose order can be read, while action visibility has its own rules. A Stripe action lookup can currently fail the whole detail read; shared secret-free status or explicit action-unavailable metadata would improve both audiences without exposing merchant-only data.

**Code:** [internal/http/routes/gate.go:585](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/gate.go#L585) · [internal/http/handlers/orders.go:226](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/orders.go#L226) · [internal/http/routes/customer_network.go:53](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/customer_network.go#L53)

### 067. `POST /v1/network/links/{link_id}/orders/{id}/pay`

**Refine · Low priority**

**Current:** Requires recent native sign-in, an active owned link and the shared Present credential policy. Payment uses a stable idempotency key and returns the order, including any next_action.

**Our recommendation:** Keep the single payment command across rails and use a discriminated request schema for the supported payment choices. Retain one payment-intent key through transport retries and recover through the linked order GET.

**Why / tradeoff:** The API should expose one order workflow while making invalid combinations hard to construct. This route requires both recent sign-in and the shared Present policy; preserve those gates rather than moving payment authority into the app surface for symmetry.

**Code:** [internal/http/routes/gate.go:585](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/gate.go#L585) · [internal/http/handlers/orders.go:157](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/orders.go#L157) · [internal/http/routes/customer_network.go:53](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/customer_network.go#L53)

**Overlap review:** See D1 in [Duplication and overlap](#duplication-and-overlap): share the payment-action parser and status interpretation while retaining the distinct resource workflows.


### 068. `POST /v1/network/links/{link_id}/orders/{id}/confirm`

**Keep · No change needed**

**Current:** After the wallet/provider action, a recently signed-in native account can ask OpenRails to recheck the linked customer order. The caller does not supply an authoritative paid flag; the service reads provider state.

**Our recommendation:** Keep the confirmation nudge separate from pay and keep the order response. Document that unchanged/pending is a valid outcome and that the client can subsequently poll the order read.

**Why / tradeoff:** Customer redirect completion is not proof of settlement. The current command expresses that correctly; inventing per-rail public confirmation routes would duplicate the lifecycle.

**Code:** [internal/http/routes/gate.go:585](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/gate.go#L585) · [internal/http/handlers/orders.go:188](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/orders.go#L188) · [internal/http/routes/customer_network.go:53](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/customer_network.go#L53)

**Overlap review:** See D1 in [Duplication and overlap](#duplication-and-overlap): share the payment-action parser and status interpretation while retaining the distinct resource workflows.


## Catalog routes — 24

### 069. `GET /v1/admin/catalog/revision`

**Keep · No change needed**

**Current:** Catalog permission admits people and applications. The handler reads the merchant-scoped revision and returns a small revision object; it performs no provider work.

**Our recommendation:** Keep this inexpensive change indicator and the separate object revisions used by edits. Describe the number as an observation, not a guarantee that separately fetched pages form one snapshot.

**Why / tradeoff:** Clients can detect changes without fetching the whole catalog. A separate snapshot protocol would add complexity without a demonstrated requirement for this route.

**Code:** [internal/http/handlers/catalog_application.go:60](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/catalog_application.go#L60) · [internal/service/catalog_transaction.go:107](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/catalog_transaction.go#L107) · [internal/http/routes/permissions.go:112](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/permissions.go#L112) · [internal/http/routes/catalog.go:36](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/catalog.go#L36)

### 070. `POST /v1/admin/catalog/applications`

**Refine · Medium priority**

**Current:** Catalog permission and recent human sign-in protect JSON/YAML application. Preparation verifies provider references without remote writes; one local transaction applies uncontested objects and reports skipped conflicts. Completed document hashes replay; conflicted documents remain retryable.

**Our recommendation:** Keep ownership, conflict receipts and content identity. If operators need to review pruning/force effects before applying, an optional read-only preview could reuse the planner; bind a reviewed plan to its base revision or clearly label it advisory. Existing apply callers need not acquire a second mandatory step.

**Why / tradeoff:** The UI currently reviews pasted text, explicitly not a database diff. Preview makes pruning and force decisions reviewable without changing intentional partial application. It must remain advisory or reject stale plans, and existing apply callers should continue working.

**Code:** [internal/service/catalog_application.go:104](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/catalog_application.go#L104) · [internal/service/catalog_apply_plan.go:39](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/catalog_apply_plan.go#L39) · [internal/service/catalog_application_prepare.go:45](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/catalog_application_prepare.go#L45) · [web/admin/src/pages/catalog/application-dialog.tsx:148](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/web/admin/src/pages/catalog/application-dialog.tsx#L148) · [internal/http/routes/catalog.go:38](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/catalog.go#L38)

**Overlap review:** See D2 in [Duplication and overlap](#duplication-and-overlap): duplicated proposal policy and price-request reconstruction have drifted; share policy/defaults and preserve price terms without merging creation and migration. See D4 in [Duplication and overlap](#duplication-and-overlap): reuse the entitlement-change payload across the receipt and host event; keep their envelopes and delivery contracts separate.


### 071. `GET /v1/admin/catalog/meters`

**Keep · No change needed**

**Current:** Catalog permission guards a keyset-paginated read ordered by meter key. Each item includes aggregation, dimensions, activity, override count, revision and the optional default rate card.

**Our recommendation:** Keep the combined meter projection and cursor pagination; the editor needs these fields together to understand which changes are safe.

**Why / tradeoff:** Embedding the default rate card avoids a discovery-and-join sequence and makes absent pricing explicit. Customer-specific overrides remain a separate collection because they have a different cardinality and ownership.

**Code:** [internal/http/handlers/admin_metering.go:15](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_metering.go#L15) · [internal/modules/money/metering_catalog.go:61](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/money/metering_catalog.go#L61) · [internal/modules/money/metering_catalog.go:194](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/money/metering_catalog.go#L194) · [internal/http/routes/catalog.go:41](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/catalog.go#L41)

### 072. `GET /v1/admin/catalog/meters/{key}`

**Keep · No change needed**

**Current:** Catalog permission guards the normalized key lookup. Missing meters return a specific not-found refusal; a present meter can legitimately have no rate card.

**Our recommendation:** Keep the natural-key address and the explicit optional rate card, revision and activity information.

**Why / tradeoff:** The same stable key is used in usage declarations and negotiated pricing, so a second opaque meter identifier would complicate callers. A missing default card should remain distinct from a missing meter.

**Code:** [internal/http/handlers/admin_metering.go:112](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_metering.go#L112) · [internal/modules/money/metering_catalog.go:98](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/money/metering_catalog.go#L98) · [internal/http/routes/catalog.go:43](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/catalog.go#L43)

### 073. `PUT /v1/admin/catalog/meters/{key}`

**Keep · No change needed**

**Current:** Catalog permission plus recent human sign-in protects an atomic meter/card edit with expected_revision. Meter fields are supplied as a definition, but omitted rate_card preserves it and null removes it. Recorded activity and dependent overrides constrain changes.

**Our recommendation:** Keep the atomic meter/card edit and its documented contract: the meter is a definition, while omitted rate_card keeps pricing and null removes it. Preserve those field semantics in generated docs and examples. Renaming the method is not warranted by an observed caller problem.

**Why / tradeoff:** The SDK already states omission/null behavior, and activity/override checks protect existing billing. A PUT-to-PATCH migration would add compatibility work without changing this useful transaction boundary.

**Code:** [internal/http/handlers/admin_metering.go:40](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_metering.go#L40) · [internal/service/enterprise.go:53](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/enterprise.go#L53) · [internal/modules/money/enterprise.go:58](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/money/enterprise.go#L58) · [internal/http/routes/catalog.go:45](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/catalog.go#L45) · [billing/metering.go:33](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/billing/metering.go#L33)

### 074. `GET /v1/admin/catalog/rate-overrides`

**Keep · No change needed**

**Current:** Catalog permission guards optional customer_id and meter_key filters. The service validates cursors and returns a bounded page ordered by customer and meter, including each override revision.

**Our recommendation:** Keep this collection and its filters rather than nesting all negotiated rates into meter or customer responses.

**Why / tradeoff:** It supports both a customer pricing screen and a merchant-wide pricing audit with the same query contract. Cursor pagination avoids increasingly expensive offsets as negotiated contracts grow.

**Code:** [internal/http/handlers/admin_rate_overrides.go:51](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_rate_overrides.go#L51) · [internal/modules/money/enterprise.go:354](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/money/enterprise.go#L354) · [internal/http/routes/catalog.go:47](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/catalog.go#L47)

### 075. `PUT /v1/admin/catalog/rate-overrides/{customer_id}/{meter_key}`

**Keep · No change needed**

**Current:** Catalog permission, recent human sign-in and grant-operation limiting protect the PUT. Price and allowance replace the override while product/filter/meter semantics are inherited. expected_revision supports create-if-absent with zero and guarded updates.

**Our recommendation:** Keep PUT on the customer/meter identity, the revision precondition and inherited default-card contract.

**Why / tradeoff:** This expresses one negotiated agreement clearly and prevents a customer override from silently changing the event interpretation or currency. Effective-date pricing would be a separate domain feature, not a necessary route redesign.

**Code:** [internal/http/handlers/admin_rate_overrides.go:18](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_rate_overrides.go#L18) · [internal/service/enterprise.go:101](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/enterprise.go#L101) · [internal/modules/money/enterprise.go:158](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/money/enterprise.go#L158) · [internal/http/routes/catalog.go:49](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/catalog.go#L49)

### 076. `DELETE /v1/admin/catalog/rate-overrides/{customer_id}/{meter_key}`

**Refine · Low priority**

**Current:** Catalog permission, recent human sign-in and destructive-operation limiting protect deletion by customer/meter. The service deletes under a catalog mutation and returns 204, including when no override remains; unlike PUT, it accepts no revision precondition.

**Our recommendation:** Keep repeated deletion harmless. If the pricing editor needs to refuse deletion based on an outdated viewed revision, offer an optional precondition consistent with PUT; retain unconditional deletion for callers intentionally removing the latest override.

**Why / tradeoff:** A stale pricing screen should not erase a newer negotiated rate. This is an additive concurrency refinement; clients intentionally deleting the latest state can retain unconditional behavior.

**Code:** [internal/http/handlers/admin_rate_overrides.go:74](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_rate_overrides.go#L74) · [internal/service/enterprise.go:184](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/enterprise.go#L184) · [internal/modules/money/enterprise.go:438](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/money/enterprise.go#L438) · [internal/http/routes/catalog.go:51](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/catalog.go#L51)

### 077. `POST /v1/admin/catalog/product-archives`

**Refine · Medium priority**

**Current:** Catalog permission and recent human sign-in protect an idempotency-keyed operation. The server persists request identity, archives the product and processes a bounded number of refunds per POST; replay continues work. complete becomes false for not_started, but can remain true with refund_pending outcomes.

**Our recommendation:** Keep complete as processing coverage and use the existing per-purchase refund_pending/refund_id outcomes for financial settlement. If a caller needs polling without resuming the next batch, add a read-only lookup on the existing operation. Aggregate pending counts would be convenience data, not a missing settlement distinction.

**Why / tradeoff:** Replay already resumes bounded work without refunding twice, and refund resources/findings expose downstream progress. A read-only lookup may simplify an operator workflow; it is not required to make recovery possible.

**Code:** [internal/http/handlers/product_archive.go:112](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/product_archive.go#L112) · [internal/http/handlers/product_archive.go:163](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/product_archive.go#L163) · [internal/http/handlers/product_archive.go:275](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/product_archive.go#L275) · [internal/http/routes/catalog.go:54](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/catalog.go#L54) · [billing/refunds.go:95](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/billing/refunds.go#L95) · [internal/http/handlers/product_archive.go:305](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/http/handlers/product_archive.go#L305)

### 078. `POST /v1/admin/catalog/ask`

**Keep · No change needed**

**Current:** Catalog permission guards a feature-gated POST. Empty questions are rejected. The model uses bounded read/draft tools, returns evidence and drafts, and never gets a mutation tool; drafting is separately enabled.

**Our recommendation:** Keep the explicit ask action and evidence-plus-draft response. Changes should continue through the ordinary reviewed catalog writes.

**Why / tradeoff:** This is an appropriate POST for a potentially expensive computation, despite being read-only in the billing domain. Folding draft execution into this endpoint would weaken the existing understandable review boundary.

**Code:** [internal/http/handlers/merchant_catalog_copilot.go:18](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/merchant_catalog_copilot.go#L18) · [internal/modules/copilot/ask.go:26](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/copilot/ask.go#L26) · [internal/modules/copilot/ask.go:68](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/copilot/ask.go#L68) · [internal/http/routes/catalog.go:59](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/catalog.go#L59)

**Overlap review:** See D2 in [Duplication and overlap](#duplication-and-overlap): duplicated proposal policy and price-request reconstruction have drifted; share policy/defaults and preserve price terms without merging creation and migration.


### 079. `POST /v1/admin/catalog/products`

**Keep · No change needed**

**Current:** Catalog permission and recent human sign-in protect creation. Key/name are normalized and required, the ID derives from merchant plus immutable key, and benefits/ownership are validated under the catalog transaction. The 201 response includes current prices.

**Our recommendation:** Keep product creation separate from price terms and destructive purchase handling. Preserve natural-key conflict behavior and direct callers to key lookup when resolving an ambiguous creation response.

**Why / tradeoff:** Products express benefits and identity while prices express financial versions. A new idempotency service or mandatory operation resource is unnecessary for this local, stable-key write.

**Code:** [internal/service/service_definition_catalog.go:33](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/service_definition_catalog.go#L33) · [internal/modules/catalog/product.go:82](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/catalog/product.go#L82) · [internal/http/handlers/admin_catalog.go:51](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_catalog.go#L51) · [internal/http/routes/catalog.go:62](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/catalog.go#L62)

### 080. `GET /v1/admin/catalog/products`

**Keep · No change needed**

**Current:** Catalog permission guards cursor pagination and archived, tier, entitlement, key and for-sale filters. Explicit IDs select batch mode. Prices are hydrated in one grouped read for the returned products.

**Our recommendation:** Keep current list and batch modes and the documented distinction that explicit IDs select named records. Preserve that distinction in generated clients; no expansion framework is needed for the current-price aggregate.

**Why / tradeoff:** The joined response fits catalog screens without one request per product. A new expansion framework is unnecessary while the current price projection is small and intentionally used by the caller.

**Code:** [internal/http/handlers/admin_catalog.go:68](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_catalog.go#L68) · [internal/modules/catalog/product.go:223](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/catalog/product.go#L223) · [internal/service/service_definition_catalog_admin.go:94](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/service_definition_catalog_admin.go#L94) · [internal/http/routes/catalog.go:64](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/catalog.go#L64)

### 081. `GET /v1/admin/catalog/products/{id}`

**Keep · No change needed**

**Current:** Catalog permission guards a typed ID lookup scoped to the merchant. The service returns product benefits and revision; the handler hydrates current prices before sending 200.

**Our recommendation:** Keep the stable product read and its current-price projection. Historical price versions should remain in the price/history surfaces.

**Why / tradeoff:** This gives the editor one useful aggregate without making the product own subscription migration or refund execution. Keeping history separate bounds the ordinary detail response.

**Code:** [internal/http/handlers/admin_catalog.go:100](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_catalog.go#L100) · [internal/service/service_definition_catalog_admin.go:25](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/service_definition_catalog_admin.go#L25) · [internal/http/handlers/admin_catalog.go:150](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_catalog.go#L150) · [internal/http/routes/catalog.go:66](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/catalog.go#L66)

### 082. `PATCH /v1/admin/catalog/products/{id}`

**Refine · Medium priority**

**Current:** Catalog permission and recent human sign-in protect a tri-state PATCH with optional revision checking. Local changes commit atomically and record edit ownership; entitlement changes affect holders. Stripe metadata/features are propagated after commit on a best-effort basis.

**Our recommendation:** Keep tri-state edits, revision checks and the separate archive-with-purchases command. If operators need immediate provider-convergence feedback, add propagation outcomes alongside the local result. Current price projections already use unknown sync status until verification, and drift scanning offers a later check; do not portray a 200 as a promise that every PSP changed.

**Why / tradeoff:** Local acceptance and provider convergence are different outcomes. An additive synchronization result prevents an operator assuming all copies changed; durable retry can follow if required, without rolling back a valid local edit because a PSP is unavailable.

**Code:** [internal/service/service_definition_catalog.go:121](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/service_definition_catalog.go#L121) · [internal/service/service_definition_catalog.go:178](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/service_definition_catalog.go#L178) · [internal/service/service_definition_catalog.go:256](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/service_definition_catalog.go#L256) · [internal/http/routes/catalog.go:68](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/catalog.go#L68)

### 083. `POST /v1/admin/catalog/prices`

**Keep · No change needed**

**Current:** Catalog permission and recent human sign-in protect creation. With product ID/key, immutable financial terms determine identity and a new version can replace the current key holder after provider preparation. Inline product_data instead creates/reuses a minimal product atomically with a local-only initial price and refuses conflicting current terms. The response can include manual provider actions.

**Our recommendation:** Keep immutable financial rows, a movable commercial key and explicit expected_revision for repricing. SDK examples should make new-version creation and subscriber migration two distinct choices.

**Why / tradeoff:** This preserves what existing subscribers agreed to and avoids turning PATCH into an implicit repricing command. The provider/manual-action result is already more truthful than promising immediate availability on every rail.

**Code:** [internal/service/service_definition_catalog.go:433](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/service_definition_catalog.go#L433) · [internal/service/catalog_price_write.go:21](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/catalog_price_write.go#L21) · [billing/catalog.go:160](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/billing/catalog.go#L160) · [internal/http/routes/catalog.go:70](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/catalog.go#L70) · [internal/service/catalog_inline_product.go:26](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/service/catalog_inline_product.go#L26)

**Overlap review:** See D2 in [Duplication and overlap](#duplication-and-overlap): duplicated proposal policy and price-request reconstruction have drifted; share policy/defaults and preserve price terms without merging creation and migration.


### 084. `GET /v1/admin/catalog/prices`

**Keep · No change needed**

**Current:** Catalog permission guards typed filters for product, key, currency, recurring and archived state plus cursor pagination. ID batch reads bypass the browsing filters; provider bindings are loaded in a batch.

**Our recommendation:** Keep a separate price collection and its filters. Clearly document that price IDs denote immutable terms while key-based results can span versions depending on archived filtering.

**Why / tradeoff:** This supports both catalog editing and historic billing references. Collapsing results to only the current key holder would make archived but still billed prices harder to inspect.

**Code:** [internal/http/handlers/admin_catalog.go:175](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_catalog.go#L175) · [internal/modules/catalog/price.go:241](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/catalog/price.go#L241) · [internal/http/routes/catalog.go:72](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/catalog.go#L72)

### 085. `GET /v1/admin/catalog/prices/{id}`

**Refine · Medium priority**

**Current:** Catalog permission guards GET. verify=true asks each linked PSP for drift and per-provider failures become error/unknown states. However a top-level VerifyPriceSync error is ignored and the original local projection is returned as an ordinary 200.

**Our recommendation:** Keep optional verification and the existing per-provider error/unknown states. Preserve any top-level verification error alongside the available local price so a caller can tell that a requested check did not complete. A timestamp is optional; the specific gap is lost error context, not an absent unknown state.

**Why / tradeoff:** The response already avoids asserting in_sync for an unchecked price. An additive verification outcome would explain why it stayed unknown while keeping useful local data.

**Code:** [internal/http/handlers/admin_catalog.go:214](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_catalog.go#L214) · [internal/service/service_definition_catalog_admin.go:425](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/service_definition_catalog_admin.go#L425) · [billing/catalog.go:98](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/billing/catalog.go#L98) · [internal/http/routes/catalog.go:74](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/catalog.go#L74) · [internal/db/models/product_catalog.go:359](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/db/models/product_catalog.go#L359)

### 086. `GET /v1/admin/catalog/prices/{id}/history`

**Keep · No change needed**

**Current:** Catalog permission guards a paginated GET addressed by a price ID. The service resolves that row’s product/key and lists movements for the entire key, loading each referenced price version.

**Our recommendation:** Keep the price-ID traversal to its commercial key’s movement history. It is already documented as key history and returns PriceKeyMovement values. Preserve that wording in clients; a separate key-addressed route needs a concrete caller requirement.

**Why / tradeoff:** Any version is a useful starting point for inspecting the same key history. Renaming this established traversal would add migration work without changing its data or solving an observed problem.

**Code:** [internal/http/handlers/admin_catalog.go:246](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_catalog.go#L246) · [internal/service/price_key.go:39](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/price_key.go#L39) · [internal/http/routes/catalog.go:76](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/catalog.go#L76)

### 087. `PATCH /v1/admin/catalog/prices/{id}`

**Refine · Medium priority**

**Current:** Catalog permission and recent human sign-in protect PATCH. Terms remain immutable; null PSP entries unlink, new links are verified before commit, and expected_revision checks the price key. Lifecycle propagation to providers after commit discards failures.

**Our recommendation:** Keep restricted PATCH and prevalidated bindings. For archive/restore edits, an additive propagation outcome could save a separate verifying GET; current sync_status remains unknown until checked, and verify=true already reports provider drift/errors. Preserve successful local edits rather than recasting every provider failure as failed local persistence.

**Why / tradeoff:** The endpoint should distinguish a committed local archive from a PSP copy that remains active. Preserve the current local transaction and manual-action behavior; changing successful local edits into generic errors would make recovery less clear.

**Code:** [internal/service/service_definition_catalog.go:577](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/service_definition_catalog.go#L577) · [internal/service/service_definition_catalog.go:618](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/service_definition_catalog.go#L618) · [internal/service/service_definition_catalog.go:769](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/service_definition_catalog.go#L769) · [internal/http/routes/catalog.go:78](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/catalog.go#L78) · [internal/db/models/product_catalog.go:359](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/db/models/product_catalog.go#L359)

### 088. `POST /v1/admin/price-migrations`

**Redesign · High priority**

**Current:** Create classifies a cohort, inserts the migration, then accepts each move separately. Provider push failures normally become durable blocked moves and do not abort the rest. A database/scheduling failure or interruption can still return early after partial acceptance, without durably recording unvisited subscribers or returning the migration identity in the error.

**Our recommendation:** Accept a stable idempotency-keyed migration with its cohort/intent durably recorded before execution, return its identity promptly, and resume unfinished acceptance/push work through that migration. Keep per-subscription renewal timing and rail-specific rules.

**Why / tradeoff:** The re-driver handles recorded failed pushes, and subscription-level checks protect existing pending changes. What is missing is resumable acceptance under the same migration identity for an interrupted cohort. Retrying POST can create another migration, so a stable acceptance receipt would make recovery more precise; this is not evidence of automatic duplicate charges.

**Code:** [internal/modules/subscriptions/price_migration.go:390](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/subscriptions/price_migration.go#L390) · [internal/modules/subscriptions/price_migration.go:463](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/subscriptions/price_migration.go#L463) · [internal/db/queries/scheduled_changes.sql:77](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/db/queries/scheduled_changes.sql#L77) · [internal/http/routes/subscriptions.go:31](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/subscriptions.go#L31) · [internal/modules/subscriptions/price_migration_redrive.go:43](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/modules/subscriptions/price_migration_redrive.go#L43)

**Overlap review:** See D2 in [Duplication and overlap](#duplication-and-overlap): duplicated proposal policy and price-request reconstruction have drifted; share policy/defaults and preserve price terms without merging creation and migration.


### 089. `POST /v1/admin/price-migrations/preview`

**Refine · Medium priority**

**Current:** Catalog permission guards a read-only POST. With a target, Preview uses the same classifier as Create and returns every subscription outcome; by key without a target it returns only matched count. The UI currently uses that count-only form before creating the target price.

**Our recommendation:** Keep both existing preview modes: null to_price_id explicitly denotes the documented count-only form. The console could use the richer targeted preview before migration acceptance when operators need per-rail eligibility, rather than treating matched as scheduled. Pagination/time markers are optional improvements if cohort size or staleness warrants them.

**Why / tradeoff:** A matched count is not a promise that all subscribers can move or meet notice rules. The richer second review is a design recommendation, not evidence the count endpoint is wrong. Preview remains advisory unless acceptance checks its snapshot.

**Code:** [internal/modules/subscriptions/price_migration.go:353](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/subscriptions/price_migration.go#L353) · [internal/modules/subscriptions/price_migration.go:296](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/subscriptions/price_migration.go#L296) · [web/admin/src/lib/api/endpoints.ts:534](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/web/admin/src/lib/api/endpoints.ts#L534) · [internal/http/routes/subscriptions.go:33](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/subscriptions.go#L33) · [billing/price_migrations.go:119](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/billing/price_migrations.go#L119)

**Overlap review:** See D2 in [Duplication and overlap](#duplication-and-overlap): duplicated proposal policy and price-request reconstruction have drifted; share policy/defaults and preserve price terms without merging creation and migration.


### 090. `GET /v1/admin/price-migrations`

**Keep · No change needed**

**Current:** Catalog permission guards cursor pagination, paired product_key/price_key filters and explicit-ID lookup. Matched/skipped are creation facts; scheduled/applied/canceled/blocked are counted from current moves.

**Our recommendation:** Keep this overview collection and its current counters. Add any future acceptance/cancellation state as fields on the same migration rather than introducing a parallel unrelated job listing.

**Why / tradeoff:** Operators can recover migration identities and compare progress without inspecting subscriptions one by one. Detailed diagnostics belong to migration detail or a bounded moves subresource; the overview should stay inexpensive and scannable.

**Code:** [internal/modules/subscriptions/price_migration.go:646](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/subscriptions/price_migration.go#L646) · [internal/db/queries/price_migrations.sql:48](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/db/queries/price_migrations.sql#L48) · [internal/http/routes/subscriptions.go:35](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/subscriptions.go#L35)

### 091. `GET /v1/admin/price-migrations/{id}`

**Refine · Medium priority**

**Current:** Catalog permission guards a merchant-scoped lookup that returns aggregate counts and canceled_at. It does not return blocked reasons/subscription identities or outstanding provider release work; those details are not represented by the migration view.

**Our recommendation:** Keep this stable read and aggregate counters. If operators need a migration-wide work list, add a bounded moves/reasons view and expose any durable cancellation-release checklist developed for cancellation. Existing migration and subscription reads already provide partial recovery; do not imply there is none.

**Why / tradeoff:** A count of blocked moves tells an operator that action is needed but not where. This can extend the existing resource without breaking list clients; avoid stuffing an unbounded subscriber array into every GET.

**Code:** [internal/modules/subscriptions/price_migration.go:629](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/subscriptions/price_migration.go#L629) · [internal/modules/subscriptions/price_migration.go:697](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/subscriptions/price_migration.go#L697) · [billing/price_migrations.go:99](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/billing/price_migrations.go#L99) · [internal/http/routes/subscriptions.go:37](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/subscriptions.go#L37)

### 092. `POST /v1/admin/price-migrations/{id}/cancel`

**Redesign · High priority**

**Current:** Cancellation marks the migration canceled and processes its still-scheduled moves individually. Applied or blocked changes and archived source prices remain. The response lists subscription IDs that require provider schedule release. Repeating cancel can continue still-scheduled rows, but does not reconstruct release instructions for rows already canceled.

**Our recommendation:** Persist cancellation progress and each outstanding provider release, make repeated cancel/read return them until resolved, and distinguish cancel_requested from fully stopped. Preserve the current rule that already-applied changes and archived source prices are not rolled back.

**Why / tradeoff:** The manual Stripe boundary is honest today, but a lost response or later reopen loses the actionable IDs; UI only shows a transient count warning. Durable state is necessary even if provider release remains manual, and requires migration of current cancellation semantics.

**Code:** [internal/modules/subscriptions/price_migration.go:715](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/subscriptions/price_migration.go#L715) · [internal/db/queries/scheduled_changes.sql:70](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/db/queries/scheduled_changes.sql#L70) · [web/admin/src/pages/catalog/price-detail.tsx:215](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/web/admin/src/pages/catalog/price-detail.tsx#L215) · [internal/http/routes/subscriptions.go:39](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/subscriptions.go#L39)

## Administrative billing routes — 40

### 093. `POST /v1/admin/credit-grants`

**Refine · Medium priority**

**Current:** AdminUpdate admits authorized people and applications; people need recent sign-in. The batch validates before a transactional deposit, repays existing debt first, and replays each customer/source coordinate. It returns grants in input order, 201 if any is new and 200 if all replay.

**Our recommendation:** Keep the atomic batch and per-item source IDs. Stamp the authenticated initiating actor separately from caller-supplied invoker; currently an omitted invoker becomes the customer, even for a staff grant.

**Why / tradeoff:** This preserves imported business attribution while making the actual issuer independently auditable. Adding actor attribution is a contract/storage addition; do not reject legitimate on-behalf-of invokers.

**Code:** [internal/http/handlers/credits.go:95](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/credits.go#L95) · [internal/service/credit_grants.go:71](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/credit_grants.go#L71) · [credits.go:46](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/credits.go#L46) · [internal/http/routes/credits.go:17](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/credits.go#L17)

### 094. `GET /v1/admin/credit-grants`

**Keep · No change needed**

**Current:** AdminRead returns merchant-scoped grants using customer_id with optional currency/source filters and keyset pagination; IDs select a bounded lookup instead. States distinguish spent, expired, scheduled and revoked lots.

**Our recommendation:** Keep this collection and its source_id recovery filter. Clearly retain the documented IDs-as-lookup mode rather than adding another bulk endpoint.

**Why / tradeoff:** Operators can reconcile the original grant independently from its ledger movements, and integrations can recover by their own stable reference. The customer requirement intentionally avoids an unbounded merchant-wide credit history.

**Code:** [internal/http/handlers/credits.go:127](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/credits.go#L127) · [internal/modules/money/credit_grants.go:52](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/money/credit_grants.go#L52) · [credits.go:67](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/credits.go#L67) · [internal/http/routes/credits.go:20](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/credits.go#L20)

### 095. `GET /v1/admin/credit-grants/{id}`

**Keep · No change needed**

**Current:** AdminRead parses a typed grant ID and reads within the merchant. The service distinguishes missing rows from operational errors and projects amounts, provenance, expiry and termination state.

**Our recommendation:** Keep the individual resource read and the same CreditGrant representation as create/list/revoke.

**Why / tradeoff:** This gives support staff and applications a stable recovery target without replaying a mutation. Remaining/spent/revoked amounts already explain the important lifecycle; a separate status-only route would duplicate it.

**Code:** [internal/http/handlers/credits.go:172](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/credits.go#L172) · [internal/modules/money/credit_grants.go:105](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/money/credit_grants.go#L105) · [internal/http/routes/credits.go:23](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/credits.go#L23)

### 096. `POST /v1/admin/credit-grants/{id}/revoke`

**Keep · No change needed**

**Current:** AdminUpdate plus recent human sign-in and the destructive limiter guard a reason-bearing command. Under the customer money lock, revocation refuses held funds and unavailable lots; an already revoked grant replays. It returns the updated grant.

**Our recommendation:** Keep the explicit revoke action, mandatory bounded reason, and refusal to steal reserved credit.

**Why / tradeoff:** This is a lifecycle transition with accounting effects, so a command is clearer than deleting a record. Returning the grant provides immediate confirmation, and the money lock protects concurrent spending without needing client-side compensation.

**Code:** [internal/http/handlers/credits.go:190](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/credits.go#L190) · [internal/modules/money/credit_grants.go:123](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/money/credit_grants.go#L123) · [internal/http/routes/credits.go:25](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/credits.go#L25)

### 097. `GET /v1/admin/customers/{customer_id}/balance/transactions`

**Keep · No change needed**

**Current:** AdminRead requires the path customer and, for normal pagination, a supported currency. Named transaction IDs instead permit one bounded lookup across currencies, still restricted to that customer and merchant.

**Our recommendation:** Keep the ledger subcollection separate from balances and credit lots, with immutable signed movements and links to grant/source coordinates.

**Why / tradeoff:** The route answers why money changed while balance answers what is available now. Cursor paging suits an append-oriented ledger; exposing a mutable transaction API here would undermine the accounting model.

**Code:** [internal/http/handlers/credits.go:217](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/credits.go#L217) · [internal/service/credit_grants.go:138](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/credit_grants.go#L138) · [internal/modules/money/money_service.go:307](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/money/money_service.go#L307) · [internal/http/routes/credits.go:28](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/credits.go#L28)

### 098. `GET /v1/admin/customers/{customer_id}/balance`

**Keep · No change needed**

**Current:** AdminRead accepts a required supported currency and returns prepaid balance, other holds, unposted running-charge accrual, available and owed amounts, and billing mode. It also returns as_of, the current accrual rate and depletes_at when running charges apply. The read derives these observations without taking the writer’s spending lock.

**Our recommendation:** Keep the per-currency balance resource and its explicit distinction between posted money, holds and running accrual. Preserve as_of and depletion information for display; admission and spending commands remain authoritative for concurrent decisions.

**Why / tradeoff:** Callers can explain changing availability without pretending that unposted accrual is already a ledger transaction. The read is an observation, not a reservation; an additional read lock or reservation API is unnecessary here.

**Code:** [internal/http/handlers/credits.go:257](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/credits.go#L257) · [internal/service/spend.go:16](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/spend.go#L16) · [internal/modules/money/money_service.go:156](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/money/money_service.go#L156) · [internal/http/routes/credits.go:31](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/credits.go#L31) · [billing/credits.go:190](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/billing/credits.go#L190) · [internal/modules/money/money_service.go:755](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/modules/money/money_service.go#L755)

### 099. `GET /v1/admin/customers`

**Refine · Medium priority**

**Current:** Normal merchant customer browsing has keyset pagination and bounded IDs lookup. Search asks the contact directory for at most the requested limit and returns no next cursor; the directory interface itself has no continuation. The console’s searched export therefore stops after the first 200 matches.

**Our recommendation:** Keep the normal collection contract. Make searched exports explicitly bounded, or extend the directory search contract and route together to support continuation before presenting them as complete exports.

**Why / tradeoff:** This prevents a successful-looking search export from hiding omitted matches. An explicit cap is a small contract/UI clarification; true continuation is a broader directory and issuer integration change, not a matter of forwarding an already available cursor.

**Code:** [internal/service/customers.go:137](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/customers.go#L137) · [billing/customers.go:55](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/billing/customers.go#L55) · [web/admin/src/lib/mutations.ts:172](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/web/admin/src/lib/mutations.ts#L172) · [internal/http/routes/customers.go:12](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/customers.go#L12) · [internal/identity/directory.go:35](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/identity/directory.go#L35) · [web/admin/src/lib/mutations.ts:172](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/web/admin/src/lib/mutations.ts#L172)

### 100. `GET /v1/admin/customers/{customer_id}`

**Refine · Medium priority**

**Current:** AdminRead loads the merchant’s billing customer and adds settings, money, contacts and optional network links. A configured contact directory failure aborts the entire read with service_unavailable, even though the billing row is available.

**Our recommendation:** Allow the billing summary to remain readable when optional contact enrichment fails, with an explicit contact availability/error marker distinct from a genuinely absent contact.

**Why / tradeoff:** Support can still inspect balances and debt during a host-directory outage. This changes partial-response semantics and requires the console to show stale/unavailable enrichment; never silently present an unavailable contact as nonexistent.

**Code:** [internal/http/handlers/customers.go:34](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/customers.go#L34) · [internal/service/customers.go:31](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/customers.go#L31) · [billing/customers.go:12](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/billing/customers.go#L12) · [internal/http/routes/customers.go:15](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/customers.go#L15)

### 101. `PATCH /v1/admin/customers/{customer_id}`

**Keep · No change needed**

**Current:** AdminUpdate with recent human sign-in validates all fields before a customer-locked transaction; an unseen customer is created. Credit limits and trust levels merge by currency. Billing policy and invoice profile explicitly distinguish omitted, value and null.

**Our recommendation:** Keep the existing PATCH contract and its field-specific merge rules. Preserve zero/empty clearing for currency entries and null inheritance for policy/profile.

**Why / tradeoff:** This already provides useful tri-state behavior and avoids resending unrelated currencies. Forcing array replacement would lose concurrent independent settings; adding generic patch operations would make ordinary support edits harder.

**Code:** [internal/http/handlers/customer_settings.go:9](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/customer_settings.go#L9) · [internal/service/customer_settings.go:31](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/customer_settings.go#L31) · [billing/customer_settings.go:37](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/billing/customer_settings.go#L37) · [internal/http/routes/customers.go:17](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/customers.go#L17)

### 102. `GET /v1/admin/entitlements`

**Refine · Low priority**

**Current:** AdminRead can page keys for named customers or holders of one key, optionally at an explicit time. The service reads a snapshot per request; when at is omitted, each page derives a fresh now.

**Our recommendation:** Keep the unified entitlement query, but pin and return the effective at time across a paginated traversal, or encode it in the cursor.

**Why / tradeoff:** Access can expire between pages, so a support export otherwise mixes different instants despite having a stable customer/key cursor. Binding time adds cursor state but avoids new routes; callers explicitly supplying at already have a workaround.

**Code:** [internal/http/handlers/entitlements.go:75](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/entitlements.go#L75) · [internal/modules/entitlements/resource_checks.go:104](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/entitlements/resource_checks.go#L104) · [web/admin/src/lib/api/endpoints.ts:72](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/web/admin/src/lib/api/endpoints.ts#L72) · [internal/http/routes/entitlements.go:12](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/entitlements.go#L12)

### 103. `GET /v1/admin/product-access`

**Keep · No change needed**

**Current:** AdminRead filters merchant-scoped windows by customer/product and live state or retrieves named IDs. Each result exposes source, payment/grant attribution, start/end times and derived scheduled/active/expired/revoked status.

**Our recommendation:** Keep product-access windows distinct from effective entitlement keys, and retain the existing bounded IDs lookup instead of adding a duplicate single-window route solely for symmetry.

**Why / tradeoff:** Windows explain provenance and revocation while entitlement reads answer the final access decision. This separation is useful for debugging overlapping purchases, grants and subscriptions; combining them would blur resource ownership.

**Code:** [internal/http/handlers/product_access.go:85](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/product_access.go#L85) · [internal/http/handlers/product_access.go:32](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/product_access.go#L32) · [internal/modules/productaccess/product_access_service.go:166](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/productaccess/product_access_service.go#L166) · [internal/http/routes/entitlements.go:16](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/entitlements.go#L16)

### 104. `POST /v1/admin/product-access`

**Redesign · High priority**

**Current:** AdminUpdate accepts an atomic batch and optionally an Idempotency-Key; missing keys are generated invisibly. Per-item keys are header:index. Existing-key comparison checks only customer/product, so changed duration silently replays and appended items can create new grants. The console sends no key.

**Our recommendation:** Require or expose a reusable batch key, fingerprint the complete normalized batch before any writes, and reject changed terms or length. Have the console retain that key across retries; keep the existing transactional grant machinery.

**Why / tradeoff:** This makes one reviewed grant request one repeatable operation. Requiring a key and rejecting formerly tolerated reuse needs migration guidance; it is preferable to silent term mismatch or duplicated access after a lost response.

**Code:** [internal/http/handlers/product_access.go:206](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/product_access.go#L206) · [internal/modules/productaccess/product_access_service.go:72](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/productaccess/product_access_service.go#L72) · [web/admin/src/lib/api/endpoints.ts:147](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/web/admin/src/lib/api/endpoints.ts#L147) · [ci/subscriptions/batch_writes_test.go:50](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/ci/subscriptions/batch_writes_test.go#L50) · [internal/http/routes/entitlements.go:18](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/entitlements.go#L18)

### 105. `POST /v1/admin/product-access/{id}/revoke`

**Keep · No change needed**

**Current:** AdminUpdate, recent human sign-in and the destructive limiter guard a bounded reason. The service locates the merchant’s window, treats an already revoked/deleted window as done, and delegates to entitlement revocation. HTTP success is 204.

**Our recommendation:** Keep the explicit reason-bearing revoke command and idempotent no-content success. Refresh the product-access and entitlement views after success.

**Why / tradeoff:** The command targets one provenance window rather than promising the customer loses all overlapping access. Returning 204 is adequate for this simple transition, and the separate effective entitlement query answers whether other sources still grant access.

**Code:** [internal/http/handlers/product_access.go:293](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/product_access.go#L293) · [internal/modules/productaccess/product_access_service.go:130](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/productaccess/product_access_service.go#L130) · [web/admin/src/lib/api/endpoints.ts:166](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/web/admin/src/lib/api/endpoints.ts#L166) · [internal/http/routes/entitlements.go:20](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/entitlements.go#L20)

### 106. `GET /v1/admin/provider-operations`

**Keep · No change needed**

**Current:** AdminRead allows both permitted people and applications. State and tri-state refused filters select merchant operations, newest first with a timestamp/operation-ID cursor; results attach qualification, refusal and resolution details.

**Our recommendation:** Keep this administrative collection and its current resource name. Its filters match the refused-hold review queue; no demonstrated workflow requires relocating all provider-operation routes.

**Why / tradeoff:** The read is oversight across operations while application writes have a different Costs permission. Preserving this permission boundary avoids broadening access or incorrectly classifying admin routes as human-only.

**Code:** [internal/http/handlers/provider_operations.go:89](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/provider_operations.go#L89) · [internal/modules/money/provider_billing_refusal.go:219](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/money/provider_billing_refusal.go#L219) · [internal/http/routes/gate.go:726](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/gate.go#L726) · [internal/http/routes/metering.go:36](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/metering.go#L36)

### 107. `GET /v1/admin/provider-operations/{operation_id}`

**Keep · No change needed**

**Current:** AdminRead permits authorized people and applications to read one merchant operation by its external operation ID. The service attaches authorization, qualification, settlement/refusal and resolution information without advancing it.

**Our recommendation:** Keep this read-only administrative recovery endpoint. A Costs-scoped application read could be considered separately if least-privilege integration requirements justify it; do not infer that current writes lack recovery.

**Why / tradeoff:** Every successful application write already returns the operation, and an application may hold AdminRead. An additional narrower read would reduce permission breadth but adds another access contract; that is an optional design choice rather than a confirmed gap.

**Code:** [internal/http/handlers/provider_operations.go:33](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/provider_operations.go#L33) · [internal/service/operation_authorization.go:79](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/operation_authorization.go#L79) · [internal/http/routes/metering.go:39](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/metering.go#L39)

### 108. `POST /v1/admin/provider-operations/{operation_id}/close`

**Refine · Medium priority**

**Current:** AdminUpdate with recent human sign-in accepts settled or written_off attestation. Under the payer lock, identical terms replay; changed terms conflict. The request’s attested_by is persisted directly while settling attested cost or releasing the hold.

**Our recommendation:** Keep the guarded close command and exact-term replay, but record the authenticated submitting actor separately from the attested_by value supplied on behalf of an external operator.

**Why / tradeoff:** This preserves legitimate delegated attestations while distinguishing who asserted them from who submitted the mutation. Additive provenance is safer than replacing the existing field or narrowing the operation’s broader intended scope.

**Code:** [internal/http/handlers/provider_operations.go:75](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/provider_operations.go#L75) · [internal/modules/money/provider_billing_resolution.go:42](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/money/provider_billing_resolution.go#L42) · [internal/modules/money/provider_billing_resolution.go:167](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/money/provider_billing_resolution.go#L167) · [internal/http/routes/metering.go:50](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/metering.go#L50)

### 109. `GET /v1/admin/findings`

**Keep · No change needed**

**Current:** AdminRead lists open findings by default, most severe then oldest, with status/severity/type filters, a composite cursor and named-ID lookup. Evidence and parsed recommendations are included for each item.

**Our recommendation:** Keep the severity-first queue and distinguish it from chronological payment/attempt history. Do not add bulk destructive approval merely for collection symmetry.

**Why / tradeoff:** Operators need the most consequential unresolved work first. Per-finding resolution preserves review context; a generic bulk command would weaken the deliberate one-at-a-time workflow and require a separate safety design.

**Code:** [internal/http/handlers/admin_findings.go:87](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_findings.go#L87) · [internal/reconcile/store.go:477](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/reconcile/store.go#L477) · [web/admin/src/lib/api/endpoints.ts:610](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/web/admin/src/lib/api/endpoints.ts#L610) · [internal/http/routes/ops.go:33](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/ops.go#L33)

### 110. `GET /v1/admin/findings/{id}`

**Refine · Medium priority**

**Current:** AdminRead returns evidence, an action string, untyped parameter maps and recursively listed alternatives. The resolve body can override permitted parameters but does not explicitly select an alternative action.

**Our recommendation:** Expose executable recommendations as a small discriminated action contract with typed allowed inputs and stable alternative identifiers; ensure each advertised alternative is actually selectable, or mark it informational.

**Why / tradeoff:** This lets a console build an accurate review form instead of interpreting opaque maps. Keep raw evidence for diagnostics; replacing the entire extensible evidence payload with rigid structs would overconstrain future detectors.

**Code:** [internal/http/handlers/admin_findings.go:62](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_findings.go#L62) · [billing/findings.go:82](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/billing/findings.go#L82) · [internal/http/handlers/admin_findings.go:250](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_findings.go#L250) · [internal/http/routes/ops.go:35](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/ops.go#L35)

### 111. `POST /v1/admin/findings/{id}/resolve`

**Refine · Medium priority**

**Current:** AdminUpdate with recent human sign-in executes the recommendation before conditionally marking the finding fixed/admin_fixed. Partial effects are recorded in notes and the finding remains open. A queued refund counts as accepted execution: the response and stored resolution evidence contain its refund ID and queued state, while the existing provider intent continues processing. Reconciliation can reopen a fixed finding when the underlying condition is detected again. Concurrent resolution is detected after action execution.

**Our recommendation:** Keep existing action idempotency, provider recovery and resolution evidence. Show queued refund evidence and its payment link in the console, and distinguish operator acceptance from verified settlement in status wording. If concurrent operator decisions require serialization, consider claiming the reviewed action and parameters before execution using the existing finding and intent machinery.

**Why / tradeoff:** This improves reviewability without implying that recovery is absent or replacing working intent execution. A separate pending/partial/completed resolution lifecycle is an optional, more expensive design choice; the concrete first improvement is to surface information the API already returns. Any claim mechanism must preserve safe retry after interruption.

**Code:** [internal/http/handlers/admin_findings.go:218](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_findings.go#L218) · [internal/http/handlers/admin_findings_actions.go:362](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_findings_actions.go#L362) · [internal/reconcile/store.go:593](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/reconcile/store.go#L593) · [internal/http/routes/ops.go:37](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/ops.go#L37) · [internal/db/queries/reconciliation.sql:243](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/db/queries/reconciliation.sql#L243) · [internal/db/queries/reconciliation.sql:38](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/db/queries/reconciliation.sql#L38) · [web/admin/src/pages/ops/index.tsx:195](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/web/admin/src/pages/ops/index.tsx#L195)

### 112. `GET /v1/admin/orders`

**Keep · No change needed**

**Current:** AdminRead returns no-store order pages with validated status/origin, customer, price, subscription and overdue filters. Merchant reads omit customer next_action and derive delinquency. Named IDs provide bounded recovery.

**Our recommendation:** Keep the unified orders collection and explicit origin filter. Do not revive the retired checkout-session versus order split.

**Why / tradeoff:** Current master intentionally represents every purchase as an order; one collection lets support follow debt and purchase history without translating resource families. Specialized usage actions remain constrained by origin rather than creating a second invoice identity.

**Code:** [internal/http/handlers/orders.go:268](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/orders.go#L268) · [internal/http/handlers/orders.go:441](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/orders.go#L441) · [internal/service/orders.go:487](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/orders.go#L487) · [internal/http/routes/orders.go:55](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/orders.go#L55)

### 113. `GET /v1/admin/orders/{id}`

**Keep · No change needed**

**Current:** AdminRead resolves one merchant-scoped order and returns its status and billing detail without customer payment next_action; delinquency is derived. It is a non-mutating read suitable after an uncertain collection response.

**Our recommendation:** Keep the administrative order projection and read-only recovery behavior. Customer authentication actions should remain in the customer’s own flow.

**Why / tradeoff:** The same order ID ties creation, charges, collections and follow-up reads together. Giving administrative readers customer payment instructions would cross roles without helping them establish the order’s accounting outcome.

**Code:** [internal/http/handlers/orders.go:287](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/orders.go#L287) · [internal/service/orders.go:469](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/orders.go#L469) · [internal/service/orders.go:484](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/orders.go#L484) · [internal/http/routes/orders.go:57](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/orders.go#L57)

### 114. `POST /v1/admin/orders/{id}/void`

**Refine · Medium priority**

**Current:** AdminUpdate, recent human sign-in and destructive limiting protect a bodyless command. The money service locks the customer/order, treats already void as done, and writes off remaining owed amount in the same transaction.

**Our recommendation:** Keep void separate from mark-uncollectible, but accept and retain a bounded reason plus authenticated actor with the void event.

**Why / tradeoff:** Voiding changes the debt ledger, unlike merely stopping automatic collection. A durable explanation makes later reconciliation understandable; introducing a required reason is a client compatibility change and should be rolled out deliberately.

**Code:** [internal/http/handlers/orders.go:490](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/orders.go#L490) · [internal/service/usage_orders.go:118](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/usage_orders.go#L118) · [internal/modules/money/usage_order.go:278](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/money/usage_order.go#L278) · [internal/http/routes/orders.go:59](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/orders.go#L59)

### 115. `POST /v1/admin/orders/{id}/mark-uncollectible`

**Refine · Medium priority**

**Current:** AdminUpdate with recent human sign-in and destructive limiting marks eligible usage orders uncollectible under customer/order locks. Repeats succeed; unlike void, debt remains and a later collection can reopen the order.

**Our recommendation:** Keep the separate action and retain a reason/actor on its transition. Describe it in the console as stopping collection while debt remains, avoiding a promise that the balance is forgiven.

**Why / tradeoff:** The accounting distinction is sound and matters to support decisions. Adding provenance improves reversibility and review; do not merge the action with void or change debt semantics just to simplify endpoint count.

**Code:** [internal/http/handlers/orders.go:496](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/orders.go#L496) · [internal/modules/money/usage_order.go:310](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/money/usage_order.go#L310) · [internal/service/usage_orders.go:124](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/usage_orders.go#L124) · [internal/http/routes/orders.go:61](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/orders.go#L61)

### 116. `POST /v1/admin/orders/{id}/retry-collection`

**Keep · No change needed**

**Current:** AdminUpdate with recent human sign-in/off-channel limiting requires an Idempotency-Key and a customer payment-method ID. The money collection machinery owns the attempt; the response returns the order, including HTTP 402 with the order on a confirmed failed charge.

**Our recommendation:** Keep one key per collection attempt and reuse it while uncertain. Preserve the order in declined responses and direct callers to its GET for observation.

**Why / tradeoff:** A new key means a new charge attempt, which is a meaningful business decision. Existing durable collection and order reads provide recovery; a generic task wrapper would not automatically make this clearer.

**Code:** [internal/http/handlers/orders.go:517](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/orders.go#L517) · [internal/service/usage_orders.go:149](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/usage_orders.go#L149) · [internal/http/routes/orders.go:63](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/orders.go#L63)

### 117. `GET /v1/admin/customers/{customer_id}/payment-methods`

**Keep · No change needed**

**Current:** AdminRead lists only the path customer’s merchant-scoped cards with cursor or named-ID lookup. Administrative projection includes ended agreements, subscription links, default currencies and payment-method health.

**Our recommendation:** Keep this read under the customer instead of exposing a merchant-wide card inventory or administrative card-creation shortcut.

**Why / tradeoff:** Staff can diagnose why a saved card is unusable while ownership remains explicit. Showing ended agreements is appropriate for support history; the customer’s active-payment selection can continue using its narrower representation.

**Code:** [internal/http/handlers/payment_methods.go:50](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/payment_methods.go#L50) · [internal/modules/paymentmethods/payment_method_repo.go:282](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/paymentmethods/payment_method_repo.go#L282) · [internal/http/handlers/payment_methods.go:420](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/payment_methods.go#L420) · [internal/http/routes/payment_methods.go:13](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/payment_methods.go#L13)

### 118. `POST /v1/admin/payments`

**Keep · No change needed**

**Current:** AdminUpdate, recent human sign-in and off-channel limiting accept an order, positive amount, transaction ID and optional paid_at. The transaction ID is the replay identity; service routes usage remittances to debt settlement and other orders to order remittance. It returns 201 new or 200 replay.

**Our recommendation:** Keep this as recording an external payment, explicitly distinct from charging a card. Retain the transaction reference as the natural idempotency coordinate.

**Why / tradeoff:** This avoids inventing a second synthetic key for a remittance already identified externally. Order currency and due constraints keep the payment tied to an actual obligation; frontend naming should continue to say record payment.

**Code:** [internal/http/handlers/admin_payments.go:546](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_payments.go#L546) · [internal/service/orders.go:698](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/orders.go#L698) · [payments.go:43](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/payments.go#L43) · [internal/http/routes/payments.go:18](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/payments.go#L18)

### 119. `GET /v1/admin/payments`

**Keep · No change needed**

**Current:** AdminRead lists merchant-scoped payments by customer, subscription/order, rail, kind, status or transaction reference with cursor paging; named IDs switch to bounded lookup. It computes refund totals for charges before projecting results.

**Our recommendation:** Keep payments as money records and payment-attempts as diagnostic attempts. Retain transaction_id lookup and linked refund totals for reconciliation.

**Why / tradeoff:** Combining failed attempts into this collection would blur whether money moved. Refunds remain payments with a kind and relationship, so the current ledger shape can support both customer histories and administrative investigation without a second refund inventory.

**Code:** [internal/http/handlers/admin_payments.go:426](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_payments.go#L426) · [internal/http/handlers/admin_payments.go:499](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_payments.go#L499) · [internal/modules/payments/payment_repo.go:545](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/payments/payment_repo.go#L545) · [internal/http/routes/payments.go:20](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/payments.go#L20)

### 120. `GET /v1/admin/payments/{id}`

**Refine · Medium priority**

**Current:** AdminRead parses a typed payment ID and loads payment, relations and refunds. The handler maps every service error to resource_not_found, including failures loading related rows or refund history.

**Our recommendation:** Return 404 only for a genuinely missing payment; retain operational failures as retryable server/service errors. Keep the payment-plus-refunds detail shape.

**Why / tradeoff:** A storage outage must not tell reconciliation clients that a committed payment disappeared. This is a small but important error-contract correction; clients that treated every error as absence must begin distinguishing temporary unavailability.

**Code:** [internal/http/handlers/admin_payments.go:525](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_payments.go#L525) · [internal/modules/payments/payment_repo.go:217](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/payments/payment_repo.go#L217) · [web/admin/src/lib/api/endpoints.ts:267](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/web/admin/src/lib/api/endpoints.ts#L267) · [internal/http/routes/payments.go:22](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/payments.go#L22)

### 121. `POST /v1/admin/payments/{id}/refunds`

**Refine · High priority**

**Current:** AdminUpdate, recent human sign-in and the destructive limiter require an Idempotency-Key. A locked reservation and provider intent protect concurrent totals; 201 returns completed refund and 202 a pending payment. The console creates a fresh random key for every invocation.

**Our recommendation:** Keep the server’s durable refund contract. Make console and examples retain one key for the reviewed refund until resolved, reuse it after transport failure, and poll the returned payment ID; only a deliberate new refund gets a new key.

**Why / tradeoff:** This prevents a lost response from becoming another partial refund. No endpoint redesign is necessary; pending already has a useful resource. Persisting intent state in the caller is more work than generating a key at each submit.

**Code:** [internal/http/handlers/admin_payments.go:112](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_payments.go#L112) · [internal/http/handlers/admin_payments.go:350](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_payments.go#L350) · [web/admin/src/lib/api/endpoints.ts:272](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/web/admin/src/lib/api/endpoints.ts#L272) · [refunds.go:16](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/refunds.go#L16) · [internal/http/routes/payments.go:24](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/payments.go#L24)

### 122. `GET /v1/admin/payment-attempts`

**Keep · No change needed**

**Current:** AdminRead offers bounded keyset pages and exact IDs with provider, customer, order, subscription, renewal, result/category, source and time filters. Results retain diagnostic response details and resource links, separate from payment success.

**Our recommendation:** Keep the diagnostic attempt resource and its stable attempt ID. Continue accepting extensible provider result filters rather than forcing every rail’s response vocabulary into one closed enum.

**Why / tradeoff:** Operations can inspect declines and authentication failures that never became payments. The time/ID cursor fits append-oriented evidence, and the dedicated GET supports deep links without replaying provider actions.

**Code:** [internal/http/handlers/payment_attempts.go:29](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/payment_attempts.go#L29) · [internal/http/handlers/payment_attempts.go:213](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/payment_attempts.go#L213) · [payment_attempts.go:15](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/payment_attempts.go#L15) · [internal/http/routes/payments.go:26](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/payments.go#L26)

### 123. `GET /v1/admin/payment-attempts/{id}`

**Keep · No change needed**

**Current:** AdminRead validates the attempt ID and performs a merchant-qualified database lookup. Missing rows yield 404 and operational errors remain 500. The projection includes outcome diagnostics, timing, card summary and linked payment/order/renewal IDs.

**Our recommendation:** Keep a simple immutable-detail GET, using the same PaymentAttempt type as the collection.

**Why / tradeoff:** This is a precise troubleshooting target independent of the eventual money record. The existing error distinction is appropriate and should be the model for payment-detail reads; no mutation endpoint belongs on historical attempt evidence.

**Code:** [internal/http/handlers/payment_attempts.go:83](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/payment_attempts.go#L83) · [internal/http/handlers/payment_attempts.go:213](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/payment_attempts.go#L213) · [payment_attempts.go:35](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/payment_attempts.go#L35) · [internal/http/routes/payments.go:29](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/payments.go#L29)

### 124. `GET /v1/admin/renewals`

**Keep · No change needed**

**Current:** AdminRead pages renewals by due time and ID, filtering owner, first outcome, miss reason, PSP/subscription and due interval. Outcome is derived as collected, lost after closing time, or open; exact IDs provide direct recovery.

**Our recommendation:** Keep renewal cycles distinct from individual attempts, and retain both first outcome and final outcome.

**Why / tradeoff:** This distinction explains recovery: a cycle may first fail and later collect through another attempt. Flattening cycles into attempts would distort success/recovery metrics; the collection already supplies the operator dimensions needed to investigate that journey.

**Code:** [internal/http/handlers/payment_attempts.go:111](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/payment_attempts.go#L111) · [internal/http/handlers/payment_attempts.go:252](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/payment_attempts.go#L252) · [payment_attempts.go:49](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/payment_attempts.go#L49) · [internal/http/routes/payments.go:31](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/payments.go#L31)

### 125. `GET /v1/admin/renewals/{id}`

**Refine · Low priority**

**Current:** AdminRead retrieves one merchant renewal and then all its attempts oldest first, embedding them in the response. No page or truncation information is available for the embedded attempt list.

**Our recommendation:** Keep the convenient detail response for typical cycles, but bound its embedded history and expose continuation through the existing payment-attempts collection filtered by cycle_id if histories can grow large.

**Why / tradeoff:** The cycle/detail relationship is sensible; an additive continuation avoids unbounded read cost without creating another resource family. This is a scale-oriented design recommendation, not evidence of a currently oversized production cycle.

**Code:** [internal/http/handlers/payment_attempts.go:165](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/payment_attempts.go#L165) · [internal/http/handlers/payment_attempts.go:61](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/payment_attempts.go#L61) · [payment_attempts.go:65](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/payment_attempts.go#L65) · [internal/http/routes/payments.go:34](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/payments.go#L34)

### 126. `GET /v1/admin/subscriptions`

**Keep · No change needed**

**Current:** AdminRead applies customer, price, status, rail, date and dunning filters with keyset pagination or named IDs. The service batches prices, scheduled changes and dunning projections; the handler attaches mandate IDs.

**Our recommendation:** Keep one subscription collection with explicit dunning and scheduled-change information, separate from renewal attempts and payment records.

**Why / tradeoff:** The resource describes the continuing agreement rather than a single charge. Batched enrichment keeps list/detail semantics useful without embedding unbounded payment history, and explicit IDs remain suitable for cross-resource linking.

**Code:** [internal/http/handlers/admin_users.go:17](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_users.go#L17) · [internal/modules/subscriptions/admin_service.go:85](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/subscriptions/admin_service.go#L85) · [internal/modules/subscriptions/subscription.go:208](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/subscriptions/subscription.go#L208) · [internal/http/routes/subscriptions.go:17](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/subscriptions.go#L17)

### 127. `GET /v1/admin/subscriptions/{id}`

**Refine · Low priority**

**Current:** AdminRead loads the subscription, scheduled change, dunning and mandate. Detail enrichment silently ignores errors loading price/product, while the list path returns an error if its price lookup fails.

**Our recommendation:** Distinguish a legitimately missing historical catalog relation from a transient catalog read failure, and make list/detail enrichment failure semantics consistent.

**Why / tradeoff:** A detail response should not quietly look incomplete because storage is unavailable. Either expose optional enrichment availability or fail consistently; the smaller correction is to propagate non-not-found errors while preserving historical missing references.

**Code:** [internal/http/handlers/admin_users.go:17](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_users.go#L17) · [internal/modules/subscriptions/admin_service.go:128](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/subscriptions/admin_service.go#L128) · [internal/modules/subscriptions/admin_service.go:85](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/subscriptions/admin_service.go#L85) · [internal/http/routes/subscriptions.go:19](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/subscriptions.go#L19)

### 128. `POST /v1/admin/subscriptions/{id}/cancel`

**Refine · Medium priority**

**Current:** AdminUpdate with recent human sign-in and destructive limiting routes engine cancellation through lifecycle rules. Provider-billed cancellation commits local status/access and a durable provider-cancel intent together, then returns the subscription; provider execution may still be pending.

**Our recommendation:** Keep the cancellation action and atomic local/intent transaction. Add visible provider-cancellation progress or a linked operation reference to distinguish locally canceled from provider-confirmed cancellation.

**Why / tradeoff:** An operator needs to know whether further provider charges are still being prevented asynchronously. This is an additive observation contract, not a reason to roll back durable cancellation or pretend the current enqueue is a synchronous provider confirmation.

**Code:** [internal/http/handlers/subscription_lifecycle.go:172](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/subscription_lifecycle.go#L172) · [internal/modules/subscriptions/admin_service.go:194](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/subscriptions/admin_service.go#L194) · [billing/subscriptions.go:68](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/billing/subscriptions.go#L68) · [internal/http/routes/subscriptions.go:21](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/subscriptions.go#L21)

### 129. `POST /v1/admin/subscriptions/{id}/resume`

**Refine · Low priority**

**Current:** AdminUpdate with recent human sign-in only resumes a canceled subscription inside its reversible window. Engine and Stripe paths resume billing; NMI supersedes deferred deletion and restores local access. A repeat after successful resume returns invalid_param because it is now active.

**Our recommendation:** Keep explicit rail/window eligibility, but give retries a recognizable already-resumed outcome or typed conflict instead of a generic invalid parameter.

**Why / tradeoff:** A lost success response should be distinguishable from a genuinely unsupported resume. Do not make every active subscription a blind success: guard against conflating a later unrelated state change, using existing lifecycle evidence where needed.

**Code:** [internal/http/handlers/subscription_lifecycle.go:151](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/subscription_lifecycle.go#L151) · [internal/http/handlers/subscription_lifecycle.go:190](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/subscription_lifecycle.go#L190) · [internal/http/handlers/subscription_lifecycle.go:237](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/subscription_lifecycle.go#L237) · [internal/http/routes/subscriptions.go:23](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/subscriptions.go#L23)

### 130. `POST /v1/admin/subscriptions/{id}/change`

**Refine · Medium priority**

**Current:** AdminUpdate with recent human sign-in/off-channel limiting requires a reason and reusable key. Durable replay runs before current-state guards. Engine increases charge under the recurring agreement; reductions schedule for renewal. Provider-billed immediate staff charges are refused; processing responses carry an operation ID.

**Our recommendation:** Keep durable replay and the console’s existing stable key across dismissal/re-preview of an unresolved request. Consider a GET for read-only observation only if callers need it beyond the existing subscription read and command replay; a new operation URL is an optional workflow improvement.

**Why / tradeoff:** Replay already recovers and may resume work, so recovery is not absent. A read makes observation side-effect-free and aligns console polling across payment workflows; it introduces an additional permission/resource contract that should reuse existing durable operation state.

**Code:** [internal/http/handlers/admin_subscription_change.go:26](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_subscription_change.go#L26) · [internal/modules/checkout/subscription_change.go:44](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/checkout/subscription_change.go#L44) · [billing/subscription_change.go:23](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/billing/subscription_change.go#L23) · [web/admin/src/lib/api/endpoints.ts:213](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/web/admin/src/lib/api/endpoints.ts#L213) · [internal/http/routes/subscriptions.go:25](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/subscriptions.go#L25) · [web/admin/src/pages/subscriptions/change-subscription-dialog.tsx:392](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/web/admin/src/pages/subscriptions/change-subscription-dialog.tsx#L392)

### 131. `POST /v1/admin/subscriptions/{id}/change/preview`

**Refine · Low priority**

**Current:** AdminRead accepts proposed price/seats, resolves the same subscription/customer and admission rules, and returns effective timing, amount due, next renewal and an estimate flag. The preview and eventual change are separate reads of mutable subscription/catalog state.

**Our recommendation:** Keep POST preview and read permission. For amounts requiring explicit operator approval, include a quote timestamp/revision and an optional maximum approved charge or stale-quote rejection on execution.

**Why / tradeoff:** This prevents an old preview being mistaken for a binding quote after pricing or period state changes. A maximum-charge guard is more useful than claiming every estimate can be frozen; it requires an additive execution parameter and clear rail-specific semantics.

**Code:** [internal/http/handlers/admin_subscription_change.go:58](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/admin_subscription_change.go#L58) · [internal/modules/checkout/subscription_change.go:280](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/checkout/subscription_change.go#L280) · [billing/subscription_change.go:55](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/billing/subscription_change.go#L55) · [internal/http/routes/subscriptions.go:27](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/subscriptions.go#L27)

### 132. `PUT /v1/admin/subscriptions/{id}/payment-method`

**Refine · Medium priority**

**Current:** AdminUpdate with recent human sign-in accepts a card ID or null, validates customer/provider custody and agreement requirements, and updates engine billing or executes a durable NMI source-update intent. Unresolved provider work returns generic resource_conflict although it continues out of band.

**Our recommendation:** Keep PUT and null-for-default semantics, but return an explicit accepted/processing representation with a readable operation reference for unresolved durable updates; reserve conflict for incompatible concurrent changes.

**Why / tradeoff:** The operator can then tell pending work from refusal and safely observe completion. This extends the response contract and console state machine, while retaining existing same-intent replay and provider-account checks.

**Code:** [internal/http/handlers/update_subscription_payment_method.go:37](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/update_subscription_payment_method.go#L37) · [internal/http/handlers/update_subscription_payment_method.go:194](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/update_subscription_payment_method.go#L194) · [web/admin/src/lib/api/endpoints.ts:213](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/web/admin/src/lib/api/endpoints.ts#L213) · [internal/http/routes/subscriptions.go:29](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/subscriptions.go#L29)

## Merchant configuration routes — 20

### 133. `GET /v1/admin/configuration`

**Keep · No change needed**

**Current:** MerchantConfig permission admits people and applications. The service reads the file/Vault-backed document plus directory API host and returns settings, display name and revision without credentials.

**Our recommendation:** Keep one non-secret configuration read with revision. Continue exposing edit capability through the existing deployment/capability contract so file-backed merchants have an honest read-only UI.

**Why / tradeoff:** The aggregate matches settings editing and supplies the revision for safe PATCH. Splitting every setting into its own endpoint would add more calls and lose the coherent document boundary.

**Code:** [internal/service/merchant_configuration.go:38](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/merchant_configuration.go#L38) · [internal/service/merchant_configuration.go:59](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/merchant_configuration.go#L59) · [web/admin/src/pages/settings/merchant-config.test.tsx:154](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/web/admin/src/pages/settings/merchant-config.test.tsx#L154) · [internal/http/routes/merchant.go:12](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/merchant.go#L12)

### 134. `PATCH /v1/admin/configuration`

**Refine · Medium priority**

**Current:** MerchantConfig permission and recent human sign-in protect Vault-only PATCH. Revision conflicts are handled, policy lists replace and assigned policy removal is checked. Profile string fields merge only when nonempty; empty/null keeps them, while whitespace can be stored and normalize to empty at runtime. The UI omits cleared inputs.

**Our recommendation:** Use an explicit keep/set/clear contract for nullable profile values and make the form send that intent. Empty/null currently keeps them and whitespace can normalize differently, so do not describe clearing as categorically impossible. Preserve existing per-field collection rules.

**Why / tradeoff:** Clearing a support URL should have one reliable wire operation, not depend on whitespace. Changing null semantics affects existing callers, so document/version it or add an explicit clear operation during migration.

**Code:** [internal/service/merchant_configuration.go:162](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/merchant_configuration.go#L162) · [internal/modules/merchantconfig/settings.go:55](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/merchantconfig/settings.go#L55) · [web/admin/src/pages/settings/index.tsx:227](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/web/admin/src/pages/settings/index.tsx#L227) · [internal/service/merchant_configuration.go:83](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/merchant_configuration.go#L83) · [internal/http/routes/merchant.go:16](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/merchant.go#L16)

### 135. `GET /v1/admin/api-host`

**Keep · No change needed**

**Current:** MerchantConfig permission guards a feature-gated directory read. The handler reads the bound API host and open claim, including proof information; it does not bind or verify a host itself.

**Our recommendation:** Keep the read-only status endpoint and the distinction between proven host and pending claim. Host ownership changes should remain with the existing operator/host-controlled proof workflow.

**Why / tradeoff:** Combining an unverified requested hostname with the active hostname would confuse public routing. The current projection lets a hosting product explain DNS work without granting ordinary settings writes the authority to claim another host.

**Code:** [internal/http/handlers/merchant_api_host.go:42](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/merchant_api_host.go#L42) · [internal/merchants/host_config.go:140](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/merchants/host_config.go#L140) · [internal/merchants/api_host_claim.go:121](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/merchants/api_host_claim.go#L121) · [internal/http/routes/merchant.go:20](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/merchant.go#L20)

### 136. `GET /v1/admin/alert-webhooks`

**Keep · No change needed**

**Current:** MerchantConfig permission guards listing with optional IDs. The document-backed store returns all matching destinations; API projection exposes host, name, format and enabled state while withholding the full webhook URL.

**Our recommendation:** Keep the safe projection and simple list for this small configuration collection. Do not return credential-bearing webhook paths merely to make the edit form resemble the stored document.

**Why / tradeoff:** Hosts identify destinations for operators without exposing embedded Slack/Discord secrets. Pagination is less urgent for a typically small configuration collection than for billing event collections.

**Code:** [internal/http/handlers/merchant_alerts.go:64](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/merchant_alerts.go#L64) · [internal/modules/alerting/store.go:35](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/alerting/store.go#L35) · [internal/modules/alerting/types.go:67](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/alerting/types.go#L67) · [internal/http/routes/merchant.go:24](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/merchant.go#L24)

### 137. `POST /v1/admin/alert-webhooks`

**Refine · Medium priority**

**Current:** MerchantConfig permission and recent human sign-in protect Vault-only creation. URL destination and format are validated, enabled defaults true, a new ID is minted and the whole document is updated with revision retries; the URL is not returned.

**Our recommendation:** For automated callers that retry creation after a lost response, an optional stable request identity would prevent accidental duplicate destinations. Keep secret-safe projection and URL validation; do not deduplicate only by URL because distinct intentional configurations may share it.

**Why / tradeoff:** A lost response followed by the same POST currently creates another destination and can duplicate future alerts. A stable request identity solves that concrete retry problem; it should not deduplicate solely by URL because intentional separate configurations can share one destination.

**Code:** [internal/modules/alerting/service.go:86](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/alerting/service.go#L86) · [internal/modules/alerting/store.go:69](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/alerting/store.go#L69) · [internal/http/routes/merchant.go:26](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/merchant.go#L26)

### 138. `DELETE /v1/admin/alert-webhooks/{id}`

**Keep · No change needed**

**Current:** MerchantConfig permission and recent human sign-in protect Vault-only deletion. A typed ID selects one destination, the store removes it with document revision retries, and the route returns 204 or 404 if absent.

**Our recommendation:** Keep this direct deletion contract. Clients retrying an uncertain delete should treat the documented missing-resource result as confirmation that the destination no longer exists.

**Why / tradeoff:** The resource is small and there is no remote deletion workflow. Standardizing every DELETE to 204 for absence would be a stylistic migration, not a necessary fix here.

**Code:** [internal/http/handlers/merchant_alerts.go:125](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/merchant_alerts.go#L125) · [internal/modules/alerting/service.go:143](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/alerting/service.go#L143) · [internal/modules/alerting/store.go:69](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/alerting/store.go#L69) · [internal/http/routes/merchant.go:28](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/merchant.go#L28)

### 139. `PATCH /v1/admin/alert-webhooks/{id}`

**Refine · Low priority**

**Current:** MerchantConfig permission and recent human sign-in protect Vault-only PATCH. Omitted fields stay, null name clears, null URL/format/enabled is rejected. Internal document revision retries merge the patch onto current state, but callers cannot supply their viewed revision.

**Our recommendation:** Preserve these already-clear tri-state semantics. Add optional destination revision/precondition support if concurrent settings editors are supported, consistent with merchant and PSP edits.

**Why / tradeoff:** Internal CAS prevents document loss but still lets a stale human overwrite a newer value of the same field. An optional precondition is a small additive refinement; no need to turn this into a generic patch language.

**Code:** [internal/modules/alerting/service.go:162](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/alerting/service.go#L162) · [internal/modules/alerting/store.go:69](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/alerting/store.go#L69) · [internal/http/routes/merchant.go:30](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/merchant.go#L30)

### 140. `GET /v1/admin/billing-archive`

**Redesign · Medium priority**

**Current:** The guarded GET deliberately places the source merchant in readonly provider-write posture before a consistent snapshot. Successful generation leaves provider mutations fenced even if HTTP delivery later fails. On export failure it attempts to restore the previous posture unless another change superseded it; this is not a freeze of local writes or webhook ingestion.

**Our recommendation:** We’d prefer an explicit POST for this documented cutover/export action, preserving the safety fence. A separate artifact-download GET is useful only if artifacts will be retained; an operation store is not inherently required to correct the method semantics.

**Why / tradeoff:** The documentation already warns that export fences provider writes and requires an offline handoff. POST would express that side effect more clearly at the HTTP boundary. This is a design/migration tradeoff, not an undisclosed safety failure or proof GET stops every billing write.

**Code:** [internal/merchantarchive/archive.go:78](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/merchantarchive/archive.go#L78) · [internal/merchantarchive/write_posture.go:20](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/merchantarchive/write_posture.go#L20) · [internal/http/handlers/merchant_archive.go:19](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/merchant_archive.go#L19) · [internal/http/routes/merchant.go:36](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/merchant.go#L36) · [docs/merchant-portability.md:71](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/docs/merchant-portability.md#L71)

### 141. `POST /v1/admin/billing-archive`

**Refine · Low priority**

**Current:** Restore validates and inserts transactionally, including running-charge records, then rebuilds their balance watches and lands a new restore in readonly provider-write posture. Exact committed-artifact replay is a no-op, including after the target has advanced or been armed. The HTTP receipt contains merchant, digest, rows and replay flag; operator arming is already documented.

**Our recommendation:** Keep transactional validation, watch rebuilding and exact-artifact replay. An optional receipt field reporting the actual current posture could make the documented handoff easier for API callers; do not always answer ‘requires arming’ on a replay or arm automatically.

**Why / tradeoff:** The existing workflow deliberately requires an operator to select the authoritative copy. This is optional response convenience, not missing safety or missing recovery support.

**Code:** [internal/merchantarchive/archive.go:169](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/merchantarchive/archive.go#L169) · [internal/merchantarchive/archive.go:260](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/merchantarchive/archive.go#L260) · [internal/http/handlers/merchant_archive.go:53](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/merchant_archive.go#L53) · [internal/http/routes/merchant.go:38](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/merchant.go#L38) · [internal/merchantarchive/archive.go:235](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/merchantarchive/archive.go#L235) · [internal/merchantarchive/archive.go:260](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/merchantarchive/archive.go#L260)

### 142. `POST /v1/admin/billing-import`

**Refine · Medium priority**

**Current:** MerchantConfig permission and recent human sign-in protect an as_of-bound JSON import. One merchant transaction lands facts, classifies subscriptions and derives access. Infrastructure failures roll back; business refusals return imported/skipped/blocked source lists and a reasons map. Exhaustive coverage is a distinct caller assertion.

**Our recommendation:** Keep this separate from archive restore and retain the documented per-source outcomes. A future additive outcome object with stable reason codes could simplify client joins across imported/skipped/blocked and reasons; current source IDs already make those joins possible, so this is API ergonomics rather than a correlation defect.

**Why / tradeoff:** Importers can associate decisions with source IDs today, but a single outcome representation simplifies retry/reporting and avoids joining response structures. Add it compatibly before removing old lists; batching must continue to avoid claiming exhaustive absence.

**Code:** [internal/http/handlers/billing_import.go:24](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/billing_import.go#L24) · [internal/billingimport/billingimport.go:72](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/billingimport/billingimport.go#L72) · [internal/billingimport/billingimport.go:310](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/billingimport/billingimport.go#L310) · [internal/http/routes/merchant.go:44](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/merchant.go#L44)

### 143. `PUT /v1/admin/dashboard`

**Refine · Low priority**

**Current:** MerchantConfig permission admits people and applications; this route does not require recent sign-in. The handler strictly decodes a full widget list, validates all widget queries together, records the actor and upserts one merchant layout.

**Our recommendation:** Keep full replacement and aggregated validation. If simultaneous dashboard editing is a supported workflow, an optional revision/If-Match guard would let clients refuse a stale whole-layout save; retain unconditional replacement for existing callers.

**Why / tradeoff:** This is a coherent PUT aggregate; separate per-widget endpoints would add complexity. Because the whole layout is replaced, a stale save has a larger overwrite surface than an ordinary field patch, but existing unconditional clients can remain supported.

**Code:** [internal/http/handlers/merchant_dashboard.go:39](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/merchant_dashboard.go#L39) · [internal/modules/dashboard/dashboard.go:37](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/dashboard/dashboard.go#L37) · [internal/modules/dashboard/service.go:107](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/dashboard/service.go#L107) · [internal/http/routes/ops.go:31](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/ops.go#L31)

### 144. `GET /v1/admin/provisioning-tokens`

**Keep · No change needed**

**Current:** MerchantConfig permission guards metadata-only listing, optionally filtered by IDs. The database returns every merchant token oldest first, including declared ownership and last-used time; the handler wraps the entire result as one ListPage without a cursor.

**Our recommendation:** Keep the metadata-only list and declared ownership indicator. Add pagination only if actual token cardinality warrants it, taking care not to silently truncate existing consumers.

**Why / tradeoff:** This is a configuration credential collection, not a high-volume event stream. Unrestricted creation alone is insufficient evidence that pagination should be a present priority.

**Code:** [internal/http/handlers/provisioning_tokens.go:28](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/provisioning_tokens.go#L28) · [internal/scim/tokens.go:60](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/scim/tokens.go#L60) · [billing/provisioning.go:49](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/billing/provisioning.go#L49) · [internal/http/routes/provisioning.go:49](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/provisioning.go#L49)

### 145. `POST /v1/admin/provisioning-tokens`

**Keep · No change needed**

**Current:** MerchantConfig permission and recent human sign-in protect creation. A trimmed bounded name is required; 32 random bytes form a recognizable token, only its SHA-256 hash is stored and the secret is returned once with metadata.

**Our recommendation:** Keep one-time secret disclosure and a separate provisioning credential family. Document the lost-response recovery path as listing/revoking the unusable token and creating another, rather than storing retrievable plaintext for replay.

**Why / tradeoff:** The present narrow secret lifecycle is preferable to a generic long-lived API-key subsystem. Expiry policies or automated rotation may be useful later, but their absence alone is not a defect in this route.

**Code:** [internal/http/handlers/provisioning_tokens.go:47](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/provisioning_tokens.go#L47) · [internal/scim/tokens.go:77](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/scim/tokens.go#L77) · [provisioning.go:25](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/provisioning.go#L25) · [internal/http/routes/provisioning.go:51](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/provisioning.go#L51)

### 146. `DELETE /v1/admin/provisioning-tokens/{id}`

**Refine · Medium priority**

**Current:** MerchantConfig permission and recent human sign-in protect typed-ID deletion. All matching token rows, including declared tokens, can be deleted; subsequent resolution refuses the hash. Initial configuration synchronization or a changed merchant document calls Declare and can recreate that configured token.

**Our recommendation:** Keep immediate database-token revocation. For a declaration-managed token, make the existing source-of-truth rule explicit in the delete response/UI; refusing the delete with instructions to remove its declaration is one option. A durable revocation override is a separate product choice that would change declarative ownership, not the default fix.

**Why / tradeoff:** The list and guide already distinguish declared tokens. The narrower issue is that an apparently successful deletion can be undone when that declaration is applied again; clarify that lifecycle without automatically inventing a second competing authority.

**Uncertainty:** Redeclaration on initial synchronization or merchant-document change is source-confirmed; its timing depends on the deployment lifecycle. Unchanged cache refreshes return early and do not themselves redeclare.

**Code:** [internal/scim/tokens.go:100](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/scim/tokens.go#L100) · [internal/scim/tokens.go:119](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/scim/tokens.go#L119) · [internal/scim/tokens.go:145](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/scim/tokens.go#L145) · [internal/app/merchants_wiring.go:133](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/app/merchants_wiring.go#L133) · [internal/http/routes/provisioning.go:53](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/provisioning.go#L53)

### 147. `GET /v1/admin/psps`

**Keep · No change needed**

**Current:** MerchantConfig permission guards a cursor-paginated list filtered by rail/archive state in the current environment. Explicit ID lookup can retrieve historical/other-environment identities. Credential values are withheld; configured/validated metadata and open obligations are included.

**Our recommendation:** Keep PSP accounts distinct from rails and keep archived identities inspectable. Document the explicit-ID mode’s broader environment visibility so callers do not assume list filters still apply.

**Why / tradeoff:** The distinction supports multiple merchant accounts on one rail and safe draining. Returning secret material or hiding archived obligations would make operations less safe, while the current view already provides useful lifecycle information.

**Code:** [internal/merchants/psps.go:105](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/merchants/psps.go#L105) · [internal/merchants/psps.go:161](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/merchants/psps.go#L161) · [internal/merchants/psps.go:607](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/merchants/psps.go#L607) · [internal/http/routes/psps.go:16](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/psps.go#L16)

### 148. `POST /v1/admin/psps`

**Refine · Medium priority**

**Current:** Creation proves applicable credentials/account ownership before writing; identical repeats return the existing PSP. A persisted document rejected for serving already returns an explicit ‘written but is not served’ message. Subsequent metadata/projection reads can still return generic errors after persistence.

**Our recommendation:** Keep stable-key repeat creation and the existing written-but-not-served distinction. If refining errors, concentrate on generic afterWrite failures: include a safe recovery hint to retry the same declaration or inspect its key. Do not require a second setup-operation resource merely to repeat existing recovery.

**Why / tradeoff:** A generic failure after successful storage can make operators re-enter or rotate credentials unnecessarily. This is an outcome-reporting refinement, not a reason to pretend Vault and SQL writes share one transaction.

**Code:** [internal/merchants/psps.go:196](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/merchants/psps.go#L196) · [internal/merchants/psps.go:259](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/merchants/psps.go#L259) · [internal/merchants/psps.go:391](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/merchants/psps.go#L391) · [internal/http/routes/psps.go:18](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/psps.go#L18) · [internal/merchants/psps.go:234](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/merchants/psps.go#L234)

### 149. `GET /v1/admin/psps/{id}`

**Keep · No change needed**

**Current:** MerchantConfig permission guards a typed PSP-ID lookup within the merchant. The service returns the identity, settings, revision, credential-slot health and live obligation count without credential values.

**Our recommendation:** Keep this stable recovery/read surface for setup, rotation and draining. Do not automatically probe a provider on each GET.

**Why / tradeoff:** A terminated provider account still needs local historical inspection, and ordinary reads should remain available without live provider connectivity. Explicit credentials validation on writes and separate refresh operations already express the expensive work.

**Code:** [internal/http/handlers/psps.go:49](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/psps.go#L49) · [internal/merchants/psps.go:178](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/merchants/psps.go#L178) · [internal/merchants/psps.go:582](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/merchants/psps.go#L582) · [internal/http/routes/psps.go:20](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/psps.go#L20)

### 150. `PATCH /v1/admin/psps/{id}`

**Keep · No change needed**

**Current:** MerchantConfig permission and recent human sign-in protect Vault-only PATCH. An optional expected_revision guards stale updates; otherwise the patch merges onto the latest document with CAS retries; supplied credentials are proved before writing. Archive must be its own request, is irreversible, skips provider calls and requires allow_last for the last active account on a rail. Existing obligations remain visible.

**Our recommendation:** Keep these distinct modes in one account resource, including webhook-secret overlap and last-account acknowledgement. Make the irreversible archive choice explicit in every client, as the current admin form does.

**Why / tradeoff:** Retiring a dead account must not require working provider credentials. A dedicated archive route would be stylistic rather than necessary; the current validation already prevents combining retirement with credential/settings edits.

**Code:** [internal/merchants/psps.go:282](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/merchants/psps.go#L282) · [internal/merchants/psps.go:361](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/merchants/psps.go#L361) · [web/admin/src/pages/settings/index.tsx:560](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/web/admin/src/pages/settings/index.tsx#L560) · [internal/http/routes/psps.go:24](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/psps.go#L24)

### 151. `POST /v1/admin/psps/routing-preview`

**Refine · Medium priority**

**Current:** MerchantConfig permission protects a preview through the production routing decision path, without creating a charge. Every error from the merchant-connection/DryRunRouting execution is wrapped as invalid_param, including storage failures; earlier missing-service errors remain separately classified.

**Our recommendation:** Keep the production decision path and explainable candidates. Preserve typed missing-resource/validation failures, but classify database/configuration outages as server or service-unavailable failures; describe the response as the price-routing decision at read time.

**Why / tradeoff:** Telling an operator to fix their input when routing storage is unavailable leads to the wrong recovery action. Error changes need SDK/error-catalog updates. The preview is not a reservation and cannot promise the same account after configuration changes.

**Code:** [internal/service/checkout_routing.go:16](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/checkout_routing.go#L16) · [internal/service/checkout_routing.go:36](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/service/checkout_routing.go#L36) · [billing/psps.go:136](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/billing/psps.go#L136) · [internal/http/routes/psps.go:28](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/psps.go#L28)

### 152. `POST /v1/admin/psps/refresh`

**Refine · Medium priority**

**Current:** The endpoint independently queues merchant refresh and catalog scanning. Both jobs are durable and deduplicated; a retry can reuse existing work. The 202 reports only the refresh job ID, while failure to enqueue the second task can follow acceptance of the first.

**Our recommendation:** Keep the existing asynchronous jobs and deduplication. If callers need to distinguish acceptance/progress of the two tasks, expose separate task outcomes or identities. A combined operation resource is optional, not a required durability repair.

**Why / tradeoff:** The response says queued rather than completed, and the UI announces that drift will appear as discovered. Its empty-state ‘Catalog is in sync’ should not be treated as evidence a requested scan completed; clearer UI freshness/acceptance information may suffice without a new workflow subsystem.

**Code:** [internal/http/handlers/psps.go:102](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/psps.go#L102) · [internal/river/jobs_catalog_reconciliation.go:28](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/river/jobs_catalog_reconciliation.go#L28) · [web/admin/src/pages/catalog/index.tsx:955](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/web/admin/src/pages/catalog/index.tsx#L955) · [internal/http/routes/psps.go:34](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/psps.go#L34) · [internal/river/jobs_provider_refresh.go:90](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/river/jobs_provider_refresh.go#L90) · [internal/river/jobs_catalog_reconciliation.go:164](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/river/jobs_catalog_reconciliation.go#L164)

## Metrics and administrative access — 6

### 153. `GET /v1/admin/access`

**Keep · No change needed**

**Current:** A signed-in principal reads effective admin read/update, catalog, merchant-config and metrics capabilities for the resolved scope. Permission backend failure is returned as failure rather than a fabricated all-false answer.

**Our recommendation:** Keep it as the admin UI’s capability read. Make clear that it describes administrative access, not all app permissions or public product capabilities.

**Why / tradeoff:** The existing compact representation supports UI navigation without exposing credentials or requiring the browser to duplicate authorization rules. A broader universal permissions document would add coupling without demonstrated need.

**Code:** [internal/http/routes/gate.go:1024](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/gate.go#L1024) · [internal/http/routes/ops.go:15](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/ops.go#L15)

### 154. `POST /v1/admin/metrics/query`

**Refine · Low priority**

**Current:** Metrics-authorized principals submit a bounded query language with aggregate validation errors. The compiler scopes SQL to the merchant; responses return columns and rows with exact money cells. Results are cut to the requested limit after sorting, without an explicit truncation marker.

**Our recommendation:** Keep POST and the validated query language. Add a truncated/has_more indicator or explicit result-limit metadata; document numeric cell types and money precision through the column schema.

**Why / tradeoff:** Explicit limit metadata would help clients communicate top-N scope, especially when a result is reused for export or natural-language explanation. The requested limit is already part of the query contract; this is an additive clarity improvement, not evidence of incorrect aggregation. Keep analytical results separate from transactional list envelopes.

**Code:** [internal/http/handlers/merchant_metrics.go:24](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/merchant_metrics.go#L24) · [internal/modules/metrics/service.go:216](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/metrics/service.go#L216) · [internal/http/routes/ops.go:17](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/ops.go#L17)

### 155. `GET /v1/admin/metrics/schema`

**Keep · No change needed**

**Current:** Returns the shared measure/dimension/formula registry and examples behind the Metrics permission. The same registry guides generated widgets and metrics questions.

**Our recommendation:** Keep this discoverable contract endpoint; keep examples and caveats tied to the executable registry.

**Why / tradeoff:** It lets clients construct valid queries without maintaining a competing list of measures. No route restructuring is needed. A schema version is only worth adding if persisted external clients need change detection.

**Code:** [internal/http/handlers/merchant_metrics.go:18](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/merchant_metrics.go#L18) · [internal/http/routes/ops.go:19](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/ops.go#L19)

### 156. `GET /v1/admin/dashboard`

**Keep · No change needed**

**Current:** Returns the merchant’s saved widget layout, or a default template when none is saved. Widget execution lives in metrics/query; layout persistence uses a separately permitted PUT.

**Our recommendation:** Keep the layout resource and the separate metrics execution endpoint. Explain the Metrics-read versus MerchantConfig-write distinction together in the dashboard docs.

**Why / tradeoff:** Reading a dashboard should not implicitly write defaults or require permission to edit merchant configuration. The different permissions reflect distinct actions; a common documentation section resolves discoverability without moving URLs.

**Code:** [internal/http/handlers/merchant_dashboard.go:17](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/merchant_dashboard.go#L17) · [internal/http/routes/ops.go:31](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/ops.go#L31) · [internal/http/routes/ops.go:21](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/ops.go#L21)

### 157. `POST /v1/admin/metrics/ask`

**Refine · Low priority**

**Current:** With explicit ask enablement, a metrics question can run a bounded number of compiler-validated merchant queries. The response includes model prose and verbatim executed-query evidence; aggregate results are sent to the configured model provider.

**Our recommendation:** Keep it separate from widget generation. Use the existing evidence array to distinguish zero-evidence prose in clients, and add missing query-limit truncation metadata to the underlying metrics result.

**Why / tradeoff:** The model can answer without tool calls, which evidence already reveals. Model-feed shortening is explicitly marked today and API evidence retains the full query result; the separate query-limit truncation remains unmarked. Improve that precise boundary rather than adding redundant provenance flags.

**Code:** [internal/modules/dashboard/ask.go:60](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/dashboard/ask.go#L60) · [internal/modules/dashboard/ask.go:100](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/dashboard/ask.go#L100) · [internal/http/routes/ops.go:27](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/ops.go#L27)

### 158. `POST /v1/admin/dashboard/widgets/generate`

**Keep · No change needed**

**Current:** Generates a proposed title/query/visualization from a prompt and optional base query. The model sees schema and the supplied input; generated queries are validated, not executed or automatically saved. Attempts are bounded.

**Our recommendation:** Keep the proposal endpoint and its separate save step. Preserve structured corrective errors and show the proposed query before saving it.

**Why / tradeoff:** This contract already gives users control over what becomes part of the dashboard. Merging generation and persistence would blur that boundary; no URL change or generic AI operation service is needed.

**Code:** [internal/http/handlers/merchant_dashboard.go:68](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/merchant_dashboard.go#L68) · [internal/modules/dashboard/generate.go:45](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/dashboard/generate.go#L45) · [internal/http/routes/ops.go:29](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/ops.go#L29)

## Public checkout and process routes — 6

### 159. `GET /v1/catalog/products`

**Refine · Low priority**

**Current:** Public lookup requires 1–100 repeated keys and only returns products that are for sale and not archived. Price projection removes internal provider fields; this is a bounded storefront lookup, not an unrestricted catalog dump.

**Our recommendation:** Keep this route and its intentionally limited lookup. Describe required keys and their bounds in the generated request contract, with an example using repeated query parameters.

**Why / tradeoff:** A public storefront should not need an application secret. Making the existing constraints machine-readable improves integration without changing discovery or exposing administrative catalog fields.

**Code:** [internal/http/handlers/catalog.go:18](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/catalog.go#L18) · [internal/http/handlers/catalog.go:58](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/catalog.go#L58) · [internal/http/routes/catalog.go:31](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/catalog.go#L31)

### 160. `GET /v1/config`

**Keep · No change needed**

**Current:** Returns public capabilities and merchant payment configuration, uses a public whitelist rather than secret values, and supports an ETag. A degraded PSP configuration is returned with no-store; healthy responses use a short public cache lifetime.

**Our recommendation:** Keep this capability/bootstrap endpoint. Keep deployment capabilities separate from the signed-in caller’s permissions, and document that distinction beside admin/access.

**Why / tradeoff:** Consumers need to know what an installation supports before rendering checkout. Combining that with user authorization would complicate public caching and make the response misleading.

**Code:** [internal/http/handlers/public_config.go:39](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/public_config.go#L39) · [internal/http/routes/config.go:13](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/config.go#L13)

### 161. `GET /health/live`

**Keep · No change needed**

**Current:** The standalone binding reports a small public process-liveness response without acquiring the merchant database connection.

**Our recommendation:** Keep the minimal shape and the separate live/ready routes.

**Why / tradeoff:** Process liveness should remain cheap and independent of upstream availability; merging it with readiness would change restart behavior. No route or response redesign is warranted.

**Code:** [server/internal/http/routes_public.go:42](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/server/internal/http/routes_public.go#L42) · [internal/http/routes/meta.go:24](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/meta.go#L24)

### 162. `GET /health/ready`

**Keep · No change needed**

**Current:** The standalone handler has a two-second budget, checks runtime and authentication readiness, and refuses while draining. Required database/worker/catalog checks affect readiness; optional dependency degradation is tracked separately. Public errors omit dependency details.

**Our recommendation:** Keep the public response small and preserve the distinction between required dependencies and degraded optional capabilities.

**Why / tradeoff:** This is a deliberate deployment contract, not a diagnostics API. Operators already have logs and separate operational surfaces; returning a large public dependency dump would not help normal clients.

**Code:** [server/internal/http/routes_public.go:61](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/server/internal/http/routes_public.go#L61) · [internal/app/ready.go:29](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/app/ready.go#L29) · [internal/http/routes/meta.go:28](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/meta.go#L28)

### 163. `GET /v1/orders/{id}/solana-pay`

**Keep · No change needed**

**Current:** Public wallet metadata returns a label/icon only after resolving a currently payable Solana attempt. The label uses the order’s item descriptions when present; the full order and customer details are not returned.

**Our recommendation:** Keep the wallet-specific label/icon response describing the payable order. If a merchant needs discreet purchase labels, a configurable merchant-only label could serve that product requirement; the public protocol itself is not a reason to remove useful item descriptions.

**Why / tradeoff:** Wallets need limited public metadata for the order they were given. The handler does not expose a full order/customer record, and it verifies a currently payable Solana attempt.

**Code:** [internal/http/handlers/solana_pay.go:45](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/solana_pay.go#L45) · [internal/modules/checkout/order_solana.go:224](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/checkout/order_solana.go#L224) · [internal/http/routes/orders.go:44](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/orders.go#L44)

### 164. `POST /v1/orders/{id}/solana-pay`

**Keep · No change needed**

**Current:** A wallet supplies its account and receives an unsigned payment transaction. The service resolves the current Solana attempt and, after an expired quote, checks for a landed or possibly in-flight transfer before preparing another attempt.

**Our recommendation:** Keep the wallet request shape and the quote/recheck safeguards. Document that this prepares payment rather than proving it paid; customer order status remains the recovery/read model.

**Why / tradeoff:** A fresh transaction can legitimately differ when a quote expires. Applying ordinary static HTTP response replay indiscriminately here would fight the wallet flow. No route migration or admin permission belongs on this public protocol surface.

**Code:** [internal/http/handlers/solana_pay.go:72](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/solana_pay.go#L72) · [internal/modules/checkout/order_solana.go:238](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/checkout/order_solana.go#L238) · [internal/http/routes/orders.go:46](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/orders.go#L46)

## SCIM provisioning routes — 12

### 165. `GET /v1/app/scim/v2/ServiceProviderConfig`

**Keep · No change needed**

**Current:** Provisioning authentication protects a SCIM capability document advertising supported PATCH, bounded bulk/filter operations, and unsupported sorting/ETags. It uses SCIM media/error shapes rather than billing envelopes.

**Our recommendation:** Keep the protocol-specific document and its existing documentationUri link to the implemented subset. The linked guide already distinguishes contact active state from billing access.

**Why / tradeoff:** A directory integrator must not assume deactivation stops billing. These semantics belong beside discovery and setup docs; replacing SCIM envelopes with the ordinary billing contract would break its intended consumers.

**Code:** [internal/scim/discovery.go:12](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/scim/discovery.go#L12) · [internal/scim/scim.go:131](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/scim/scim.go#L131) · [docs/customer-contacts.md:68](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/docs/customer-contacts.md#L68) · [internal/http/routes/provisioning.go:23](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/provisioning.go#L23) · [internal/scim/discovery.go:18](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/scim/discovery.go#L18)

### 166. `GET /v1/app/scim/v2/ResourceTypes`

**Keep · No change needed**

**Current:** Returns the supported User resource type and its endpoint/schema under provisioning authentication. The implementation does not advertise a general directory of billing entities.

**Our recommendation:** Keep the small discovery collection and current capitalization/protocol shape.

**Why / tradeoff:** This is an external provisioning protocol boundary, so consistency with its consumers is more useful than forcing lowercase billing-resource conventions. Add types only when a real provisioned resource exists.

**Code:** [internal/scim/discovery.go:42](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/scim/discovery.go#L42) · [internal/http/routes/provisioning.go:25](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/provisioning.go#L25)

### 167. `GET /v1/app/scim/v2/ResourceTypes/{id}`

**Keep · No change needed**

**Current:** Returns the User resource-type document for a recognized identifier and a SCIM not-found response otherwise.

**Our recommendation:** Keep the direct discovery read and ensure it stays consistent with ResourceTypes.

**Why / tradeoff:** The individual resource supports provisioning clients that follow discovery links. There is no justified route migration or additional payload here.

**Code:** [internal/scim/discovery.go:52](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/scim/discovery.go#L52) · [internal/http/routes/provisioning.go:27](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/provisioning.go#L27)

### 168. `GET /v1/app/scim/v2/Schemas`

**Keep · No change needed**

**Current:** Returns the implemented User schema. The contact mirror uses the host’s UUID externalId as customer identity, stores one selected email/display name, and treats active as displayed directory information.

**Our recommendation:** Keep this deliberately restricted User schema and its existing field descriptions. It already explains host UUID identity, selected email and display-only active; provisioning examples should preserve those semantics.

**Why / tradeoff:** Directory clients often carry richer profiles than billing needs. Explicitly describing what is retained avoids mistaken expectations without expanding OpenRails into an identity-management service.

**Code:** [internal/scim/discovery.go:105](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/scim/discovery.go#L105) · [docs/customer-contacts.md:68](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/docs/customer-contacts.md#L68) · [internal/http/routes/provisioning.go:29](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/provisioning.go#L29) · [internal/scim/discovery.go:79](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/scim/discovery.go#L79)

### 169. `GET /v1/app/scim/v2/Schemas/{id}`

**Keep · No change needed**

**Current:** Looks up the implemented User schema by identifier and returns a protocol error for an unknown schema.

**Our recommendation:** Keep this direct schema lookup and its shared schema source.

**Why / tradeoff:** Discovery references should resolve to the same document the collection contains. No custom billing wrapper or new versioning mechanism is necessary for the current single-schema implementation.

**Code:** [internal/scim/discovery.go:115](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/scim/discovery.go#L115) · [internal/http/routes/provisioning.go:31](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/provisioning.go#L31)

### 170. `POST /v1/app/scim/v2/Users`

**Refine · Low priority**

**Current:** Creates a provisioned contact at the host’s UUID externalId, ensuring the billing customer exists. userName is required/unique, already-provisioned identities conflict, and successful creation returns 201 with Location. A contact update does not transfer ownership of billing history.

**Our recommendation:** Keep stable host identity and protocol-native conflict recovery. An optional lost-response example can show GET by externalId before retrying creation; the existing UUID identity and filter already support this without adding billing-style Idempotency-Key.

**Why / tradeoff:** An integration that loses the first response should recover the same customer, not invent another externalId. The existing deterministic identity and lookup support this without adding billing-style Idempotency-Key to SCIM.

**Code:** [internal/scim/users.go:370](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/scim/users.go#L370) · [internal/scim/users.go:517](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/scim/users.go#L517) · [internal/http/routes/provisioning.go:33](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/provisioning.go#L33)

### 171. `GET /v1/app/scim/v2/Users`

**Keep · No change needed**

**Current:** Lists provisioned contacts with startIndex/count, totalResults and one supported equality filter on id/externalId/userName/email. It bounds counts and rejects unsupported filter forms rather than silently ignoring them.

**Our recommendation:** Keep SCIM offset paging and explicit refusal of unsupported filter forms. The setup guide already lists supported equality filters; broader search is a product requirement, not missing general billing cursor consistency.

**Why / tradeoff:** Billing cursor consistency is not a good reason to break a provisioning protocol. The constrained search is adequate for identity matching, with any broader filter support driven by actual directory-client needs.

**Code:** [internal/scim/list.go:40](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/scim/list.go#L40) · [internal/scim/list.go:74](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/scim/list.go#L74) · [internal/http/routes/provisioning.go:35](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/provisioning.go#L35) · [docs/customer-contacts.md:77](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/docs/customer-contacts.md#L77)

### 172. `GET /v1/app/scim/v2/Users/{id}`

**Keep · No change needed**

**Current:** Returns only a provisioned contact for the authenticated merchant; a billing customer row alone does not make a SCIM User readable. The resource uses the same stable host/customer UUID.

**Our recommendation:** Keep the distinction between directory-provisioned contact and billing identity, together with the existing limited representation and documentation.

**Why / tradeoff:** Provisioning state and billing existence answer different questions. Exposing every billing customer as a directory User would obscure whether the directory actually manages that contact.

**Code:** [internal/scim/users.go:500](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/scim/users.go#L500) · [internal/scim/users.go:554](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/scim/users.go#L554) · [internal/http/routes/provisioning.go:37](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/provisioning.go#L37)

### 173. `PUT /v1/app/scim/v2/Users/{id}`

**Refine · Low priority**

**Current:** Replaces the contact profile under a row lock, keeps externalId immutable, and can ignore an older directory report using its supplied modification time. Omitted replaceable attributes take replacement defaults rather than PATCH semantics.

**Our recommendation:** Keep replacement semantics and the documented newest-wins rule. A stale-replacement example showing retained current values in a 200 would be optional integration guidance, not an absent concurrency safeguard.

**Why / tradeoff:** A caller needs to read the returned resource rather than assume every submitted field won. Do not silently turn PUT into a merge to avoid this; any stronger revision protocol would require directory-client compatibility work.

**Code:** [internal/scim/users.go:433](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/scim/users.go#L433) · [internal/http/routes/provisioning.go:39](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/provisioning.go#L39)

### 174. `PATCH /v1/app/scim/v2/Users/{id}`

**Refine · Medium priority**

**Current:** Applies a parsed sequence of operations atomically to the stored contact. Supported fields can be removed, but unsupported attributes are silently ignored; email value-path filters select the single stored email without evaluating the filter expression.

**Our recommendation:** Keep atomic operations and the documented limited contact model, including ignored unretained attributes. Specifically document that emails[…].value addresses the one stored email rather than evaluating the bracket condition. If supported provisioners depend on conditional targeting, test them before rejecting unsupported expressions or implementing their meaning; do not expand the model or reject harmless extension attributes automatically.

**Why / tradeoff:** The supported field subset already appears in documentation and discovery. The narrower ambiguity is conditional email targeting; tightening it is a compatibility decision, not evidence that a complete directory model is required.

**Code:** [internal/scim/patch.go:70](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/scim/patch.go#L70) · [internal/scim/patch.go:73](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/scim/patch.go#L73) · [internal/scim/users.go:460](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/scim/users.go#L460) · [internal/http/routes/provisioning.go:41](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/provisioning.go#L41)

### 175. `DELETE /v1/app/scim/v2/Users/{id}`

**Keep · No change needed**

**Current:** Erases the provisioned contact values while retaining the billing customer/history and a timestamped row to prevent older directory reports from restoring erased data. It returns 204 after the contact operation.

**Our recommendation:** Keep the already documented contact-only erasure and timestamp protection. Billing cancellation, payment authority and financial history remain separate explicitly authorized workflows.

**Why / tradeoff:** A directory delete is not automatically an instruction to reverse financial obligations. If the product wants deprovisioning to trigger another workflow, specify and authorize that separately rather than adding a silent cascade.

**Code:** [internal/scim/users.go:486](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/scim/users.go#L486) · [internal/scim/users.go:626](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/scim/users.go#L626) · [internal/http/routes/provisioning.go:43](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/provisioning.go#L43) · [docs/customer-contacts.md:80](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/docs/customer-contacts.md#L80)

### 176. `POST /v1/app/scim/v2/Bulk`

**Refine · Low priority**

**Current:** Processes bounded User operations sequentially in separate transactions, returns per-operation outcomes, supports references to earlier successful bulk creates, and can stop after failOnErrors. Earlier successes remain committed.

**Our recommendation:** Keep the per-operation protocol result and documented partial commits. A retry example can help callers recover successful creates before resubmitting remaining operations; it is optional guidance, not a missing transaction or replay mechanism.

**Why / tradeoff:** Treating HTTP 200 as all operations succeeding or retrying the whole document blindly is the integration risk. A new global transaction would change failure semantics and increase lock time without improving the intended provisioning flow.

**Code:** [internal/scim/bulk.go:30](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/scim/bulk.go#L30) · [internal/scim/bulk.go:92](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/scim/bulk.go#L92) · [internal/http/routes/provisioning.go:45](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/provisioning.go#L45)

## Provider webhook route — 1

### 177. `POST /v1/webhooks/{rail}/{account_id}`

**Keep · No change needed**

**Current:** Provider account identity resolves merchant scope, and rail-specific authenticity/source checks precede dispatch. Stripe thin events hydrate with the merchant’s credentials. Dispatch can perform synchronous work or enqueue domain convergence, with transient failures requesting redelivery. The acknowledgement is not a customer payment result.

**Our recommendation:** Keep the account-qualified ingress, provider redelivery contract and existing domain convergence jobs. Consider a verified raw-event inbox only if measured acknowledgement latency or recovery requirements justify another durable boundary; its absence is not a present defect.

**Why / tradeoff:** The current handler can wait on upstream reads and business processing. A durable inbox could shorten acknowledgement latency, but adds storage/recovery machinery and should follow measured delivery needs. Preserve account binding, authenticity and rail-specific response expectations.

**Code:** [internal/http/handlers/webhook.go:66](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/webhook.go#L66) · [internal/http/handlers/webhook.go:120](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/handlers/webhook.go#L120) · [internal/modules/webhooks/dispatcher.go:110](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/modules/webhooks/dispatcher.go#L110) · [internal/http/routes/psps.go:40](https://github.com/open-rails/openrails/blob/17ccbcf6a100cc762bb6cf79f37047b8df7696a3/internal/http/routes/psps.go#L40) · [internal/modules/webhooks/dispatcher.go:164](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/modules/webhooks/dispatcher.go#L164)

## Running-charge routes — 4

Running charges bill elapsed time; they are distinct from a request admission/capture and from provider-evidence settlement.

### 178. `POST /v1/app/running-charges`

**Keep · No change needed**

**Current:** An application with Usage permission starts a merchant-scoped charge at a rate per hour. A merchant-unique host reference identifies immutable charging terms; an exact domain retry returns the existing charge, while changed terms conflict. minimum_amount is an optional capacity gate, not a reservation or an immutable term. The response separates charged_amount from posted_amount and includes as_of.

**Our recommendation:** Keep the dedicated resource and caller reference. The contract already explains start time, rounding, capacity gating and the host’s responsibility to stop its work. Use GET for a fresh projection: retained HTTP replay returns the original response, whereas a domain replay can calculate current accrued spend.

**Why / tradeoff:** A charge accruing over hours or days has a different lifecycle from one captured request or provider-settled operation. There is already a complete start/read/stop workflow, so a generic operation wrapper or extra status route would duplicate it.

**Code:** [internal/http/routes/metering.go:35](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/http/routes/metering.go#L35) · [internal/http/handlers/running_charges.go:16](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/http/handlers/running_charges.go#L16) · [internal/modules/money/running_charge.go:91](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/modules/money/running_charge.go#L91) · [billing/running_charges.go:48](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/billing/running_charges.go#L48) · [running_charges.go:30](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/running_charges.go#L30)

### 179. `POST /v1/app/running-charges/{reference}/stop`

**Refine · High priority**

**Current:** A Usage-authorized application stops the merchant’s charge at an explicit instant or now. The transaction locks the customer, validates the stop against start and prior posting times, posts the remaining amount and returns the final charge. An already stopped charge replays when the instant matches or is omitted; a different explicit instant conflicts.

**Our recommendation:** Keep the stop command and final receipt. Apply the shared replay-fingerprint correction from recommendation 1: the concrete reference must participate in identifying an HTTP request. The existing GET already supports observing the result.

**Why / tradeoff:** Domain identity and posting are protected, but the outer HTTP replay can return the first reference’s response when a caller reuses the same key and body on another reference. This is the same shared finding as capture/provider-operation mutations, not a separate flaw in the running-charge ledger. Do not replace stopping with a generic status PATCH.

**Code:** [internal/http/routes/metering.go:38](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/http/routes/metering.go#L38) · [internal/http/handlers/running_charges.go:39](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/http/handlers/running_charges.go#L39) · [internal/modules/money/running_charge.go:208](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/modules/money/running_charge.go#L208) · [internal/modules/money/running_charge_posting.go:62](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/modules/money/running_charge_posting.go#L62) · [internal/http/routes/app_requests.go:55](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/http/routes/app_requests.go#L55)

### 180. `GET /v1/app/running-charges/{reference}`

**Keep · No change needed**

**Current:** A Usage-authorized application reads the charge by its merchant-scoped reference. A running charge projects accrued total as of the current clock without posting it; a stopped charge returns the final amount and stop instant. Unknown references return running_charge_not_found.

**Our recommendation:** Keep the explicit as_of, charged_amount and posted_amount fields. This is the narrow programmatic read that the running-charge workflow needs, already using the same permission as its commands.

**Why / tradeoff:** Callers can distinguish calculated spend from money already written to the ledger. A new polling resource or moving this read to admin would add complexity without a demonstrated need.

**Code:** [internal/http/handlers/running_charges.go:61](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/http/handlers/running_charges.go#L61) · [internal/modules/money/running_charge.go:282](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/modules/money/running_charge.go#L282) · [internal/modules/money/running_charge.go:385](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/modules/money/running_charge.go#L385) · [billing/running_charges.go:87](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/billing/running_charges.go#L87)

### 181. `GET /v1/app/running-charges`

**Keep · No change needed**

**Current:** A Usage-authorized application lists merchant charges newest first with a cursor and customer_id/state/depleted filters. depleted=true selects running charges whose customer’s stored depletion forecast has passed; it does not run a fresh capacity decision for every result. The documented flow rechecks GetBalance before stopping host work.

**Our recommendation:** Keep the bounded collection and the documented balance recheck. Preserve the distinction between discovering candidates and deciding whether to stop work, including when balance changes are waiting for the watcher to refresh its forecast.

**Why / tradeoff:** This is useful state discovery alongside balance events, not an unnecessary second delivery mechanism. The host owns stopping its resource; the API already makes that boundary explicit. No additional route or synchronous scan is justified by this review.

**Code:** [internal/http/handlers/running_charges.go:82](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/http/handlers/running_charges.go#L82) · [internal/modules/money/running_charge.go:312](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/modules/money/running_charge.go#L312) · [internal/db/queries/running_charges.sql:116](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/internal/db/queries/running_charges.sql#L116) · [docs/running-charges.md:112](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/docs/running-charges.md#L112) · [running_charges.go:69](https://github.com/open-rails/openrails/blob/440cda618c231a16cbacda210fd2b85b4988762e/running_charges.go#L69)


## Verification and limits

The route inventory was matched to every review entry: 181 unique keys, no missing routes and no duplicate assessments. All source references were checked against the pinned snapshot for file existence and valid line locations. The 10 generated network routes received separate assessments of their native-account, active-link and recent-sign-in gates, as well as their shared customer handlers.

We checked route declarations, handlers, permissions, request and response types, relevant service/database behavior, and the available Go and TypeScript callers. The duplication review also compared shared handlers, OpenAPI shapes and SDK/admin usage. Recommendations account for existing safeguards, documentation and recovery paths.

**Executed checks:**

- At `440cda618c231a16cbacda210fd2b85b4988762e`, the full `./internal/http/routes` and `./internal/contract` tests passed. These cover route declarations/mounting and generated-contract consistency, not the running-charge HTTP/database lifecycle.
- At `17ccbcf6a100cc762bb6cf79f37047b8df7696a3`, the full route, contract and metrics tests passed, along with focused subscription authentication/setup-binding, customer-gate, catalog, billing, merchant, copilot and archive checks. These results apply to that commit. `./internal/scim` has no test files.
- Admin behavior was assessed through source and existing test inspection; no admin runtime tests were executed for this report.

These checks support the inspected contracts; they are not a route-by-route live acceptance suite. No database integration environment, real PSP, Solana chain, provisioning directory, browser journey or deployment was exercised. Findings that depend on those systems are source-grounded recommendations, with no claim of observed production impact.

**Scope:** Recommendations only. No application code, dependencies, migrations or production systems were changed.
