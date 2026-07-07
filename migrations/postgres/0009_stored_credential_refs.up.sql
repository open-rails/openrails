-- #297 Phase A: card-network stored-credential (CIT/MIT) replay references.
--
-- The networks track a SEPARATE credential-on-file sequence per agreement type
-- (recurring vs unscheduled) and an initial reference from one sequence is not
-- valid on the other (NMI: "This transaction ID cannot be used for
-- 'unscheduled' ... credential-on-file transactions"), so an instrument
-- carries one replay reference per agreement type.
--
-- The value is RAIL-SCOPED: whatever the rail needs to replay the credential
-- on a merchant-initiated charge. For NMI it is the gateway transactionid of
-- the sequence's initial customer-initiated transaction (sent back as
-- initial_transaction_id; NMI maps it to the network transaction identifier
-- internally). A vaulted-card rail stores that rail's reference instead.
--
-- '' = no reference captured yet (legacy/pre-#297 instrument, or no charge on
-- that agreement type yet). Merchant-initiated charges on such instruments run
-- reference-less with a warning and the next successful charge back-fills the
-- reference (write-once: captures never overwrite a non-empty value).
ALTER TABLE openrails.payment_methods
    ADD COLUMN stored_credential_recurring_ref text DEFAULT ''::text NOT NULL,
    ADD COLUMN stored_credential_unscheduled_ref text DEFAULT ''::text NOT NULL;

COMMENT ON COLUMN openrails.payment_methods.stored_credential_recurring_ref IS 'Rail-scoped stored-credential replay reference for the RECURRING card-network agreement (NMI: gateway transactionid of the initial recurring CIT, replayed as initial_transaction_id on recurring MITs). Empty = not captured yet.';

COMMENT ON COLUMN openrails.payment_methods.stored_credential_unscheduled_ref IS 'Rail-scoped stored-credential replay reference for the UNSCHEDULED card-network agreement (NMI: gateway transactionid of the initial unscheduled CIT, replayed as initial_transaction_id on unscheduled MITs). Empty = not captured yet.';
