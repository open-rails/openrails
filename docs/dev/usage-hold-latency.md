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

## Merchant-defined counters

Define each counter with a merchant-scoped key, a subject type such as customer
or project, a unit such as requests, tokens or credits, an integer capacity, and
a refill rule. For example, a merchant might grant each customer 500 requests
per UTC day and each project 100,000 tokens per UTC month. A prepaid counter
starts from funded credits and replenishes through explicit top-ups. These are
proposed quota mechanics for work that must stay within an allowance.

- One request carries a stable operation ID and the amount needed from each
  applicable counter. Resolve definitions on the server. Check all balances,
  decrement them together, and record the decision under that operation ID in
  the same transaction. If one balance is insufficient, charge none. Replay the
  original result for the same terms even across a reset, and reject reuse of
  that ID with changed terms.
- For a fixed window, serialize the refresh and decrement, then derive the
  generation from the authority's clock. On the first operation in a new
  window, initialize it to its configured capacity and apply the decrement
  before commit.
  This keeps a delayed scheduler or concurrent refresh from unlocking extra
  capacity. An explicit top-up also needs a stable ID and an atomic balance
  update. A rolling-window policy calls for accounting over the trailing interval.
- When cost is known, consume the exact amount. When cost is estimated, hold an
  enforceable upper bound before starting, settle actual usage against the
  hold, and release any remainder. Keep the originating window on the hold so
  a later capture or release never credits the following window. If more work
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
[Balegas et al., sections II.D and IV.D](https://arxiv.org/pdf/1503.09052).

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
