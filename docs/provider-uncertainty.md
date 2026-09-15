# Provider mutation uncertainty

A non-idempotent operation may have reached its provider once its durable
attempt has been claimed. After possible submission, an empty successful
search is not proof that it failed. NMI order references correlate an operation;
they do not themselves establish provider-enforced idempotency or definitive
negative/read-after-write semantics.

Checkout sales, recurring enrollment, custodian-proxied sales, manual rebills
and invoice collection therefore retain unknown state until a positive receipt
can be reconciled. Resumed intent handlers verify rather than submit another
mutation. A captured provider receipt survives local finalization failure in
the existing intent result evidence. Local effects are retried against that
same receipt. Query unavailability or delayed visibility never authorizes a
new sale. NMI processor communication/duplicate response codes 420, 421 and 430
also require verification rather than release/resend. Top-up and non-idempotent refund receipt rules follow the same rule.

Known pre-submission refusal (unconfigured/read-only provider, failed local
prerequisite) parks the intent without consuming an attempt. It can be retried
when repaired. A crash between the durable attempt claim and the actual send
is conservatively uncertain; reconciliation is required even if no request
ultimately reached the provider. A parsed definitive decline terminates that
attempt. A provider-idempotent operation, such as the supported Stripe refund
path, may repeat the same provider idempotency key; this is a capability of
that operation, not an inference from an empty search.

Unknown attempts do not expire or become superseded merely because a business
deadline passes. An expired in-flight lease is still reclaimable for
verification. Re-enqueuing the operation cannot reset it to a fresh charge.
Verification retains the existing lease/backoff schedule; it is not a tight
polling loop. Queued operations that were never attempted can expire normally.

There is no evidence-free invoice unpark/force-resend method. The original
attempt and its amount remain durable until a receipt resolves it. No
automatic compensating cancel or refund is triggered by uncertainty.

Local HTTP/PostgreSQL fixtures qualify response loss, delayed receipt
visibility, restart, expiry, positive receipt finalization and one-time local
effects. They do not demonstrate live NMI replication timing or qualify every
processor's duplicate checking. Provider-specific live guarantees remain
separate release evidence.

Provider reference: NMI's [transaction processing](https://docs.nmi.com/reference/transactions-processing)
describes merchant order correlation and processor-dependent duplicate checking;
its [Query API](https://docs.nmi.com/reference/query) does not establish a
terminal-negative guarantee for a missing search result. The engine therefore
uses absence as inconclusive evidence rather than assuming a provider guarantee.
