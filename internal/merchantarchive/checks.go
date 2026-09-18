package merchantarchive

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/merchantarchive/format"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Every billing-schema table has an explicit decision. These are deployment/operations
// data or unsupported opaque evidence, not additional archive row profiles.
var excludedTables = map[string]string{
	"merchants":                 "destination identity and host authority are explicitly provisioned",
	"worker_state":              "deployment-wide worker health and fair sweep cursors",
	"destructive_action_switch": "deployment-wide safety switch",
	"webhook_health":            "telemetry", "webhook_health_daily": "telemetry", "admission_denials_hourly": "telemetry",
	"dashboard_configs": "presentation",
	"merchant_deks":     "encryption key material", "merchant_secrets": "credentials are re-entered at destination",
	"merchant_destructive_policy": "deployment safety policy", "merchant_webhooks": "destinations and signing-key versions are reconfigured",
	"notifications": "inbox and notification delivery", "rail_mutation_logs": "operator evidence; raw bodies excluded",
	"reconciliation_findings":         "operator observations",
	"account_updater_batches":         "unsupported provider job evidence; any rows refused",
	"destructive_run_before_images":   "unsupported byte-exact undo history; any rows refused",
	"operation_authorizations":        "unsupported opaque byte-exact operation evidence; any rows refused",
	"provider_billing_qualifications": "unsupported opaque byte-exact provider evidence; any rows refused",
	"provider_billing_observations":   "unsupported opaque byte-exact provider bodies; any rows refused",
}

// Explicit exclusions cover only these reviewed columns. A later column is
// unclassified even on a diagnostic table and must receive a new decision.
var excludedColumns = map[string]string{
	"destructive_action_switch":       "id singleton enabled updated_by reason updated_at",
	"worker_state":                    "worker_kind cursor_merchant_id cursor_version registered_at expected_period_seconds last_success_at last_error_at last_error consecutive_failures last_alerted_at updated_at",
	"merchants":                       "id slug status permission_group_id created_at updated_at deleted_at display_name api_host retired_at group_release_completed_at",
	"webhook_health":                  "merchant_id rail last_accepted_at last_pull_at created_at updated_at",
	"webhook_health_daily":            "merchant_id rail day_at rejected drift",
	"admission_denials_hourly":        "merchant_id customer_id denial_reason hour_at denials updated_at",
	"dashboard_configs":               "merchant_id layout updated_at updated_by",
	"merchant_deks":                   "merchant_id wrapped_dek created_at updated_at",
	"merchant_secrets":                "merchant_id name value version created_at updated_at",
	"merchant_destructive_policy":     "id merchant_id destructive_actions_enabled enforce_armed_at first_pull_completed_at updated_by reason updated_at",
	"merchant_webhooks":               "id merchant_id name destination_host secret_version format enabled created_at updated_at",
	"notifications":                   "id event_type data recipient_kind read_at severity title body link created_at merchant_id customer_id emailed_at",
	"rail_mutation_logs":              "id merchant_id rail psp_id rail_intent_id intent_type idempotency_key attempt phase reason evidence created_at custodian_id",
	"reconciliation_findings":         "id merchant_id finding_type rail psp_id openrails_resource_type openrails_resource_id external_resource_id field openrails_value external_value subject_key severity status recommended_action first_seen_run last_seen_run last_seen_at resolved_at resolution operator_notes created_at updated_at evidence resolved_by notified_at notified_severity seen_run_class",
	"account_updater_batches":         "id merchant_id custodian_id job_ref status instruments result_counts failure_reason submitted_at last_polled_at completed_at created_at updated_at",
	"destructive_run_before_images":   "id merchant_id destructive_run_id table_name row_id before captured_at restored_at destructive_run_class",
	"operation_authorizations":        "operation_id merchant_id payer_id record_owner ledger_account_id authorized_usd_micros claim_reference authorization_body_bytes authorization_body_digest state terminal_reference created_at released_at settled_at settlement_provider_cost_usd_micros settlement_rated_usd_micros settlement_body_bytes settlement_body_digest",
	"provider_billing_qualifications": "merchant_id operation_id provider provider_resource_id provider_lifetime_start provider_lifetime_end provider_absent_at provider_absence_reference billing_stop_reference windows_closed_at windows_closed_reference lifecycle_evidence_bytes lifecycle_evidence_digest quiescence_seconds state reason baseline_observation_id qualified_observation_id qualified_provider_cost_usd_micros qualified_at created_at updated_at",
	"provider_billing_observations":   "merchant_id operation_id observation_id normalized_query query_start query_end raw_body_available raw_body_bytes raw_body_digest normalized_records_bytes normalized_records_digest provider_cost_usd_micros has_negative_record refusal_kind covers_lifetime qualification_reason observed_at",
}

// Omitted columns are either reconstructed by PostgreSQL, deployment
// credential watermarks, or explicitly excluded raw/operational
// data. A new unclassified column fails closed even when currently empty.
var omittedColumns = map[string]string{
	"custodians":        "credential_versions",
	"subscriptions":     "destructive_run_class",
	"payments":          "metadata discount_metadata destructive_run_class",
	"payment_methods":   "metadata",
	"checkout_sessions": "destructive_run_class",
	"entitlements":      "period destructive_run_class",
	"usage_events":      "metadata",
	"invoice_items":     "metadata",
	"maintenance_runs":  "run_class coverage affected note summary error inventory_manifest inventory_total_rows",
	"rail_intents":      "destructive_run_class",
}

func checkSchema(ctx context.Context, tx pgx.Tx) error {
	known := map[string]bool{}
	for _, p := range format.Profiles {
		known[p.Name] = true
	}
	for t := range excludedTables {
		known[t] = true
	}
	rows, err := tx.Query(ctx, `SELECT c.relname,c.relrowsecurity AND c.relforcerowsecurity,a.attname IS NOT NULL FROM pg_class c LEFT JOIN pg_attribute a ON a.attrelid=c.oid AND a.attname='merchant_id' AND NOT a.attisdropped
		WHERE c.relnamespace=(SELECT relnamespace FROM pg_class WHERE oid='openrails.merchants'::regclass) AND c.relkind IN ('r','p') ORDER BY c.relname`)
	if err != nil {
		return err
	}
	found := map[string]bool{}
	for rows.Next() {
		var name string
		var secured, scoped bool
		if err := rows.Scan(&name, &secured, &scoped); err != nil {
			rows.Close()
			return err
		}
		if scoped && !secured {
			rows.Close()
			return &Error{Code: "unsupported_state", Table: name, Err: fmt.Errorf("merchant RLS is not enforced")}
		}
		if !known[name] {
			rows.Close()
			return &Error{Code: "unsupported_state", Table: name, Err: fmt.Errorf("unclassified merchant table")}
		}
		found[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range format.Profiles {
		if !found[p.Name] {
			return &Error{Code: "unsupported_state", Table: p.Name, Err: fmt.Errorf("required archive table absent")}
		}
	}
	return checkColumns(ctx, tx)
}

func checkColumns(ctx context.Context, tx pgx.Tx) error {
	profiles := append([]format.Profile(nil), format.Profiles...)
	for table, names := range excludedColumns {
		p := format.Profile{Name: table}
		for _, name := range strings.Fields(names) {
			p.Columns = append(p.Columns, format.Column{Name: name})
		}
		profiles = append(profiles, p)
	}
	for _, p := range profiles {
		wanted := map[string]format.Column{}
		for _, c := range p.Columns {
			wanted[c.Name] = c
		}
		omitted := map[string]bool{}
		for _, c := range strings.Fields(omittedColumns[p.Name]) {
			omitted[c] = true
		}
		rows, err := tx.Query(ctx, `SELECT a.attname,t.typname,a.atttypmod FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_type t ON t.oid=a.atttypid
			WHERE c.relname=$1 AND c.relnamespace=(SELECT relnamespace FROM pg_class WHERE oid='openrails.merchants'::regclass) AND a.attnum>0 AND NOT a.attisdropped`, p.Name)
		if err != nil {
			return err
		}
		for rows.Next() {
			var name, typ string
			var mod int32
			if err := rows.Scan(&name, &typ, &mod); err != nil {
				rows.Close()
				return err
			}
			if c, ok := wanted[name]; ok {
				expected := map[string]string{"uuid": "uuid", "text": "text", "text[]": "_text", "bigint": "int8", "integer": "int4", "boolean": "bool", "jsonb": "jsonb", "timestamp with time zone": "timestamptz", "timestamptz": "timestamptz", "openrails.payment_status": "payment_status", "openrails.subscription_status": "subscription_status"}[c.Type]
				if strings.HasPrefix(c.Type, "character varying(") {
					expected = "varchar"
					n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(c.Type, "character varying("), ")"), 10, 32)
					if err != nil || int64(mod) != n+4 {
						expected = "invalid"
					}
				}
				if c.Type != "" && typ != expected {
					rows.Close()
					return &Error{Code: "unsupported_state", Table: p.Name, Err: fmt.Errorf("column type changed: %s", name)}
				}
				delete(wanted, name)
			} else if !omitted[name] {
				rows.Close()
				return &Error{Code: "unsupported_state", Table: p.Name, Err: fmt.Errorf("unclassified column: %s", name)}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(wanted) > 0 {
			return &Error{Code: "unsupported_state", Table: p.Name, Err: fmt.Errorf("required archive column missing")}
		}
	}
	return nil
}

func refuseRows(ctx context.Context, tx pgx.Tx, id merchant.ID, table, predicate string) error {
	var count int64
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM openrails."+table+" WHERE merchant_id=$1 AND ("+predicate+")", id.UUID()).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return &Error{Code: "unsupported_state", Table: table, Count: count}
	}
	return nil
}

func preflight(ctx context.Context, tx pgx.Tx, id merchant.ID) error {
	checks := []struct{ table, predicate string }{
		{"operation_authorizations", "true"}, {"provider_billing_qualifications", "true"}, {"provider_billing_observations", "true"},
		{"destructive_run_before_images", "true"}, {"account_updater_batches", "true"},
		{"checkout_sessions", "status NOT IN ('succeeded','failed','expired','canceled')"},
		{"rail_intents", "status IN ('pending','in_flight','unknown_needs_verify','failed_retryable')"},
		{"payments", "status='pending'"}, {"invoice_payments", "status='attempted'"},
		{"admission_operations", "state='open'"},
		{"host_outbox", "delivered_at IS NULL"}, {"webhook_events", "completed_at IS NULL"},
		{"maintenance_runs", "status='running' OR kind NOT IN ('billing_restore','reconciliation','prune','converge_enforce','merchant_purge')"},
		{"maintenance_runs", "kind IN ('prune','converge_enforce','merchant_purge') AND (coverage IS NOT NULL OR affected IS NOT NULL OR summary IS NOT NULL OR inventory_manifest IS NOT NULL OR inventory_total_rows IS NOT NULL)"},
		{"psps", "jsonb_typeof(evidence)<>'object' OR evidence - ARRAY['settings','signer','public_config','source','credential_versions','credentials_validated','api_key'] <> '{}'::jsonb"},
	}
	for _, c := range checks {
		if err := refuseRows(ctx, tx, id, c.table, c.predicate); err != nil {
			return err
		}
	}
	// These blobs have no v1 portable contract. Even benign-looking nonempty
	// metadata may carry replay terms or financial facts; never erase it.
	for table, columns := range map[string][]string{
		"payments": {"metadata", "discount_metadata"}, "payment_methods": {"metadata"},
		"usage_events": {"metadata"}, "invoice_items": {"metadata"},
	} {
		for _, column := range columns {
			if err := refuseRows(ctx, tx, id, table, "coalesce("+column+",'null'::jsonb) NOT IN ('null'::jsonb,'{}'::jsonb)"); err != nil {
				return err
			}
		}
	}
	return validateReferences(ctx, tx, id)
}

func validateReferences(ctx context.Context, tx pgx.Tx, id merchant.ID) error {
	// The ledger intentionally has no control-plane FKs. Archive restoration
	// still refuses missing/cross-payer retained business references.
	checks := []struct{ table, predicate string }{
		{"ledger_transfers", `(customer_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM openrails.customers c WHERE c.merchant_id=$1 AND c.id=ledger_transfers.customer_id))
		 OR (grant_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM openrails.grants g WHERE g.merchant_id=$1 AND g.id=ledger_transfers.grant_id AND g.customer_id=ledger_transfers.customer_id))
		 OR (invoice_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM openrails.invoices i WHERE i.merchant_id=$1 AND i.id=ledger_transfers.invoice_id AND i.customer_id=ledger_transfers.customer_id AND i.currency=ledger_transfers.currency))`},
		// Restore preserves historical denormalized tiers, but a live subscription
		// must still agree with its product, as required by the ordinary tier
		// derivation and product-update guards. Include remaining paid access.
		{"subscriptions", `deleted_at IS NULL
		 AND (status IN ('active','pending','past_due','unknown') OR COALESCE(current_period_ends_at,ended_at)>now())
		 AND EXISTS(SELECT 1 FROM openrails.products p WHERE p.merchant_id=$1 AND p.id=subscriptions.product_id AND p.tier_group IS DISTINCT FROM subscriptions.tier_group)`},
		{"metered_rating_watermarks", `NOT EXISTS(SELECT 1 FROM openrails.customers c WHERE c.merchant_id=$1 AND c.id=metered_rating_watermarks.customer_id)`},
		{"rail_intents", `(subscription_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM openrails.subscriptions s WHERE s.merchant_id=$1 AND s.id=rail_intents.subscription_id))
		 OR (payment_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM openrails.payments p WHERE p.merchant_id=$1 AND p.id=rail_intents.payment_id))
		 OR (price_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM openrails.prices p WHERE p.merchant_id=$1 AND p.id=rail_intents.price_id))`},
	}
	for _, c := range checks {
		if err := refuseRows(ctx, tx, id, c.table, c.predicate); err != nil {
			return err
		}
	}
	return nil
}
