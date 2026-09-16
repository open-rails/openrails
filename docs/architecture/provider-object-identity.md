# Provider object identity

Provider object references are scoped by the merchant and immutable PSP UUID captured during checkout selection or webhook authentication. A rail name, mutable PSP key, or customer identity does not identify a provider account.

Price bindings belong to the immutable PSP UUID. Catalog labels can change without moving these bindings. Archived accounts remain addressable for authenticated inbound events and existing obligations; purchase admission selects only active accounts.

Local collision regressions must exercise checkout and webhook workflows with two same-rail accounts sharing external references. Provider probes, reconciliation evidence, card updater support, dashboards and webhook health remain intact.
