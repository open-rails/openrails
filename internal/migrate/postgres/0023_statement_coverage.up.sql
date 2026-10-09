-- parent: 22 sha256:09a81dbe64902874020e8d5354747e32cd927f4903bc4a5f19ba1f5bd647d718
-- A period statement is paid once the invoices billing its charges are. These
-- find a period's billed charges, and the statements still waiting on other
-- invoices, so a payment costs only the statements it reaches.
CREATE INDEX invoice_items_billed_invoice_at_idx ON billing.invoice_items USING btree (merchant_id, customer_id, currency, invoice_at) WHERE (invoice_id IS NOT NULL);
CREATE INDEX invoices_awaiting_period_starts_at_idx ON billing.invoices USING btree (merchant_id, customer_id, currency, period_starts_at) WHERE ((status = ANY (ARRAY['open'::text, 'past_due'::text, 'uncollectible'::text])) AND ((amount_paid + amount_due) < total_amount));
