# Catalog checkout admission audit (#1051)

Status: implementation plan. This records source findings at
`579653413a0b278e53003874904ed2069e4545a6`; it does not declare the missing
invariants implemented. The chosen HTTP integration is the thin host wrapper.

## Existing reusable contract

`Client.CreateCheckoutSession` already accepts an opaque price key in `PriceID`.
`catalog.ResolveReference` resolves it within the selected merchant. `Price.Key`
names the active member of an immutable price history; `Product.Key` can name
the host's opaque resource without a billing foreign key to host tables.
`Client.Catalog.Apply` supplies atomic catalog writes with revision checks and
application replay. `TestCatalogApplicationRollbackAndPriceHistory` covers a
price-key move, archive and deliberate reactivation of historical terms.

This is sufficient to express a current offer without adding a product-selector
API: the host maps its resource to a stable product and price key. Billing owns
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

The model documents `AccessDurationHours == nil` as perpetual ownership and a
positive duration as a finite access window. Credits use their separate deposit
and ledger operation. That documentation is useful evidence, but an explicit
product repeat-purchase policy may still be needed if real catalog consumers
use perpetual prices for repeatable goods. Do not apply a blanket
already-owned prohibition to finite access, subscriptions or credit deposits.

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
