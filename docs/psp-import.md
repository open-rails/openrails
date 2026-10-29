# PSP import: onboarding subscribers straight from the provider

A merchant moving onto OpenRails hands over its PSP credentials; OpenRails
observes the provider's catalog, pulls every customer, saved card, live
subscription and charge from the provider, links each to one of the
merchant's customers, and lands the linked ones as real subscriptions,
payment methods and payments on the provider prices a person mapped. The
provider is the source of truth, so no legacy database export is needed.
[Batch import](batch-import.md) remains for books only a legacy system holds.

Rails:

- NMI: v5 plans, customers and subscriptions, the Query API's recurring and
  transaction reports. A live schedule lands with NMI's own terms as its
  accepted schedule; a paused one as canceled at the pull.
- Stripe: products and prices (active and inactive), customers with their
  cards, subscriptions in every status, and
  the charges and refunds in the window with the invoices they paid. A
  canceled subscription lands canceled at its end; an unpaid, expired or
  paused one as ended by the provider.
- CCBill: DataLink's active members (username, email, signup and rebill
  dates) and the rebills of each history window, read a day at a time.
  CCBill has no vault and no catalog to read: see [CCBill prices](#ccbill-prices).

## Safety

The pull only reads: list, report and get calls on a read-only client. It
never refunds, voids, cancels or edits anything at the provider. A landed
subscription stays provider-run, exactly as an imported one does.

## Onboarding import

The first time a process loads a PSP's configuration (an NMI, Stripe or CCBill
account that is declared, live and not a duplicate), OpenRails creates the
PSP's **onboarding import** (`origin: onboarding`) and queues its pull, in one
transaction. A PSP has at most one: whichever replica or host sharing the
merchant loads it first creates it, and the others find it.
Existing PSPs get theirs on the first boot after upgrading. It runs in the
background, also with provider writes `readonly`, and lands nothing: a person
reviews, maps, links and applies it as any import. Find it with
`ListPSPImports{PSP, Origin: onboarding}`. A tool that just started an engine
calls `Engine.EnsurePSPOnboarding` first: it does the same step at once (and
restarts a failed import), so the list shows the import's latest state or its
restart. A PSP another import is pulling or applying is passed over and named
in an error matching `billing.ErrPSPImportInProgress`; call again once that
import finishes. An import a person creates is
`manual` and never stands in for it; deleting the onboarding import makes it
again at the PSP's next load.

**Retries.** Import steps run on their own River queue (`psp_import`, two
workers per fleet), apart from renewals and dunning. A provider that does not
answer (network, timeout, 5xx, 429), an unreachable secret store or database
is retried without failing a pull: attempt⁴ seconds apart, at most an hour,
up to 10 000 attempts (about 14 months), each resuming from the last committed
history window. Any other failure (an answer OpenRails cannot read, a row the
database refuses) is retried 5 times, then fails the import with its `error`,
so it can be read and deleted. A landing makes at most 10 attempts; a failed
one is applied again. Two causes fail a pull at once, with `error_code`:

- `psp_credentials_rejected`: the provider refused the credentials (NMI v5
  401/403 or a Query API authentication error, Stripe 401/403, DataLink
  401/403 or "Authentication failed"). It also opens one
  `consistency.psp_import.credentials_rejected` finding per PSP (high,
  review queue) and alerts the operator. The finding closes when a pull of
  the PSP is answered again, or the PSP is no longer importable.
- `psp_not_readable`: the PSP is gone, its rail has no import, or a
  credential the pull needs is not declared.

A failed onboarding import restarts in place (same import, same staged
records) when a process first loads the PSP's configuration or sees it change:
fix the credentials and it pulls. A pull refused with credentials replaced
while it ran retries with the new ones. While they stay rejected, each boot
costs one provider read.

**History** reads from the earliest object the provider lists, customers
included (an NMI vault's or Stripe customer's creation, a subscription's
start, a CCBill signup), unless a manual import names `history_since`.

## Flow

1. **Customer mappings** (optional). `Client.SetPSPImportMappings`
   (`POST /v1/merchant/psp-import-mappings`) maps the merchant's own
   identifiers to customers, up to 100 per call: `{mapping: "legacy_user",
   key: "1002", customer: "<uuid>"}`. A null customer deletes one.
2. **Create**, or take the onboarding import. `Client.CreatePSPImport`
   (`POST /v1/merchant/psp-imports`) names the PSP, the rules and optionally
   `history_since` (default: the earliest object the provider lists). Rules
   default to those of the PSP's latest import, and for its first import to
   `billing.DefaultPSPImportRules` (the directory's email). One import per
   PSP pulls or applies at a time: another is `409 psp_import_in_progress`
   (a pulled, unapplied onboarding import does not hold it). The pull runs
   as a job; `GetPSPImport` reports `pulling`, then `pulled`. It observes the
   PSP's catalog, reads the roster once, then the transaction history in
   30-day windows, committing each (`history_through`), so a retried job
   resumes. `UpdatePSPImport` replaces the rules of either kind.
3. **Review.** `ListPSPImportRecords` lists the staged objects
   (`customer`, `subscription`, `transaction`) with their facts, filtered by
   `state`: `unmatched`, `conflict`, `linked`, `imported`, `blocked`, `held`.
4. **Map prices.** `ListProviderCatalog` with `import` lists the plans the
   import's subscriptions name, with the provider's terms, how many
   subscriptions name each, and the provider price mapping it (`price`, null
   when unmapped). Map one by creating a provider price on a product
   (`CreateProviderPrice`, or a catalog document's `provider_prices`); its
   terms come from the observed plan. Nothing is mapped by default.
5. **Link.** `LinkPSPImport` stores manual links (`{record, customer}`; a
   manual link wins over every rule) and CCBill price picks (`prices`), then
   relinks in the background: every record not yet imported is linked again
   by the rules and the current mappings, and proposed again from what the
   pull read. The import is `pulling` until it has; wait for `pulled` before
   applying. Once a later import of the PSP has pulled, an earlier one is
   neither linked nor applied (`409`): use the later one.
6. **Apply.** `ApplyPSPImport` lands the linked records through the billing
   import as the provider's book, a few customers per book, at the pull's
   horizon, then approves the import's [corrections](#reconcile). Only a
   person signed in, or the in-process host, applies; an API key is
   `403 person_required`. A subscription lands on the provider price mapping
   its plan; one whose plan is unmapped stays `blocked` ("plan X is
   unmapped"): map it, link and apply again. A customer the billing import
   refuses blocks only its own records. Applying again lands what was linked
   since; landed records stay `imported`, so nothing lands twice, and a later
   import of the same PSP finds them held. A linked NMI vault lands as the
   customer's payment method. A linked Stripe customer also lands as the
   customer's link at the PSP (`psp_customers`), so a later subscription
   Stripe bills on it attributes to the customer. A customer with several
   picks the one a live subscription bills, else the newest; a customer
   already linked to another keeps it; either way the record's reason says
   so. One another customer holds blocks its customer.
7. `DeletePSPImport` removes the import and its staged copies; what it
   landed stays. The observed catalog stays.

Creating a provider price needs `merchant:catalog:manage`; the import routes
need `merchant:config:manage`.

The billing console (`BillingAdmin`, Ops) runs the same flow: PSP imports
(Reconcile, mappings upload), an import's records, Provider prices (Map to
product) and Apply; see [billing administration](billing-admin.md#ops-pages).

## Reconcile

Every import, the first one and any later one, compares what it read with
what OpenRails already holds of the PSP. A later import is how a person
re-reads a PSP: OpenRails trails it by webhook and never pulls on a
schedule.

- **Basis.** Before the roster read the import records every subscription of
  the PSP at its row version. A state change is proposed only for a row still
  at that version; a row created since is never read as absent. A row that
  moved says so on its record ("changed after the pull read it").
- **Held.** A staged object OpenRails already holds is `held`, with its
  holder as the customer: a subscription by its provider id, a provider
  customer by the cards or customer link naming it, a transaction by its
  payment or by its held subscription. If a rule or a manual link names
  another customer it is a `conflict` naming both, and nothing is proposed
  about it until a person links it: a landed subscription never moves. What
  belongs to a conflicting object and is not held itself is a `conflict`
  too. Held records never land.
- **Proposals.** The differences in what it holds become proposals (pull.*
  findings) tagged with the import (`pull_id`): a missed renewal
  (`adopt_subscription` and `record_charge`), a schedule gone at the
  provider (`cancel_subscription`), a changed card (`update_card`), and a
  provider's other price, schedule terms or start (`correct_subscription`).
  A Stripe price counts once an invoice for it is paid; no price moves while
  a tier change is scheduled. Unmapped plans, vault moves and pauses are
  review-only (`pull.subscription.drift`). An NMI roster under a tenth of the
  PSP's live book fails the import with `pull_roster_implausible` before it
  records anything. `PSPImport.proposals` counts them; `ListProposals` with
  `pull_id` lists them. A later pull resolves the proposals it no longer
  raises; a relink resolves none. The legacy provider refresh leaves a
  pull's proposals alone.
- **Corrections.** Apply approves, in its requester's name and under one
  approval run, the import's open `record_charge`, `update_card`, renewal
  and period-end `adopt_subscription` proposals, and `correct_subscription`
  ones that keep access, unless a person once dismissed them. Everything
  else waits for a person: cancels, past due, refunds, revokes, a move to
  another product's price. A dismissed correction opens again only when the
  facts it adopts change, not when the provider bills another period.
  `openrails undo-run` reverses the run's corrections of price, product and
  start; a provider schedule it accepted stays, as accepted schedule versions
  are immutable, and recorded charges stay.
- **Apply re-reads what changed since the pull.** A record OpenRails came to
  hold after the pull (a webhook landed it, or a host's book) is held at
  Apply, not landed twice. One a host's book landed is diffed then, its basis
  its current row version.

The catalog list is a deletion source too: a complete, non-empty list that
lacks an object an earlier list held marks it deleted, and seeing it again
clears that. Only a pull whose roster lists a subscription (and passes the
NMI breaker) marks anything: wrong credentials list another account.

An onboarding import applied days after it pulled reads what was true then.
Apply holds what landed meanwhile; to propose what changed meanwhile, create
another import right after Apply.

## Provider facts outrank a host's book

Each subscription records whose facts it keeps (`host` or `provider`), one
way only, from host to provider. A [host's legacy book](batch-import.md)
lands `host` rows and keeps converging them; it skips a `provider` row
(reason `provider_authority`) and writes nothing for it: no state, schedule,
mandate or charge. An import's book lands `provider` rows and skips every row
OpenRails holds (`held`): its differences are proposals. A row becomes the
provider's when an Apply reads it unchanged, when a person approves a
proposal on it, or when a webhook or verification read converges it. So
provider truth wins in either order: a legacy book after a pull is skipped,
and a pull after a legacy book corrects it.

## Observed catalog

`ListProviderCatalog` (`GET /v1/merchant/provider-catalog`) lists one PSP's
catalog as OpenRails last observed it: Stripe products and prices, NMI plans
(as prices), each with `active`, `deleted_at`, `retired`, the provider's
`terms` (`stated_amount` in major units as the provider states it; NMI names
no `currency`) and when and how it was observed. `psp` names the PSP by id
or key. Filters: `kind`, `mapped`, `retired`, `import`.

OpenRails only observes; it never writes a provider's catalog. A pull lists
the whole catalog and reads by id what the lists lack: a Stripe price created
inline on a subscription, an NMI plan gone from the gateway (recorded from
its subscriptions' copy of it, as deleted), every object a provider price
binds and every object stored earlier. An object is recorded deleted when
the provider answers that it is missing (Stripe `resource_missing`, NMI
`E_NOT_FOUND`), or when a complete list lacks what an earlier list held; any
other answer leaves it as it was. A gone object never
observed is recorded from the terms its provider price stores. Reading a
Stripe price by id reads its product too. The latest observation wins: what
a pull listed never replaces a later read. A price in a currency OpenRails
does not support, or without a fixed amount, is recorded without an amount
and cannot be mapped; an object the provider does not answer for is logged
and left for the next read. Mapping a plan never observed reads it once by
id; verifying a provider price (`GetPrice` with `verify`) reads it again.

An import pulled before OpenRails observed provider catalogs has no
catalog: create a new import to pull the PSP again.

Each price row also carries `parked` (the live subscriptions parked on it
while unmapped) and `finding` (its open `catalog.price.unmapped` finding).
After the first pull, Stripe `product.*` and `price.*` webhooks and the plans
NMI subscriptions name keep the catalog current (`observed_via: webhook` or
`read`).

A provider price whose object the provider retired (inactive, deleted, or
its Stripe product is) is not for sale: it is no tier-change target and its
`provider.retired` is true. It still resolves, so its subscriptions keep
billing and keep their access. A deleted plan still maps, so its
grandfathered subscriptions land.

## Backlog and webhooks

Until a PSP's first pull, while one is pending, and while its latest pull
stages a subscription not yet landed, the import owns that subscription: a
webhook about it adopts nothing, and a park it needs is `import_owned`, with no
finding, so a fix still wakes it. A failed pull is no pull. Once pulled and not staged (a subscription that
began after the pull), converge lands it, adopts it (NMI) or parks it with a
finding (`ListParkedSubscriptions`). A park closes when its subscription lands,
by converge or by an import's Apply; the trigger on `subscriptions` closes it
either way, so nothing lands twice.

## CCBill prices

CCBill has no catalog to read: its provider prices are declared with their
amount and interval. A member lands on the price picked for it by hand
(`LinkPSPImport` `prices: [{record, price}]`, shown as the record's
`manual_price`; a null price clears it), else on the one recurring provider
price of the import's PSP, on any product and archived ones included, whose
amount equals its latest rebill read in that price's currency and whose
interval is the cadence the member rebills at (in calendar days, from its
latest rebill's date to its next rebill or expiry date, else between its last
two rebills; a tenth either way, at least two days). No rebill, no match or several matches
leave it `blocked`, naming the count. A pick is a live recurring provider
price of the import's PSP, for a subscription; only plan-less (CCBill)
imports take picks. Its terms become the
accepted schedule, and a rebill's amount is read in that price's currency.

## Facts and rules

Each staged object carries the identity facts its provider holds: `email`
(lowercased), `name`, `username`, `order_id`, `customer_ref` (the vault or
customer id), `subscription_ref`, `billing_id`, `shipping_id`, `plan`, and
NMI's merchant-defined fields as `mdf.<field>`.

Stripe also stages each object's metadata as `metadata.<key>`. A rule reads one fact, optionally through a `pattern` with one `{key}`
placeholder, and looks the key up `via`:

| via | looks up |
|---|---|
| `directory` | `email` or `username` in the merchant's [directory](customer-contacts.md); exactly one customer must hold it |
| `mapping` | the merchant's mapping named `mapping` |
| `customer_id` | the key itself, as a customer id |

Without rules an import takes its PSP's latest import's, else links by the
directory's email (`billing.DefaultPSPImportRules`).

```json
{"psp": {"key": "mobius"},
 "rules": [
   {"fact": "shipping_id", "pattern": "shipment-{key}", "via": "mapping", "mapping": "legacy_user"},
   {"fact": "order_id", "via": "mapping", "mapping": "legacy_order"},
   {"fact": "email", "via": "directory"}]}
```

Linking runs customers first, then subscriptions, then transactions. A
subscription belongs to its vault's customer and a transaction to its
subscription's; a vault no rule links takes the customer all its
subscriptions name. Every rule and parent that names a customer must agree:
otherwise the record is a `conflict` (its reason lists who said what), and
what belongs to it is too. Nothing unmatched or conflicting lands.

A transaction is placed on a subscription when the provider names it, else
by the subscription's order id, else by a vault with one subscription that
had started. A charge no subscription holds stays `blocked`; one-off history
does not land yet.
