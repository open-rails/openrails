-- #511 step 2 (entitlements-on-grants): entitlements gain a dedicated grant_id
-- link to their source grant in the #514 ledger. MaterializeGrant (the sole
-- creator) sets grant_id and PRESERVES the entitlement's semantic source_type/
-- source_id, so every existing source-keyed reader keeps working unchanged while
-- DERIVE links grant→effect via grant_id. Nullable during the transition (legacy
-- direct-created rows have none); once creation routes through grants it is set.
ALTER TABLE openrails.entitlements ADD COLUMN grant_id uuid;
ALTER TABLE openrails.entitlements
    ADD CONSTRAINT entitlements_grant_fk FOREIGN KEY (grant_id) REFERENCES openrails.grants(id);
CREATE INDEX idx_entitlements_grant_id ON openrails.entitlements USING btree (grant_id) WHERE (grant_id IS NOT NULL);
