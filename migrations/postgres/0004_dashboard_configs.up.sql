-- #741 configurable dashboard: one saved widget layout per merchant.
-- layout is the ordered widget array [{id, title, viz, query, grid}] — query is
-- a #733 metrics body validated by the compiler before every persist; grid is
-- the react-grid-layout {x,y,w,h}. No row = the code-side default template.

CREATE TABLE openrails.dashboard_configs (
    merchant_id uuid NOT NULL,
    layout jsonb NOT NULL,
    updated_at timestamp with time zone DEFAULT now() NOT NULL,
    updated_by text
);

COMMENT ON TABLE openrails.dashboard_configs IS '#741 per-merchant dashboard widget layout: [{id, title, viz(stat|line|area|bar|donut|table), query(#733 body), grid{x,y,w,h}}]. Absent row = seeded default template (in code, not DB).';
COMMENT ON COLUMN openrails.dashboard_configs.updated_by IS 'acting principal (user id) of the last PUT; informational.';

ALTER TABLE ONLY openrails.dashboard_configs
    ADD CONSTRAINT dashboard_configs_pkey PRIMARY KEY (merchant_id);

ALTER TABLE ONLY openrails.dashboard_configs
    ADD CONSTRAINT dashboard_configs_merchant_fk FOREIGN KEY (merchant_id) REFERENCES openrails.merchants(id) ON DELETE RESTRICT;

ALTER TABLE openrails.dashboard_configs ENABLE ROW LEVEL SECURITY;

ALTER TABLE ONLY openrails.dashboard_configs FORCE ROW LEVEL SECURITY;

CREATE POLICY merchant_isolation ON openrails.dashboard_configs USING ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid)) WITH CHECK ((merchant_id = (NULLIF(current_setting('app.merchant_id'::text, true), ''::text))::uuid));

GRANT SELECT,INSERT,UPDATE,DELETE ON TABLE openrails.dashboard_configs TO openrails_app;
