-- This server owns its reading position; AuthKit never registers a consumer.
CREATE TABLE billing.network_event_cursors (
    issuer text NOT NULL,
    tenant_id uuid NOT NULL,
    cursor text NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (issuer, tenant_id)
);
COMMENT ON TABLE billing.network_event_cursors IS 'State: optional standalone customer-network account event reading position, scoped to its AuthKit issuer and account tenant; retained until that server authority is retired.';
