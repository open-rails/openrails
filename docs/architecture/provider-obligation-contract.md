# Provider obligations and host transactions

Status: pre-v1 contract, #1004. Tensorhub references are to `cozy_v2/tracker-v2` (`tracker/tensorhub/th-005-monetary-preauth.md`, `th-045-provider-cost-settlement.md`, `tracker/cross-cutting/proto-022-*.md`, `proto-023-*.md`, `tensorhub-customer-spend.md`, decisions #598, #599 and #604).

## Shared Client

Ordinary billing code uses `*openrails.Client` in embedded, standalone and SaaS deployments. Each command commits in an OpenRails-owned transaction, with the same DTOs, HTTP codes and error classes everywhere. The embedded Client dispatches to the same handlers.

| Command | Route |
|---|---|
| `OpenOperationAuthorization` | `POST /v1/merchant/provider-operations` |
| `GetOperationAuthorization` | `GET /v1/merchant/provider-operations/{operation_id}` |
| `ReleaseOperationAuthorization` | `POST …/{operation_id}/release` |
| `RecordProviderBillingObservation` | `POST …/{operation_id}/observations` |
| `GetProviderBillingQualification` | `GET …/{operation_id}/qualification` |

An authorization binds an immutable operation id, payer, record owner, claim reference, exact body bytes, SHA-256 and USD micros. An identical retry replays; a changed field returns `operation_authorization_conflict` with the field as `param`. Ambiguous provider creation leaves the authorization open. Release requires proven non-creation and is refused once billing evidence exists.

Observations carry immutable facts: provider lifecycle evidence, the normalized query, exact raw response and typed records, or a typed adapter refusal. The canonical JSON of one observation must fit in 768 KiB (`ProviderBillingObservationMaxBytes`) so embedded and HTTP callers accept the same inputs. Otherwise the adapter submits `response_too_large`.

### Permissions

The three writes (open, release, observations) require `merchant:admissions:create`; the two reads require `merchant:usage:read` (`internal/http/routes/routes.go`, `admissionMW` / `usageReadMW` on the `/provider-operations` group). Recording an observation is deliberately a spend-authority write, not a read or a usage-report grant: an observation is an admission-side fact about the payer's reservation, and an eligible one settles that reservation in the same commit (`internal/modules/money/provider_billing.go`, `RecordProviderBillingObservation` qualifies and then posts the settlement). A credential that may only read usage cannot move customer money; a credential that may open holds may also close them with evidence. `TestProviderObligationObservationNeedsSpendAuthority` proves both directions over real HTTP.

### One cap in every deployment

The 768 KiB cap is enforced once, on the shared service path every deployment runs: `validateProviderBillingInput` (`internal/modules/money/provider_billing.go`) re-encodes the request with `json.Marshal` and refuses anything over `ProviderBillingObservationMaxBytes` as `ErrInvalid`, which the route reports as `400 invalid_param`. The HTTP request type is the same Go type the service validates (`ProviderBillingObservationInput = openrails.ProviderBillingObservationRequest`), and the remote Client sends its compact `json.Marshal` encoding, so the wire body is byte-for-byte the value the cap measures. The standalone server and the embedded HTTP mount also apply the transport's 1 MiB body limit (`middleware.BodyLimitHTTP(middleware.DefaultMaxBodyBytes)`) before decoding; because 768 KiB is below it, no observation the cap accepts is ever refused by the transport. The in-process embedded Client (`embed/transport.go`) has no body limit and relies on the cap alone. So that an observation beyond 1 MiB is refused identically everywhere, the remote Client measures the encoded body against the same constant before sending and returns the same `400 invalid_param` (`remote_provider_obligations.go`) rather than the transport's `413 request_body_too_large`. The host-transaction extension calls the service directly and gets the same `ErrInvalid`. `TestProviderObligationClientIsIdenticalEmbeddedAndStandalone` records both the over-cap and the over-1 MiB refusal in the embedded and standalone transcripts and requires them equal.

## OpenRails owns rating

No request type has a rated, settlement or charge field. The routes decode strictly, so a smuggled amount returns 400 before any command runs. OpenRails qualifies evidence after absence, closed windows, full lifetime coverage and two equal observations separated by the configured quiescence. Refused, negative, corrective or decreasing evidence keeps the money reserved. Eligible evidence settles in the same transaction through the unexported pass-through primitive. Its only caller is the qualifier, which a unit test enforces. The database requires rated cost to equal provider cost for this contract. Cost above the hold posts as owed rather than being clamped.

## Host transaction extension

`embed.NewHostTransactions(runtime)` is for an embedding host that must commit its own provider obligation, absence fact or billing fact atomically with OpenRails. It has the same five commands, taking a `pgx.Tx` from the host's pool on the runtime database. The extension binds the runtime merchant to that transaction. A transaction bound to another merchant is refused. OpenRails never commits or rolls back. Tx reads observe the transaction's uncommitted work. After any error the host rolls back.

The remote Client cannot join a host database transaction and does not pretend to. A compensating outbox is not a substitute for this extension. Real PostgreSQL tests prove that open, release and settlement roll back and commit together with a host row.

## Decision record: th-005 and proto-022/023

**Accepted (th-005 with #598/#599; product arming deferred by #604).** OpenRails supplies these obligations:

- One operation id is the authorization id and the provider create identity. Exact bytes replay; changed bytes conflict.
- Authorization opens in the same transaction as the provider obligation, with no separately committed held phase and no outbox.
- Tx-native read and proven-non-create release in the host transaction. This closes th-005's release gap.
- State is `open`, `released` or `settled`. Only OpenRails qualifies evidence, rates at pass-through, handles owed amounts and posts one final settlement. There are no partial captures.
- The payer row is the capacity mutex. Admission holds and authorizations are both PostgreSQL rows since #989, so th-005's cross-store Redis read no longer applies.

**Proposed, not accepted.** `tensorhub-customer-spend.md` is marked "awaiting owner rulings". These changes are not implemented:

- proto-022 monotone authorization extension (`ExtendOperationAuthorizationTx` with ordinal and minimum grant). Its tx-native read/release item coincides with the accepted th-005 obligation above and is delivered on that basis only.
- proto-023 repayment of owed amounts from the next deposit, and prepaid capacity that subtracts outstanding owed.

**Future schema if ratified.** Every item is additive, so it is deferred rather than frozen now:

- `operation_authorization_extensions (merchant_id, operation_id, ordinal, requested_usd_micros, minimum_usd_micros, granted_usd_micros, created_at)` with a composite primary key, a foreign key to the authorization and forced RLS.
- `operation_authorizations.authorized_total_usd_micros` equal to the opening amount plus grants. Holds and the settlement manifest would read the total. `authorized_usd_micros` stays the immutable opening amount.
- Ledger transfer type `owed_repayment_from_balance` and a repaid invoice-item transition.
- Prepaid capacity subtracting owed amounts would change behaviour but needs no schema.

Tensorhub product scope stays out of OpenRails: tranches, reclaim, owner absence, the shared pool (th-103/104/105, cl-077). No live provider billing read or charge is part of this proof.
