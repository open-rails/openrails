# Durable request admission

How admissions and their holds behave. The routes are `/v1/app/admissions`
([routes](api/routes.md)), your backend's own: they take an application's
credential and an `Idempotency-Key` on each write. The Go methods are `Admit`,
`CaptureAdmissions`, `ReleaseAdmissions` and `ExtendAdmissions`. Each takes up
to 1,000 items and answers one result per item, decided on its own.

## Priced from the catalog

An admit item states the usage it expects, `usage: {event_type, dimensions,
metadata}` (a call: `{requests: 1}`; an LLM request: `{input_tokens: <prompt>,
output_tokens: <max_tokens>}`). The rate cards of its event type's meters price
it at the customer's current period quantities, without advancing them, as
[usage pricing](usage-pricing.md) prices a recorded event; the price is the
hold and the answer's `estimated_amount`. The cards that priced it are pinned
with the admission: its capture pays the price it was admitted at, whatever
the catalog says later. `usage` and a host-stated `estimated_amount` are
exclusive; with neither the admission holds nothing. An event type no rate
card prices, or a matrix gap, is `422 usage_not_priced`.

A capture item (`POST /v1/app/admissions/capture`) names its request and the
usage the work consumed, `{request_id, usage: {event_type, dimensions,
metadata, resource, source, source_id}}`; its event type is the admitted one
(`400 invalid_param` otherwise). The pinned cards price it (the cards in force
for an admission that pinned none), advancing the customer's period
quantities, and the usage is recorded as an event with its `charges`. The hold
leaves the held total and the charges post in one transaction: balance first,
the rest owed. Priced above the hold, the whole charge posts and the excess is
owed, never clamped; bound it by the estimate (`max_tokens`). An `amount`
instead settles at a host-stated price, recording the usage when it is named.

The receipt is `request_id`, `customer_id`, `currency`, `estimated_amount`,
`amount` (what was charged), `charges`, `usage_event_id`,
`balance_transaction_id` (the first charge's posting; null when nothing was
charged) and `replayed`.

## In the host's transaction

Embedded, `AdmitTx`, `CaptureAdmissionsTx` and `ReleaseAdmissionsTx` take a
transaction from the host's pool on the engine's database: the host's request
row and its hold, or its finished state and its capture, commit together. No
outbox, no orphan hold, no ambiguous admit. OpenRails never commits or rolls
the transaction back; after an error the host rolls back.

## Behaviour

Malformed request IDs (empty or over 255 UTF-8 bytes), negative estimates and negative prospective rates return structured 400 errors before account creation or money locking.

Every request binds one immutable `(merchant_id, request_id)` operation to its customer, unit, estimate, original deadline and request terms. Terms record invoker/type, caller-supplied trust/roles, resource, source and prospective accrual rate. Normalize role ordering and unit spelling before comparing. A repeated key with different terms conflicts. An exact retry never extends a deadline or opens another reservation. Zero estimates still enforce delegation and prospective-rate policy; only zero estimates may omit a deadline.

Replayed admission returns the original allowed decision and capacity receipt, plus `replayed=true` and current `state` (`open`, `expired`, `released`, `captured`). `allowed` alone is not permission to start new work after a terminal replay; state identifies a currently live reservation. Expiry is derived from the declared deadline, without a worker.

An admitted request is kept 61 days: the longest spend window (31 days) plus 30. A hold's deadline, as admitted or as extended, is at most 30 days past its admission, and a spend window is at most 31 days, so nothing a kept request still counts toward outlives it. `admission_operations` is partitioned by month on `admitted_at` and whole months are dropped; after 61 days a request id reads as never admitted, and a late capture of it answers `admission_not_found`. A request id is unique per merchant among kept requests: admission serializes on it.

Request reservations and hard spend windows use PostgreSQL under the existing customer money lock. Redis remains available for rate limits and abuse signals; loss of Redis cannot erase monetary reservations, alter request ownership or reset a hard spend window. There is no compatibility fallback from missing request state to caller-supplied capture coordinates.

| Record | Capacity reserved | Original admission-window usage |
| --- | --- | --- |
| Open, deadline in the future | Estimated amount | Estimated amount |
| Open, deadline passed | Zero | Estimated amount until definite release or capture |
| Released | Zero | Zero |
| Captured | Zero | Actual captured amount |

Capture accounts to the original admission window, including a late capture after expiry or release. It updates actual usage atomically with the ledger charge, and subsequent admissions see that total. An actual charge above its estimate may consume the remaining window or create owed money; it is recorded rather than discarded. Captured money cannot be erased by release. The first capture fixes its terms (event type, resource, source/id, dimensions, metadata and a host-stated amount) and its charge. Exact retries recover the original receipt, even after a price change; changing or adding terms conflicts (`idempotency_key_reused`). If a usage event is requested, its insert shares the capture transaction, so any insert failure rolls back the ledger, state and window update. A usage identity already claimed by another request conflicts; it never silently drops the new usage evidence.

Extension applies only to a still-open, unexpired request and moves its effective deadline later. The original requested deadline remains part of the immutable request identity. A late worker uses the original operation to record consumed work; it cannot manufacture a new authorization by replaying an expired request with different terms.

Window identity includes merchant, customer, applicable scope/invoker and stable policy key. Limits and duration are not part of identity, so changing them re-evaluates the relevant history without clearing it. Store the exact applicable window keys with admission; fixed-window boundaries remain staggered per customer. Indexed SQL reads sum open estimates and captured amounts in the bounded original window and exclude released requests. There are no mutable financial counter mirrors or repair workers.

All spending paths use one held total: live request estimates, pending refunds of purchased credit, and what the customer's [running charges](running-charges.md) have accrued and not yet posted to the ledger, computed as of the read. Each is counted once. Capture transitions its own request out of held state before posting the charge in the same transaction, preserving every other reservation; a running charge's posting does the same with what it posts.

The separate accrual-rate policy compares observed, reported usage with the requested prospective delta. Zero-estimate requests still pass this policy. It is not a strict instantaneous concurrency quota: newly started work can be ahead of the usage meter; work billed by the hour is a [running charge](running-charges.md). Durable request spend windows do not claim to solve that separate policy.

Capture, release and extension resolve the durable customer and enforce the credential's customer scope before mutation. Financial application code remains identical through the shared embedded and remote Client.
