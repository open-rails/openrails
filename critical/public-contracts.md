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

Implementation and qualification are in progress. No passing count or
whole-SDK coverage is claimed by this ownership checkpoint. The parent
critical-suite branch retains the shared fixtures, runner, and final gate.
