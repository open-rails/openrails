package merchantarchive

import (
	"context"
	"fmt"
	"strings"

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
	"catalog_restore_receipts":  "local restore replay receipts, excluded from re-export like billing_restore maintenance receipts",
	"credential_publications":   "deployment credential custody receipts; secrets are re-entered at destination",
	"merchants":                 "destination identity and host authority are explicitly provisioned",
	"merchant_slug_aliases":     "former names are directory identity, not billing history",
	"merchant_api_host_claims":  "an unproven host claim; the destination claims and proves its own host",
	"worker_state":              "deployment-wide worker health and fair sweep cursors",
	"destructive_action_switch": "deployment-wide safety switch",
	"webhook_health":            "telemetry", "webhook_health_daily": "telemetry", "admission_denials_hourly": "telemetry",
	"card_attempt_failures":  "card-testing telemetry",
	"payment_attempts":       "decline analytics, kept 25 months; money is payments and provider_intents",
	"rebill_cycles":          "rebill analytics: expected rebills, derived from subscriptions",
	"payment_method_updates": "card-update analytics: account updater and customer card changes",
	"idempotency_keys":       "short-lived request claims and replays, not moved: a replay at the destination reruns against the moved sessions and provider_intents; money is provider_intents and webhook_events",
	"dashboard_configs":      "presentation",
	"federated_grants":       "access authority; the destination invites its own staff",
	"merchant_deks":          "encryption key material", "merchant_secrets": "credentials are re-entered at destination",
	"merchant_destructive_policy": "deployment safety policy", "merchant_webhooks": "destinations and signing-key versions are reconfigured",
	"checkout_sessions": "short-lived checkout capabilities, not moved: an open payment refuses the archive through its checkout attempt; a buyer mints a new one at the destination",
	"notifications":     "inbox and notification delivery", "provider_mutation_logs": "operator evidence; raw bodies excluded",
	"reconciliation_findings":       "operator observations",
	"product_archive_operations":    "operation replay receipts; the refunds they produced are archived payments",
	"account_updater_batches":       "unsupported provider job evidence; any rows refused",
	"destructive_run_before_images": "unsupported byte-exact undo history; any rows refused",
	"operation_authorizations":      "unsupported opaque byte-exact operation evidence; any rows refused",
	"cost_qualifications":           "unsupported opaque byte-exact provider evidence; any rows refused",
	"cost_observations":             "unsupported opaque byte-exact provider bodies; any rows refused",
	"subscription_verifications":    "derived: re-detected from unverified subscriptions",
	"nmi_bulk_checkpoints":          "transient read progress",
	"nmi_history_months":            "decline analytics re-read from NMI, kept 25 months",
	"nmi_history_reads":             "read progress: the destination backfills its own",
	"solana_pay_references":         "transient Solana Pay watch state; pending references refuse the archive",
	"solana_pay_receipts":           "Solana transfer receipts; credited transfers are archived as payments, unresolved reviews refuse the archive",
}

// Explicit exclusions cover only these reviewed columns. A later column is
// unclassified even on a diagnostic table and must receive a new decision.
var excludedColumns = map[string]string{
	"catalog_restore_receipts":      "merchant_id digest rows restored_at",
	"credential_publications":       "merchant_id operation_id rail environment account_id expected_revision request_metadata state result created_at published_at",
	"destructive_action_switch":     "singleton enabled updated_by reason updated_at",
	"worker_state":                  "worker_kind cursor_merchant_id cursor_version registered_at expected_period_seconds last_success_at last_error_at last_error consecutive_failures last_alerted_at updated_at",
	"merchants":                     "id slug status permission_group_id created_at updated_at deleted_at display_name api_host retired_at group_release_completed_at catalog_revision slug_changed_at",
	"merchant_slug_aliases":         "slug merchant_id expires_at created_at",
	"merchant_api_host_claims":      "merchant_id api_host token created_at",
	"webhook_health":                "merchant_id psp_id custodian_id last_accepted_at last_pull_at created_at updated_at",
	"webhook_health_daily":          "merchant_id psp_id custodian_id day_at rejected drift",
	"admission_denials_hourly":      "merchant_id customer_id denial_reason hour_at denials updated_at",
	"card_attempt_failures":         "merchant_id subject bucket_at failures",
	"payment_attempts":              "id merchant_id customer_id psp_id rail kind owner card_entry source observed_via category reason action response_code response_text transaction_id avs_result cvv_result card_brand card_last4 token_type amount currency attempted_at checkout_id checkout_target subscription_id payment_method_id payment_id provider_intent_id step created_at cycle_id card_bin issuer_code issuer_text enriched_at",
	"rebill_cycles":                 "id merchant_id subscription_id customer_id psp_id rail owner due_at amount currency created_at missed_at miss_reason",
	"payment_method_updates":        "id merchant_id payment_method_id customer_id psp_id source kind event_ref occurred_at created_at",
	"idempotency_keys":              "merchant_id operation idempotency_key status token claims result error lease_expires_at expires_at created_at updated_at",
	"checkout_sessions":             "merchant_id id_hash customer_id price_id offer success_url origin attempt attempt_id expires_at purge_at created_at",
	"dashboard_configs":             "merchant_id layout updated_at updated_by",
	"federated_grants":              "merchant_id id email role issuer subject accepted_at created_at updated_at",
	"merchant_deks":                 "merchant_id wrapped_dek created_at updated_at",
	"merchant_secrets":              "merchant_id name value version created_at updated_at",
	"merchant_destructive_policy":   "merchant_id destructive_actions_enabled enforce_armed_at first_pull_completed_at updated_by reason updated_at",
	"merchant_webhooks":             "id merchant_id name destination_host secret_version format enabled created_at updated_at",
	"notifications":                 "id event_type data recipient_kind read_at severity title body link created_at merchant_id customer_id emailed_at",
	"provider_mutation_logs":        "id merchant_id rail psp_id provider_intent_id intent_type idempotency_key attempt phase reason evidence created_at custodian_id",
	"subscription_verifications":    "merchant_id subscription_id unverified_at reads last_read_at last_error",
	"nmi_bulk_checkpoints":          "merchant_id psp_id window_starts_at window_ends_at next_page started_at",
	"nmi_history_months":            "merchant_id psp_id month_at kind category reason authorizations",
	"nmi_history_reads":             "merchant_id psp_id read_at",
	"solana_pay_references":         "merchant_id reference checkout_attempt_id kind status expires_at watch_ends_at next_poll_at signature seen_until scan_stack scan_below built_transaction built_valid_height created_at updated_at",
	"solana_pay_receipts":           "merchant_id reference signature checkout_attempt_id disposition review_reason recipient token_mint expected_amount received_amount payer landed_at payment_id resolved_at resolution created_at",
	"product_archive_operations":    "merchant_id id idempotency_key request_sha256 product_id purchase_action purchase_window_starts_at reason created_at",
	"reconciliation_findings":       "id merchant_id finding_type rail psp_id openrails_resource_type openrails_resource_id external_resource_id field openrails_value external_value subject_key severity status recommended_action first_seen_run last_seen_run last_seen_at resolved_at resolution operator_notes created_at updated_at evidence resolved_by notified_at notified_severity seen_run_class",
	"account_updater_batches":       "id merchant_id custodian_id job_ref status instruments result_counts failure_reason submitted_at last_polled_at completed_at created_at updated_at",
	"destructive_run_before_images": "id merchant_id destructive_run_id table_name row_id before captured_at restored_at destructive_run_class",
	"operation_authorizations":      "operation_id merchant_id customer_id record_owner ledger_account_id currency amount claim_reference authorization_body_bytes authorization_body_digest state terminal_reference created_at released_at settled_at settlement_cost_amount settlement_amount settlement_body_bytes settlement_body_digest",
	"cost_qualifications":           "merchant_id operation_id provider provider_resource_id provider_lifetime_starts_at provider_lifetime_ends_at provider_absent_at provider_absence_reference billing_stop_reference windows_closed_at windows_closed_reference lifecycle_evidence_bytes lifecycle_evidence_digest quiescence_seconds state reason baseline_observation_id qualified_observation_id qualified_cost_amount qualified_at created_at updated_at",
	"cost_observations":             "merchant_id operation_id observation_id normalized_query query_starts_at query_ends_at raw_body_available raw_body_bytes raw_body_digest normalized_records_bytes normalized_records_digest cost_amount has_negative_record refusal_kind covers_lifetime qualification_reason observed_at",
}

// Omitted columns are either reconstructed by PostgreSQL, deployment
// credential watermarks, or explicitly excluded raw/operational
// data. A new unclassified column fails closed even when currently empty.
var omittedColumns = map[string]string{
	"custodians": "credential_versions",
	// Credential publication state belongs to the source deployment's secret
	// custody; credentials are re-entered at the destination.
	"psps":              "credential_custody credential_refs credential_versions retired_credentials credentials_validated_at webhook_endpoint_id webhook_overlap_expires_at revision",
	"subscriptions":     "destructive_run_class lifecycle_rev row_version",
	"payments":          "destructive_run_class",
	"checkout_attempts": "destructive_run_class",
	"entitlements":      "period destructive_run_class",
	"maintenance_runs":  "run_class coverage affected note summary error inventory_manifest inventory_total_rows",
	"provider_intents":  "destructive_run_class",
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
	var count int64
	if err := tx.QueryRow(ctx, refuseQuery(table, predicate), id.UUID()).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return &Error{Code: "unsupported_state", Table: table, Count: count}
	}
	return nil
}

// rowCheck refuses the archive when any of the merchant's rows match.
type rowCheck struct{ table, predicate string }

// preflightChecks are states the archive cannot carry.
var preflightChecks = []rowCheck{
	{"operation_authorizations", "true"}, {"cost_qualifications", "true"}, {"cost_observations", "true"},
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
	{"payments", "status='pending'"}, {"invoice_payments", "status='attempted'"},
	{"invoices", "collection_intent_id IS NOT NULL"},
	{"admission_operations", "state='open'"},
	{"host_outbox", "delivered_at IS NULL"}, {"webhook_events", "completed_at IS NULL"},
	{"maintenance_runs", "status='running' OR kind NOT IN ('billing_restore','reconciliation','prune','converge_enforce','merchant_purge')"},
	{"maintenance_runs", "kind IN ('prune','converge_enforce','merchant_purge') AND (coverage IS NOT NULL OR affected IS NOT NULL OR summary IS NOT NULL OR inventory_manifest IS NOT NULL OR inventory_total_rows IS NOT NULL)"},
}

func preflight(ctx context.Context, tx pgx.Tx, id billing.MerchantID) error {
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
	// Current available offers obey the same product/price contract as ordinary
	// catalog writes; incompatible archived history remains portable.
	{"prices", `NOT archived AND EXISTS (SELECT 1 FROM billing.products pr
   WHERE pr.merchant_id=$1 AND pr.id=prices.product_id AND NOT pr.archived AND (
    (prices.customer_amount IS NOT NULL AND (pr.credit_grant IS NULL OR NOT COALESCE((pr.credit_grant->>'from_payment')::boolean,false)))
    OR (pr.credit_grant IS NOT NULL AND (prices.billing_interval_hours IS NOT NULL
       OR prices.currency IS DISTINCT FROM pr.credit_grant->>'currency'
       OR (prices.customer_amount IS NULL AND prices.amount<=0)))))`},

	// A native lot retains its payment's accepted promise. The product's current
	// benefit is deliberately irrelevant to previously purchased credit.
	{"payments", `credit_grant_snapshot IS NOT NULL AND status='completed' AND NOT EXISTS (
  SELECT 1 FROM billing.grants g JOIN billing.prices pr ON pr.merchant_id=$1 AND pr.id=payments.price_id
  WHERE g.merchant_id=$1 AND g.payment_id=payments.id AND g.kind='credit' AND g.event='grant'
    AND g.source_type='purchase' AND g.source_id=payments.id::text
    AND g.customer_id=payments.customer_id AND g.product_id=pr.product_id
    AND g.currency=payments.currency AND g.amount=(payments.credit_grant_snapshot->>'amount')::bigint
    AND (g.spec_snapshot->'deposit'->>'paid_amount')::bigint=payments.amount AND payments.amount>0
    AND g.starts_at=(payments.credit_grant_snapshot->>'starts_at')::timestamptz
    AND g.ends_at=(payments.credit_grant_snapshot->>'expires_at')::timestamptz)`},
	{"grants", `kind='credit' AND event='grant' AND spec_snapshot->'deposit'->'paid_amount' IS NOT NULL AND NOT EXISTS (
  SELECT 1 FROM billing.payments p WHERE p.merchant_id=$1 AND p.id=grants.payment_id
    AND p.status='completed' AND p.credit_grant_snapshot IS NOT NULL
    AND p.customer_id=grants.customer_id AND p.currency=grants.currency
    AND (p.credit_grant_snapshot->>'amount')::bigint=grants.amount
    AND p.amount=(grants.spec_snapshot->'deposit'->>'paid_amount')::bigint)`},

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
	 OR (invoice_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM billing.invoices i WHERE i.merchant_id=$1 AND i.id=ledger_transfers.invoice_id AND i.customer_id=ledger_transfers.customer_id AND i.currency=ledger_transfers.currency))`},
	// Restore preserves historical denormalized tiers, but a live subscription
	// must still agree with its product, as required by the ordinary tier
	// derivation and product-update guards. Include remaining paid access.
	{"subscriptions", `deleted_at IS NULL
	 AND (status IN ('active','pending','past_due','unknown') OR COALESCE(current_period_ends_at,ended_at)>now())
	 AND EXISTS(SELECT 1 FROM billing.products p WHERE p.merchant_id=$1 AND p.id=subscriptions.product_id AND p.tier_group IS DISTINCT FROM subscriptions.tier_group)`},
	{"metered_rating_watermarks", `NOT EXISTS(SELECT 1 FROM billing.customers c WHERE c.merchant_id=$1 AND c.id=metered_rating_watermarks.customer_id)`},
	{"ledger_transfers", `source_id LIKE 'invoice_collection:%' AND
	 (source<>'invoice_charge' OR operation<>'invoice_payment' OR transfer_type<>'owed_payment' OR
	 NOT EXISTS(SELECT 1 FROM billing.invoice_payments a WHERE a.merchant_id=$1
	 AND a.ledger_transfer_id=ledger_transfers.id AND a.idempotency_key=ledger_transfers.source_id
	 AND a.customer_id=ledger_transfers.customer_id AND a.invoice_id=ledger_transfers.invoice_id
	 AND a.currency=ledger_transfers.currency AND a.status='settled'))`},
	{"invoice_payments", `idempotency_key LIKE 'invoice_collection:%'
	 AND NOT EXISTS(SELECT 1 FROM billing.provider_intents i WHERE i.merchant_id=$1
	 AND i.intent_type='invoice_collection' AND i.idempotency_key=invoice_payments.idempotency_key)`},
	{"provider_intents", `(subscription_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM billing.subscriptions s WHERE s.merchant_id=$1 AND s.id=provider_intents.subscription_id))
	 OR (payment_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM billing.payments p WHERE p.merchant_id=$1 AND p.id=provider_intents.payment_id))
	 OR (price_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM billing.prices p WHERE p.merchant_id=$1 AND p.id=provider_intents.price_id))`},
}

func validateReferences(ctx context.Context, tx pgx.Tx, id billing.MerchantID) error {
	purchaseInvalid, err := gen.New(tx).CountInvalidPurchaseCheckoutReferences(ctx, id.UUID())
	if err != nil {
		return err
	}
	if purchaseInvalid != 0 {
		return &Error{Code: "unsupported_state", Table: "checkout_attempts", Count: purchaseInvalid}
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
	quoteInvalid, quoteErr := gen.New(tx).CountInvalidEngineCheckoutReferences(ctx, id.UUID())
	if quoteErr != nil {
		return quoteErr
	}
	if quoteInvalid != 0 {
		return &Error{Code: "unsupported_state", Table: "checkout_attempts", Count: quoteInvalid}
	}
	stripeInvalid, stripeErr := gen.New(tx).CountInvalidStripeSetupReferences(ctx, id.UUID())
	if stripeErr != nil {
		return stripeErr
	}
	if stripeInvalid != 0 {
		return &Error{Code: "unsupported_state", Table: "checkout_attempts", Count: stripeInvalid}
	}
	invalid, err := gen.New(tx).CountInvalidCheckoutCaptureReferences(ctx, id.UUID())
	if err != nil {
		return err
	}
	if invalid != 0 {
		return &Error{Code: "unsupported_state", Table: "checkout_attempts", Count: invalid}
	}

	for _, c := range referenceChecks {
		if err := refuseRows(ctx, tx, id, c.table, c.predicate); err != nil {
			return err
		}
	}
	var after *uuid.UUID
	for {
		rows, err := gen.New(tx).ListEncodedInvoiceAttemptsForArchive(ctx, gen.ListEncodedInvoiceAttemptsForArchiveParams{MerchantID: id.UUID(), AfterID: after, PageSize: 256})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, row := range rows {
			a, operation := row.BillingInvoicePayment, row.BillingProviderIntent
			p, err := intents.DecodeInvoiceCollectionPayload(operation)
			if err != nil {
				return &Error{Code: "unsupported_state", Table: "invoice_payments", Err: err}
			}
			methodMatches := a.PaymentMethodID != nil && *a.PaymentMethodID == p.PaymentMethodID
			if a.PaymentMethodID == nil {
				deletions, err := gen.New(tx).ListMethodDeletesForArchive(ctx, gen.ListMethodDeletesForArchiveParams{MerchantID: operation.MerchantID, PaymentMethodID: p.PaymentMethodID})
				if err != nil {
					return err
				}
				if len(deletions) == 1 {
					method, customer, err := intents.DeletedMethod(deletions[0])
					methodMatches = err == nil && method == p.PaymentMethodID && customer == p.CustomerID
				}
			}
			receipt, collected, receiptErr := intents.LoadCollectedReceipt(operation)
			amount, amountErr := moneyutil.RailMinorToNative(p.Currency, p.AmountMinor)
			matches := a.MerchantID == operation.MerchantID && a.ID == p.AttemptID && a.CustomerID == p.CustomerID && a.InvoiceID == p.InvoiceID &&
				methodMatches && a.PspID != nil && *a.PspID == p.Instrument.PSPID &&
				a.IdempotencyKey != nil && *a.IdempotencyKey == operation.IdempotencyKey && a.Currency == p.Currency && a.Amount == amount &&
				(p.Initiator == charge.InitiatorCustomer || operation.Origin == string(intents.OriginAdmin) && intents.InvoiceCollectionRetryKeyValid(p.InvoiceID, operation.IdempotencyKey) || p.Initiator == charge.InitiatorMerchant && operation.Origin == string(intents.OriginSystem)) && a.Rail != nil && *a.Rail == p.Rail
			terminal := operation.Status == intents.StatusFailedTerminal && a.Status == "failed" && !collected ||
				operation.Status == intents.StatusSucceeded && a.Status == "settled" && collected && row.LedgerMatches && row.LedgerAmount != nil && *row.LedgerAmount == p.Amount && a.RailPaymentID != nil && *a.RailPaymentID == receipt.TransactionID()
			if receiptErr != nil || amountErr != nil || !matches || !terminal {
				return &Error{Code: "unsupported_state", Table: "invoice_payments", Err: fmt.Errorf("encoded attempt key does not name its canonical collection outcome")}
			}
		}
		next := rows[len(rows)-1].BillingInvoicePayment.ID
		after = &next
	}
}
