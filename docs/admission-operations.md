# Durable request admission

Implementation contract for issue989; replaces the Redis hold/pointer authority before v1.

Every request binds one immutable `(merchant_id, request_id)` operation to its payer, unit, estimate, original deadline and request terms. Terms record invoker/type, caller-supplied trust/roles, resource, source and prospective accrual rate. Normalize role ordering and unit spelling before comparing. A repeated key with different terms conflicts. An exact retry never extends a deadline or opens another reservation. Zero estimates still enforce delegation and prospective-rate policy; only zero estimates may omit a deadline.

Replayed admission returns the original allowed decision and capacity receipt, plus `replayed=true` and current `state` (`open`, `expired`, `released`, `captured`). `allowed` alone is not permission to start new work after a terminal replay; state identifies a currently live reservation. Expiry is derived from the declared deadline, without a worker.

Request reservations and hard spend windows use PostgreSQL under the existing payer money lock. Redis remains available for rate limits and abuse signals; loss of Redis cannot erase monetary reservations, alter request ownership or reset a hard spend window. There is no compatibility fallback from missing request state to caller-supplied capture coordinates.

| Record | Capacity reserved | Original admission-window usage |
| --- | --- | --- |
| Open, deadline in the future | Estimated amount | Estimated amount |
| Open, deadline passed | Zero | Estimated amount until definite release or capture |
| Released | Zero | Zero |
| Captured | Zero | Actual captured amount |

Capture accounts to the original admission window, including a late capture after expiry or release. It updates actual usage atomically with the ledger charge, and subsequent admissions see that total. An actual charge above its estimate may consume the remaining window or create owed money; it is recorded rather than discarded. Captured money cannot be erased by release. Repeated captures must carry the same amount.

Extension applies only to a still-open, unexpired request and moves its effective deadline later. The original requested deadline remains part of the immutable request identity. A late worker uses the original operation to record consumed work; it cannot manufacture a new authorization by replaying an expired request with different terms.

Window identity includes merchant, payer, applicable scope/invoker and stable policy key. Limits and duration are not part of identity, so changing them re-evaluates the relevant history without clearing it. Store the exact applicable window keys with admission; fixed-window boundaries remain staggered per payer. Indexed SQL reads sum open estimates and captured amounts in the bounded original window and exclude released requests. There are no mutable financial counter mirrors or repair workers.

Capture returns one shared `CaptureReceipt` in Go and HTTP: request ID, original customer UUID/unit, nonnegative actual amount, optional real ledger-transfer ID, and replay status. A zero-actual receipt creates no ledger transfer. HTTP monetary amounts are decimal strings; Go fields remain `int64`.

All spending paths use one held total: live request estimates plus open provider-operation authorizations. Each is counted once. Capture transitions its own request out of held state before posting the charge in the same transaction, preserving every other reservation. Provider-operation authorizations retain their distinct lifetime and settlement rules.

The separate accrual-rate policy compares observed, reported usage with the requested prospective delta. Zero-estimate requests still pass this policy. It is not a strict instantaneous concurrency quota: newly started work can be ahead of the usage meter, and live rate ownership requires its own provider-obligation contract. Durable request spend windows do not claim to solve that separate policy.

Capture, release and extension resolve the durable payer and enforce the credential's customer scope before mutation. Financial application code remains identical through the shared embedded and remote Client.
