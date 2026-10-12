package merchantarchive

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchantarchive/contract"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Every OpenRails-owned table has an explicit decision. These are deployment/operations
// data or unsupported opaque evidence, not additional archive row profiles.
var excludedTables = map[string]string{
	"destructive_actions":       "deployment command admissions; never imported, re-exported or reset by merchant restore",
	"destructive_holds":         "deployment incident authority and clears; never imported or reset by merchant restore",
	"notification_consumers":    "deployment delivery subscriptions; hosts register at destination",
	"notification_event_acks":   "deployment consumer processing receipts",
	"notification_events":       "immutable source facts, drained before archive",
	"catalog_restore_receipts":  "local restore replay receipts, excluded from re-export like billing_restore maintenance receipts",
	"merchants":                 "destination identity is explicitly provisioned",
	"merchant_slug_aliases":     "former names are directory identity, not billing history",
	"worker_state":              "deployment-wide worker health and fair sweep cursors",
	"destructive_action_switch": "deployment-wide safety switch",
	"book_identity":             "the deployment's database identity",
	"merchant_write_posture":    "deployment write posture: export sets the source readonly, restore the destination",
	"webhook_health":            "telemetry", "webhook_health_daily": "telemetry", "admission_denials_hourly": "telemetry",
	"failed_usage_windows":        "live grace and cutoff windows; the destination counts afresh",
	"payment_attempts":            "decline analytics, kept 25 months; money is payments and provider_intents",
	"rebill_cycles":               "rebill analytics: expected rebills, derived from subscriptions",
	"idempotency_keys":            "short-lived request claims and replays, not moved: a replay at the destination reruns against the moved sessions and provider_intents; money is provider_intents and webhook_events",
	"dashboard_configs":           "presentation",
	"customer_contacts":           "a copy of the merchant's directory; the directory provisions the destination again",
	"customer_links":              "openrails.dev's data, not the merchant's: revoked when the merchant leaves",
	"provisioning_tokens":         "credentials; the destination mints its own",
	"merchant_destructive_policy": "deployment safety policy",
	"carts":                       "a customer's basket, not billing history: priced live, it holds no money and the customer fills it again at the destination",
	"notifications":               "inbox and notification delivery", "provider_mutation_logs": "operator evidence; raw bodies excluded",
	"reconciliation_findings":       "operator observations",
	"account_updater_batches":       "unsupported provider job evidence; any rows refused",
	"destructive_run_before_images": "unsupported byte-exact undo history; any rows refused",
	"subscription_verifications":    "derived: re-detected from unverified subscriptions",
	"nmi_bulk_checkpoints":          "transient read progress",
	"nmi_history_months":            "decline analytics re-read from NMI, kept 25 months",
	"nmi_history_reads":             "read progress: the destination backfills its own",
	"solana_pay_references":         "transient Solana Pay watch state; pending references refuse the archive",
	"solana_pay_receipts":           "Solana transfer receipts; credited transfers are archived as payments, unresolved reviews refuse the archive",
	"access_cutover_approvals":      "the source deployment's cutover evidence; the archive carries the converted product access",

	"customer_entitlement_cache":        "derived from product access and product keys; rebuilt on read",
	"customer_entitlement_cache_stamps": "derived from product access and product keys; rebuilt on read",
	"price_keys":                        "derived: the prices and price_psp_bindings triggers rebuild it as the prices are restored",
	"balance_watches":                   "derived: when a watched balance is next looked at; restore watches every prepaid balance and running charge again",
}

// Explicit exclusions cover only these reviewed columns. A later column is
// unclassified even on a diagnostic table and must receive a new decision.
var excludedColumns = map[string]string{
	"destructive_actions":           "merchant_id id admission_sequence kind target action_key request_digest origin actor_issuer actor_id actor_key credential units admitted_at state started_at finished_at completion_sequence result",
	"destructive_holds":             "merchant_id id scope scope_key actor_issuer actor_id credential action_kind target window_name observed requested ceiling opened_at cleared_at clear_through_sequence cleared_by_issuer cleared_by_id clear_reason",
	"catalog_restore_receipts":      "merchant_id digest rows restored_at",
	"destructive_action_switch":     "singleton enabled updated_by reason updated_at",
	"book_identity":                 "singleton system_identifier database_oid schema_oid armed_by armed_at",
	"merchant_write_posture":        "merchant_id mode reason set_by set_at",
	"worker_state":                  "worker_kind cursor_merchant_id cursor_version registered_at expected_period_seconds last_success_at last_error_at last_error consecutive_failures last_alerted_at updated_at",
	"merchants":                     "id slug status permission_group_id created_at updated_at deleted_at retired_at group_release_completed_at catalog_revision slug_changed_at entitlement_generation",
	"merchant_slug_aliases":         "slug merchant_id expires_at created_at",
	"webhook_health":                "merchant_id psp_id custodian_id last_accepted_at last_pull_at created_at updated_at",
	"webhook_health_daily":          "merchant_id psp_id custodian_id day_at rejected drift",
	"admission_denials_hourly":      "merchant_id customer_id denial_reason hour_at denials updated_at",
	"failed_usage_windows":          "merchant_id customer_id currency invoker window_key window_start window_end amount",
	"payment_attempts":              "id merchant_id customer_id psp_id rail kind owner card_entry source observed_via category reason action response_code response_text transaction_id avs_result cvv_result card_brand card_last4 token_type amount currency attempted_at checkout_id checkout_target subscription_id payment_method_id payment_id provider_intent_id step created_at cycle_id card_bin issuer_code issuer_text enriched_at mandate_id sent_initial_transaction_id order_id",
	"rebill_cycles":                 "id merchant_id subscription_id customer_id psp_id rail owner due_at amount currency created_at missed_at miss_reason quantity",
	"idempotency_keys":              "merchant_id operation idempotency_key status token claims result error lease_expires_at expires_at created_at updated_at",
	"carts":                         "merchant_id customer_id lines last_changed_at created_at",
	"dashboard_configs":             "merchant_id layout updated_at updated_by",
	"customer_contacts":             "merchant_id customer_id email display_name user_name active provisioned_at directory_updated_at created_at updated_at",
	"provisioning_tokens":           "merchant_id id name declared token_sha256 last_used_at created_at",
	"merchant_destructive_policy":   "merchant_id destructive_actions_enabled enforce_armed_at first_pull_completed_at updated_by reason updated_at",
	"notification_consumers":        "id merchant_id owner name created_at",
	"notification_event_acks":       "consumer_id merchant_id event_id acknowledged_at",
	"notification_events":           "merchant_id id customer_id event_type data created_at inserted_at",
	"notifications":                 "id event_type data recipient_kind read_at severity title body link created_at merchant_id customer_id emailed_at email_lease_expires_at",
	"provider_mutation_logs":        "id merchant_id rail psp_id provider_intent_id intent_type idempotency_key attempt phase reason evidence created_at custodian_id",
	"subscription_verifications":    "merchant_id subscription_id unverified_at reads last_read_at last_error",
	"nmi_bulk_checkpoints":          "merchant_id psp_id window_starts_at window_ends_at next_page started_at",
	"nmi_history_months":            "merchant_id psp_id month_at kind category reason authorizations",
	"nmi_history_reads":             "merchant_id psp_id read_at",
	"solana_pay_references":         "merchant_id reference checkout_attempt_id kind status expires_at watch_ends_at next_poll_at signature seen_until scan_stack scan_below built_transaction built_valid_height created_at updated_at",
	"solana_pay_receipts":           "merchant_id reference signature checkout_attempt_id disposition review_reason recipient token_mint expected_amount received_amount payer landed_at payment_id resolved_at resolution created_at",
	"reconciliation_findings":       "id merchant_id finding_type rail psp_id openrails_resource_type openrails_resource_id external_resource_id field openrails_value external_value subject_key severity status recommended_action first_seen_run last_seen_run last_seen_at resolved_at resolution operator_notes created_at updated_at evidence resolved_by notified_at notified_severity seen_run_class change source evidence_digest approval_run_id",
	"account_updater_batches":       "id merchant_id custodian_id job_ref status instruments result_counts failure_reason submitted_at last_polled_at completed_at created_at updated_at",
	"destructive_run_before_images": "id merchant_id destructive_run_id table_name row_id before captured_at restored_at destructive_run_class after_lifecycle_rev",
	"access_cutover_approvals":      "id merchant_id customer_id entitlement change approved_by approved_at",

	"customer_entitlement_cache":        "id merchant_id customer_id entitlement quantity",
	"customer_entitlement_cache_stamps": "id merchant_id customer_id entitlement_generation access_version valid_from valid_until keys held_products created_at updated_at",
	"price_keys":                        "merchant_id product_id key revision created_at updated_at",
	"balance_watches":                   "merchant_id customer_id currency next_check_at depletes_at low_notified_at depleted_at updated_at funded_at usage_rate_per_hour rate_sampled_at",
}

// Omitted columns are either reconstructed by PostgreSQL, deployment
// credential watermarks, or explicitly excluded raw/operational
// data. A new unclassified column fails closed even when currently empty.
var omittedColumns = map[string]string{
	// Canonical price IDs are reconstructed from immutable keys and revision.
	"prices": "public_id",
	// Credential evidence belongs to the source deployment's configuration;
	// the destination validates and fingerprints its own.
	"psps":              "credentials_validated_at webhook_endpoint_id credential_fingerprint credential_duplicate_at",
	"subscriptions":     "destructive_run_class lifecycle_rev row_version",
	"customers":         "access_version",
	"payments":          "destructive_run_class",
	"checkout_attempts": "destructive_run_class",
	"product_access":    "destructive_run_class",
	"maintenance_runs":  "run_class coverage affected note summary error inventory_manifest inventory_total_rows",
	"provider_intents":  "destructive_run_class destructive_action_id",
	"price_migrations":  "destructive_action_id",
	// Derived from the lines: a restored open order is not found by it.
	"orders": "lines_digest",
}

func checkSchema(ctx context.Context, tx pgx.Tx) error {
	known := map[string]bool{}
	archived := map[string]bool{}
	for _, p := range contract.Profiles {
		known[p.Name] = true
		archived[p.Name] = true
	}
	for t := range excludedTables {
		known[t] = true
	}
	rows, err := tx.Query(ctx, `SELECT c.relname,a.attname IS NOT NULL FROM pg_class c LEFT JOIN pg_attribute a ON a.attrelid=c.oid AND a.attname='merchant_id' AND NOT a.attisdropped
		WHERE c.relnamespace=(SELECT relnamespace FROM pg_class WHERE oid='billing.merchants'::regclass) AND c.relkind IN ('r','p') AND c.relname=ANY($1) ORDER BY c.relname`, ownedTables)
	if err != nil {
		return err
	}
	found := map[string]bool{}
	for rows.Next() {
		var name string
		var scoped bool
		if err := rows.Scan(&name, &scoped); err != nil {
			rows.Close()
			return err
		}
		if archived[name] && !scoped {
			rows.Close()
			return &Error{Code: "unsupported_state", Table: name, Err: fmt.Errorf("archive table lacks explicit merchant_id")}
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
	for _, p := range contract.Profiles {
		if !found[p.Name] {
			return &Error{Code: "unsupported_state", Table: p.Name, Err: fmt.Errorf("required archive table absent")}
		}
	}
	return checkColumns(ctx, tx)
}

func checkColumns(ctx context.Context, tx pgx.Tx) error {
	profiles := append([]contract.Profile(nil), contract.Profiles...)
	for table, names := range excludedColumns {
		p := contract.Profile{Name: table}
		for _, name := range strings.Fields(names) {
			p.Columns = append(p.Columns, contract.Column{Name: name})
		}
		profiles = append(profiles, p)
	}
	for _, p := range profiles {
		wanted := map[string]contract.Column{}
		for _, c := range p.Columns {
			wanted[c.Name] = c
		}
		omitted := map[string]bool{}
		for _, c := range strings.Fields(omittedColumns[p.Name]) {
			omitted[c] = true
		}
		rows, err := tx.Query(ctx, `SELECT a.attname,t.typname FROM pg_attribute a JOIN pg_class c ON c.oid=a.attrelid JOIN pg_type t ON t.oid=a.atttypid
			WHERE c.relname=$1 AND c.relnamespace=(SELECT relnamespace FROM pg_class WHERE oid='billing.merchants'::regclass) AND a.attnum>0 AND NOT a.attisdropped`, p.Name)
		if err != nil {
			return err
		}
		for rows.Next() {
			var name, typ string
			if err := rows.Scan(&name, &typ); err != nil {
				rows.Close()
				return err
			}
			if c, ok := wanted[name]; ok {
				expected := map[string]string{"bytea": "bytea", "uuid": "uuid", "text": "text", "text[]": "_text", "bigint": "int8", "integer": "int4", "smallint": "int2", "boolean": "bool", "jsonb": "jsonb", "timestamp with time zone": "timestamptz", "timestamptz": "timestamptz"}[c.Type]
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

func refuseQuery(table, predicate string) string {
	return "SELECT count(*) FROM billing." + table + " WHERE merchant_id=$1 AND (" + predicate + ")"
}

func refuseRows(ctx context.Context, tx pgx.Tx, id billing.MerchantID, table, predicate string) error {
	return refuseRowsAs(ctx, tx, id, "unsupported_state", table, predicate)
}

func refuseRowsAs(ctx context.Context, tx pgx.Tx, id billing.MerchantID, code, table, predicate string) error {
	var count int64
	if err := tx.QueryRow(ctx, refuseQuery(table, predicate), id.UUID()).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return &Error{Code: code, Table: table, Count: count}
	}
	return nil
}

// rowCheck refuses the archive when any of the merchant's rows match.
type rowCheck struct{ table, predicate string }

// preflightChecks are states the archive cannot carry.
var preflightChecks = []rowCheck{
	{"notification_events", `EXISTS (SELECT 1 FROM billing.notification_consumers c WHERE (c.merchant_id IS NULL OR c.merchant_id=notification_events.merchant_id) AND NOT EXISTS(SELECT 1 FROM billing.notification_event_acks a WHERE a.consumer_id=c.id AND a.merchant_id=notification_events.merchant_id AND a.event_id=notification_events.id))`},
	{"destructive_run_before_images", "true"}, {"account_updater_batches", "true"},
	{"checkout_attempts", "status NOT IN ('succeeded','failed','expired','canceled')"},
	// A request still running under a live claim has an outcome the archive
	// would miss; settled, failed and lapsed claims are not moved (#1099).
	{"idempotency_keys", "status='processing' AND lease_expires_at > now()"},
	// Money awaiting a transfer, or received and not yet refunded or
	// resolved, stays with the deployment that watches the chain (#1086).
	{"solana_pay_references", "status='pending'"},
	{"solana_pay_receipts", "review_reason IS NOT NULL AND disposition <> 'duplicate' AND resolved_at IS NULL"},
	{"provider_intents", "status IN ('pending','in_flight','unknown_needs_verify','failed_retryable')"},
	{"payments", "status='pending'"},
	{"admission_operations", "state='open'"},
	{"host_outbox", "delivered_at IS NULL"}, {"webhook_events", "completed_at IS NULL"},
	{"maintenance_runs", "status='running' OR kind NOT IN ('billing_restore','reconciliation','prune','approval','merchant_purge')"},
	{"maintenance_runs", "kind IN ('prune','approval','merchant_purge') AND (coverage IS NOT NULL OR affected IS NOT NULL OR summary IS NOT NULL OR inventory_manifest IS NOT NULL OR inventory_total_rows IS NOT NULL)"},
}

func preflight(ctx context.Context, tx pgx.Tx, id billing.MerchantID) error {
	if err := destructiveArchiveBarrier(ctx, tx, id); err != nil {
		return err
	}
	for _, c := range preflightChecks {
		if err := refuseRows(ctx, tx, id, c.table, c.predicate); err != nil {
			return err
		}
	}
	return validateReferences(ctx, tx, id)
}

// referenceChecks: the ledger intentionally has no control-plane FKs. Archive
// restoration still refuses missing/cross-payer retained business references.
var referenceChecks = []rowCheck{
	// Current available prices obey the same product/price contract as ordinary
	// catalog writes; incompatible archived history remains portable.
	{"prices", `NOT archived AND EXISTS (SELECT 1 FROM billing.products pr
   WHERE pr.merchant_id=$1 AND pr.id=prices.product_id AND NOT pr.archived AND (
    (prices.customer_amount IS NOT NULL AND (pr.credit_grant IS NULL OR NOT COALESCE((pr.credit_grant->>'from_payment')::boolean,false)))
    OR (pr.credit_grant IS NOT NULL AND (prices.billing_interval_hours IS NOT NULL
       OR prices.currency IS DISTINCT FROM pr.credit_grant->>'currency'
       OR (prices.customer_amount IS NULL AND prices.amount<=0)))))`},

	// A native lot retains its payment's accepted promise. The product's current
	// benefit is deliberately irrelevant to previously purchased credit.
	{"payments", `credit_grant_snapshot IS NOT NULL AND status='succeeded' AND NOT EXISTS (
  SELECT 1 FROM billing.grants g JOIN billing.order_lines l ON l.merchant_id=$1 AND l.order_id=payments.order_id AND l.product_id=g.product_id
  WHERE g.merchant_id=$1 AND g.payment_id=payments.id AND g.kind='credit' AND g.event='grant'
    AND g.source_type='purchase' AND g.source_id=payments.id::text
    AND g.customer_id=payments.customer_id AND g.product_id=l.product_id
    AND g.currency=payments.currency AND g.amount=(payments.credit_grant_snapshot->>'amount')::bigint
    AND (g.spec_snapshot->'deposit'->>'paid_amount')::bigint=payments.amount AND payments.amount>0
    AND g.starts_at=(payments.credit_grant_snapshot->>'starts_at')::timestamptz
    AND g.ends_at=(payments.credit_grant_snapshot->>'expires_at')::timestamptz)`},
	// An order's lot keeps its line's promise instead: the frozen benefit per
	// unit, and the line's amount as what was paid for it.
	{"grants", `kind='credit' AND event='grant' AND spec_snapshot->'deposit'->'paid_amount' IS NOT NULL AND NOT EXISTS (
  SELECT 1 FROM billing.payments p WHERE p.merchant_id=$1 AND p.id=grants.payment_id
    AND p.status='succeeded' AND p.customer_id=grants.customer_id AND p.currency=grants.currency
    AND ((p.credit_grant_snapshot IS NOT NULL
        AND (p.credit_grant_snapshot->>'amount')::bigint=grants.amount
        AND p.amount=(grants.spec_snapshot->'deposit'->>'paid_amount')::bigint)
      OR EXISTS (SELECT 1 FROM billing.order_lines l WHERE l.merchant_id=$1 AND l.order_id=p.order_id
        AND l.product_id=grants.product_id AND l.credit_grant IS NOT NULL
        AND (l.credit_grant->>'amount')::bigint*COALESCE(l.quantity,1)=grants.amount
        AND l.amount=(grants.spec_snapshot->'deposit'->>'paid_amount')::bigint)))`},

	{"provider_intents", `intent_type='nmi_vault_delete' AND status='succeeded' AND EXISTS(SELECT 1 FROM billing.payment_methods m WHERE m.merchant_id=$1 AND
          (m.id::text=(CASE WHEN provider_intents.intent_type='initial_membership' THEN provider_intents.payload->'terms'->>'payment_method_id' ELSE provider_intents.payload->>'payment_method_id' END) OR
           (m.custodian='psp' AND m.psp_id=provider_intents.psp_id AND m.rail_customer_ref=provider_intents.payload->>'rail_customer_ref' AND
            (provider_intents.payload->>'billing_entry_only' IS DISTINCT FROM 'true' OR m.rail_method_ref=provider_intents.payload->>'rail_method_ref'))))`},
	{"provider_intents", `intent_type='hyperswitch_method_delete' AND
          (NOT EXISTS(SELECT 1 FROM billing.customers c WHERE c.merchant_id=$1 AND c.id::text=provider_intents.payload->>'customer_id') OR
           EXISTS(SELECT 1 FROM billing.payment_methods m WHERE m.merchant_id=$1 AND
             (m.id::text=(CASE WHEN provider_intents.intent_type='initial_membership' THEN provider_intents.payload->'terms'->>'payment_method_id' ELSE provider_intents.payload->>'payment_method_id' END) OR
              (provider_intents.payload->>'detach_only'='false' AND m.custodian_id=provider_intents.custodian_id AND m.rail_method_ref=provider_intents.payload->'instrument'->>'rail_method_ref'))))`},
	{"ledger_transfers", `(customer_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM billing.customers c WHERE c.merchant_id=$1 AND c.id=ledger_transfers.customer_id))
	 OR (grant_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM billing.grants g
   WHERE g.merchant_id=$1 AND g.id=ledger_transfers.grant_id AND (
     g.customer_id=ledger_transfers.customer_id OR (
       ledger_transfers.customer_id IS NULL AND g.kind='credit'
       AND g.spec_snapshot->'deposit'->'paid_amount' IS NOT NULL
       AND ledger_transfers.transfer_type IN ('credit_purchase_revenue','credit_refund_cash','credit_refund_cash_restore','credit_refund_funding')
       AND NOT EXISTS(SELECT 1 FROM billing.ledger_accounts a WHERE a.merchant_id=$1
         AND a.id IN (ledger_transfers.debit_account_id,ledger_transfers.credit_account_id) AND a.customer_id IS NOT NULL)))))
	 OR (order_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM billing.orders o WHERE o.merchant_id=$1 AND o.id=ledger_transfers.order_id AND o.customer_id=ledger_transfers.customer_id AND o.currency=ledger_transfers.currency))`},
	// Restore preserves historical denormalized tiers, but a live subscription
	// must still agree with its product, as required by the ordinary tier
	// derivation and product-update guards. Include remaining paid access.
	{"subscriptions", `deleted_at IS NULL
	 AND (status IN ('active','pending','past_due','unknown') OR COALESCE(current_period_ends_at,ended_at)>now())
	 AND EXISTS(SELECT 1 FROM billing.products p WHERE p.merchant_id=$1 AND p.id=subscriptions.product_id AND p.tier_group IS DISTINCT FROM subscriptions.tier_group)`},
	{"meter_period_quantities", `NOT EXISTS(SELECT 1 FROM billing.customers c WHERE c.merchant_id=$1 AND c.id=meter_period_quantities.customer_id)`},
	{"ledger_transfers", `(source_id LIKE 'order_collection:%' OR source_id LIKE 'invoice_collection:%') AND
	 (source<>'invoice_charge' OR operation<>'invoice_payment' OR transfer_type<>'owed_payment' OR
	 NOT EXISTS(SELECT 1 FROM billing.payments a WHERE a.merchant_id=$1
	 AND a.ledger_transfer_id=ledger_transfers.id
	 AND a.customer_id=ledger_transfers.customer_id AND a.order_id=ledger_transfers.order_id
	 AND a.currency=ledger_transfers.currency AND a.status='succeeded'))`},
	{"provider_intents", `(subscription_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM billing.subscriptions s WHERE s.merchant_id=$1 AND s.id=provider_intents.subscription_id))
	 OR (payment_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM billing.payments p WHERE p.merchant_id=$1 AND p.id=provider_intents.payment_id))
	 OR (price_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM billing.prices p WHERE p.merchant_id=$1 AND p.id=provider_intents.price_id))`},
}

func validateReferences(ctx context.Context, tx pgx.Tx, id billing.MerchantID) error {
	if err := validateProviderBillReferences(ctx, tx, id); err != nil {
		return err
	}
	if err := validateSubscriptionCollectionReferences(ctx, tx, id); err != nil {
		return err
	}
	if err := validateInitialEnrollmentReferences(ctx, tx, id); err != nil {
		return err
	}
	if err := validateSaleReferences(ctx, tx, id); err != nil {
		return err
	}
	// Platform-vault cards leave only by a custody export; the archive never carries one.
	if err := refuseRowsAs(ctx, tx, id, "platform_cards", "payment_methods", "custodian='platform' AND status<>'removed'"); err != nil {
		return err
	}
	cardCaptureInvalid, err := gen.New(tx).CountInvalidPaymentMethodCaptureReferences(ctx, id.UUID())
	if err != nil {
		return err
	}
	if cardCaptureInvalid != 0 {
		return &Error{Code: "unsupported_state", Table: "payment_methods", Count: cardCaptureInvalid}
	}

	for _, c := range referenceChecks {
		if err := refuseRows(ctx, tx, id, c.table, c.predicate); err != nil {
			return err
		}
	}
	var after *uuid.UUID
	for {
		rows, err := gen.New(tx).ListOrderCollectionsForArchive(ctx, gen.ListOrderCollectionsForArchiveParams{MerchantID: id.UUID(), AfterID: after, PageSize: 256})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, row := range rows {
			if !orderCollectionCanonical(row) {
				return &Error{Code: "unsupported_state", Table: "payments", Err: fmt.Errorf("order collection %s does not name its canonical payment", row.BillingProviderIntent.ID)}
			}
		}
		next := rows[len(rows)-1].BillingProviderIntent.ID
		after = &next
	}
}

// orderCollectionCanonical reports whether a terminal order collection and
// the payment its payload names agree: a settled one wrote exactly its
// receipt's payment and owed-payment transfer, any other wrote none.
func orderCollectionCanonical(row gen.ListOrderCollectionsForArchiveRow) bool {
	operation := row.BillingProviderIntent
	p, err := intents.DecodeOrderCollectionPayload(operation)
	if err != nil {
		return false
	}
	origin := p.Initiator == charge.InitiatorCustomer || operation.Origin == string(intents.OriginAdmin) && intents.OrderCollectionRetryKeyValid(p.OrderID, operation.IdempotencyKey) ||
		p.Initiator == charge.InitiatorMerchant && operation.Origin == string(intents.OriginSystem)
	receipt, collected, receiptErr := intents.LoadCollectedReceipt(operation)
	charged, amountErr := moneyutil.RailMinorToNative(p.Currency, p.AmountMinor)
	if !origin || receiptErr != nil || amountErr != nil {
		return false
	}
	if operation.Status != intents.StatusSucceeded {
		return row.PaymentID == nil && !collected
	}
	return collected && row.PaymentID != nil && *row.PaymentID == p.PaymentID && row.PaymentCustomerID != nil && *row.PaymentCustomerID == p.CustomerID &&
		row.PaymentOrderID != nil && *row.PaymentOrderID == p.OrderID && row.PaymentPspID != nil && *row.PaymentPspID == p.Instrument.PSPID &&
		row.PaymentRail != nil && *row.PaymentRail == p.Rail && row.PaymentTransactionID != nil && *row.PaymentTransactionID == receipt.TransactionID() &&
		row.PaymentAmount != nil && *row.PaymentAmount == charged && row.PaymentCurrency != nil && *row.PaymentCurrency == p.Currency &&
		row.PaymentStatus != nil && *row.PaymentStatus == "succeeded" && row.LedgerMatches && row.LedgerAmount != nil && *row.LedgerAmount == p.Amount
}

// Deployment admission/hold history never travels in a merchant artifact.
// Refuse moving a book while it could discard live authority or recent budget.
func destructiveArchiveBarrier(ctx context.Context, tx pgx.Tx, id billing.MerchantID) error {
	held, err := gen.New(tx).HasDestructiveArchiveBarrier(ctx, gen.HasDestructiveArchiveBarrierParams{MerchantID: id.UUID(), Since: time.Now().UTC().Add(-24 * time.Hour)})
	if err != nil {
		return err
	}
	if held {
		return &Error{Code: "unsupported_state", Table: "destructive_actions", Count: 1}
	}
	return nil
}
