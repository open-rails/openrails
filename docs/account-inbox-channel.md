# Account inbox notification channel

OpenRails can publish selected billing notices directly to an optional account
inbox supplied by the host. The inbox implementation and its authenticated user
interface belong to AuthKit, including the small `authkit/notify` contract.
The retained OpenRails event log remains an independent host integration feed.

The host supplies an AuthKit notification publisher and an explicit mapping from
the merchant's customer identity (issuer and UUID) to an account. Equal UUIDs
from unrelated issuers are not the same recipient. A missing channel disables
only inbox publication, not the underlying billing event.

```go
publisher, err := accounts.Notifications("openrails")
if err != nil { return err }
deps.Notifications = authkitadapter.Notifications(publisher, merchantID, accountIssuer)
```

The AuthKit producer declaration must bind source system `openrails`, the exact
merchant ID as its source scope, and tenant-wide account visibility. Multiple
merchants require an explicitly routing channel with verified recipient mappings;
the single-merchant adapter skips unrelated merchants before invoking its sink
and never infers another tenant from a customer UUID. Its merchant binding must
be nonzero; an absent binding is a configuration error, not a wildcard.

The producer interface has only `Publish` and `Withdraw`. Each accepts a bounded
batch of deterministic notification IDs and revisions, with an optional source
transaction. AuthKit binds source, audience and application visibility once;
OpenRails does not configure inbox tables, read receipts or manage a delivery
transport. No Helpers notification package is required.

For a shared database the publisher applies notifications in the billing transaction.
A remote publisher records its delivery job in that transaction and retries after
commit. OpenRails never performs a best-effort network notification after a
financial commit.

Notifications have deterministic source identities and positive, increasing
producer revisions. Retrying an immutable billing fact must not create a newer
notification revision. Recovery withdraws exact known issue identities; it never
lists or searches the destination inbox. Ranges, wildcard prefixes and generic
dependency tracking are outside this contract.

The default selection preserves the current application policy: unexpected
payment failures, required payment-method updates or authentication, stopped
collection, actual access loss, and applied downgrades. Routine renewal receipts
and failures directly resulting from checkout or a subscription-change click do
not create inbox items. A historical event remains a fact even when its related
inbox notification is withdrawn.

Successful payment, accepted card replacement, and restored access withdraw only
the issues their domain outcomes resolve. Closing dunning alone is insufficient:
it also occurs on cancellation or when billing waits for the customer.
Confirmed subscription cancellation or expiration also withdraws its obsolete
payment requests; access loss while collection remains live does not. The
separate access-ended notice and original billing facts keep their own lifecycle.
Order requests are withdrawn when the order completes, is canceled, expires or
is voided. Full payment by a provider, a recorded remittance or funded credit has
the same effect. Partial payment and marking debt uncollectible keep its request.

Preserve retained receipt IDs and read/archive state when
adopting the account inbox, and retire ContentKit's billing-event inbox importer
only after the producer channel and host wiring have been qualified.

## Explicit cutover

1. Stop old billing writers and the ContentKit billing inbox importer. Retain a
   database backup of the source ordering metadata and account inbox. Apply the
   AuthKit-owned inbox and OpenRails source-ordering migrations. The package
   move preserves the six existing inbox migration files and their ledger.
2. Configure the AuthKit producer and issuer-verifying OpenRails notification
   channel. Call the trusted local merchant's `ExportNotificationAdoption` while
   writers remain stopped. Save the returned artifact durably. Repeating the
   export against unchanged source state preserves its revisions.
3. Pass its exact `Adoptions` to AuthKit's administrative `AdoptNotifications`.
   Existing receipts are mapped to deterministic issue identities, preserving
   the chosen receipt ID and read/archive state. Duplicate old issue receipts
   are hidden; currently resolved issues are withdrawn. Facts never consumed by
   the old importer must not become newly visible historical notifications.
   Informational historical receipts keep their existing identities.
4. Verify the adoption receipts, retire the old importer through its coordinated
   migration, and start the new producer and account-inbox readers together.
   Do not run old readers after producer withdrawals begin: old readers do not
   understand hidden withdrawal tombstones.

The export reads only retained producer facts and authoritative billing state;
it never enumerates the destination inbox. Facts already removed by retention
cannot reconstruct a legacy mapping and must be accounted for explicitly in the
operator's cutover inventory rather than guessed from current customer names.

The `Ordering` portion is separate from the financial archive. Preserve it in
ordinary database backups and transfer it when moving a merchant to a new
database. A financial archive restore sets a publication guard. Call
`RestoreNotificationOrdering` with the matching source snapshot before enabling
notification delivery; lower or conflicting revisions are refused. An explicitly
empty snapshot is an administrator's declaration that this producer had no
notification ordering to transfer, never an automatic reset of a live inbox.
