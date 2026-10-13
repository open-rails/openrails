# Destructive-action limits and incident review

Merchant refunds, cancellations, payment-method removal, access/credit revocation,
write-offs and substantive catalog/configuration changes share one admission policy.
It counts the logical actions of each staff member and application. The verified
issuer and invoker identify the actor; credential rotation does not create a new
allowance. Customer self-service and autonomous maintenance keep separate policies.

An in-process Client call that names no person is the embedded host's own work.
It is recorded in the action audit with origin `host`, but spends no allowance,
counts toward no actor, merchant or deployment limit, and an incident does not
hold it. A host acting for a signed-in staff member names that person on the call:

```go
ctx = openrails.WithInvoker(ctx, openrails.Invoker{Issuer: issuer, ID: subject})
_, err := client.RefundPayment(ctx, paymentID, params)
```

The call is then that person's, with every limit and incident applying exactly as
to their own HTTP call. Over HTTP the credential alone names the caller.

A denied admission returns HTTP 429. The incident persists until explicit review
and clearance: waiting, restarting, rotating a key or ignoring a finding does not
release it. Reads and recovery of effects already committed remain available.
New provider requests are held; verification and necessary completion of an
already-started effect continue. A pending or uncertain action remains reserved,
including after clearance, so clearing an incident cannot unleash an old queue
alongside a new allowance.

The fixed staff limits are 5 actions per actor per hour and 15 per day. Merchant
limits are at least 15/hour and 25/day, increasing with active customers; deployment
limits are at least 50/hour and 100/day. The limits count affected targets in a
batch, not a single HTTP request. Replays and proven no-ops do not spend again.
They are implementation constants rather than merchant configuration.

Creating a new catalog object is additive and spends nothing; changing, archiving
or removing an existing product, price or meter's billable, access or routing
terms is counted per object. A catalog edit that fails before it commits spends
nothing, even after a provider call. Canceling a price migration keeps
subscriptions where they are, so it spends nothing and no incident holds it. A
file applied with `openrails catalog apply` is admitted on the merchant's
separate autonomous budget rather than an actor's.

## Review and clear

The findings queue shows the incident. Inspect the compromised actor, revoke its
credential where appropriate, and reconcile pending effects before clearing it.
The merchant can read incident summaries with
`GET /v1/merchant/destructive-holds`. Ordinary finding resolution cannot clear one.

`POST /v1/merchant/destructive-holds/{id}/clear` accepts `{"reason":"reviewed"}`.
It requires merchant configuration authority plus `root:admin-rate-limits:unlock`
from a self-acting human. Application keys, personal API keys, delegated identities
and the triggering actor are refused. This adds no recent-sign-in requirement.

Trusted host tooling can call `Engine.ListOpenDestructiveHolds` and
`Engine.ClearDestructiveHold`; the standalone server exposes the same methods.
The host must authenticate and authorize its human operator before using that
trusted in-process capability. The CLI offers:

```sh
openrails destructive-holds list --limit 100
openrails destructive-holds clear merchant-name dsh_00000000-0000-4000-8000-000000000001 \
  --operator-issuer https://identity.example --operator operator-subject \
  --reason "Credential revoked; pending effects reviewed"
```

Clearance is for one episode and records the operator and reason. It does not
remove the action audit or release pending reservations. A further breach creates
a new incident.

## Events, alerts and backup

The incident and its host event commit together. Hosts receive
`destructive.hold_opened` and `destructive.hold_cleared` through the existing host
outbox. Merchant-visible payloads omit cross-merchant actor evidence.

A durable job delivers opening alerts to configured operator email/webhook
channels. Failed deliveries retry; webhook retries carry a stable incident and
destination idempotency key. Delivery is at least once, so a destination must
handle duplicate receipts. A failed or missing external destination cannot make a
held action execute. The durable incident remains available for review.

Action audit and incident authority belong to the deployment. Keep them in full
database backups. Merchant import/export cannot reset active incidents or pending
actions, and will refuse an unsafe transfer. No historical command is invented
when applying the schema migration.
