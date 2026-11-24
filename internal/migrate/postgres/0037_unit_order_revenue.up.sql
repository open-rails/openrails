-- An order line priced in a merchant's own unit is paid from the customer's
-- lots, each bought in its own currency: the line has one revenue schedule per
-- currency its lots were paid in.
ALTER TABLE billing.revenue_schedules DROP CONSTRAINT revenue_schedules_order_line_id_key;
ALTER TABLE billing.revenue_schedules ADD CONSTRAINT revenue_schedules_order_line_id_key UNIQUE (merchant_id, order_line_id, currency);
COMMENT ON TABLE billing.revenue_schedules IS 'One row per paid order line, and per currency its money was paid in, whose revenue the general ledger defers: what was funded (credited to deferred_revenue), what has been recognised, and over what service. A line priced in a merchant''s own unit is funded by its lots'' money, in each lot''s paid currency. A ratable schedule recognises by elapsed service at every midnight on the 1st in the merchant''s accounting timezone and at its end; a point-in-time one at once. Retention: permanent, never pruned.';
