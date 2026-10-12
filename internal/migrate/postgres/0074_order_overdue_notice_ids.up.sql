-- parent: 73 sha256:cf82387a7355477b0ee7fefa074ddd0a5b15f16c6fbf19fc8ddc887612806342
-- Repair: none-needed An overdue notice named its order by a bare id, which no reader decodes; the repair names it ord_<uuid>, as every notice does.
-- An order_overdue notice names its order the way every notice does.
UPDATE billing.notifications
SET data = jsonb_set(data, '{order_id}', to_jsonb('ord_' || (data ->> 'order_id')))
WHERE event_type = 'order_overdue'
  AND data ->> 'order_id' NOT LIKE 'ord\_%';
