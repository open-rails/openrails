-- Repair: none-needed New indexes for bounded operational inspection.
CREATE INDEX admission_operations_admitted_at_request_id_idx
    ON billing.admission_operations (merchant_id, admitted_at DESC, request_id COLLATE "C" DESC);
CREATE INDEX admission_operations_customer_admitted_at_request_id_idx
    ON billing.admission_operations (merchant_id, customer_id, admitted_at DESC, request_id COLLATE "C" DESC);
CREATE INDEX usage_events_source_occurred_at_id_idx
    ON billing.usage_events (merchant_id, source, source_id, occurred_at DESC, id DESC);
