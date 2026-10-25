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
