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

## Flow

1. **Customer mappings** (optional). `Client.SetPSPImportMappings`
   (`POST /v1/merchant/psp-import-mappings`) maps the merchant's own
   identifiers to customers, up to 100 per call: `{mapping: "legacy_user",
   key: "1002", customer: "<uuid>"}`. A null customer deletes one.
2. **Create.** `Client.CreatePSPImport` (`POST /v1/merchant/psp-imports`)
   names the PSP, the rules and optionally `history_since` (default: the
   earliest live subscription's start). The pull runs as a job;
   `GetPSPImport` reports `pulling`, then `pulled`. It observes the PSP's
   catalog, reads the roster once, then the transaction history in 30-day
   windows, committing each (`history_through`), so a retried job resumes. An
   unreachable provider fails the job, which retries.
3. **Review.** `ListPSPImportRecords` lists the staged objects
   (`customer`, `subscription`, `transaction`) with their facts, filtered by
   `state`: `unmatched`, `conflict`, `linked`, `imported`, `blocked`.
4. **Map prices.** `ListProviderCatalog` with `import` lists the plans the
   import's subscriptions name, with the provider's terms, how many
   subscriptions name each, and the provider price mapping it (`price`, null
   when unmapped). Map one by creating a provider price on a product
   (`CreateProviderPrice`, or a catalog document's `provider_prices`); its
   terms come from the observed plan. Nothing is mapped by default.
5. **Link.** `LinkPSPImport` links every record not yet imported again by
   the rules and the current mappings, after storing manual links
   (`{record, customer}`; a manual link wins over every rule) and CCBill
   price picks (`prices`).
6. **Apply.** `ApplyPSPImport` lands the linked records through the billing
   import, a few customers per book, at the pull's horizon. A subscription
   lands on the provider price mapping its plan; one whose plan is unmapped
   stays `blocked` ("plan X is unmapped"): map it, link and apply again. A
   customer the billing import refuses blocks only its own records. Applying
   again lands what was linked since; landed records stay `imported`, so
   nothing lands twice.
7. `DeletePSPImport` removes the import and its staged copies; what it
   landed stays. The observed catalog stays.

Creating a provider price needs `merchant:catalog:manage`; the import routes
need `merchant:config:manage`.

## Observed catalog

`ListProviderCatalog` (`GET /v1/merchant/provider-catalog`) lists one PSP's
catalog as OpenRails last observed it: Stripe products and prices, NMI plans
(as prices), each with `active`, `deleted_at`, `retired`, the provider's
`terms` (`stated_amount` in major units as the provider states it; NMI names
no `currency`) and when and how it was observed. Filters: `kind`, `mapped`,
`retired`, `import`.

OpenRails only observes; it never writes a provider's catalog. A pull lists
the whole catalog and reads by id what the lists lack: a Stripe price created
inline on a subscription, an NMI plan gone from the gateway (recorded from
its subscriptions' copy of it, as deleted), and every object stored earlier.
An object is recorded deleted only when a read by id finds it gone. Mapping
a plan never observed reads it once by id; verifying a provider price
(`GetPrice` with `verify`) reads it again.

A provider price whose object the provider retired (inactive, deleted, or
its Stripe product is) is not for sale: it is no tier-change target and its
`provider.retired` is true. It still resolves, so its subscriptions keep
billing and keep their access. A deleted plan still maps, so its
grandfathered subscriptions land.

## CCBill prices

CCBill has no catalog to read: its provider prices are declared with their
amount and interval. A member lands on the price picked for it by hand
(`LinkPSPImport` `prices: [{record, price}]`, shown as the record's
`manual_price`; a null price clears it), else on the one provider price of
the import's PSP, on any product and archived ones included, whose amount
equals its latest rebill read in that price's currency. No rebill, no match
or several matches leave it `blocked`, naming the count. Its terms become the
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

Without rules an import links by the directory's email.

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
