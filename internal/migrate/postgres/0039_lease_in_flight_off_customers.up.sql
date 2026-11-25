-- A gate's in-flight report is the lease's own rows: writing them never
-- touches the customer's row, so a sync takes no lock on it, not even the
-- foreign key's key share. A line for a customer with no row counts against
-- nothing; the lease's lapse, close or deletion takes its rows with it.
ALTER TABLE billing.lease_in_flight DROP CONSTRAINT lease_in_flight_customer_id_fkey;
