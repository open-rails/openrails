-- Repair: none-needed. Parks start empty: converge parks what it cannot land
-- from now on. Dropping the table and the trigger returns converge to retries.
CREATE TABLE billing.parked_provider_subscriptions (
    merchant_id uuid NOT NULL,
    id uuid DEFAULT uuidv7() NOT NULL,
    psp_id uuid NOT NULL,
    rail text NOT NULL,
    subscription_ref text NOT NULL,
    reason text NOT NULL,
    price_ref text,
    customer_ref text,
    import_owned boolean NOT NULL,
    evidence jsonb NOT NULL,
    parked_at timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL,
    released_at timestamptz,
    released_by text,
    CONSTRAINT parked_provider_subscriptions_pkey PRIMARY KEY (merchant_id, id),
    CONSTRAINT parked_provider_subscriptions_psp_id_rail_fkey FOREIGN KEY (merchant_id, psp_id, rail) REFERENCES billing.psps(merchant_id, id, rail) ON DELETE RESTRICT,
    CONSTRAINT parked_provider_subscriptions_reason_check CHECK (reason IN ('price_unmapped', 'customer_unlinked', 'product_held')),
    CONSTRAINT parked_provider_subscriptions_ref_check CHECK (subscription_ref <> '' AND octet_length(subscription_ref) <= 256),
    CONSTRAINT parked_provider_subscriptions_cause_check CHECK ((price_ref IS NULL OR price_ref <> '') AND (customer_ref IS NULL OR customer_ref <> '')
        AND (reason <> 'price_unmapped' OR price_ref IS NOT NULL)),
    CONSTRAINT parked_provider_subscriptions_evidence_check CHECK (jsonb_typeof(evidence) = 'object'),
    CONSTRAINT parked_provider_subscriptions_released_check CHECK ((released_at IS NULL) = (released_by IS NULL)
        AND (released_by IS NULL OR released_by IN ('landed', 'converged', 'gone')))
);
CREATE UNIQUE INDEX parked_provider_subscriptions_open_key ON billing.parked_provider_subscriptions (merchant_id, psp_id, subscription_ref) WHERE released_at IS NULL;
CREATE INDEX parked_provider_subscriptions_price_ref_idx ON billing.parked_provider_subscriptions (merchant_id, psp_id, reason, price_ref) WHERE released_at IS NULL;
CREATE INDEX parked_provider_subscriptions_customer_ref_idx ON billing.parked_provider_subscriptions (merchant_id, psp_id, reason, customer_ref) WHERE released_at IS NULL;
CREATE INDEX parked_provider_subscriptions_parked_at_id_idx ON billing.parked_provider_subscriptions (merchant_id, parked_at DESC, id DESC);
CREATE INDEX parked_provider_subscriptions_released_at_idx ON billing.parked_provider_subscriptions (merchant_id, released_at) WHERE released_at IS NOT NULL;
COMMENT ON TABLE billing.parked_provider_subscriptions IS 'Live provider subscriptions converge cannot land: its price is unmapped, its customer is linked to no one, or the customer already holds the product. One open park per (PSP, subscription); it mirrors the latest converge. Fixing the cause wakes converge; the park closes when the subscription lands by any path (released_by landed), when converge no longer needs it (converged) or when the provider ended it unlanded (gone). Retention: released parks are deleted 90 days after released_at.';
COMMENT ON COLUMN billing.parked_provider_subscriptions.price_ref IS 'The provider price or plan the subscription bills; set on every park whose provider names one.';
COMMENT ON COLUMN billing.parked_provider_subscriptions.customer_ref IS 'The provider customer the subscription bills: a Stripe customer, an NMI vault.';
COMMENT ON COLUMN billing.parked_provider_subscriptions.import_owned IS 'The PSP''s import owns the subscription (no pull yet, a pull pending, or staged unlanded): the park raises no finding, and the import lands it on Apply.';

-- Landing by any path (converge, adoption, an import's Apply) closes the open
-- park and resolves the finding it raised. Idempotent: only an open park closes.
CREATE FUNCTION billing.close_landed_park() RETURNS trigger
    LANGUAGE plpgsql
    AS $$
BEGIN
    IF NEW.rail_subscription_id IS NULL OR NEW.deleted_at IS NOT NULL THEN
        RETURN NULL;
    END IF;
    WITH closed AS (
        UPDATE billing.parked_provider_subscriptions p
        SET released_at = now(), released_by = 'landed'
        WHERE p.merchant_id = NEW.merchant_id AND p.psp_id = NEW.psp_id AND p.subscription_ref = NEW.rail_subscription_id
          AND p.released_at IS NULL
        RETURNING p.subscription_ref
    )
    UPDATE billing.reconciliation_findings f
    SET status = 'fixed', resolution = 'auto_vanished', resolved_at = now(), notified_at = NULL, notified_severity = NULL, updated_at = now()
    FROM closed
    WHERE f.merchant_id = NEW.merchant_id AND f.psp_id = NEW.psp_id AND f.finding_type = 'life.subscription.unattributed'
      AND f.subject_key = closed.subscription_ref AND f.status IN ('reconcile_required', 'requires_review');
    RETURN NULL;
END;
$$;
CREATE TRIGGER subscriptions_close_landed_park AFTER INSERT ON billing.subscriptions
    FOR EACH ROW EXECUTE FUNCTION billing.close_landed_park();

-- catalog.price.unmapped (a provider price) and life.subscription.unattributed
-- (a provider subscription) name the PSP they were observed on. At the rebase
-- onto #1295 Phase 2 the drift branch goes and this replaces
-- reconciliation_findings_psp_check.
ALTER TABLE billing.reconciliation_findings
    DROP CONSTRAINT reconciliation_findings_catalog_check,
    ADD CONSTRAINT reconciliation_findings_scope_check CHECK (
        (finding_type IN ('catalog.orphan_in_stripe','catalog.missing_in_stripe','catalog.orphan_in_nmi','catalog.missing_in_nmi','catalog.missing_in_solana','catalog.field_drift')
         AND rail IN ('stripe','nmi','solana') AND psp_id IS NOT NULL AND openrails_resource_type IN ('product','price')
         AND (finding_type = 'catalog.field_drift' OR finding_type LIKE 'catalog.%_in_' || rail)
         AND first_seen_run IS NULL AND last_seen_run IS NULL
         AND subject_key = jsonb_build_array(psp_id::text,openrails_resource_type,coalesce(openrails_resource_id,''),coalesce(external_resource_id,''),coalesce(field,''))::text)
        OR (finding_type IN ('catalog.price.unmapped', 'life.subscription.unattributed')
         AND rail IS NOT NULL AND psp_id IS NOT NULL AND openrails_resource_type IS NULL
         AND openrails_resource_id IS NULL AND external_resource_id IS NULL AND field IS NULL
         AND openrails_value IS NULL AND external_value IS NULL)
        OR (finding_type NOT LIKE 'catalog.%' AND finding_type <> 'life.subscription.unattributed' AND openrails_resource_type IS NULL
         AND openrails_resource_id IS NULL AND external_resource_id IS NULL AND field IS NULL
         AND openrails_value IS NULL AND external_value IS NULL
         AND ((rail IS NULL AND psp_id IS NULL) OR (finding_type LIKE 'pull.%' AND rail IS NOT NULL AND psp_id IS NOT NULL)))
    );
COMMENT ON COLUMN billing.reconciliation_findings.psp_id IS 'Catalog, pull.* and life.subscription.unattributed findings: the PSP whose read raised the finding. Part of the identity; absence can be proven only by a complete read of this PSP.';
