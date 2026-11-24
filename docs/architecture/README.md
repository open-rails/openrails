# Architecture

One page per OpenRails system: what it does, how it works, and how it keeps the guarantees below. Each page ends with review checks, so a reviewer can verify one system on its own. These pages are for people changing or reviewing OpenRails; integrators start at the [integration guide](../guide.md).

## Systems

| System | Covers | Page |
|---|---|---|
| Books | The units book (prepaid balances, credit lots, usage owed) and the general ledger, per merchant and currency | [books.md](books.md) |
| Revenue recognition | Revenue schedules, recognised monthly in the accounting timezone; deferred revenue and breakage; adjustments for refunds and chargebacks | in [books.md](books.md#cash-and-revenue-recognition) |
| Catalog | Products, prices, meters, rate cards and limits; the catalog document and applying it; price migrations; the catalog assistant | [catalog.md](catalog.md) |
| Orders and checkout | Orders (one-off, renewal, usage), carts, previews, the payment page | [orders.md](orders.md) |
| Subscriptions | The state machine, renewals, cancel and resume, plan and seat changes | [subscriptions.md](subscriptions.md) |
| Recurring revenue (MRR) | MRR movements at every subscription change, scheduled churn and contraction, MRR at risk, the rollups | [mrr.md](mrr.md) |
| Collection and dunning | Retry schedules, the decline table and network advice, arrears delinquency | [dunning.md](dunning.md) |
| Refunds and disputes | Refunds, chargebacks, won disputes, payments recorded from outside | [refunds.md](refunds.md) |
| Payment health | Decline causes and network advice, authorization rates; dispute monitoring and prevention | in [dunning.md](dunning.md), [reporting.md](reporting.md#health-overview) and [refunds.md](refunds.md#dispute-monitoring) |
| Usage billing | Meters, rate cards, the usage event store, settlement priced on totals, usage orders in arrears, running charges | [usage-billing.md](usage-billing.md) |
| Entitlements and limits | Product keys and access; limits per window, the balance limit, spend limits; views and holds; the Go SDK's entitlement cache and gate | [entitlements.md](entitlements.md) |
| Credits and merchant currencies | Credit lots: staff grants, packs and deposits, recurring credits, expiry, spending from a balance; units a merchant defines, bought, given and spent | [credits.md](credits.md) |
| Payment providers | NMI, Stripe, CCBill, Solana: charges, refunds, provider-run subscriptions, webhook signatures, staying in sync | [payment-providers.md](payment-providers.md) |
| Cards and payment methods | Card vaults, saved cards, charge agreements, the card-number firewall | [payment-methods.md](payment-methods.md) |
| Imports | Subscribers from a PSP, legacy billing books | [imports.md](imports.md) |
| Customer directory (SCIM) | Customer contacts, pushed over SCIM or read in process | [customer-directory.md](customer-directory.md) |
| Merchant configuration | Registration, configuration (file or Vault, including merchant-defined currencies and the accounting timezone), PSPs and custodians, archive and restore | [merchant-configuration.md](merchant-configuration.md) |
| History | Configuration history (`change_log`) and operations history (`operation_log`), append-only | [history.md](history.md) |
| Events | The event log and host events | [events.md](events.md) |
| Customer messages | Email to customers, the account inbox, templates, per-event settings, the delivery log | [customer-messages.md](customer-messages.md) |
| Findings and alerts | The staff review queue (findings, proposals, parking) and alert delivery | [findings.md](findings.md) |
| Reporting | The metrics engine and its measures, the dashboard and its health overview | [reporting.md](reporting.md) |
| UI library | `@openrails/billing-ui`: admin pages and customer self-service | [billing-ui.md](billing-ui.md) |
| Standalone server | The `openrails` binary, CLI, configuration, the AuthKit control plane, the console | [standalone-server.md](standalone-server.md) |

AuthKit is a library OpenRails uses, not one of its systems: OpenRails does no authentication of its own. Route permissions are OpenRails' own, under guarantee 4.

## Guarantees

Every system keeps these. A page says how, or why one does not apply.

1. **Exactly-once provider calls.** A charge, refund or other change at a payment provider is recorded before it is sent, so a crash or a lost response never repeats it or loses track of it.
2. **Agreement with providers.** OpenRails' records match the provider's, even when callbacks arrive late, twice or never.
3. **Exact money.** Amounts are integer micros in the currency's registered scale; each provider receives exactly the value it expects; the books never mix currencies.
4. **Access control and tenant isolation.** Every route declares its tier and permission, and the gate fails closed: the route catalog refuses a merchant, application or provisioning route without its permission pair, `Mount` fails a route whose permissions the Authenticator does not know, and a scenario generated from the catalog refuses every route to a credential lacking its permission. A merchant route acts on the one merchant its credential is bound to, a customer route only as the signed-in customer, and every tenant query names its merchant.
5. **Contracts match the code.** The Go API, HTTP routes and schema snapshots in `api/` are regenerated with every change, and the in-process and remote clients answer alike. Before v1.0.0 any of them may change; from v1.0.0 they change only by adding to them ([compatibility](../../compatibility/README.md)).
6. **Data lifecycle.** Every table has a retention class saying how its rows leave it: permanent (never), partitioned (whole months dropped), rows (deleted past an age) or state (gone when the thing is). The class is stated in the table's comment and `internal/retention`; reads of a partitioned table name a time range. Schema migrations themselves are migratekit's (`internal/migrate`).
7. **Work scales with activity.** Background work is driven by events, watermarks and indexed due work, never a routine sweep over every row.
8. **Abuse protection.** Public and customer-facing entry points are rate-limited, and card-testing and similar attacks are refused.
9. **Destructive-action safeguards.** Irreversible or bulk commands are bounded by ceilings and holds, and every one is attributable.
10. **Consistent time.** Instants are UTC at microsecond precision, each decision reads one injected clock (`Deps.Clock`), and calendar periods handle month ends, daylight saving (IANA zones) and leap years.

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
<... the same for guarantees 2 to 10 ...>

## Tests
<Critical journeys (OR-Jxx) and other tests that cover this system, and gaps.>
```
