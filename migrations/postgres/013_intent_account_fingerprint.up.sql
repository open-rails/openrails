-- #365: account fingerprint guard on the provider intent ledger. Intents are
-- stamped at enqueue with a fingerprint of the provider ACCOUNT the producer
-- was configured against (NMI: hash of the security key; Stripe: the acct id).
-- The executor/verifier park the intent when the current credentials resolve
-- to a DIFFERENT fingerprint — swapping credentials to another provider
-- account must never execute a queue built against the old one (NMI ids are
-- small numerics; on the wrong account they address someone else's objects).
--
-- NULL = enqueued before #365 (or fingerprint unresolvable at enqueue):
-- grandfathered, executes without the check.

ALTER TABLE billing.provider_intents
    ADD COLUMN account_fingerprint text;

COMMENT ON COLUMN billing.provider_intents.account_fingerprint IS
    'Fingerprint of the provider account the intent was enqueued against (#365). NULL = pre-guard or unresolvable: executes ungated. Mismatch with the current credentials parks the intent.';
