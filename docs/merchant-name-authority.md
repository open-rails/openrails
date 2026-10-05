# Merchant names

A merchant's billing UUID is its identity. Its name (`slug`) is a lookup key
that can change.

- **Where names live.** OpenRails owns merchant names. With a control plane, a
  merchant is bound to one AuthKit group by UUID, and that binding never
  changes; the group's own AuthKit name is not the merchant's name. Without a
  control plane the host's merchants share one local namespace.
- **Uniqueness.** A name is unique among live merchants. A rename keeps the
  former name as an alias that forwards to the merchant and that no other
  merchant can claim until it expires; the merchant can take it back. Retiring
  or deleting a merchant releases its name and aliases. The rename interval and
  how long a former name is kept are the deployment's naming policy
  ([merchant provisioning](merchant-provisioning.md#merchant-identity-and-names)).
- **Resolution.** Name-bearing boundaries (CLI `--merchant`, a manifest, the
  `OpenRails-Merchant` header, a Host) resolve the current name or a valid
  alias once. Everything after runs against the merchant UUID: imports, ledger
  operations, jobs and foreign keys never store a name.
- **Reclaimed names.** Provisioning a name another merchant released creates a
  distinct billing identity. The former merchant keeps its UUID, money and
  provider state. A display name updates the selected UUID; it never upserts
  over another owner's row by name.

On the command line a bare `--merchant` value is a name, even a UUID-shaped
one; `--merchant id:<uuid>` addresses the merchant directly. In Go,
`openrails.WithMerchant` takes a name and `openrails.ForMerchantID` the UUID
([choosing the merchant](client-merchant-selection.md)).
