-- #358 phase C: manual rebill attempts live on the provider intent ledger.
--
-- billing.manual_rebill_attempts was the durable effectively-once claim table
-- for processor-side dunning charges (migration 007 resolved its stranded
-- rows; that history stands). The ledger generalizes it: the dunning worker
-- now enqueues intent_type='manual_rebill' rows on billing.provider_intents —
-- same content-addressed identity (subscription + period end + processor +
-- order reference, plus the attempt ordinal), same never-blind-retry
-- semantics, with ambiguity resolved by the intent verifier via the NMI Query
-- API. Hard cut: the table, its models and queries are deleted outright.

DROP TABLE billing.manual_rebill_attempts;
