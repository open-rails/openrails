# Move a merchant's billing data

`openrails billing export` and `openrails billing import` move a complete supported
merchant billing snapshot between embedded, standalone, and hosted OpenRails
deployments. Both commands use the same `openrails.Client` methods as Go hosts:

```go
err := client.ExportBillingArchive(ctx, archiveWriter)
result, err := client.ImportBillingArchive(ctx, archiveReader)
```

For HTTP, construct the client with `openrails.NewRemote`; in process, use the
client `openrails.New` returned. Bind either client with
`openrails.WithMerchantID(sourceMerchantID)`. Archive operations can take longer
than the SDK's default request deadline: use `openrails.WithTimeout(0)` with a
caller-controlled context, or choose an explicit deadline. The CLI defaults to
caller cancellation and also accepts `--timeout`.

Local export/import and `prepare-target --unbound-merchants` load only the `db`
configuration. They do not start
providers, workers, FX refresh, a secret store, or AuthKit. Hosted
`prepare-target` still needs normal destination AuthKit configuration to verify
live group ownership. Remote export/import require only `--url` and
`--token-file`.

The archive routes are `GET /v1/admin/billing-archive` (export) and `POST` on
the same path (import), mounted and checked with `Permissions.MerchantConfig`.
Archive transfer streams with a 1 GiB bound; ordinary
API requests retain their 1 MiB cap. The client verifies the archive footer and
returns an error for incomplete downloads. The CLI publishes its private output
file only after that verification succeeds.

## Archive format and metadata

Exports use archive format **v1**, the only supported format. Export and import
use the same current row layout; there are no legacy readers or format converters.
Artifacts from discarded draft formats must be exported again from the source.

Application metadata is preserved as stored, including arbitrary keys, nested
JSON values and exact numbers. Export does not sanitize or redact that metadata.
Dedicated credential storage remains excluded; protect archives as copies of the
application's billing data.

## What moves

The archive preserves the merchant UUID, billing record IDs, customer subject
references, monetary amounts, ledger history, catalog identities, subscription
state, and supported provider references. Import restores stored facts in one
database transaction. It does not charge, refund, rebill, create subscriptions at
a provider, deliver notifications, or replay financial commands.

This is a versioned billing snapshot, not a database dump or an identity backup.
It does **not** transfer:

- AuthKit users, groups, memberships, sessions, credentials, or merchant authority.
- Dedicated API-key, provider-secret, webhook-secret and encryption-key storage.
  Application metadata is copied as stored and is not screened for credentials.
- River queues, live execution, or infrastructure state.
- A payment provider's or card custodian's ownership of stored payment methods.

Payment method and subscription references can point to existing external
provider or custodian objects. Copying those references does not move those
objects or grant the destination access to them. Arrange continued ownership and
access separately with each provider/custodian before enabling the destination.
A migration to a different vault is a separate procedure.

The export preflight refuses unsupported or unsettled state instead of silently
omitting it. Resolve the named state on the source and export again; do not edit
the archive or delete financial history to force acceptance.

A successful export leaves the source merchant readonly, and an import lands
the destination readonly: no provider write (charge, renewal, refund, cancel,
checkout) happens for that merchant on either copy until an operator arms one
with `openrails merchant arm --merchant NAME --by NAME`. A refused or failed
export restores the posture it found. Stopping the source writers is still
the procedure; the fence keeps a forgotten worker from billing.

## One authoritative database

Normal replicas share one writable PostgreSQL database. Database claims and
stable provider operation keys coordinate their work. Independently writable
copies of a billing book using the same provider credentials are not replicas;
operating both is an operator error. Migration is an offline handoff, not an
active-active deployment or a lease service shared with OpenRails SaaS.

`--source-stopped` and `--target-stopped` record operator attestations. They do
not stop processes or verify another fleet. `provider_write_mode=readonly`
blocks provider mutations but still permits local writes and webhook ingestion;
it is not a substitute for stopping all writers. Direct SDK callers must enforce
the same stopped-source/stopped-target procedure.

## Prerequisites

1. Use compatible OpenRails builds and apply the destination migration chain
   before preparing the target. An incompatible archive version or schema is
   refused. This operation does not upgrade or rewrite old snapshots.
2. Record the immutable source merchant UUID. The CLI requires that exact UUID
   for export and import; `--merchant id:UUID` is also accepted. A slug is not an
   archive identity, and import does not remap merchant or domain IDs.
3. Prepare destination authentication separately. Keep customer subject UUIDs
   meaningful to the destination application: the archive does not import or
   remap AuthKit users. For an OpenRails control-plane deployment, first create
   the destination AuthKit merchant group and establish its owner.
4. Keep destination request writers and workers stopped. The target billing book
   must be empty and have no stored credentials. Do not apply a catalog, merchant
   configuration, seed credits, or arm providers before import.
5. Use a private directory for archive and bearer files. Archives contain
   sensitive billing/customer data even though credentials are excluded. Protect
   their storage and transfer using your normal encrypted backup procedures.

## Prepare the destination identity

Run against the **destination** database configuration with the normal
application role. The command does not run migrations or workers.

For an OpenRails control plane:

```sh
openrails --config destination.yaml --provider-write-mode readonly \
  billing prepare-target --merchant "$MERCHANT_UUID" --slug shop \
  --authkit-group-id "$DESTINATION_GROUP_UUID" \
  --owner-user-id "$DESTINATION_OWNER_UUID"
```

The destination group must already exist. Preparation checks live ownership and
claims `--slug` as the merchant name. It creates only the merchant identity with
the original merchant UUID and the destination group binding. It cannot adopt an
existing merchant with different identity or authority.

For a host that manages its own authentication and uses an unbound billing
merchant:

```sh
openrails --config destination.yaml --provider-write-mode readonly \
  billing prepare-target --merchant "$MERCHANT_UUID" \
  --unbound-merchants --slug shop
```

Go hosts run the same preparation before binding their client:
`openrails billing prepare-target --unbound-merchants` for an engine without a
control plane, or `--authkit-group-id` and `--owner-user-id` for one with it.
Preparation imports no billing data and no provider credentials. Repeating the same identity preparation is safe; conflicting
identity or group bindings are refused.

For a hosted destination where you do not operate its database, its operator
runs this preparation. Remote billing import
never provisions or rebinds the target implicitly.

## Final export and restore

1. Stop source writes from application requests, scheduled jobs, workers,
   maintenance tools, and webhook ingestion. Drain or resolve pending financial
   work before stopping the last writer. Disable automatic restarts, scheduled
   jobs and deployment reconciliation that would restart the old writer fleet.
   Record the stopped instances and selected source database. Keep incoming webhook deliveries queued
   outside the source during the move so they can be delivered to the selected
   destination after validation. A database snapshot alone does not establish a
   safe cutover boundary.
2. Export while the source remains quiescent. `--source-stopped` is an operator
   attestation that all billing writers are stopped; the exporter cannot observe
   or prove fleet-wide quiescence. For a direct local database export:

   ```sh
   openrails --config source.yaml --provider-write-mode readonly \
     billing export --source-stopped --merchant "$MERCHANT_UUID" --out merchant-billing.jsonl
   ```

   For a remote source, leave the maintenance export endpoint reachable while
   blocking ordinary mutation routes and stopping every worker:

   ```sh
   openrails billing export --source-stopped --merchant "$MERCHANT_UUID" \
     --url https://source.example --token-file source-bearer.txt \
     --out merchant-billing.jsonl
   ```

   `--token-file` holds a bearer credential authorized for the archive operation.
   It must accompany `--url`; no local database is opened in remote mode.
3. Transfer the completed file securely to the destination operator. The export
   writes a mode `0600` temporary file in the destination directory, checks the
   complete response, then publishes it atomically. Existing paths are refused
   unless `--overwrite` is explicit. A failed or truncated export leaves the
   previous file intact. There is no stdout export or stdin import, so archive
   contents do not enter normal CLI output or shell pipelines.
4. Import into the prepared destination with its writers still stopped.
   `--target-stopped` confirms this operational condition; neither a prepared
   merchant nor a successful import enables or disables external workers:

   ```sh
   openrails --config destination.yaml --provider-write-mode readonly \
     billing import --target-stopped --merchant "$MERCHANT_UUID" --in merchant-billing.jsonl
   ```

   Or use its protected remote maintenance endpoint:

   ```sh
   openrails billing import --target-stopped --merchant "$MERCHANT_UUID" \
     --url https://destination.example --token-file destination-bearer.txt \
     --in merchant-billing.jsonl
   ```

   The result reports `merchant`, `digest`, `rows`, and `already_imported`, without
   printing billing records. Record these with the build versions and cutover
   boundary. A malformed, incomplete, oversized, wrong-merchant, or incompatible
   archive is refused atomically. An occupied target is refused.
5. If the connection is lost after submission, retry **the same file** against the
   same target. A committed restore receipt makes that retry a no-op and returns
   `already_imported=true`. Import does not replace an existing book with a newer
   snapshot. If you need a new final export, prepare another empty destination
   through your normal database recovery procedure.
6. Validate customer balances, credit blocks, subscriptions, entitlements,
   catalog, and ledger totals through the ordinary destination client/API. Check
   the retained provider/custodian references. The archive carries no merchant
   configuration (settings, PSPs, credentials): copy the merchant's Vault
   documents or manifest entry to the destination separately, only after the
   billing restore succeeds. Its PSP and custodian identities are restored, so
   the documents must name the same accounts.
7. Switch application traffic and provider webhook destinations, then enable only
   the destination workers and arm the destination merchant
   (`openrails merchant arm --merchant NAME --by NAME` against the destination
   database). Retire the source or leave it fenced; never arm it again.
   **Never let the stale source and restored destination run billing concurrently.**
   Record the destination activation and confirm the source remains stopped.

Before destination activation, a failed import leaves the source as the authority;
keep the destination stopped while retrying the same archive or abandoning that
empty/partially prepared target. If restarting the source changes its book, the
next attempt needs a fresh final export and an empty target. Once the destination
has accepted new billing work, restarting the old source is not rollback: move
the now-authoritative destination book back through the same offline procedure.

Rotating or revoking source provider credentials is optional stronger protection
against an accidental restart, when the provider supports suitably scoped keys.
It is not required for the normal stopped-source procedure and may affect other
merchants when an account shares credentials. Provider-managed subscriptions are
not recreated by import; their references and provider ownership must remain
valid. Idempotency keys protect retries of the same logical operation, not
arbitrary concurrent work from independently writable copies.

A remote server's HTTP limits and any reverse proxy must permit the full archive
request/response. The CLI's `--timeout` cannot override a proxy/server limit. A
nonzero exit requires investigation or an idempotent retry, not starting the
billing workers on the assumption that import probably completed.

## Format and limits

The JSONL v1 stream has a header, ordered table/row records, and a terminal footer
with the total row count and SHA-256 digest of the exact pre-footer bytes,
including line separators. The header identifies the merchant; snapshot
consistency does not prove that source writers have stopped. The default limit is 1 GiB total and 8 MiB per line.
Unknown versions, fields, tables, bad ordering, truncated streams, and digest or
row-count mismatches are rejected. Use the supported exporter/importer rather
than hand-generating or editing snapshots.

The digest detects corruption and binds retry identity; it is not a digital
signature or encryption. Only import archives obtained from a trusted source
through an authenticated transfer.

This procedure is an offline cutover. It does not provide incremental sync,
active-active billing, automatic customer identity migration, credential transfer,
or card-vault portability. Successful restore proves the local billing book was
accepted; provider access, identity integration, webhook routing, and worker
readiness must be qualified before live operation.

## Qualification boundary

The core `TestOfflineBillingHandoff` runs the normal CLI against two independent
PostgreSQL databases. It prepares the destination through `prepare-target`, checks
both stop attestations, rejects an interrupted import without retained billing
rows, and retries the same completed import. It then starts the destination with
separately supplied provider credentials, renews the retained subscription once,
and cancels/refunds through the ordinary embedded or HTTP Client. NMI and Stripe
use local provider fixtures; no live provider is qualified by this test.
`TestBillingRestoreTargetUsesDestinationAuthority` checks the group-bound
operator preparation against real AuthKit, including wrong-owner and conflicting
binding refusals.

These core checks are not a test of the separately deployed OpenRails SaaS
application. Before declaring SaaS-to-self-hosted or the reverse complete, run the
same operator procedure through the actual host's supported dependency graph,
authentication, routing and worker lifecycle. A host still using a discarded
archive layout must first adopt the current supported library contract; a test
against a substitute core HTTP server does not qualify that host.
