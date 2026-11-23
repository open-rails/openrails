# Merchant configuration

This system registers merchants and runs their lifecycle; serves each merchant's
configuration (display name, settings, alert webhooks, PSPs and custodians with
credentials) from a file or Vault, never PostgreSQL; records the identities history
points at; routes callbacks to their PSP; and moves a book between deployments. Guides:
[merchant configuration](../client.md#merchant-configuration-and-psps), [Vault](../standalone.md#vault),
[portability](../client.md#imports-and-archives), [tenant binding](../embedded.md#merchants).

## Concepts

- **Merchant**: a `billing.merchants` row: `id` (the identity), `slug` (a renamable
  name), `status` (`active`, `deleted`), optional `permission_group_id` (its AuthKit
  group, equal to `id`) and `tenant_id`. Global by design.
- **Declaration**: `openrails.MerchantDeclaration`, an entry of `Config.Merchants` or
  of the standalone manifest.
- **Documents**: `merchantdocs.Merchant`, `merchantdocs.PSP`, `merchantdocs.Custodian`;
  a `merchantdocs.Set` is one merchant's, with revisions and those it does not serve
  (`Rejected`).
- **Source**: `merchantdocs.FileSource` (read-only) or `merchantdocs.VaultSource`
  (KV v2, check-and-set).
- **PSP and custodian identities**: `billing.psps` and `billing.custodians` rows;
  their settings and credentials are their documents.
- **Write posture**: `billing.merchant_write_posture`, a `readonly` or `limited` hold
  on the merchant's provider writes; no row is full.

## How it works

### Registration and lifecycle

- **Embedded.** `openrails.New` provisions each declaration (`upsertMerchantConfig`
  → `merchantbootstrap.ProvisionMerchant`): found by slug, else registered without a
  group (`db.RegisterUnboundMerchant`). A `Binding` is verified first
  (`merchantbinding.Verify` over `Deps.MerchantGroups`) and never inserted.
- **Standalone.** The control plane creates the row and its AuthKit group in one
  transaction (`merchants.Service.Provision`): [standalone server](standalone-server.md).
- **Names.** `Service.Rename` applies `config.NamingPolicy`: an interval from
  `slug_changed_at` (`billing.MerchantRenameTooSoonError`) and former names in
  `merchant_slug_aliases`. `GetBySlug` resolves a current or unexpired former name.
- **Soft delete.** `Service.SoftDelete` and `Service.Restore` keep every record; route
  gates refuse a merchant that is not live (`merchant_unresolved`).
- **Retirement.** `Service.RetireUnused` refuses any activity (`MerchantHasActivity`),
  another group or a reserved name, tombstones the row and releases its group;
  `CompletePendingGroupReleases` retries a failed release.
- **Purge.** `Service.TakePurgeInventory` records counts and document names in
  `maintenance_runs`. `Service.Purge` deletes `merchantOwnedTables`, tombstones the
  row and records a `merchant_purge` run in one transaction, and queues
  `openrails.merchant_secret_cleanup` (`SecretCleanupArgs`), retried by River until
  the merchant's Vault subtree is empty.

### Configuration

- **Source.** `wireMerchantConfig` (`internal/app/merchants_wiring.go`) uses
  `VaultSource` when `vault.kv_mount` is set, else `FileSource`, behind one
  `merchantdocs.Cache` that every database handle reads settings through
  (`merchantconfig.Store`). With a file, `PutDeclaration` fills the `FileSource` and
  edit routes are not mounted (`FeatureMerchantConfigManages`).
- **Vault.** One document per object under `VaultSource.MerchantPath`: `merchant`,
  `psps/<key>`, `custodians/<key>`. Keys match `merchantdocs.KeyShape`; decoding
  refuses unknown fields (`decodeStrict`). A declaration naming configuration beside
  Vault is refused (`config.RefuseDeclarationBesideVault`).
- **Cache.** A merchant loads on first use (`Cache.Get`); a Vault set reloads when an
  access finds it older than `merchantdocs.Recheck` (30 s) or a replica announces an
  edit (`NotifyMerchantConfig`, `Cache.Listen`). A failed reload keeps serving; with
  Vault down at start the cache is held (`Cache.Hold`, `ServeMerchantsService`).
- **Reconciliation.** Each load runs `merchants.Service.SyncConfig` in one merchant
  transaction: validate each document, refuse another merchant's account
  (`AssertPSPUnowned`, `AssertCustodianUnowned`), register identities, refuse a changed
  one, and fingerprint credentials (`CredentialFingerprinter`) to catch one gateway
  account declared twice. A refused document keeps its previous version serving
  (`keepServed`) and opens `consistency.merchant_config.not_served`.
- **Follow-ups.** `syncMerchantConfig` normalizes settings (`merchantconfig.Normalize`)
  and applies `secrets.scim_token` (`scim.Declare`); `followPSPPosture`,
  `followStripeWebhooks` and others react to the change.
- **Edits** (Vault only). `Cache.PutMerchant`, `PutPSP` and `PutCustodian` write with
  check-and-set (`expected_revision`, `409 revision_mismatch`). A change to a billable,
  access, routing or security field is a destructive `configuration_change`
  (`destructive.ConfigurationChange`), compared by keyed HMAC fingerprints, never
  values. A write of unknown outcome must be reconciled first (`ErrWriteUncertain`).

### PSPs, custodians and card vaults

- `CreatePSP` probes credentials with the provider before storing anything
  (`probePaymentProviderCredentials`, bounded by `providerCredentialProbeTimeout`).
  `UpdatePSP` changes settings, rotates or archives against `expected_revision`;
  archiving a rail's last active PSP is `409 psp_last_active` unless `allow_last`.
- `settings.card_vault` is a card PSP's key or `platform`; `charge_psps` lists at most
  `MaxChargePSPs` (20). `CardVaultRefusal` refuses a charge PSP that cannot charge the
  vault's cards (`charge_psp_incompatible`), unknown keys and an unusable platform
  vault (`platform_vault_unavailable` and its siblings). Custody:
  [payment methods](payment-methods.md).

### Webhook routing

Callbacks land on `POST /v1/webhooks/{rail}/{account_id}`; for NMI and CCBill the
payload's account must equal the path's. `Service.ResolvePSPsByAccount` finds every
PSP declaring the account, across merchants, and `routeSignedWebhook` checks each
one's secret (CCBill signs nothing; its source-IP check stands in). Exactly one must
verify: of a live and a non-live PSP of one account, Stripe keeps the one matching
the event's `livemode`; any other tie, or none, is refused. The winner's merchant
connection is pinned (`pinWebhookMerchantConn`); configuration that cannot load
answers `503`, so the provider retries.

### Archive and restore

- **Export** (`merchantarchive.Export`). `fenceExport` first makes the source
  `readonly`. One `REPEATABLE READ, READ ONLY` transaction checks the schema
  (`checkSchema`), refuses unresolved work (`preflight`), checks the ledger
  (`CheckBillingRestoreLedger`) and writes and counts each `contract.Profiles` table.
  The footer holds the row count and SHA-256 (`internal/archivewire`). A failed export
  puts back the posture it found.
- **Restore** (`merchantarchive.Restore`). The header's merchant and tenant must be the
  target's. In one merchant transaction: `destructiveArchiveBarrier`, a
  `billing_restore` receipt (`BeginBillingRestore`), the rows, `validateReferences`,
  due work rebuilt for this merchant (`RebuildBalanceWatches`, `RebuildDelinquencyDue`,
  `RebuildUsageDue`), live operations queued for their due time (`queueWakes`), the
  merchant left `readonly` (`landReadonly`), the receipt finished. The same archive
  again is a no-op (`AlreadyImported`); another is `billing_archive_not_empty`.
- **Not moved**: configuration and credentials, `customer_contacts`,
  `provisioning_tokens`, the directory (`internal/merchantarchive/checks.go`). The
  destination identity is prepared first (`ProvisionForRestore`, `RegisterForRestore`,
  `openrails billing prepare-target`); `openrails merchant arm` restores writes.

## Interfaces

- Go: `Client.GetMerchantConfiguration`, `UpdateMerchantConfiguration`, the alert
  webhook, PSP and custodian methods, `ExportBillingArchive`, `ImportBillingArchive`;
  `openrails.MerchantDeclaration`, `openrails.PSPConfig`, `billing.MerchantBinding`.
- HTTP (`RouteGroups.MerchantConfig`): `/v1/merchant/configuration`,
  `/v1/merchant/alert-webhooks`, `/v1/merchant/psps`, `/v1/merchant/custodians`,
  `/v1/merchant/billing-archive`. No route creates or deletes a merchant.
- CLI: `openrails merchants`, `merchant arm|hold|purge`, `book`, `billing
  export|import|prepare-target`, `get-merchant-config`, `apply-merchant-config`,
  `push-merchant-config`, `dump-merchant-config`.
- Configuration: `vault.kv_mount`, `vault.scope_prefix`, `card_vault.kv_mount`, `naming`.

## Guarantees

### 1. Exactly-once provider calls
Configuration and archives change no provider state: credential probes are reads, and
the endpoint registration a PSP change triggers is [provider](payment-providers.md) due
work. Export fences the source and refuses an unresolved operation; restore lands
`readonly` and queues retained operations instead of running them.

Checks:
- `internal/merchantarchive` imports no `internal/integrations` package; `Restore`
  only inserts rows and queues wakes (`queueWakes`).
- `fenceExport` commits before the snapshot opens; its `undo` runs only on failure.
- OR-J31-S03 (no charge before arming) and OR-J31-S04 (unresolved operation refuses).

### 2. Agreement with providers
A PSP's identity is declared, then held: reconciliation refuses a change, an
undeclared account that still holds billing stays named
(`consistency.merchant_config.undeclared_account`), a callback is attributed only by a
verified signature, and archives keep the identities restored references name.

Checks:
- `undeclaredBilling` (`internal/merchants/sync.go`) keeps accounts that hold billing
  but no document declares.
- `routeSignedWebhook` dispatches nothing unless exactly one candidate verifies; a
  refusal only counts in webhook health (`WebhookHealth.Rejected`).
- OR-J06-S02 and OR-J06-S08: wrong-environment, unarmed or stale-posture credentials
  make no write.

### 3. Exact money
Money settings are `int64` micros, decimal strings on the wire
(`billing.UsageBillingCurrency`), with validated currencies. The archive moves every
SQL scalar as a string and checks ledger totals at export and at commit.

Checks:
- `internal/modules/merchantconfig/usage_billing.go` refuses negative amounts and a
  minimum invoice above the credit limit.
- OR-J31-S01 (totals kept) and OR-J31-S02 (`restore-ledger-is-validated-at-commit`).

### 4. Access control and tenant isolation
`MerchantConfig` routes need `merchant:config:read` or `merchant:config:manage` (or the
`root:` twins) at the credential's merchant; archives need manage. Credentials and
alert webhook URLs are write-only. Vault paths come from the merchant id, webhooks act
for the verifying PSP's merchant, and restore refuses another merchant's archive.

Checks:
- `merchantRoutes` and `pspsRoutes` declare `Group: MerchantConfig`; both
  `/v1/merchant/billing-archive` routes are `LevelUpdate`.
- `VaultSource.MerchantPath` is the only builder of a merchant's Vault path.
- `Restore` compares `header.MerchantID` and `header.TenantID` with the target.
- OR-J06-S04 (secrets redacted), OR-J06-S06 (custody ownership), OR-J31-S02.

### 5. Contracts match the code
The client methods are in `api/go.txt`. Declarations and documents parse strictly and
name retired keys (`retiredDeclarationKeys`). Import reads only `archivewire.Version`;
version 2 is `ErrPriceOwnershipFormat`. The CLI calls the same `ExportBillingArchive`
and `ImportBillingArchive` in process and over HTTP.

Checks:
- `go run ./scripts/contracts` covers these methods and routes.
- `archivewire` refuses a header whose `Version` is not `Version`.
- OR-J31-S08 and OR-J31-S11: archives written before a schema cleanup still restore.

### 6. Data lifecycle
The directory, alias, posture, book, PSP and custodian tables are `State`;
`maintenance_runs` (inventories, purges, restore receipts) is `Permanent`. Restore
creates partitions before its transaction (`retention.EnsureRetainedPartitions`). The
archive refuses an OpenRails table or column without a reviewed decision.

Checks:
- `internal/retention/tables.go` classifies `merchants`, `merchant_slug_aliases`,
  `merchant_write_posture`, `book_identity`, `psps`, `custodians`.
- `ownedTables` and the exclusion lists in `checks.go` cover every owned table.

### 7. Work scales with activity
Nothing sweeps every merchant: configuration loads on use and reloads on access or
announcement, an unchanged revision skips reconciliation, a purge queues one job, and
a restore rebuilds due work for its own merchant.

Checks:
- `Cache` has no ticker; `refresh` runs from `Get` or `Invalidate`.
- `SyncConfig` returns early when the loaded revisions equal `previous.Revisions()`.
- `CompletePendingGroupReleases` takes 1 to 500 over `merchants_retired_at_id_idx`.

### 8. Abuse protection
The only unauthenticated entry is the webhook route: the `webhook` bucket
([limits](../standalone.md#abuse-limits)), per-rail body caps and the CCBill source check. Staff inputs
are bounded, and outbound URLs cannot reach private peers.

Checks:
- `processPSPWebhook` reads through `readLimitedWebhookBody` and checks
  `ccbillWebhookIPAllowed`.
- Documents are capped at `MaxMerchantConfigurationBytes` (1 MiB), archives at
  `archivewire.MaxBytes` and `MaxRecordBytes` per line.
- OR-J06-S05: merchant URLs and provider redirects cannot reach a private peer.

### 9. Destructive-action safeguards
Configuration edits are admitted destructive actions with an actor. A purge needs its
phrase, row count, inventory, the destructive gate and an actor; retirement refuses
any activity; archives fence provider writes until an operator arms (`set_by`). Gaps:
`SoftDelete`, `Restore` and `Rename` record no actor, and a purge leaves
`customer_contacts` and `provisioning_tokens`.

Checks:
- `Cache.admittedWrite` admits `destructive.ConfigurationChange`, one unit per changed
  target.
- `Service.Purge` checks `PurgeConfirmPhrase`, `ExpectRows`, `AllowDestructive` and
  `CountMatchingPurgeInventories` before deleting.
- `HasDestructiveArchiveBarrier` refuses a restore beside a pending, uncertain or
  24-hour action or an uncleared hold.

## Tests

- **J32**: OR-J32-S01 provision, find, list and rename keep identity; S02 concurrent
  claims and the rename interval; S03 used retirement refused, deletion closes
  authority; S04 a retirement retry keeps a reclaimed name's owner (S01 to S04 on the
  standalone fixture); S05 the CLI purge queues its Vault cleanup.
- **J31** (archives): OR-J31-S01 identities and totals; S02 bad archive or occupied
  target refused atomically; S03 no charge before arming; S04 unresolved work refuses
  export; S06, S09 NMI and Stripe state verbatim; S08, S11 older archives; S10 restored
  Solana pull; S12, S13 baseline adoption; S14 limit windows; S40 notification export.
- **J06** (PSP configuration): OR-J06-S01 to S06 and S08; S07 is in
  [imports](imports.md); S30, S31 and S60 to S65 are in
  [payment providers](payment-providers.md).
- Gaps: nothing covers a refused Vault document (previous version serving, its
  finding), a declaration beside Vault, the cross-replica reload, Vault down at start,
  the `configuration_change` admission or `ErrWriteUncertain`. These packages have no
  tests of their own.
