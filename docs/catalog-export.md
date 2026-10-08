# Catalog backups and editable exports

To back up the catalog with its original identities and all persisted history:

```sh
openrails catalog export --merchant MERCHANT_UUID --out catalog.snapshot.yaml
openrails catalog import --merchant MERCHANT_UUID --in catalog.snapshot.yaml
```

These host commands use the configured local database. Export takes a consistent
snapshot and writes a private file atomically; an existing file requires
`--overwrite`. The YAML is a versioned `kind: catalog_snapshot` document with
named rows and a SHA-256 integrity digest. It is a backup artifact, separate from
an editable `ApplyCatalog` declaration.

## What the snapshot retains

Every row and stored column from these eight catalog tables is included:

| Table | Retained state |
|---|---|
| `products` | All products, including archived products, original UUIDs, current revisions, entitlements, credit grants and tiers |
| `prices` | All immutable price revisions, including archived ones, original UUIDs, revisions, durations, trials and deposit bounds |
| `price_key_movements` | Complete retained price-key movement history |
| `price_psp_bindings` | Original provider-account IDs and remote product/price references |
| `catalog_meters` | Current meter definitions |
| `catalog_rate_cards` | All current default and customer-specific rate cards, with IDs |
| `catalog_applications` | Original application receipts, canonical hashes and revision coordinates |
| `product_archive_operations` | Original product-archive request and replay receipts |

The merchant's catalog revision is retained too. Product revisions are counters:
older mutable product bodies and deleted or overwritten meter/rate-card definitions
were never retained in the database, so the snapshot cannot reconstruct them.

Archived products and prices are essential: a purchase keeps its original price
ID after an offer changes or retires. Restoring this snapshot preserves that ID
and its owning product. It never substitutes today's price or creates a new
revision for an old purchase.

## Restore requirements and behavior

Select the destination merchant explicitly with `--merchant`; its UUID must match
the artifact. Provision that merchant separately. For a new local destination,
`openrails billing prepare-target --merchant MERCHANT_UUID --slug my-app --unbound`
creates a host-local merchant identity; hosted identity preparation instead uses
the command's group/owner flags.

The catalog must be empty, with catalog revision zero. Restore refuses to merge,
replace or delete an existing catalog. All rows and the import receipt commit in
one transaction; corruption, foreign-key failures or unmet prerequisites leave
no partial catalog. Concurrent imports of the same artifact commit once.

Provider accounts referenced by bindings must already exist with their original
UUID, rail, environment and account ID. Customers referenced by negotiated rate
cards must also exist with their original merchant-scoped UUID. The snapshot
lists these prerequisites without copying credentials or customer records; it
never remaps a reference merely because a destination account has the same name.

For ordinary OpenRails accounts, the PSP UUID is derived from rail, environment
and provider account ID. Applying the same merchant PSP configuration in the
destination recreates that UUID; supply credentials separately and use the same
sandbox/production environment. Verify those IDs against the snapshot before
import. The public `Client.EnsureCustomer` accepts each original customer UUID.
Older externally imported PSPs may have non-derived UUIDs: the normal creation
API cannot select an arbitrary replacement ID. Those require explicit identity
provisioning by a database operator; importing the whole billing archive instead
retains its PSP/customer rows directly. Never rewrite snapshot IDs to bypass an
unmet dependency.

An identical successfully imported artifact remains a no-op even after later
catalog edits. A different artifact is refused on that occupied destination.
**Restore is not rollback.** Original catalog-application hashes are retained, so
old startup YAML also remains a replay after restore.

Neither command contacts providers, charges customers, grants access, schedules
jobs, or imports financial records. Archive-operation receipts retain their
accepted request; their associated refunds, reviews and provider jobs are outside
this catalog-only snapshot. Purchases, subscriptions, grants, balances and billing
history require the full [merchant billing archive](../internal/merchantarchive/README.md).
For a whole billing-book restore, use `billing export/import` directly into its
empty destination, rather than importing a catalog first. The full billing
archive currently excludes product-archive operation replay receipts; this
catalog snapshot includes them. Its [documented exclusions](../internal/merchantarchive/README.md#exclusions-and-refusals)
remain relevant when planning a complete deployment move.

Artifacts are bounded at 64 MiB, with bounded YAML depth and node counts. Export
also bounds accumulated rows before building YAML. An oversized or unsupported
catalog is refused rather than truncated; aliases, tags, duplicate keys and
unknown fields are refused on import.

## Export editable current offers

The existing declaration workflow remains available:

```sh
openrails dump-merchant-catalog --slug my-app > catalog.yaml
openrails apply-catalog --merchant my-app --file catalog.yaml
```

This export includes unarchived products and their unarchived prices, all meters,
and default rate cards attached to those products. It preserves entitlements,
tiers, recurring and trial terms, prepaid credit grants and expiry, deposit
ranges, and configured provider references. It omits database IDs, revision
history, archived offers and customer-specific rate-card overrides.

This document follows ordinary partial-application rules. Omitted products and
prices remain unchanged; the export does not set `prune: true`. Adding that flag
archives omitted products and prices, without deleting omitted meters. A
product's explicit `rate_cards` list replaces its default cards, subject to
dependency checks.

Applying the same canonical document again returns its original receipt, even if
the catalog changed afterward. Reapplying an old declaration is not rollback;
formatting changes do not create a new application. Edit relevant declarations
to make a new change, or use the catalog client/API.

Products and prices match by product key and product-local price key. A fresh
merchant creates equivalent local offers with its own identities. Provider links
remain account-specific. To copy offers to a different merchant, remove `psps`
and `psp_links` declarations or replace them with independently provisioned
references. Omitting them preserves existing destination bindings; an empty list
cannot remove an existing binding. Catalog application never creates remote
provider objects.
