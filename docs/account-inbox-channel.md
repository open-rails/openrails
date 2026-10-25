# Account inbox notification channel

OpenRails can publish selected billing notices directly to an optional account
inbox supplied by the host. The inbox implementation and its authenticated user
interface belong to AuthKit; the shared mutation contract belongs to Helpers.
The retained OpenRails event log remains an independent host integration feed.

The host supplies a transactional notification sink and an explicit mapping from
the merchant's customer identity (issuer and UUID) to an account. Equal UUIDs
from unrelated issuers are not the same recipient. A missing channel disables
only inbox publication, not the underlying billing event.

For a shared database the sink applies notifications in the billing transaction.
A remote sink records its delivery job in that transaction and retries after
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

Implementation and rollout are tracked in AuthKit #470 and OpenRails #1291 in
the central tracker. Preserve retained receipt IDs and read/archive state when
adopting the account inbox, and retire ContentKit's billing-event inbox importer
only after the producer channel and host wiring have been qualified.

## Explicit cutover

1. Stop old billing writers and the ContentKit billing inbox importer. Retain a
   database backup of the source ordering metadata and account inbox. Apply the
   additive Helpers, AuthKit and OpenRails migrations.
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
