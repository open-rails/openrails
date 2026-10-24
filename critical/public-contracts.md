# Public contract replacement subset

Tracker: open-rails-tracker #1290. Owner: `/root/review_auth_then_or_contracts`.

This child branch implements the selected public Engine/client lifecycle,
catalog, cart/order, and entitlement contracts: OR-J04-S01 through S06,
OR-J05-S01 through S05, OR-J08-S01 through S04, and OR-J27-S01 through S04.
The browser SDK scope is the 27 methods marked `included-critical-invocation`
in the agreed TypeScript matrix.

All stateful checks use real PostgreSQL, AuthKit, OpenRails, and public reads.
Only external protocol peers, clocks, and network delivery are controlled.
Expected identities, amounts, errors, and windows are literal reviewed
outcomes. Method observation alone is not a behavioral assertion.

The independently named Go scenario owners are:

| Cases | Source | Decisive public observation |
|---|---|---|
| OR-J04-S01, S03, S04 | `public_sdk_test.go` | Built Chromium SDK outcomes, real network cancellation/retry, expired-token refusal and one accepted write. |
| OR-J04-S02, S05, S06 | `public_lifecycle_test.go` | Merchant cache isolation, owner closure versus borrowed resources, and net/http/Gin/Fiber credential/path parity. |
| OR-J05-S01 through S05 | `public_catalog_test.go` | Literal money/provider cents, invalid inputs, revision/manual ownership, archive history and immutable accepted terms. |
| OR-J08-S01 through S04 | `public_cart_test.go` | Exact cart totals, unpaid staff orders, refusal without charges, and real expiry worker preserving settled orders. |
| OR-J27-S01 through S04 | `public_entitlement_test.go` | Overlapping purchase/grant union, surviving sources, exact microsecond boundaries and explicit refund/access policy. |

The SDK's `critical/methods.tsv` records 27 selected methods and their actual
semantic assertions. Seven executable browser cases expand the three SDK Go
scenarios. The result checker requires every selected executable case and
its observed methods, and rejects failures and skips. Real standalone network
linking uses the core owner's server fixture and real OAuth consent; it does
not manufacture ID tokens or link rows.

USD's native precision is six decimals: 9.99 USD times three is 29,970,000
native units, settled by the card provider as 2,997 cents. The early design
scenario title's “2997 native units” is a unit-label error, not a reason to
change money semantics.

Expiry tests explicitly drive the registered worker through River's public
host API. They establish due-work behavior, not periodic scheduler cadence.
An unpaid order refuses payment at its deadline before a sweep materializes
its expired status. An upgrade can return a successor subscription ID; the
SDK follows that returned identity and reads its resulting plan.

Implementation and final qualification remain in progress. No whole-SDK or
full 148-scenario claim is made by this child branch. Root owns the complete
suite, required CI, merge, and release. Local receipts live in the task's Git
administrative directory; the parent runner retains the final required gate.
