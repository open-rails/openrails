-- Repair: none-needed A new index.
-- A wallet's Solana Pay request names only its order: the route finds the
-- order's merchant by id, before any merchant is known. Not unique: a restore
-- into another merchant of the same database repeats order ids.
CREATE INDEX orders_id_idx ON billing.orders USING btree (id);
