# Catalog checkout admission audit (#1051)

Status: implementation and qualification in progress. Initial source findings at
`579653413a0b278e53003874904ed2069e4545a6` are distinguished below from the
implementation. The chosen HTTP integration is the thin host wrapper.

## Existing reusable contract

The core catalog resolver already supports opaque keys, but the original
merchant service rejected non-ID `PriceID` inputs. This change supplies explicit
`PriceID` or `PriceKey` selectors end to end instead of overloading an ID. `Price.Key`
names the active member of an immutable price history; `Product.Key` can name
the host's opaque resource without a billing foreign key to host tables.
`Client.Catalog.Apply` supplies atomic catalog writes with revision checks and
application replay. `TestCatalogApplicationRollbackAndPriceHistory` covers a
price-key move, archive and deliberate reactivation of historical terms.

This is sufficient to express a current offer using one checkout operation: the host maps its resource to a stable product
and price key. Billing owns
resolution, active product/price checks, and the commercial amount. A merchant
selector chooses scope; it does not grant authority. Customer identity comes
from the verified host or the authenticated customer route, never unchecked
browser customer fields.

## Source findings

| Invariant | Current behavior | Remaining work |
| --- | --- | --- |
| Available current offer | `CheckoutSessionService.createSessionWithValidation` and `CheckoutService.Checkout` resolve a price and check both price and product `IsPurchasable`. | Exercise stale UUID/key refusal and concurrent catalog edits through both public transports. |
| Replay after purchase | `CreateSession` reads a successful replay-cache result before catalog validation; NMI sale admission rechecks its durable key under the customer lock before eligibility. | Database-only session resume currently resolves the current key and routing before locating the original session. A reprice/archive can reject an accepted attempt. |
| Permanent ownership | `CheckPurchaseEligibility` and `GetUserProductCoverage` inspect subscription and entitlement windows. `ProductAccessService` is used only as a grant writer by this service. | An ownership-only product with no entitlement spec does not contribute coverage. Decide the explicit repeat-purchase contract before broadening this check. |
| Different-key concurrency | NMI intent admission takes `LockCustomerForSpend` and checks `GetUnresolvedSaleForCustomerProduct` before accepting an independent sale. Stripe creates hosted sessions with independent request keys. | Preserve the NMI intent guard; coordinate session and intent admission across providers for permanent ownership. Use existing sessions/intents for exclusion, not a second purchase ledger. |
| Immutable accepted benefits | NMI `prepareAcceptedSale` freezes amount, currency, entitlement spec and ownership windows in its existing sale payload. | Stripe `handleCheckoutSessionCompleted` calls `RegisterPurchase`, which reloads the current product entitlement spec. Immutable price amounts alone do not freeze purchased benefits. |
| Provider uncertainty | Existing NMI intent execution and Stripe provider idempotency are the recovery mechanisms. | New admission exclusion must remain held while a prior provider outcome is uncertain. Wall-clock checkout expiry alone does not prove an unknown provider session unpayable. |
| Refund and history | Product ownership and feature grants use the existing grant/payment lifecycle. Historical prices remain addressable. | Verify refund revocation permits the intended next purchase while the original accepted key still replays its original result. |

The existing model defines a non-recurring price with `AccessDurationHours == nil`
as perpetual ownership. That precise contract scopes the new ownership guard.
Finite access and subscriptions retain their existing eligibility rules; credit
deposits use a separate ledger operation. No product-policy schema is added.

## Chosen integration and implementation sequence

The browser calls the blog server. The blog verifies the user and performs its
live channel/content checks, then calls `Client.CreateCheckoutSession` once.
Embedded and remote clients share the operation. The wrapper does not precheck
purchased access or accept a browser amount/price as commercial authority.
The generic public checkout route must not bypass this wrapper; the authorized
merchant SDK operation remains available. No billing callback to the blog,
signed checkout permit, AuthKit dependency, or billing reads of blog tables are
introduced. This work is separate from #1050's authentication interfaces.

1. Query the existing ownership ledger for durable one-off eligibility.
   The existing public price contract defines non-recurring, nil-duration access
   as perpetual ownership. Finite access, subscriptions and credit deposits keep
   their existing behavior.
2. Admit permanent-purchase sessions under the existing customer mutex; check
   existing ownership and unresolved sessions/intents across providers. Preserve
   NMI's existing unresolved-sale guard. Unknown provider outcomes retain their
   exclusion until authoritative resolution; local time passing is insufficient.
3. Resolve the durable session and compare the buyer-bound request fingerprint
   before mutable catalog or routing lookup. Store accepted one-off terms in
   the existing session state and use them when resuming.
4. Bind Stripe settlement to the local session and use its immutable benefit
   snapshot with the existing atomic payment/grant writer. Preserve legacy
   provider observations without an accepted session and recurring engine flows.
5. Prove concurrency, database-only replay, reprice/archive, settlement and
   refund behavior with real PostgreSQL and deterministic providers, then qualify
   the same thin SDK operation through embedded and remote transports.

## Ownership and qualification

Core owner: `/root/astra_checkout_core`.
Worktree: `/home/fidika/cozy/.worktrees/openrails/1051-wrapped-checkout-20260923`.
Branch: `feat/1051-wrapped-checkout-20260923`.
Base: `579653413a0b278e53003874904ed2069e4545a6`.

Proposed core files: checkout admission/session/purchase modules, narrowly
required catalog or grant queries, Stripe settlement snapshot integration, and
their real PostgreSQL/deterministic-provider regressions. Consumer/demo edits use the chosen wrapper and existing public operation. Existing #1042 runtime,
#1050 authentication and Stripe qualification lanes remain independently owned.

Required proofs include buyer/merchant isolation, product-price scope,
reprice/archive versus accepted replay, permanent versus repeatable purchase,
different-key concurrency, unknown-provider retry, accepted benefit retention,
refund revocation and embedded/remote transport parity. No live provider charge,
refund, schedule mutation or deployment is part of this qualification.

## Explicit catalog selectors

`CreateCheckoutSessionRequest` requires exactly one `PriceID` or `PriceKey`.
`PriceID` retains the typed `price_<uuid>` contract. `PriceKey` is always an
opaque catalog key, even when its text looks like a UUID. This is a hard cut:
move old keys from `price_id` into `price_key`; do not resolve the key in the
host first. The request fingerprint includes the selector, so changing from
key to ID under one accepted idempotency key conflicts even if both currently
resolve to the same offer. Empty new key fields do not alter existing ID-only
request fingerprints.

`ListCheckoutRailOptions` continues to take an ID;
`ListCheckoutRailOptionsByKey` takes a key. HTTP checkout-options and routing
dry-run accept exactly one `price_id` or `price_key`. Catalog `RetrieveByKey`
methods remain explicit. Product-access and price-creation product selectors
are delivered by the coordinated #1051 product-selector lane. Creation `Key`
fields still declare a handle; they are not alternate IDs.

Recurring change-tier/preview and Solana tier-change new-offer key selectors
are a separate linked #1051 follow-up because their accepted-term replay and
financial lifecycle need their own qualification. Historical subscription,
payment, import and accepted-operation coordinates remain immutable IDs.

## Recovery and qualification evidence

Hosted Stripe dispatch is claimed in the existing session immediately before
HTTP submission, after local validation. A process crash or unknown transport
result cannot issue another hosted create merely because the provider's
idempotency retention or the local TTL has elapsed. A known pre-dispatch
validation failure closes only its never-submitted attempt atomically.
Verified Stripe expiration/failure can release the exclusion; payment and
provider closure cannot be overwritten by a late handler response. Accepted
terms, dispatch and closure facts survive stale session-state writes.

The core PostgreSQL regressions cover different-key concurrency, buyer binding,
DB-only replay, crash-before-submit, archived accepted terms, unknown provider
retention, provider-verified closure, original benefit settlement, ownership-only
duplicate eligibility, refund revocation/rebuy, cross-provider NMI exclusion,
stale state and a webhook winning the create-response race. The public Client
regression exercises embedded and actual HTTP transports with catalog apply,
reprice, old-price refusal, explicit UUID-shaped keys and archive replay.
These proofs use local provider transports and owned test databases only.

## Resource offers and thin host replay

A resource key such as `post:<stable-id>` identifies access independently of the
product key. Several products can grant that resource through `EntitlementsSpec`;
one product can grant several resources. `ListOffersForEntitlements` performs an
exact reverse lookup of active products and prices for up to 100 keys in one
request and one query. Kind is required (`permanent`, `finite`, or `recurring`),
each key's page caps at 100 with its own cursor, and preferred currency only
changes ordering. Every row retains its native currency, amount, duration
and renewal flag. It never substitutes a monthly plan for permanent access.

`HasEntitlement` and `CheckEntitlements` query only the requested keys in the
existing grant projection. They do not join mutable product contents to invent
access. Permanent bundle admission rejects an already-owned product or benefits
that are all already permanently held/reserved. Partial ownership does not
block a bundle with additional value. Customer-row serialization and accepted
benefit snapshots extend the existing session/intent exclusion across products.
Refunds revoke their own payment sources without erasing independent grants.

The host sends `Entitlement` and `OfferKind` assertions alongside the selected
immutable price ID (or current price key). Admission checks the resource and
commercial kind and freezes the benefits. PPV uses `permanent`; channel membership
uses `recurring`. Membership checks remain host policy; purchased PPV access
survives the membership's end.

A wrapper first calls `LookupCheckoutSession` with the original complete request.
It returns a read-only projection of that buyer's accepted session, even after
host policy/catalog changes, or `ErrNotFound`. A changed request under the same
key returns `ErrIdempotencyKeyReused`. Only on not-found does the host run its live
new-purchase policy and call `CreateCheckoutSession`. Lookup performs no provider
work and does not resume a financial operation. A concurrent creation still
passes through the existing idempotency and admission locks.

Management-only customer routes expose GET `/me/checkout/:id` and POST
`/me/checkout/:id/confirm` for an app-admitted session. Generic checkout creation
remains absent. Confirmation preserves the existing verified interactive-payer
requirement; merchant credentials cannot impersonate that proof.
