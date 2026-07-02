-- #666: delete the never-consulted abuse blocklist — IsBlocked had zero callers (blocklist.go's own
-- doc said the deny wiring was intentionally not built), so no row could ever gate a payment.
-- Also removed from the 001 baseline; this drop covers DBs migrated before the removal.
DROP TABLE IF EXISTS openrails.payment_blocklist;
