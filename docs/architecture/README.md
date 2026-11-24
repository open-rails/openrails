# Architecture

One page per OpenRails system: what it does, how it works, and how it keeps the
guarantees below. Each page ends with review checks, so a reviewer can verify one
system on its own. Integration guides are in [docs/](../); these pages are for
people changing or reviewing OpenRails.

## Systems

| System | Page |
|---|---|
| Ledger | [ledger.md](books.md) |
| Catalog | [catalog.md](catalog.md) |
| Orders and checkout | [orders.md](orders.md) |
| Subscriptions | [subscriptions.md](subscriptions.md) |
| Collection and dunning | [dunning.md](dunning.md) |
| Refunds and disputes | [refunds.md](refunds.md) |
| Usage billing | [usage-billing.md](usage-billing.md) |
| Entitlements and limits | [entitlements.md](entitlements.md) |
| Prepaid credits | [prepaid-credits.md](credits.md) |
| Payment providers | [payment-providers.md](payment-providers.md) |
| Cards and payment methods | [payment-methods.md](payment-methods.md) |
| Imports | [imports.md](imports.md) |
| Customer directory (SCIM) | [customer-directory.md](customer-directory.md) |
| Merchant configuration | [merchant-configuration.md](merchant-configuration.md) |
| Events and notifications | [notifications.md](notifications.md) |
| Reporting | [reporting.md](reporting.md) |
| UI library | [billing-ui.md](billing-ui.md) |
| Standalone server | [standalone-server.md](standalone-server.md) |

AuthKit is a library OpenRails uses, not one of its systems: OpenRails does no
authentication of its own.

## Guarantees

Every system keeps these. A page says how, or why one does not apply.

1. **Exactly-once provider calls.** A charge, refund or other change at a payment
   provider is recorded before it is sent, so a crash or a lost response never
   repeats it or loses track of it.
2. **Agreement with providers.** OpenRails' records match the provider's, even
   when callbacks arrive late, twice or never.
3. **Exact money.** Amounts are integer micros in the currency's registered scale;
   each provider receives exactly the value it expects; the ledger never mixes
   currencies.
4. **Access control and tenant isolation.** Every route checks its permission and
   acts on the one merchant its credential is bound to; a customer route acts only
   as the signed-in customer; every tenant query names its merchant.
5. **Contracts match the code.** The Go API, HTTP routes and schema snapshots in
   `api/` are regenerated with every change, and the in-process and remote clients
   answer alike. Before v1.0.0 any of them may change; from v1.0.0 they change
   only by adding to them ([compatibility](../../compatibility/README.md)).
6. **Data lifecycle.** Every table has a retention class saying how its rows leave
   it: permanent (never), partitioned (whole months dropped), rows (deleted past an
   age) or state (gone when the thing is). The class is stated in the table's
   comment and `internal/retention`; reads of a partitioned table name a time
   range. Schema migrations themselves are migratekit's (`internal/migrate`).
7. **Work scales with activity.** Background work is driven by events, watermarks
   and indexed due work, never a routine sweep over every row.
8. **Abuse protection.** Public and customer-facing entry points are rate-limited,
   and card-testing and similar attacks are refused.
9. **Destructive-action safeguards.** Irreversible or bulk commands are bounded by
   ceilings and holds, and every one is attributable.

## Page template

```markdown
# <System>

<One paragraph: what the system does and for whom.>

## Concepts
<The terms a reader needs, one line each.>

## How it works
<The flow, the tables it owns, the jobs it runs, the key Go identifiers.>

## Interfaces
<Go methods, HTTP routes, events and configuration, linking the integration docs.>

## Guarantees
### 1. Exactly-once provider calls
<How this system keeps it, or "Does not apply: <reason>.">
Checks:
- <A concrete thing a reviewer verifies in the code or tests.>
<... the same for guarantees 2 to 9 ...>

## Tests
<Critical journeys (OR-Jxx) and other tests that cover this system, and gaps.>
```
