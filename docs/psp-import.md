# PSP import: onboarding subscribers straight from the provider

A merchant moving onto OpenRails hands over its PSP credentials; OpenRails
pulls every customer, saved card, live subscription and charge from the
provider, links each to one of the merchant's customers, and lands the
linked ones as real subscriptions, payment methods and payments. The
provider is the source of truth, so no legacy database export is needed.
[Batch import](batch-import.md) remains for books only a legacy system holds.

Rails:

- NMI: v5 customers and subscriptions, the Query API's recurring and
  transaction reports. A live schedule lands with NMI's own terms as its
  accepted schedule; a paused one as canceled at the pull.
- Stripe: customers with their cards, subscriptions in every status, and
  the charges and refunds in the window with the invoices they paid. A
  canceled subscription lands canceled at its end; an unpaid, expired or
  paused one as ended by the provider.
- CCBill: DataLink's active members (username, email, signup and rebill
  dates) and the rebills of each history window, read a day at a time.
  CCBill has no vault and no catalog to read: a member lands on the one
  provider price its product holds on the PSP (declare it with its amount
  and interval), whose terms become the accepted schedule, and a rebill's
  amount is read in that price's currency.

## Safety

The pull only reads: list, report and get calls on a read-only client. It
never refunds, voids, cancels or edits anything at the provider. A landed
subscription stays provider-run, exactly as an imported one does.

## Flow

1. **Mappings** (optional). `Client.SetPSPImportMappings`
   (`POST /v1/merchant/psp-import-mappings`) maps the merchant's own
   identifiers to customers, up to 100 per call: `{mapping: "legacy_user",
   key: "1002", customer: "<uuid>"}`. A null customer deletes one.
2. **Create.** `Client.CreatePSPImport` (`POST /v1/merchant/psp-imports`)
   names the PSP, the rules, the products its plans grant, and optionally
   `history_since` (default: the earliest live subscription's start). The pull
   runs as a job; `GetPSPImport` reports `pulling`, then `pulled`. It reads
   the roster once, then the transaction history in 30-day windows,
   committing each (`history_through`), so a retried job resumes.
3. **Review.** `ListPSPImportRecords` lists the staged objects
   (`customer`, `subscription`, `transaction`) with their facts, filtered by
   `state`: `unmatched`, `conflict`, `linked`, `imported`, `blocked`.
4. **Link.** `LinkPSPImport` links every record again by the rules and the
   current mappings, after storing manual links (`{record, customer}`; a
   manual link wins over every rule).
5. **Apply.** `ApplyPSPImport` makes each plan the linked subscriptions name
   an archived provider price of its product (terms read from the
   provider; CCBill's are declared), then lands the linked records through the billing import, a
   few customers per book, at the pull's horizon. A customer the billing
   import refuses blocks only its own records. Applying again lands what was
   linked since; landed records stay `imported`.
6. `DeletePSPImport` removes the import and its staged copies; what it
   landed stays.

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
   {"fact": "email", "via": "directory"}],
 "products": [{"product": "premium"}]}
```

`products` maps a plan to the product key it grants; an entry without
`plan` covers every plan no other entry names.

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
