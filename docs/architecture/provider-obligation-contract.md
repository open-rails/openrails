# Provider obligations and host transactions

A host that buys compute from an upstream provider on a customer's behalf
authorizes the spend before the provider creates anything, then reports what
the provider billed. OpenRails holds the authorization, qualifies the evidence
and settles the customer's reservation at the provider's cost.

## The six commands

Ordinary billing code uses `*openrails.Client` in every deployment. Each command
commits in an OpenRails-owned transaction, with the same types, HTTP codes and
error classes; the embedded Client dispatches to the same handlers.

| Command | Route | Permission |
|---|---|---|
| `OpenOperationAuthorization` | `POST /v1/merchant/provider-operations` | `merchant:admissions:create` |
| `GetOperationAuthorization` | `GET /v1/merchant/provider-operations/{operation_id}` | `merchant:usage:read` |
| `ExtendOperationAuthorization` | `POST /v1/merchant/provider-operations/{operation_id}/extend` | `merchant:admissions:create` |
| `ReleaseOperationAuthorization` | `POST /v1/merchant/provider-operations/{operation_id}/release` | `merchant:admissions:create` |
| `RecordProviderBillingObservation` | `POST /v1/merchant/provider-operations/{operation_id}/observations` | `merchant:admissions:create` |
| `GetProviderBillingQualification` | `GET /v1/merchant/provider-operations/{operation_id}/qualification` | `merchant:usage:read` |

An authorization binds an immutable operation id, customer, record owner, claim
reference, the exact body bytes with their SHA-256, and an `amount` in `USD`.
An identical retry replays; a changed field is 409
`operation_authorization_conflict` with the field as `param`. An ambiguous
provider creation leaves the authorization open. Release requires proven
non-creation and is refused once billing evidence exists
(`operation_authorization_has_billing_evidence`). Its state is `open`,
`released` or `settled`; there are no partial captures.

## Growing a hold

Work billed by the second holds one tranche when it starts and grows the hold
while it runs. `ExtendOperationAuthorization` asks for up to `amount` and
accepts no less than `minimum_amount`, under the same customer lock and
capacity rule as opening: it grants `min(amount, capacity)` when capacity
covers `minimum_amount`, and otherwise refuses with 402 `insufficient_credits`
and writes nothing. A refusal is the host's signal to stop the work while the
hold still covers it. Only an `open` authorization grows; released and settled
ones answer `operation_authorization_not_open`. `authorized_amount` plus
`amount` must fit in an int64.

Extensions are numbered by `ordinal` from 1 without gaps. Repeating a committed
ordinal with the same amounts replays its grant (`replayed`); other amounts are
409 `operation_authorization_conflict` with the field as `param`, and a skipped
ordinal is the same conflict on `ordinal`. The authorization keeps its opening
`amount`; `authorized_amount` is the opening amount plus every grant. Held and
available balance, admission capacity and settlement use `authorized_amount`,
and the settlement body lists the opening amount and each grant.

Observations carry immutable facts: provider lifecycle evidence, the normalized
query, the exact raw response and typed records, or a typed adapter refusal.
The JSON of one observation must fit in 768 KiB
(`billing.ProviderBillingObservationMaxBytes`); a larger one is 400
`invalid_param` from every transport, so embedded and HTTP callers accept the
same inputs. An adapter that cannot fit a response submits `response_too_large`.

Recording an observation is a spend-authority write, not a usage read: an
eligible observation settles the customer's reservation in the same commit. A
credential that may only read usage cannot move customer money; one that may
open holds may also close them with evidence.

## OpenRails owns rating

No request type has a rated, settlement or charge field, and the routes decode
strictly, so a smuggled amount is refused before any command runs. OpenRails
qualifies evidence after absence, closed windows, full lifetime coverage and
two equal observations separated by the configured quiescence. Refused,
negative, corrective or decreasing evidence keeps the money reserved. Eligible
evidence settles at pass-through: rated cost equals provider cost, and cost
above the hold posts as owed instead of being clamped.

## Owed and the next funding

Settlement never re-gates, so cost above the hold is owed even by a prepaid
customer with no credit line. A prepaid customer's open, extension and
admission capacity is the balance net of holds and of what is owed, so no new
hold is granted against money already owed; arrears capacity is unchanged.

The next funding repays the debt first, in the same transaction and under the
same customer lock: a purchased-credit lot repays from its paid part (a bonus
never repays debt), and `CreateCreditGrant` from its amount. Each repayment is
an `owed_repayment` credit transaction on the funding lot, so a replayed
funding repays once. It pays the invoices that claim the debt first, oldest
first, as balance payments; an invoice whose collection is in flight keeps its
claim. Debt no invoice claims yet is repaid directly, and a later invoice
claims only what is still owed. The repaid part of a lot is used credit: a
refund returns only unused credit, and a chargeback of the lot makes the
repaid part owed again (a won dispute repays it).

Observations are kept 90 days after their operation is settled or released
([data retention](../operations.md#data-retention)); authorizations and
qualifications are permanent.

## Host transaction extension

The embedded Client's `Tx` operations (`OpenOperationAuthorizationTx`,
`GetOperationAuthorizationTx`, `ExtendOperationAuthorizationTx`,
`ReleaseOperationAuthorizationTx`,
`RecordProviderBillingObservationTx`, `GetProviderBillingQualificationTx`) are
for a host that must commit its own provider obligation, absence fact or
billing fact atomically with OpenRails. They take a `pgx.Tx` from the host's
pool on the engine's database and bind the declared merchant to that
transaction; a transaction bound to another merchant is refused. OpenRails
never commits or rolls back. Tx reads observe the transaction's uncommitted
work. After any error the host rolls back.

The remote Client cannot join a host database transaction and does not pretend
to: `Tx` operations on it return `openrails.ErrRemoteClient`. A compensating
outbox is not a substitute for this extension.
