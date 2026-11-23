# Low-latency usage holds

A hard quota or prepaid balance should authorize work only after one atomic
operation checks capacity and reserves it. Keep that operation on the request
path, and send committed usage to ClickHouse asynchronously. If cross-region
coordination becomes material, preallocate exclusive spend rights so most
requests can reserve locally. An eventually consistent balance read followed by
a queued hold is suitable only when the product accepts overshoot.

## What the ClickHouse path guarantees

Suppose a customer has 10 credits left and two requests each need 6. Both can
read 10 before either hold is visible. Each enqueues a different valid event and
starts work. All 12 credits may eventually appear in the ledger. A durable queue,
insert deduplication, or a read from the same replica still permits this
ordering. The acceptance decision must observe and consume the same capacity
atomically. An ordered queue can back an authority if its consumer performs that
check and the caller waits for the decision before starting work.

ClickHouse with `async_insert=1, wait_for_async_insert=0` acknowledges receipt
into a node's memory buffer. Queries see the rows only after a flush. A crash
can lose acknowledged data and validation failures can surface after the client
has gone on. With `wait_for_async_insert=1`, the client waits for the flush and
receives flush errors. Insert quorum and fsync settings govern separate
replication and persistence guarantees. Atomicity for the documented insert
shapes protects the inserted block. A separate balance read and hold insert
still race. ClickHouse's guidance for application workloads is to keep a
transactional system of record and replicate changes for analytics.
[Asynchronous inserts](https://clickhouse.com/docs/concepts/features/operations/insert/asyncinserts),
[transactional guarantees](https://clickhouse.com/docs/concepts/features/operations/insert/transactions).

A bounded-overage policy needs an independent bound on the work that can start
while the balance is stale. A maximum cost per request, a limit on how many
requests can pass during that interval, and a cutoff when the view exceeds its
freshness bound can supply one. An eventually consistent counter by itself gives
no fixed overshoot bound.

### ClickHouse features that can make coordinated writes

`KeeperMap` provides a Keeper or ZooKeeper-backed key-value store with
linearizable writes and sequentially consistent reads. In strict mode it
supports atomic fetch-and-update, including updates with a predicate. That
makes it a candidate for testing a single-key conditional decrement. A
production quota would still need to demonstrate that all applicable counters
and the operation receipt commit together, including when the client retries
after an ambiguous reply. The write path coordinates through Keeper and has a
different latency profile from buffered MergeTree inserts.
[KeeperMap engine](https://clickhouse.com/docs/reference/engines/table-engines/special/keepermap#updates).

Lightweight MergeTree updates write patch parts and have an
`update_sequential_consistency` setting for update visibility. ClickHouse
classifies multi-statement transactions as experimental and documents them for
non-replicated MergeTree. These are different mechanisms from the proposed
balance read followed by a queued insert.
[Lightweight updates](https://clickhouse.com/blog/updates-in-clickhouse-2-sql-style-updates#coordinating-concurrent-updates),
[transaction requirements](https://clickhouse.com/docs/concepts/features/operations/insert/transactions#requirements).

## What production systems document

| System | Documented behavior |
| --- | --- |
| Cloudflare Workers Rate Limiting | Checks local state and synchronizes in the background. Requests can exceed the limit even in one location. [Accuracy](https://developers.cloudflare.com/workers/runtime-apis/bindings/rate-limit/#accuracy) |
| Amazon API Gateway | Uses a token bucket and treats request throttles and quotas as best-effort targets. [Request throttling](https://docs.aws.amazon.com/apigateway/latest/developerguide/api-gateway-request-throttling.html) |
| Stripe Issuing | Checks funds and spending controls, and reserves the approved amount until capture, void, or expiry. The docs also describe possible late captures after expiry. [Issuing authorizations](https://docs.stripe.com/issuing/purchases/authorizations) |
| TigerBeetle | A pending transfer reserves funds and can enforce that pending plus posted debits stay within posted credits. Posting can settle some or all of the hold. [Two-phase transfers](https://docs.tigerbeetle.com/coding/two-phase-transfers/), [transfer refusal conditions](https://docs.tigerbeetle.com/reference/requests/create_transfers/) |
| OpenAI API prepaid billing | Usage can continue briefly after exhaustion and leave a negative balance that is deducted from the next credit purchase. [Prepaid API billing](https://help.openai.com/en/articles/8264644-what-is-prepaid-billing) |

Codex and Claude publish customer-facing behavior for included allowances and
paid credits. These docs establish the product behavior only. Anthropic
separately documents that Claude API input-token rate limits start with an
estimate and adjust to actual input during a request.
[Codex usage limits](https://help.openai.com/en/articles/11369540-using-codex-with-your-chatgpt-plan),
[Claude usage credits](https://support.claude.com/en/articles/12429409-manage-usage-credits-for-paid-claude-plans),
[Claude API rate limits](https://platform.claude.com/docs/en/api/rate-limits).

### Merchant integration precedents

| System | What runs before work | How usage or settlement reaches the authority |
| --- | --- | --- |
| Stripe Payments | A remote PaymentIntent confirmation can authorize funds on an eligible payment method and return `requires_capture` when configured for manual capture. Confirmation can also require customer action. | A separate capture request collects up to the authorized amount in the standard flow, before the hold expires. [Confirm a PaymentIntent](https://docs.stripe.com/api/payment_intents/confirm), [authorization and expiration](https://docs.stripe.com/payments/place-a-hold-on-a-payment-method), [capture a PaymentIntent](https://docs.stripe.com/api/payment_intents/capture). |
| Stripe Billing Meters | Stripe Entitlements documents provisioning subscription features in the merchant's system from active entitlement updates and list reads. | Stripe advises pre-aggregating usage or sending batches through its meter event stream. Meter events aggregate asynchronously, thresholds trigger invoices or alerts, and billing credits apply when invoices finalize. Stripe points new usage-based integrations to Metronome. [Entitlements](https://docs.stripe.com/billing/entitlements?dashboard-or-api=api), [record meter events](https://docs.stripe.com/billing/subscriptions/usage-based/recording-usage-api), [usage thresholds](https://docs.stripe.com/billing/subscriptions/usage-based/thresholds), [credit grants](https://docs.stripe.com/billing/subscriptions/usage-based/billing-credits#preview-draft-or-finalized-invoices), [usage recording options](https://docs.stripe.com/billing/subscriptions/usage-based/recording-usage). |
| Metronome | Its prepaid implementation guide tells merchants to keep a `customer_entitled` flag in their own database and check it before work. A zero-balance webhook tells the merchant to revoke access. | The example reports usage after work. Ingest accepts 100 events per batch, deduplicates event `transaction_id`s for 34 days, and recommends a reliable delivery queue so ambiguous batches can be retried. [Prepaid access check](https://docs.metronome.com/guides/pricing-packaging/billing-model-guides/prepaid-credits), [usage events](https://docs.metronome.com/guides/events/send-usage-events), [batch ingestion](https://docs.metronome.com/guides/events/high-volume-ingestion). |
| Orb | The app checks its own `messages_blocked_until` value per request. Because depletion and recovery webhooks can arrive out of order, the app re-reads Orb's balance before updating that value. A scheduled unblock also prompts a fresh read. | Orb accepts concurrent usage batches of up to 500 events by default and deduplicates per event within the account's grace period. The prepaid guide describes how a delay of seconds or minutes before the merchant acts on a depletion webhook can allow extra usage. [Prepaid product access](https://docs.withorb.com/self-serve/product-access), [event ingestion](https://docs.withorb.com/events-and-metrics/event-ingestion). |
| Stigg | The merchant can call a colocated Sidecar via REST or gRPC. Its default in-memory cache stores entitlements and current usage. The Sidecar fetches upstream data on a cache miss. Stigg currently documents Redis for caching its separate Governance budget checks. | Stigg exposes a check for requested units before work and separate ingestion. `reportUsage` updates usage synchronously and can return a credit balance. `reportEvent` batches raw events for asynchronous aggregation. The Governance docs specify that ingestion records usage and the check produces the permit decision. [Sidecar overview](https://docs.stigg.io/documentation/high-availability-and-scale/sidecar/overview), [cache architecture](https://docs.stigg.io/documentation/high-availability-and-scale/sidecar/architecture), [Governance check and ingest](https://docs.stigg.io/documentation/governance/check-and-ingest), [ingestion methods](https://docs.stigg.io/documentation/getting-usage-data-into-stigg/choosing-an-ingestion-method). |

Two public protocols show where to place the boundary. Google Cloud Service
Control recommends caching policy `Check` results for 60 seconds where feasible
and aggregating completed operation `Report` data for up to five seconds. It
documents policy propagation lag and exposes a separate `QuotaController` for
counter allocation. Diameter Credit-Control reserves units at the credit-control
server, lets the service element meter use against that grant, and reports used
units when requesting more. The grant can expire even with units remaining.
[Google Cloud Service Control](https://docs.cloud.google.com/service-infrastructure/docs/service-control/reference/rpc/google.api/servicecontrol.v1),
[Diameter credit reservation and renewal, RFC 8506 §§5.2–5.3](https://www.rfc-editor.org/rfc/rfc8506.html#section-5.2).

Both Metronome and Orb document pricing leaked usage at zero after prepaid
credit is exhausted. This caps what the customer is billed and leaves the cost
of any extra work with the merchant.
[Metronome overage pricing](https://docs.metronome.com/guides/pricing-packaging/apply-credits-and-commits/guarantee-zero-overages.md),
[Orb spend management](https://docs.withorb.com/usage-alerting/spend-management).

Once an event is ready for delivery, batching reduces calls and event IDs let
ingestion deduplicate retries. Two different requests can still consume the
same apparent balance if each admission only reads a cached view. Metronome and
Orb show a local access flag backed by billing signals. Stigg shows an
entitlement cache beside the application. Diameter shows the additional grant
needed when each local approval must consume reserved capacity. These are
documented integration contracts. The target of roughly 2 ms mean admission
with thousands of concurrent requests requires an OpenRails workload test.

## Merchant-defined counters

Define each counter with a merchant-scoped key, a subject type such as customer
or project, a unit such as requests, tokens or credits, an integer capacity, and
a refill rule. For example, a merchant might grant each customer 500 requests
per UTC day and each project 100,000 tokens per UTC month. A prepaid counter
starts from funded credits and replenishes through explicit top-ups. These are
proposed quota mechanics for work that must stay within an allowance.

- For a gateway request with a fixed cost, call
  `Consume(customer, request_id, counter_set, 1)` once. Resolve the configured
  counter set and subjects on the server. Approval includes checking every
  applicable balance, decrementing them together, and recording the success
  under `request_id` in one transaction. If one balance is insufficient, charge
  none. Replay an earlier success even across a reset, and reject reuse of its
  ID with changed terms. OpenRails records admitted holds and can re-evaluate
  a decline on retry. A product can choose to persist declines if it needs those
  decisions to replay as well.
  [OpenRails admission lookup](../usage-billing.md#admissions).
- For a fixed window, serialize the refresh and decrement, then derive the
  generation from the authority's clock. On the first operation in a new
  window, initialize it to its configured capacity and apply the decrement
  before commit. A reset requested explicitly should identify the intended
  generation so a retry cannot refill it twice. A funded top-up instead adds
  units in an atomic operation with its own stable ID. A rolling-window policy
  calls for accounting over the trailing interval.
- For variable work, reserve an enforceable upper bound with `Hold` before
  starting. Settle actual usage with `Capture` and release any remainder. Keep
  the originating window on the hold so a later capture or release never
  credits the following window. If more work
  is needed, reserve the additional amount first.

Overlapping counters such as a customer cap and a project cap require one
atomic decision for both scopes. A single PostgreSQL transaction can lock or
conditionally update the affected rows together. A Redis Cluster script can
cover them if the keys involved can be placed in the same hash slot.
[PostgreSQL row locks](https://www.postgresql.org/docs/18/explicit-locking.html#LOCKING-ROWS),
[Redis Cluster key slots](https://redis.io/docs/latest/develop/reference/errors/common/crossslot/).

## Ways to keep the reservation fast

**A conditional write at one authority.** Keep an indexed balance or quota
record per owner. In PostgreSQL, a guarded `UPDATE` can decrement the available
units only when enough remain. At Read Committed, a concurrent update waits and
re-evaluates the predicate on the updated row. Store the hold identity and
immutable terms in the same transaction so a retry after an ambiguous response
returns the existing decision. When several independent limits apply, the
reservation must consume them together. DynamoDB exposes the same single-item
pattern through conditional `UpdateItem`, with transactions for limits that
span items. The topology must preserve acknowledged commits for the failures the
contract covers.
[PostgreSQL isolation](https://www.postgresql.org/docs/18/transaction-iso.html),
[PostgreSQL commit durability](https://www.postgresql.org/docs/18/runtime-config-wal.html#GUC-SYNCHRONOUS-COMMIT),
[DynamoDB concurrent updates](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/BestPractices_ImplementingVersionControl.html),
[DynamoDB transactions](https://docs.aws.amazon.com/amazondynamodb/latest/developerguide/transaction-apis.html).

**A Redis script at one primary.** A Lua script can check capacity, record the
request identity, and reduce capacity in one call without another command
interleaving. Related keys must share a hash slot in Redis Cluster. Default
replication is asynchronous. `WAIT` reduces the chance of lost writes, and
`WAITAOF` can wait for append-only-file fsync acknowledgements. A deployment using
Redis as the sole balance authority would still need a failover protocol that
preserves acknowledged spend before granting more. A replacement that starts
from a stale balance can spend the same units twice.
[Lua scripting](https://redis.io/docs/latest/develop/interact/programmability/eval-intro/),
[Redis Cluster key slots](https://redis.io/docs/latest/develop/reference/errors/common/crossslot/),
[Redis replication](https://redis.io/docs/latest/operate/oss_and_stack/management/replication/),
[`WAITAOF`](https://redis.io/docs/latest/commands/waitaof/).

**Escrow rights for geographically distributed admission.** Allocate part of a
shared budget exclusively to each region and atomically spend from that region's
local allocation. With 10 units split 6 and 4, only the region with 6 can approve
a 6-unit request. Refill or transfer rights when a region runs low. A disconnected
region can keep spending only its remaining allocation. This trades some
availability near exhaustion for local request latency. Google's Doorman uses
capacity leases for distributed rate limiting. The bounded-counter paper gives
the numerical invariant for spend rights. Applying it to prepaid credits also
requires durable local consumption and a safe reclaim protocol that fences stale
workers and reconciles spent rights before reallocating any remainder.
[Doorman design](https://github.com/youtube/doorman/blob/master/doc/design.md),
[Balegas et al., sections II.D and IV.D–E](https://arxiv.org/pdf/1503.09052).

## Local storage measurements

A Go 1.26.10 client with warmed connections and `GOMAXPROCS=4` used TCP loopback
on an Intel Core i9-13950HX host. Each service had a two-CPU container limit.
Redis 7.4.12 had snapshots and AOF disabled. PostgreSQL 18.6 used an NVMe-backed
Docker volume with `synchronous_commit`, `fsync`, and `full_page_writes` on.
These are different durability settings.

The write-only test used `HINCRBY` on a customer field of a shared Redis hash
and `EXPIRE` in one pipeline, an hourly counter upsert in PostgreSQL, and a
PostgreSQL append with a primary key and an index on
`(merchant_id, customer_id, occurred_at)`. Each cell summarizes three trials
with 500 warmup and 3,000 timed operations each. All 27 trials verified the
expected writes. Both tables give the median of per-trial p95 milliseconds and
the median throughput in operations per second.

| Workload | Redis counter | PostgreSQL upsert | PostgreSQL append |
| --- | ---: | ---: | ---: |
| 1 worker, 1,024 customers | 0.253 / 10,075 | 3.591 / 338 | 3.425 / 350 |
| 16 workers, 1,024 customers | 0.342 / 107,583 | 5.188 / 3,457 | 5.068 / 3,542 |
| 16 workers, 1 customer | 0.457 / 78,272 | 178.467 / 417 | 5.084 / 3,532 |

The hot-key upsert illustrates contention when many writers serialize on one
row. An append can avoid that row contention for telemetry. The append test
measured event recording and has no capacity check.

The conditional-decrement test used the same setup. Redis ran a cached Lua
script to check a customer balance field and subtract one. PostgreSQL ran
`UPDATE balances SET remaining = remaining - 1 WHERE customer_id = $1 AND remaining >= 1 RETURNING remaining`
in one autocommitted statement. Each cell summarizes three trials with 500
warmup and 1,500 timed operations each. All 18 trials reconciled the expected
writes. In a separate last-unit test, each store allowed exactly one of 32
concurrent contenders.

| Workload | Redis Lua | PostgreSQL guarded update |
| --- | ---: | ---: |
| 1 worker, 1,024 customers | 0.2995 / 7,171 | 3.5830 / 341 |
| 16 workers, 1,024 customers | 0.6970 / 50,195 | 5.3093 / 3,456 |
| 16 workers, 1 customer | 0.3200 / 83,014 | 135.0207 / 407 |

Timing covered the storage client call through its reply. The decrement test
covered one counter per request. End-to-end admission, request deduplication,
multiple-counter transactions, failover, and ClickHouse remain unmeasured.

## What this means for OpenRails

Start from the existing PostgreSQL reservation under the customer's money lock.
The hold shares capacity with pending credit refunds and running charges, and
an embedded host can commit its own row and the hold in one transaction.
Removing a round trip from this path has to preserve those effects, the durable
`request_id` replay contract, and the applicable spend windows.
[OpenRails admissions and settlement](../usage-billing.md#admissions),
[OpenRails spend limits](../spend-limits.md).

An exact hold prevents concurrent admissions from reserving the same capacity.
A hard cap on *actual work* also requires an enforceable cost ceiling. OpenRails
captures the full amount and records excess as owed if usage comes in above the
hold. For a strictly prepaid workload, the host must enforce the reserved upper
bound or acquire an additional hold before incurring additional cost. Reconcile
commit timeouts through the same request ID before launching, and tie that ID to
a single execution. In-flight work must stop or extend its hold before deadline
if expiry would release capacity it can still consume.
[OpenRails admissions and settlement](../usage-billing.md#admissions).

Use a committed event row or change data capture to feed ClickHouse for reporting.
The relay can retry failed analytical inserts, and a stable event identity lets
the consumer apply a replay idempotently. Approvals can return as soon as the
authoritative reservation commits. A ClickHouse balance can show usage and
remaining capacity as of the relay's reported progress. Read the authority when
the caller requires a current spendable balance.
[Transactional outbox and CDC](https://docs.aws.amazon.com/prescriptive-guidance/latest/cloud-design-patterns/transactional-outbox.html),
[ClickHouse insert acknowledgements](https://clickhouse.com/docs/concepts/features/operations/insert/asyncinserts#choosing-a-return-mode).
