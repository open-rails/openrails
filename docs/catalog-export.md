# Exporting and reapplying a catalog

Export the merchant's current offers from the database as ordinary catalog YAML:

```sh
openrails dump-merchant-catalog --slug my-app > catalog.yaml
openrails apply-catalog --merchant my-app --file catalog.yaml
```

The commands use the host's configured database. The export takes a consistent
catalog snapshot and includes unarchived products and their unarchived prices,
all meters, and default rate cards attached to those products. It preserves
entitlements, tiers, recurring and trial terms, prepaid credit grants (including
expiry), customer-selected deposit ranges, and configured provider references.
It writes explicit empty or null values where needed so an imported declaration
does not accidentally inherit a destination's optional terms.

This is an editable application document, with the same rules as a handwritten
catalog. Omitted products and prices remain unchanged. The export does not set
`prune: true`. Adding that flag archives omitted products and prices; it does not
restore archived history or delete omitted meters. A product's explicit
`rate_cards` list replaces its default cards, subject to dependency validation.
Customer-specific rate-card overrides are outside this export.

Applying the same canonical document again returns its original receipt, even
if the catalog changed afterward. **Reapplying an old export is not a rollback.**
To make a new change, edit the relevant declarations and apply the resulting new
document, or use the catalog client/API. YAML formatting changes do not create a
new application.

Products and prices are matched by product key and product-local price key.
The export intentionally omits database IDs and revision counters: a fresh
merchant can create equivalent local offers, while an existing merchant keeps
its own immutable identities and history. New immutable price terms create a
new revision; matching old terms may reuse an existing revision.

Provider references are account-specific. The default export retains their
`psp_id` and remote object identifiers; applying them requires the same configured
PSP accounts and validates their ownership. To copy offers to a different merchant,
remove the `psps` and `psp_links` declarations from the exported prices, or replace
them with independently provisioned references for that merchant. Omitting them
preserves existing destination bindings; an empty list cannot remove an existing
binding. Catalog application never creates remote provider objects.

## Back up all billing state

The YAML export excludes archived products, archived price revisions, application
receipts, payments, subscriptions, grants, balances and ledger history. Use the
existing billing archive when these identities and records must survive:

```sh
openrails billing export --merchant MERCHANT_UUID --out billing.snapshot --source-stopped
openrails billing import --merchant MERCHANT_UUID --in billing.snapshot
```

For export, `--source-stopped` attests that source writers have been stopped. The
import target must be empty and separately provisioned with the same merchant
UUID; keep its writers and workers stopped until cutover. This versioned archive
restores billing state atomically, without replaying purchases or moving money.
Credentials are provisioned separately. See the [merchant archive contract](../internal/merchantarchive/README.md)
for the full portability and cutover requirements.
