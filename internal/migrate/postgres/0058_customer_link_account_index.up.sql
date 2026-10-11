-- parent: 57 sha256:5058fa540277cc7fa82fb0fd97a690042c81aa120cd86edcaa26904bedbd2aec
-- Repair: none-needed A new index.
-- One of the account's links by id, active or ended: unlinking again finishes
-- what an unlink left undone.
CREATE INDEX customer_links_account_idx ON billing.customer_links USING btree (account_id, id);
