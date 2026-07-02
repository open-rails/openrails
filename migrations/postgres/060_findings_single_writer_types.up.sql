-- #665 single-writer-per-invariant: no plane emits another plane's finding
-- types. Two legacy pull-engine emissions moved/renamed; align existing
-- findings-ledger rows so stable identities keep upserting in place.
--
-- 1. PS-8 remote-duplicate detection needs the provider snapshot (local
--    duplicates are schema-blocked), so it is a PULL-plane finding:
--    consistency.duplicate.subscription -> pull.subscription.duplicate.
--    (chk_reconciliation_findings_type is pattern-based; no constraint change.)
UPDATE openrails.reconciliation_findings
   SET finding_type = 'pull.subscription.duplicate'
 WHERE finding_type = 'consistency.duplicate.subscription';

-- 2. PS-9 (derive.grant_effect.mismatch) moved into the Convergence Engine's
--    DERIVE pass, which keys subjects as 'subscription:<uuid>' (legacy engine
--    used the bare uuid). Rekey so re-emissions update rather than duplicate.
UPDATE openrails.reconciliation_findings
   SET subject_key = 'subscription:' || subject_key
 WHERE finding_type = 'derive.grant_effect.mismatch'
   AND subject_key NOT LIKE 'subscription:%';

-- (life.provider_intent.stuck moved to the LIFE pass unchanged — same type,
-- same bare-intent-id subject keys — so its rows need no rewrite.)
