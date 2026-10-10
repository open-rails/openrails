# Catalog ownership: a file and edits together

A merchant's catalog changes in two ways, and both may be used at once:

- **Documents**, the `apply` manager: `Config.Catalog` on every start,
  `Client.ApplyCatalog`, `POST /v1/admin/catalog/applications` and
  `openrails apply-catalog`.
- **Edits**, the `edit` manager: the console, the product, price, meter and
  rate-override routes, and the `Client` edit methods (`UpdateProduct`,
  `CreatePrice`, `UpdatePrice`, `SetMeter`, …).

OpenRails records which manager last set each field of each catalog object, as
Kubernetes server-side apply does (`catalog_field_owners`).

## Objects and fields

| Object | Fields |
|---|---|
| product | `display_name`, `description`, `tier_group`, `tier_rank`, `archived`, `ownership`, `entitlements`, `credit_grant`, `rate_cards` |
| price key (every version under a product's key) | `currency`, `unit_amount`, `access_duration_hours`, `billing_interval_hours`, `trial_unit_amount`, `trial_duration_hours`, `customer_amount`, `quantity`, `archived`, `psp_links` |
| meter | `event_type`, `value_property`, `aggregation`, `unit`, `group_by` |

A product and its prices are separate objects: a price change never touches its
product, and the other way round.

## Applying a document

- Each object the document names applies **whole or not at all**. It is
  skipped when a field it names has a live value an edit set differently; the
  other objects still apply. A rename across products can therefore land on
  some of them; the result names each skipped one.
- Equal values never conflict: the file and the edit then share the field. A
  later change of the file to a shared field conflicts again.
- The receipt's `changes` lists every object changed, its fields and its new
  revision; `conflicts` lists every object skipped, with each field's file
  value, live value (as the document writes them), who set it and when. Over
  HTTP it is a `200`; `openrails apply-catalog` prints it readably and exits
  non-zero when anything was skipped.
- A document that skipped nothing is recorded by its canonical content hash
  and replays from then on. One that skipped something is not recorded: the
  next start or apply retries it, and what already applied is a no-op. One
  finding, `life.catalog.conflicts`, lists the skipped objects and closes when a
  document applies whole.
- A conflict never fails `openrails.New`: the uncontested objects apply and the
  finding opens.

To resolve a conflict:

1. Change the document to agree with the live value.
2. Remove the field, or the entry, from the document: the file relinquishes it
   and the live value stays.
3. Force it: `billing.ApplyCatalogParams{Force: true}`, `?force=true` or
   `--force-conflicts` overwrites the edit and takes the field. Start-up never
   forces, and force never undoes a replay: a document already recorded as
   applied changes nothing.

Edits never conflict: they always apply and take the fields they change.

A document's `entitlement_replacements` are an operator's rename: they move the
key on every product granting it, edited or not, never conflict, and leave each
product's keys owned as they were.

## Prune

`prune: true` makes the document the whole catalog. An omitted product or price
is archived only when documents alone set its fields; one an edit touched or
created stays, and the file lets go of its fields.

## Revisions and concurrent edits

Every product, price key, meter and rate override has a `revision` that starts at
1 and advances on each change, from an edit or a document. The catalog-wide
revision (`GetCatalogRevision`) still advances on every catalog write.

An edit may send the revision it read as `expected_revision` (`UpdateProduct`,
`UpdatePrice`, `CreatePrice` of a new version, `SetMeter` (its rate card
included), `SetRateOverride`; 0 expects no meter or override yet). If the object changed
since, the edit is refused with `409 revision_mismatch` and
`metadata.revision`, the current one. Without it an edit is unconditional. The
console always sends it. Documents never carry revisions: field ownership
covers them.
