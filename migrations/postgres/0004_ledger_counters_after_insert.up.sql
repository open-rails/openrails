-- A BEFORE INSERT trigger also runs for ON CONFLICT DO NOTHING attempts.
-- Only a retained transfer may change counters or consume account capacity.
-- The existing function retains its account locks, currency/floor checks and
-- transactional updates; a rejected transfer rolls back all of these effects.
SET statement_timeout = '300s';
SET lock_timeout = '10s';

DROP TRIGGER trg_ledger_transfers_apply_counters ON openrails.ledger_transfers;
CREATE TRIGGER trg_ledger_transfers_apply_counters
    AFTER INSERT ON openrails.ledger_transfers
    FOR EACH ROW EXECUTE FUNCTION openrails.ledger_transfers_apply_counters();
