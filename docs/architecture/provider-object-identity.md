# Provider object identity

Provider object references are scoped by the merchant and immutable PSP UUID captured during checkout selection or webhook authentication. A rail name, mutable PSP key, or customer identity does not identify a provider account.

Price bindings belong to the immutable PSP UUID. Catalog labels can change without moving these bindings. Archived accounts remain addressable for authenticated inbound events and existing obligations; purchase admission selects only active accounts.

Local collision regressions must exercise checkout and webhook workflows with two same-rail accounts sharing external references. Provider probes, reconciliation evidence, card updater support, dashboards and webhook health remain intact.

Price bindings are stored in `price_psp_bindings`, with a composite merchant/PSP foreign key and account-scoped unique plan, Stripe price, CCBill recurring billing option and Solana plan references. The catalog `psp_links` map is a projection using current labels and explicit `psp_id` values; duplicate labels project under UUID keys. It is not persisted JSON. A shared CCBill FlexForm is a checkout page rather than price identity: recurring billing option lookup remains exact, and ambiguous form-only events fail closed.

Saved-method and stored-credential references also require PSP identity. Custody import manifests carry `source_psp_id` so the same source vault reference at a sibling gateway cannot be adopted. Webhook deduplication namespaces include the captured PSP or custodian UUID.

Operations resume through `PSPScopeByID`, retaining the selected account's credentials after a key rename or archive. Fresh checkout still resolves active admission separately. The NMI adoption path verifies customer and price identity before completing a pre-existing local subscription.

Custodian-held instruments capture a same-merchant `custodian_id` independently of their PSP. Token references, network-token updates, account-updater selection and job completion use that captured account. PSP configuration changes cannot move an existing instrument to a different vault. An updater result can unpark and rotate the selected account's instrument without touching a sibling account with the same token.

The fresh v1 schema is a hard cut: the `prices.psp_links` storage column is removed. Existing pre-v1 databases are not upgraded by this change.
