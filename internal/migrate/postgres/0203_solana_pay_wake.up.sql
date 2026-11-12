-- The Solana Pay poller sleeps until the earliest next_poll_at; a reference added or
-- rescheduled wakes it (#1319). Row-level, so a claim that matched nothing notifies no one;
-- a transaction's notifications fold into one.
CREATE FUNCTION billing.solana_pay_references_wake() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    PERFORM pg_notify('openrails_solana_pay:' || TG_TABLE_SCHEMA, '');
    RETURN NULL;
END;
$$;

CREATE TRIGGER solana_pay_references_wake AFTER INSERT OR UPDATE OF next_poll_at, status, watch_ends_at
    ON billing.solana_pay_references FOR EACH ROW EXECUTE FUNCTION billing.solana_pay_references_wake();
