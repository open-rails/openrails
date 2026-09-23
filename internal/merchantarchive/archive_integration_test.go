//go:build integration

package merchantarchive

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchantarchive/contract"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

func archiveDB(t *testing.T, schema string) *db.DB {
	t.Helper()
	super, app := dbtest.SharedRLSPostgres(t)
	if schema != config.DefaultSchema {
		dbtest.ApplyPostgresMigrations(t, super, app, schema)
	}
	d, err := db.NewDB(t.Context(), &config.DBConfig{URL: app, Schema: schema})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	dbtest.BindRiver(t, d)
	return d
}

func provision(t *testing.T, d *db.DB, id merchant.ID) {
	t.Helper()
	_, err := d.Qx(t.Context()).Exec(t.Context(), "INSERT INTO openrails.merchants(id,slug) VALUES($1,$2)", id.UUID(), "archive-"+id.String())
	require.NoError(t, err)
}

func seedBook(t *testing.T, d *db.DB, id merchant.ID) {
	t.Helper()
	ctx := merchant.WithID(t.Context(), id)
	require.NoError(t, d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		exec := func(q string, args ...any) {
			t.Helper()
			_, err := tx.Exec(ctx, q, args...)
			require.NoError(t, err, q)
		}
		customer, product, price, psp, pm, sub, payment, refund, grant, world, balance, invoice := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
		evidenceSnapshot, evidenceTransaction, evidenceSubscription, evidencePaymentMethod := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		exec(`INSERT INTO openrails.customers(merchant_id,id,issuer) VALUES($1,$2,'https://identity.example')`, id.UUID(), customer)
		exec(`INSERT INTO openrails.products(merchant_id,id,key,display_name,entitlements_spec) VALUES($1,$2,'access','Access','{"access":24}')`, id.UUID(), product)
		exec(`INSERT INTO openrails.prices(merchant_id,id,product_id,key,amount,currency,access_duration_hours,auto_renew) VALUES($1,$2,$3,'monthly',1000000,'USD',720,true)`, id.UUID(), price, product)
		exec(`INSERT INTO openrails.psps(merchant_id,id,rail,environment,account_id,key,evidence) VALUES($1,$2,'nmi','test',$3,'primary','{"api_key":"must-not-export","settings":{}}')`, id.UUID(), psp, uuid.NewString())
		exec(`INSERT INTO openrails.provider_evidence_snapshots(id,merchant_id,reconciliation_run_id,provider,psp_id,fetched_at,window_since,window_until,capabilities,coverage) VALUES($1,$2,$3,'nmi',$4,'2026-01-31','2026-01-01','2026-01-31','{"transactions":true}','{"transactions_exhaustive":true,"transactions_paginated_complete":true,"transaction_window_since":"2026-01-01T00:00:00Z","transaction_window_until":"2026-01-31T00:00:00Z"}')`, evidenceSnapshot, id.UUID(), uuid.New(), psp)
		exec(`INSERT INTO openrails.provider_evidence_transactions(id,merchant_id,psp_id,provider,event_key,transaction_id,subscription_ref,type,success,amount_cents,currency,occurred_at,source,customer_ref,customer_email,order_ref,raw,first_snapshot_id,last_snapshot_id,first_seen_at,last_seen_at) VALUES($1,$2,$3,'nmi','archive-event','archive-charge','remote-sub','sale',true,1000000,'USD','2026-01-15','api','remote-customer','buyer@example.test','signup-order','{"transaction_id":"archive-charge"}',$4,$4,'2026-01-31','2026-01-31')`, evidenceTransaction, id.UUID(), psp, evidenceSnapshot)
		exec(`INSERT INTO openrails.provider_evidence_subscriptions(id,snapshot_id,merchant_id,psp_id,provider,record_key,provider_subscription_ref,status,raw_status,customer_ref,customer_email,username,plan_ref,next_billing_at,last_billed_at,amount_cents,currency,raw) VALUES($1,$2,$3,$4,'nmi','archive-sub','remote-sub','active','active','remote-customer','buyer@example.test','buyer','provider-plan','2026-02-15','2026-01-15',1000000,'USD','{"subscription_id":"remote-sub"}')`, evidenceSubscription, evidenceSnapshot, id.UUID(), psp)
		exec(`INSERT INTO openrails.provider_evidence_payment_methods(id,snapshot_id,merchant_id,psp_id,provider,record_key,customer_ref,card_last4,card_expiry,customer_email,raw) VALUES($1,$2,$3,$4,'nmi','archive-pm','remote-customer','4242','0127','buyer@example.test','{"customer_vault_id":"remote-customer"}')`, evidencePaymentMethod, evidenceSnapshot, id.UUID(), psp)
		exec(`INSERT INTO openrails.reconciliation_state(merchant_id,source_domain,fully_reconciled) VALUES($1,'payments',true)`, id.UUID())
		exec(`INSERT INTO openrails.rail_refresh_watermarks(merchant_id,rail,psp_id,event_domain,watermark_at) VALUES($1,'nmi',$2,'events','2026-01-01')`, id.UUID(), psp)
		exec(`INSERT INTO openrails.payment_methods(merchant_id,id,customer_id,psp_id,rail,initial_transaction_id,rail_customer_ref,stored_credential_recurring_ref,last_four) VALUES($1,$2,$3,$4,'nmi','initial','vault-ref','network-anchor','4242')`, id.UUID(), pm, customer, psp)
		exec(`INSERT INTO openrails.subscriptions(merchant_id,id,customer_id,psp_id,product_id,price_id,payment_method_id,rail,rail_subscription_id,status,current_period_starts_at,current_period_ends_at,gateway_response) VALUES($1,$2,$3,$4,$5,$6,$7,'nmi','remote-sub','active','2026-01-01','2026-02-01','{"order_id":"signup-order","provider_transaction_id":"charge-1"}')`, id.UUID(), sub, customer, psp, product, price, pm)
		exec(`INSERT INTO openrails.payments(merchant_id,id,customer_id,psp_id,price_id,subscription_id,rail,transaction_id,amount,list_amount,currency,status,money_movement) VALUES($1,$2,$3,$4,$5,$6,'nmi','charge-1',1000000,1000000,'USD','completed','rail')`, id.UUID(), payment, customer, psp, price, sub)
		exec(`INSERT INTO openrails.payments(merchant_id,id,customer_id,psp_id,price_id,subscription_id,refunded_payment_id,rail,transaction_id,amount,list_amount,currency,status,money_movement,reversal_kind) VALUES($1,$2,$3,$4,$5,$6,$7,'nmi','refund-1',-100000,-100000,'USD','completed','rail','refund')`, id.UUID(), refund, customer, psp, price, sub, payment)
		exec(`INSERT INTO openrails.checkout_sessions(merchant_id,customer_id,id,price_id,psp_id,mode,rail,status,amount,currency,payment_id,subscription_id,rail_fields,rail_state,routing_reason) VALUES($1,$2,$3,$4,$5,'subscription','nmi','succeeded',1000000,'USD',$6,$7,'{"rail":"nmi","psp":"primary","email":"buyer@example.test"}','{"_openrails_request_fingerprint":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}','{"policy":"default","selected":"primary","rail":"nmi"}')`, id.UUID(), customer, uuid.New(), price, psp, payment, sub)
		exec(`UPDATE openrails.host_outbox SET delivered_at=now() WHERE merchant_id=$1`, id.UUID())
		exec(`INSERT INTO openrails.grants(merchant_id,id,customer_id,kind,source_type,source_id,amount,currency,spec_snapshot) VALUES($1,$2,$3,'credit','admin','deposit-1',9007199254740993,'USD','{}')`, id.UUID(), grant, customer)
		exec(`INSERT INTO openrails.ledger_accounts(merchant_id,id,account_type,currency) VALUES($1,$2,'world','USD')`, id.UUID(), world)
		exec(`INSERT INTO openrails.ledger_accounts(merchant_id,id,customer_id,account_type,currency,debits_must_not_exceed_credits) VALUES($1,$2,$3,'customer_balance','USD',true)`, id.UUID(), balance, customer)
		exec(`INSERT INTO openrails.ledger_transfers(merchant_id,debit_account_id,credit_account_id,amount,currency,transfer_type,operation,source,source_id,grant_id,customer_id) VALUES($1,$2,$3,9007199254740993,'USD','deposit','deposit','grant','deposit-1',$4,$5)`, id.UUID(), world, balance, grant, customer)
		exec(`INSERT INTO openrails.invoices(merchant_id,id,customer_id,currency,period_from,period_to,status,total_amount,amount_due) VALUES($1,$2,$3,'USD','2026-01-01','2026-02-01','open',500000,500000)`, id.UUID(), invoice, customer)
		exec(`INSERT INTO openrails.metered_rating_watermarks(merchant_id,customer_id,currency,source,period_from,rated_through,accrued_amount) VALUES($1,$2,'USD','metered:tokens','2026-01-01','2026-01-15',500000)`, id.UUID(), customer)
		exec(`INSERT INTO openrails.webhook_events(merchant_id,op,event_id) VALUES($1,'nmi','event-1')`, id.UUID())
		refundPayload, err := json.Marshal(intents.RefundPayload{OriginalPaymentID: payment, ReservationID: refund, AmountCents: 10, Currency: "USD", ProviderTarget: "charge-1"})
		require.NoError(t, err)
		exec(`INSERT INTO openrails.rail_intents(merchant_id,rail,intent_type,idempotency_key,status,origin,psp_id,payment_id,payload,executed_at,result_evidence) VALUES($1,'nmi','nmi_refund','refund-key','succeeded','admin',$2,$3,$4,now(),'{"transaction_id":"refund-1"}')`, id.UUID(), psp, payment, refundPayload)
		custodian, rootGrant, run, batch, nextPrice := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
		exec(`INSERT INTO openrails.custodians(merchant_id,id,key,kind,account_id,settings,credential_versions) VALUES($1,$2,'vault','basis_theory',$3,'{"public_api_key":"public-token","network_tokens":true,"account_updater":true,"account_updater_lookahead_days":30}','{"api_key":9}')`, id.UUID(), custodian, uuid.NewString())
		exec(`INSERT INTO openrails.merchant_configurations(merchant_id,config) VALUES($1,'{"profile":{"display_name":"Merchant"},"collection_threshold":1000000,"checkout_routing":[{"prefer":["primary"]}]}')`, id.UUID())
		exec(`INSERT INTO openrails.merchant_configuration_applications(merchant_id,application_id,request_sha256,result)
         VALUES($1,'seed-book-metadata',decode(repeat('ab',32),'hex'),jsonb_build_object('application_id','seed-book-metadata','revision',repeat('cd',32),'replayed',false))`, id.UUID())
		exec(`INSERT INTO openrails.billing_policies(merchant_id,name,policy) VALUES($1,'standard','{"kind":"window_spend_cap","spend_windows":[{"key":"daily","window_seconds":86400,"limit":9007199254740993,"currency":"USD"}]}')`, id.UUID())
		exec(`INSERT INTO openrails.billing_policy_bindings(merchant_id,customer_id,policy_name) VALUES($1,$2,'standard')`, id.UUID(), customer)
		exec(`INSERT INTO openrails.catalog_meters(merchant_id,key,event_type,value_property,aggregation,unit,group_by) VALUES($1,'tokens','generation','tokens','sum','token','{"model":"model"}')`, id.UUID())
		exec(`INSERT INTO openrails.catalog_rate_cards(merchant_id,product_id,ordinal,meter_key,filter,price) VALUES($1,$2,1,'tokens','{"model":["large"]}','{"model":"per_unit","currency":"USD","per_unit":{"unit_amount":"2"}}')`, id.UUID(), product)
		exec(`INSERT INTO openrails.price_psp_bindings(merchant_id,price_id,psp_id,plan_id) VALUES($1,$2,$3,'provider-plan')`, id.UUID(), price, psp)
		exec(`INSERT INTO openrails.price_key_movements(merchant_id,key,price_id,effective_at) VALUES($1,'monthly',$2,'2026-01-01')`, id.UUID(), price)
		exec(`INSERT INTO openrails.rail_customer_accounts(merchant_id,customer_id,psp_id,rail,account_id) VALUES($1,$2,$3,'nmi','remote-customer')`, id.UUID(), customer, psp)
		exec(`INSERT INTO openrails.money_settings(merchant_id,customer_id,currency,collection_payment_method_id) VALUES($1,$2,'USD',$3)`, id.UUID(), customer, pm)
		exec(`INSERT INTO openrails.customer_invoice_profiles(merchant_id,customer_id,billing_contacts) VALUES($1,$2,'[{"name":"Billing","email":"billing@example.test"}]')`, id.UUID(), customer)
		exec(`INSERT INTO openrails.invoker_spend_limits(merchant_id,customer_id,scope,scope_key,windows) VALUES($1,$2,'invoker','service-1','[{"key":"daily","window_seconds":86400,"limit":1000000,"currency":"USD"}]')`, id.UUID(), customer)
		exec(`INSERT INTO openrails.grants(merchant_id,id,customer_id,product_id,kind,source_type,source_id,spec_snapshot,starts_at) VALUES($1,$2,$3,$4,'entitlement','subscription',$5,'{"entitlements":["access"]}','2026-01-01')`, id.UUID(), rootGrant, customer, product, sub.String())
		exec(`INSERT INTO openrails.grants(merchant_id,customer_id,product_id,kind,source_type,source_id,event,supersedes_id,starts_at) VALUES($1,$2,$3,'entitlement','admin','adjust-1','adjust',$4,'2026-01-15')`, id.UUID(), customer, product, rootGrant)
		exec(`INSERT INTO openrails.entitlements(merchant_id,customer_id,grant_id,entitlement,start_at,source_id,source_type) VALUES($1,$2,$3,'access','2026-01-01',$4,'subscription')`, id.UUID(), customer, rootGrant, sub)
		exec(`INSERT INTO openrails.usage_events(merchant_id,customer_id,invoker_id,currency,event_type,dimensions,amount,source,source_id,pricing_authority) VALUES($1,$2,'service-1','USD','generation','{"tokens":9007199254740993}',0,'host','usage-1','catalog')`, id.UUID(), customer)
		exec(`INSERT INTO openrails.invoice_items(merchant_id,customer_id,currency,invoice_id,source_type,source_id,invoice_at,amount,status) VALUES($1,$2,'USD',$3,'usage','usage-1','2026-01-10',500000,'invoiced')`, id.UUID(), customer, invoice)
		exec(`INSERT INTO openrails.invoice_payments(merchant_id,customer_id,invoice_id,currency,amount,status,rail,settled_at,idempotency_key) VALUES($1,$2,$3,'USD',100000,'settled','manual','2026-01-11','remittance-1')`, id.UUID(), customer, invoice)
		exec(`INSERT INTO openrails.customer_delinquency(merchant_id,customer_id,currency) VALUES($1,$2,'USD')`, id.UUID(), customer)
		exec(`INSERT INTO openrails.admission_operations(merchant_id,request_id,payer_id,currency,estimated_amount,available_amount,terms,admitted_at,window_keys,state,capture_terms,captured_amount,captured_at) VALUES($1,'zero-capture',$2,'USD',0,0,'{"invoker":"service-1","roles":[],"accrual_rate_delta_per_hour":0}','2026-01-01','{}','captured','{"event_type":"generation","dimensions":{"tokens":10}}',0,'2026-01-01')`, id.UUID(), customer)
		exec(`INSERT INTO openrails.maintenance_runs(merchant_id,id,kind,actor,status,finished_at) VALUES($1,$2,'prune','operator','completed',now())`, id.UUID(), run)
		exec(`UPDATE openrails.payments SET deleted_at=now(),destructive_run_id=$2 WHERE id=$1`, refund, run)
		exec(`INSERT INTO openrails.prices(merchant_id,id,product_id,key,amount,currency,access_duration_hours,auto_renew) VALUES($1,$2,$3,'next-monthly',2000000,'USD',720,true)`, id.UUID(), nextPrice, product)
		exec(`INSERT INTO openrails.reprice_batches(merchant_id,id,to_price_id,source_price_id,effective_at) VALUES($1,$2,$3,$4,'2027-01-01')`, id.UUID(), batch, nextPrice, price)
		exec(`INSERT INTO openrails.subscription_reprices(merchant_id,subscription_id,from_price_id,to_price_id,effective_at,reprice_batch_id) VALUES($1,$2,$3,$4,'2027-01-01',$5)`, id.UUID(), sub, price, nextPrice, batch)
		exec(`INSERT INTO openrails.solana_subscriptions(merchant_id,subscription_id,subscriber_wallet,authority_pda,subscription_pda,plan_pda,merchant_address,mint,plan_created_at_fingerprint,next_pull_at) VALUES($1,$2,'wallet','authority',$2::uuid::text,'plan','merchant','mint',123,'2027-01-01')`, id.UUID(), sub)
		exec(`INSERT INTO openrails.custody_migrations(merchant_id,batch_id,payment_method_id,rail,from_custodian,to_custodian,to_custodian_id,to_rail_method_ref,outcome) VALUES($1,$2,$3,'nmi','psp','basis_theory',$4,'vault-token','remapped')`, id.UUID(), uuid.New(), pm, custodian)
		exec(`INSERT INTO openrails.catalog_applications
			(merchant_id,application_id,catalog_id,schema_version,request_sha256,base_revision,applied_revision,result)
			SELECT m.id,'seed-book',p.catalog_id,1,decode(repeat('ab',32),'hex'),m.catalog_revision-1,m.catalog_revision,
			jsonb_build_object('application_id','seed-book','catalog_id','cat_' || p.catalog_id::text,
			'base_revision',m.catalog_revision-1,'applied_revision',m.catalog_revision,'replayed',false,
			'products_changed',1,'prices_changed',2)
			FROM openrails.merchants m JOIN openrails.products p ON p.merchant_id=m.id
			WHERE m.id=$1 AND p.id=$2`, id.UUID(), product)
		return nil
	}))
}

func TestBillingBookRoundTripAndRetry(t *testing.T) {
	source := archiveDB(t, "openrails")
	target := archiveDB(t, "archive_target")
	back := archiveDB(t, "archive_back")
	id := merchant.ID(uuid.New())
	for _, d := range []*db.DB{source, target, back} {
		provision(t, d, id)
	}
	seedBook(t, source, id)
	var original bytes.Buffer
	require.NoError(t, Export(t.Context(), source, id, &original))
	seen := map[string]int{}
	_, err := contract.Read(bytes.NewReader(original.Bytes()), nil, func(p contract.Profile, _ []*string) error { seen[p.Name]++; return nil })
	require.NoError(t, err)
	for _, p := range contract.Profiles {
		require.Positive(t, seen[p.Name], "every retained profile must be populated in this roundtrip: "+p.Name)
	}

	require.NotContains(t, original.String(), "must-not-export")
	require.Contains(t, original.String(), "9007199254740993")
	result, err := Restore(t.Context(), target, id, bytes.NewReader(original.Bytes()))
	require.NoError(t, err)
	require.False(t, result.Replayed)
	var second bytes.Buffer
	require.NoError(t, Export(t.Context(), target, id, &second))
	require.Equal(t, original.String(), second.String())
	_, err = Restore(t.Context(), back, id, bytes.NewReader(second.Bytes()))
	require.NoError(t, err)
	var third bytes.Buffer
	require.NoError(t, Export(t.Context(), back, id, &third))
	require.Equal(t, original.String(), third.String())
	replay, err := Restore(t.Context(), target, id, bytes.NewReader(original.Bytes()))
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	require.Equal(t, result.Digest, replay.Digest)
	ctx := merchant.WithID(t.Context(), id)
	require.NoError(t, target.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var events, transitions int
		require.NoError(t, tx.QueryRow(ctx, "SELECT count(*) FROM openrails.host_outbox WHERE merchant_id=$1", id.UUID()).Scan(&events))
		require.Equal(t, 1, events)
		require.NoError(t, tx.QueryRow(ctx, "SELECT count(*) FROM openrails.subscription_status_transitions WHERE merchant_id=$1", id.UUID()).Scan(&transitions))
		require.Equal(t, 1, transitions)
		return nil
	}))
}

// Deployment credential publications must not prevent a billing book from
// moving, and restoring that book must never arm the source credential plane.
func TestProviderPublicationEvidenceStaysAtSource(t *testing.T) {
	source := archiveDB(t, "openrails")
	target := archiveDB(t, "archive_publication_target")
	id := merchant.ID(uuid.New())
	provision(t, source, id)
	provision(t, target, id)
	ctx := merchant.WithID(t.Context(), id)
	psp, operation := uuid.New(), uuid.New()
	candidate := "credential_candidates/" + operation.String() + "/psps/stripe/test/acct_archive/secret_key"
	evidence, err := json.Marshal(map[string]any{
		"public_config":                 map[string]string{"publishable_key": "pk_test_portable"},
		"credential_versions":           map[string]int{"secret_key": 7, "webhook_signing_secret_previous": 5},
		"credential_refs":               map[string]any{"secret_key": map[string]any{"name": candidate, "version": 1, "custody": "vault"}},
		"credential_custody":            "vault",
		"credential_custody_transition": map[string]any{"operation_id": operation, "from": "db", "to": "vault"},
		"configuration_revision":        9,
		"credentials_validated":         true,
		"retired_credentials":           map[string]bool{"webhook_signing_secret_previous": true},
		"webhook_endpoint_id":           "we_source_only",
	})
	require.NoError(t, err)
	require.NoError(t, source.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO openrails.psps(merchant_id,id,rail,environment,account_id,key,evidence) VALUES($1,$2,'stripe','test',$2::uuid::text,'stripe',$3)`, id.UUID(), psp, evidence)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO openrails.credential_publications(merchant_id,operation_id,rail,environment,account_id,expected_revision,request_metadata,state,result,published_at) VALUES($1,$2,'stripe','test',$3,8,'{}','published',$4,now())`, id.UUID(), operation, psp.String(), evidence)
		return err
	}))
	var artifact bytes.Buffer
	require.NoError(t, Export(ctx, source, id, &artifact))
	for _, excluded := range []string{candidate, "credential_refs", "credential_custody", "configuration_revision", "credential_versions", "credentials_validated", "retired_credentials", "webhook_endpoint_id", "we_source_only"} {
		require.NotContains(t, artifact.String(), excluded)
	}
	_, err = Restore(ctx, target, id, bytes.NewReader(artifact.Bytes()))
	require.NoError(t, err)
	require.NoError(t, target.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var restored []byte
		require.NoError(t, tx.QueryRow(ctx, `SELECT evidence FROM openrails.psps WHERE merchant_id=$1 AND id=$2`, id.UUID(), psp).Scan(&restored))
		require.JSONEq(t, `{"public_config":{"publishable_key":"pk_test_portable"}}`, string(restored))
		var receipts, secrets int
		require.NoError(t, tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM openrails.credential_publications WHERE merchant_id=$1),(SELECT count(*) FROM openrails.merchant_secrets WHERE merchant_id=$1)`, id.UUID()).Scan(&receipts, &secrets))
		require.Zero(t, receipts)
		require.Zero(t, secrets)
		return nil
	}))
	// Unreviewed evidence is still refused; classification is not a blanket
	// permission to discard future replay or financial facts.
	require.NoError(t, source.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE openrails.psps SET evidence=evidence || '{"unclassified_replay_fact":true}'::jsonb WHERE merchant_id=$1 AND id=$2`, id.UUID(), psp)
		return err
	}))
	require.ErrorContains(t, Export(ctx, source, id, &bytes.Buffer{}), "unsupported_state: psps")
}

func TestRestoreBadFooterRollsBackAndCannotForgeGuard(t *testing.T) {
	d := archiveDB(t, "openrails")
	id := merchant.ID(uuid.New())
	provision(t, d, id)
	var b bytes.Buffer
	w, err := archivewire.NewWriter(&b, id.String())
	require.NoError(t, err)
	for _, p := range contract.Profiles {
		require.NoError(t, w.Table(p.Name))
		if p.Name == "customers" {
			values := make([]*string, len(p.Columns))
			for i, c := range p.Columns {
				v := ""
				switch c.Name {
				case "merchant_id":
					v = id.String()
				case "id":
					v = uuid.NewString()
				case "created_at", "last_seen_at":
					v = "2026-01-01 00:00:00+00"
				default:
					continue
				}
				values[i] = &v
			}
			require.NoError(t, w.Row(values))
		}
	}
	require.NoError(t, w.Close())
	_, err = Restore(t.Context(), d, id, bytes.NewReader(b.Bytes()[:b.Len()-1]))
	require.Error(t, err)
	ctx := merchant.WithID(t.Context(), id)
	require.NoError(t, d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		require.NoError(t, tx.QueryRow(ctx, "SELECT count(*) FROM openrails.customers WHERE merchant_id=$1", id.UUID()).Scan(&n))
		require.Zero(t, n)
		require.NoError(t, tx.QueryRow(ctx, "SELECT count(*) FROM openrails.maintenance_runs WHERE merchant_id=$1", id.UUID()).Scan(&n))
		require.Zero(t, n)
		return nil
	}))
	err = d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "INSERT INTO openrails.maintenance_runs(merchant_id,kind,actor) VALUES($1,'billing_restore','merchantarchive')", id.UUID())
		return err
	})
	require.ErrorContains(t, err, "restore receipts")
	err = d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "SELECT openrails.begin_billing_restore($1)", id.UUID())
		return err
	})
	require.ErrorContains(t, err, "unfinished billing restore")
	_, err = Restore(t.Context(), d, id, bytes.NewReader(b.Bytes()))
	require.NoError(t, err)
	_, err = Restore(t.Context(), d, merchant.ID(uuid.New()), bytes.NewReader(b.Bytes()))
	require.Error(t, err)
}

func TestArchiveSchemaCoverageAndBusyRefusal(t *testing.T) {
	d := archiveDB(t, "openrails")
	id := merchant.ID(uuid.New())
	provision(t, d, id)
	seedBook(t, d, id)
	ctx := merchant.WithID(t.Context(), id)
	var accepted []byte
	require.NoError(t, d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "SELECT payload FROM openrails.rail_intents WHERE merchant_id=$1 AND intent_type='nmi_refund'", id.UUID()).Scan(&accepted); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "UPDATE openrails.rail_intents SET payload=payload-'currency' WHERE merchant_id=$1 AND intent_type='nmi_refund'", id.UUID())
		return err
	}))
	var incomplete bytes.Buffer
	err := Export(t.Context(), d, id, &incomplete)
	var invalid *Error
	require.ErrorAs(t, err, &invalid)
	require.Equal(t, "rail_intents", invalid.Table)
	_, err = archivewire.Read(bytes.NewReader(incomplete.Bytes()), nil, nil)
	require.Error(t, err, "a failed streaming export must not produce a valid archive")
	require.NoError(t, d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE openrails.rail_intents SET payload=$2 WHERE merchant_id=$1 AND intent_type='nmi_refund'", id.UUID(), accepted)
		return err
	}))
	require.NoError(t, d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE openrails.payments SET status='pending' WHERE merchant_id=$1 AND transaction_id='refund-1'", id.UUID())
		return err
	}))
	var b bytes.Buffer
	err = Export(t.Context(), d, id, &b)
	var ae *Error
	require.ErrorAs(t, err, &ae)
	require.Equal(t, "payments", ae.Table)
	require.Zero(t, b.Len())
}
