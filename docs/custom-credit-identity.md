# Custom credit identity

Custom credits are merchant-owned consumable units. Their existing `custom_credit_types.id` UUID is immutable; ledger accounts, transfers, grants, product credit specs, subscription/payment snapshots, catalog balances, usage events and admission holds store `credit:<UUID>`. The registry's `merchant_id`, name and decimal scale remain authoritative. Built-in currencies retain their existing ISO codes and scales.

Public APIs accept `merchant-slug/unit-name`. The configured merchant naming authority resolves that slug, including active forwards, before the service verifies that the owner UUID matches the authorized merchant. A reclaimed name therefore cannot address its former owner's units. Responses project the unit UUID through the owning merchant's current canonical name. Bound merchants require AuthKit authority; explicitly unbound host-mode merchants use their local binding. Internal UUID input still requires an owned registry lookup.

A merchant-scoped catalog can declare an unqualified custom name, or qualify it through the same authority. Repeated catalog pushes preserve the registry UUID and scale. Catalog export writes the local unit name so the manifest remains portable. Removing a catalog balance does not delete units still referenced by grants or history.

Custom units can be granted, reserved, spent, captured, expired and revoked through the existing credit ledger. They cannot create debt, invoices or FX. Capture beyond funded custom balance fails without releasing its hold or posting a partial debit; a corrected retry may succeed. ISO capture retains its existing overdraft behavior.

## Pre-launch cutover

Migration `0021_custom_credit_identity` refuses databases with old name-based custom financial codes or catalog units. Its error names the affected table. It does not rewrite append-only history, interpret a former slug as ownership, or delete data. Migration `0022_custom_credit_identity_validate` validates the new constraints in a separate transaction so the short schema-change locks are released before scanning existing rows. Both run before application startup.

Before applying the release, inventory any retained development database for custom `currency` values containing `/` name-based `catalog_credit_balances.unit` values, and noncanonical unit fields in product credit specs or subscription/payment snapshots. Export the affected merchant state and decide explicitly how to recreate that pre-launch state in a fresh database; preserve unrelated databases. The numbered tracker issue for retained environment inventory records any state awaiting that owner decision. New installations and the isolated integration fixtures use UUID identities directly.

Do not run old and new writers against one database. Rollback requires the matching pre-cutover application and database snapshot; there is no mutable-name compatibility reader. Existing volatile admission reservations for name-based units require re-admission by their owning host after the coordinated cutover; do not reinterpret an old hold using whoever owns that name later.
