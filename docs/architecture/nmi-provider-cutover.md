# NMI account cutover

Owner: root / resume657cutover. Worktree: `657-nmi-provider-contract-20260918`.
Branch: `design/657-nmi-provider-contract`. Base: `6d213560c`.

One subscriber supplies a replacement payment method already vaulted on the
target account. The merchant or the authenticated owner submits
`POST /v1/merchant/subscriptions/{id}/provider-cutover` (or `/v1/me/...`), with
`Idempotency-Key` and `target_payment_method_id`, `expected_source_psp_id`, and
`expected_target_psp_id`. The latter two are assertions, never routing overrides.
A `/preview` POST validates the same request without creating an operation.
GET with `idempotency_key` reads its durable outcome.

The existing rail intent freezes both PSP UUIDs, customer, subscription UUID,
old provider obligation, target vault, price/plan, amount, currency and paid
period. The operation retains step submission markers and exact receipts in
`result_evidence`. A replay cannot change these inputs or create another intent.

1. POST NMI v5 `/subscriptions` using `customer_vault:{id}`, `plan_id`,
   `paused_subscription:true`, and the frozen future `start_date`.
2. GET that exact target ID and require the frozen vault, plan, amount,
   schedule, next billing date and paused state.
3. DELETE the exact source subscription through its frozen PSP credentials;
   GET proves absence or an identity-matching inactive tombstone.
4. PUT the target with the future anchor and `paused_subscription:false`;
   GET verifies the active target and the same commercial terms.
5. Atomically repoint the local subscription's PSP, provider subscription ID
   and payment method, preserving its UUID, customer and paid period.

Before every mutation, persist a submission marker. Missing create receipt
never permits another create. Unknown target enrollment remains paused if the
provider accepted the request. Source darkness is unresolved, never cancellation
evidence. Lost delete/update responses are resolved by exact readback. The
verifier only reads; the executor performs remaining writes under normal gates.
An elapsed billing anchor or changed local/provider state blocks continuation.

The [NMI create specification](https://docs.nmi.com/reference/create-subscription-v5)
documents vault creation, pause and an 8/14 digit future start date. The
[update specification](https://docs.nmi.com/reference/update-subscription-v5)
documents pause and next-date updates. This operation never calls the classic
sale-and-enroll endpoint. NMI's subscription resource does not expose currency;
the initial lane is limited to USD prices and requires external qualification
of the target account's USD configuration. Local HTTP/Postgres/loopback tests
prove engine behavior, not live PSP charge timing or a termination campaign.
