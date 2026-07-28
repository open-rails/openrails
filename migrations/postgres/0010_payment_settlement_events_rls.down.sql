DROP POLICY merchant_isolation ON openrails.payment_settlement_events;
ALTER TABLE ONLY openrails.payment_settlement_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE openrails.payment_settlement_events DISABLE ROW LEVEL SECURITY;
