# Exact order-payment receipts

A current order view can show payment from a different checkout attempt. It
cannot prove which saved method funded the caller's original request after a
lost response. This slice adds a read-only receipt for an exact order and its
original payment idempotency key, using the existing durable attempt/operation.

The customer route requires the same owned-order authority as other customer
reads. The merchant route requires scoped order permission. Neither lookup
initiates, retries, confirms or changes payment. Unknown keys remain not found
even when another attempt paid the order. No provider token, card data, internal
request fingerprint or raw provider payload becomes part of the public receipt.

The acceptance journey will use real first-party HTTP/Go APIs and an independent
external-provider ledger to verify exact attribution, foreign-customer/merchant
refusal, distinct keys and side-effect-free repeated reads. Source remains draft
until that journey and the combined contract checks pass.
