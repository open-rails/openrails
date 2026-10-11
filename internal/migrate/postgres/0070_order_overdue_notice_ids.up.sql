-- parent: 69 sha256:d84591d9a76c13d0e87cdb64f36c0a6d2b2d3f9306d97f54d97fe0d60e081184
-- Repair: none-needed An overdue notice named its order by a bare id, which no reader decodes; the repair names it ord_<uuid>, as every notice does.
-- An order_overdue notice names its order the way every notice does.
UPDATE billing.notifications
SET data = jsonb_set(data, '{order_id}', to_jsonb('ord_' || (data ->> 'order_id')))
WHERE event_type = 'order_overdue'
  AND data ->> 'order_id' NOT LIKE 'ord\_%';
