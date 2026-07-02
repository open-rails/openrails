-- #682: the rebill-driver mode gets its own column, decoupled from identity.
--
-- Until now the NMI dunning routing INFERRED who drives rebills from
-- rail_method_ref emptiness: a billing id present = doujins-legacy import =
-- OpenRails drives manual rebills (recurring=rebill_subscription); absent =
-- native vault = NMI's own recurring engine bills the subscription. That
-- overload made the identity field load-bearing as a behavior flag and blocked
-- ever capturing billing ids for native vaults (#663 discards the v5 create
-- response's billing[].id deliberately because of exactly this).
--
--   'provider'  = the rail's own recurring engine rebills (never manual-rebill)
--   'openrails' = the OpenRails dunning worker drives manual rebills
ALTER TABLE openrails.payment_methods
    ADD COLUMN rebill_driver text NOT NULL DEFAULT 'provider'
    CONSTRAINT payment_methods_rebill_driver_check CHECK (rebill_driver IN ('provider', 'openrails'));

-- Exact backfill of the previously inferred rule — NMI methods carrying a
-- billing id are the legacy imports whose rebills OpenRails drives. ('mobius'
-- kept alongside 'nmi' for pre-#630 rows, mirroring migration 030.)
UPDATE openrails.payment_methods
   SET rebill_driver = 'openrails'
 WHERE rail IN ('nmi', 'mobius') AND rail_method_ref <> '';

-- #682 disposition note: existing openrails.rail_customers rows with
-- rail='nmi' (per-card vault ids materialized by the pre-#682 #635 rule) are
-- LEFT IN PLACE deliberately — nothing reads NMI rows from rail_customers
-- (the charge/identity paths resolve NMI via payment_methods.rail_customer_ref),
-- and deleting them would erase forensic mapping. New NMI rows are no longer
-- created (rails registry: NMI HasRemoteCustomer=false).
