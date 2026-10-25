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

The routes are `GET /v1/me/orders/{id}/payment-attempt` and the merchant-scoped
`GET /v1/app/orders/{id}/payment-attempt`, both taking the original
`Idempotency-Key` header. The Go client exposes `GetOrderPaymentAttempt`; embedded
and remote clients use the same contract. Mapped customer routes preserve the
original authenticated actor and selected customer.

The receipt describes a purchase payment, including the initial order of a
subscription. Existing renewal and usage collection mechanisms remain separate.
A declined attempt stays declined after another key pays the order. Its intended
method UUID is not a saved card receipt: `payment_method_id` is present only for
the exact succeeded operation that retained the card. No provider operation is
started when reading the receipt.

The focused acceptance journey passes with real first-party HTTP/Go APIs and an
independent provider ledger: original decline versus later success, embedded and
remote client parity, foreign customer/merchant refusal, mapped customer reads,
missing keys and repeated side-effect-free reads. Combined contract generation
and the final required gate belong to the canonical extraction/custody PR.
