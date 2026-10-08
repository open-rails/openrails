//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchantarchive"
)

// A paid subscription is a real retained book, not an empty-schema archive.
// Application metadata survives alongside the writer's period/correlation facts.
func TestBillingArchivePreservesApplicationMetadata(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	client := w.client[embedded]
	grant, err := client.CreateCreditGrant(t.Context(), e.c.cid(), billing.CreateCreditGrantParams{
		Currency: "USD", Amount: 2_500_000, Source: "archive-test", SourceID: "retained-credit",
	})
	require.NoError(t, err)
	require.EqualValues(t, 2_500_000, grant.RemainingAmount)
	balance, err := client.GetBalance(t.Context(), e.c.cid(), "USD")
	require.NoError(t, err)
	require.EqualValues(t, 2_500_000, balance.BalanceAmount)
	w.settle()
	cadence, err := w.jobs.Insert(t.Context(), riverjobs.InvoiceArgs{Collect: true, UseMonthlyFloor: true}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(t, err)
	w.waitJob(cadence.Job.ID)
	merchantID := client.MerchantID()
	events, err := client.ListHostEvents(t.Context(), billing.HostEventListParams{})
	require.NoError(t, err)
	for _, event := range events.Items {
		_, err := client.AcknowledgeHostEvent(t.Context(), event.ID)
		require.NoError(t, err)
	}
	w.stop() // The source has no writer while its billing book is moved.
	providerAttempts := e.providerAttempts()

	const metadata = `{"application":{"nested":[null,true,1.25,{"new_key":"value"}],"exact":9007199254740993},"solana_subscription_pda":"subscription-pda","solana_token_symbol":"USDC","solana_token_mint":"token-mint","solana_token_amount":9007199254740993,"application_solana":{"signature":"transfer-1"},"campaign":"sk_test_campaign","order_number":"4111111111111111"}`
	for _, update := range []string{
		`UPDATE billing.payments SET metadata = coalesce(metadata, '{}'::jsonb) || $1::jsonb, discount_metadata = $1::jsonb`,
		`UPDATE billing.payment_methods SET metadata = coalesce(metadata, '{}'::jsonb) || $1::jsonb`,
		`UPDATE billing.checkout_attempts SET metadata = coalesce(metadata, '{}'::jsonb) || $1::jsonb`,
		`UPDATE billing.subscriptions SET gateway_response = coalesce(gateway_response, '{}'::jsonb) || $1::jsonb`,
	} {
		result, err := w.pool.Exec(t.Context(), w.q(update), metadata)
		require.NoError(t, err)
		require.Positive(t, result.RowsAffected(), update)
	}
	var paidPeriod string
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT metadata->>'period_start' FROM billing.payments WHERE subscription_id = $1`), e.sub.UUID()).Scan(&paidPeriod))
	require.NotEmpty(t, paidPeriod, "retain the normal subscription writer's paid-period marker")

	// Zero-valued usage/capture and an unbilled item exercise their application
	// JSON without fabricating a financial transfer or changing the paid book.
	for _, insert := range []string{
		`INSERT INTO billing.usage_events (merchant_id, customer_id, invoker_id, currency, event_type, dimensions, amount, source, source_id, pricing_authority, metadata, occurred_at)
		 VALUES ($1, $2, 'archive-application', 'USD', 'archive-metadata', '{}', 0, 'archive-test', 'usage-metadata', 'host', $3::jsonb, now())`,
		`INSERT INTO billing.invoice_items (merchant_id, customer_id, currency, source_type, source_id, invoice_at, amount, status, metadata)
		 VALUES ($1, $2, 'USD', 'archive-test', 'invoice-metadata', now(), 0, 'pending', $3::jsonb)`,
		`INSERT INTO billing.admission_operations (merchant_id, request_id, customer_id, currency, estimated_amount, available_amount, terms, window_keys, state, capture_terms, captured_amount, captured_at)
		 VALUES ($1, 'capture-metadata', $2, 'USD', 0, 0, '{}', '{}', 'captured', jsonb_build_object('metadata', $3::jsonb), 0, now())`,
	} {
		_, err := w.pool.Exec(t.Context(), w.q(insert), merchantID.UUID(), uuid.MustParse(e.c.id), metadata)
		require.NoError(t, err)
	}
	_, err = w.pool.Exec(t.Context(), w.q(`INSERT INTO billing.usage_events (merchant_id, customer_id, invoker_id, currency, event_type, dimensions, amount, source, source_id, pricing_authority, metadata, occurred_at)
		SELECT $1, $2, 'archive-application', 'USD', 'archive-metadata', '{}', 0, 'archive-test', name, 'host', metadata, now()
		FROM (VALUES ('sql-null', NULL::jsonb), ('json-null', 'null'::jsonb), ('empty-object', '{}'::jsonb), ('array', '[1,true,null]'::jsonb), ('scalar', '"app note"'::jsonb)) AS cases(name, metadata)`), merchantID.UUID(), uuid.MustParse(e.c.id))
	require.NoError(t, err)

	source, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	var archive bytes.Buffer
	err = merchantarchive.Export(t.Context(), source, merchantID, &archive)
	require.NoError(t, err, "export must preserve application metadata: %v", errors.Unwrap(err))
	require.Contains(t, archive.String(), `"version":1`)

	destinationSchema := "archive_metadata_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	require.NoError(t, openrails.Migrate(t.Context(), w.pool, openrails.Config{Schema: destinationSchema, River: openrails.RiverHostOwned}))
	destinationName := pgx.Identifier{destinationSchema}.Sanitize()
	t.Cleanup(func() {
		_, _ = w.pool.Exec(context.Background(), "DROP SCHEMA "+destinationName+" CASCADE")
	})
	_, err = w.pool.Exec(t.Context(), "INSERT INTO "+destinationName+`.merchants (id, slug, status, permission_group_id, display_name) VALUES ($1, 'archive-destination', 'active', $2, 'Destination authority')`, merchantID.UUID(), uuid.New())
	require.NoError(t, err)
	destination, err := db.NewWithPGXPool(w.pool, destinationSchema)
	require.NoError(t, err)

	// An old positional format is refused, without partially restoring rows.
	old := bytes.Replace(archive.Bytes(), []byte(`"version":1`), []byte(`"version":3`), 1)
	_, err = merchantarchive.Restore(t.Context(), destination, merchantID, bytes.NewReader(old))
	var archiveErr *merchantarchive.Error
	require.ErrorAs(t, err, &archiveErr)
	require.Equal(t, "invalid_artifact", archiveErr.Code)
	require.ErrorContains(t, errors.Unwrap(err), "unsupported version", "refuse the old profile at its header, not at its changed digest")
	var customers int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+destinationName+".customers").Scan(&customers))
	require.Zero(t, customers)

	result, err := merchantarchive.Restore(t.Context(), destination, merchantID, bytes.NewReader(archive.Bytes()))
	require.NoError(t, err)
	require.False(t, result.Replayed)
	require.Positive(t, result.Rows)

	// Compare stored JSONB, including its exact numeric values and SQL nulls,
	// independently of archive parsing and without decoding through float64.
	for _, tc := range []struct{ table, key, fields string }{
		{"invoice_collection_cadence", "merchant_id", "monthly_period_started_at,completed_at"},
		{"payments", "id", "metadata,discount_metadata,amount,list_amount,currency,status,subscription_id,transaction_id"},
		{"payment_methods", "id", "metadata,rail_customer_ref,rail_method_ref,card_last4"},
		{"checkout_attempts", "id", "metadata,payment_id,subscription_id,status"},
		{"subscriptions", "id", "gateway_response,status,price_id,current_period_starts_at,current_period_ends_at"},
		{"invoice_items", "id", "metadata,amount,status"},
		{"usage_events", "id", "metadata,(metadata IS NULL) AS metadata_is_sql_null,amount,currency"},
		{"admission_operations", "request_id", "capture_terms,captured_amount,state"},
		{"grants", "id", "customer_id,payment_id,starts_at,ends_at,amount"},
		{"entitlements", "id", "customer_id,entitlement,starts_at,ends_at,grant_id"},
		{"ledger_accounts", "id", "credits_posted,debits_posted,currency"},
		{"ledger_transfers", "id", "amount,currency,debit_account_id,credit_account_id"},
	} {
		read := func(schema string) string {
			var snapshot string
			query := fmt.Sprintf("SELECT coalesce(jsonb_agg(to_jsonb(v) ORDER BY %s), '[]'::jsonb)::text FROM (SELECT %s,%s FROM %s.%s) v", tc.key, tc.key, tc.fields, pgx.Identifier{schema}.Sanitize(), tc.table)
			require.NoError(t, w.pool.QueryRow(t.Context(), query).Scan(&snapshot))
			return snapshot
		}
		before := read(w.schema)
		require.NotEqual(t, "[]", before, tc.table)
		require.Equal(t, before, read(destinationSchema), tc.table)
	}
	// A lost response retries the same archive without duplicating money or work.
	replayed, err := merchantarchive.Restore(t.Context(), destination, merchantID, bytes.NewReader(archive.Bytes()))
	require.NoError(t, err)
	require.True(t, replayed.Replayed)
	require.Equal(t, result.Digest, replayed.Digest)
	require.Equal(t, providerAttempts, e.providerAttempts(), "archive/restore never contacts a provider")
}

// The portable billing archive's schema check classifies every subscription
// column the lifecycle added: an export is never refused for the schema
// itself (count 0), only for live rows it cannot move yet.
func TestBillingArchiveClassifiesLifecycleColumns(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "stripe", embedded)
	e.toPeriodEnd()
	w.runRenewals()
	status, body := w.staff(http.MethodGet, "/v1/merchant/billing-archive")
	if status == http.StatusOK {
		return
	}
	var refusal struct {
		Error struct {
			Code     string `json:"code"`
			Metadata struct {
				Table string `json:"table"`
				Count int    `json:"count"`
			} `json:"metadata"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &refusal), body)
	require.Positive(t, refusal.Error.Metadata.Count, "a refusal names live rows, never an unclassified schema: %s", body)
}

// #1099: idempotency claims are not moved by the archive, but a request still
// running under a live claim holds the export back; settled and lapsed claims
// never do.
func TestBillingArchiveWaitsForALiveClaim(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	archiveRefusal := func() (string, int) {
		status, body := w.staff(http.MethodGet, "/v1/merchant/billing-archive")
		if status == http.StatusOK {
			return "", 0
		}
		var refusal struct {
			Error struct {
				Metadata struct {
					Table string `json:"table"`
					Count int    `json:"count"`
				} `json:"metadata"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &refusal), body)
		return refusal.Error.Metadata.Table, refusal.Error.Metadata.Count
	}
	_, err := w.pool.Exec(t.Context(), w.q(`INSERT INTO billing.idempotency_keys (merchant_id, operation, idempotency_key, status, token, result, lease_expires_at, expires_at)
		SELECT id, 'checkout_attempt_create', k, s, gen_random_uuid(), CASE WHEN s = 'succeeded' THEN '{}'::jsonb END, now() + interval '1 hour', now() + interval '1 day'
		FROM billing.merchants, (VALUES ('running', 'processing'), ('done', 'succeeded')) AS v(k, s) WHERE slug = $1`), w.slug)
	require.NoError(t, err)
	table, count := archiveRefusal()
	require.Equal(t, "idempotency_keys", table)
	require.Equal(t, 1, count, "only the live claim holds the export back")

	_, err = w.pool.Exec(t.Context(), w.q(`UPDATE billing.idempotency_keys SET lease_expires_at = now() - interval '1 second' WHERE idempotency_key = 'running'`))
	require.NoError(t, err)
	table, _ = archiveRefusal()
	require.NotEqual(t, "idempotency_keys", table, "a lapsed claim does not block the export")
}

// A membership refused before declines became payment attempts (#1111) left a
// failed payments row under its payment id. The archive keeps accepting that
// record; any other payment under a refused enrollment is refused.
func TestBillingArchiveKeepsPreCutDeclineRecords(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	price := w.membership("content:members", 9_990_000)
	h := hostedPay{w: w, c: w.newCustomer(), tp: embedded, price: price.ID.String()}
	_, err := h.pay("pay-nsf", billing.CheckoutPaymentOptions{PaymentToken: w.nmi.Tokenize(card{Brand: "visa", Last4: "0002", Decline: "202"})})
	require.ErrorIs(t, err, billing.ErrPaymentRefused)
	w.settle()
	refusedTable := func() string {
		status, body := w.staff(http.MethodGet, "/v1/merchant/billing-archive")
		if status == http.StatusOK {
			return ""
		}
		var refusal struct {
			Error struct {
				Metadata struct {
					Table string `json:"table"`
				} `json:"metadata"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &refusal), body)
		return refusal.Error.Metadata.Table
	}
	require.NotEqual(t, "provider_intents", refusedTable())

	// The decline record the checkout wrote before #1111.
	record, err := w.pool.Exec(t.Context(), w.q(`INSERT INTO billing.payments (merchant_id, id, customer_id, price_id, psp_id, channel, rail, transaction_id, amount, list_amount, currency, status, money_movement, purchased_at)
		SELECT merchant_id, (payload->'terms'->>'payment_id')::uuid, (payload->'terms'->>'customer_id')::uuid, (payload->'terms'->>'price_id')::uuid,
		       psp_id, 'rail', rail, rail || '_sub_declined:' || id, (payload->'terms'->>'amount')::bigint, (payload->'terms'->>'recurring_amount')::bigint,
		       payload->'terms'->>'currency', 'failed', 'none', created_at
		FROM billing.provider_intents WHERE intent_type = 'initial_membership' AND status = 'failed_terminal'`))
	require.NoError(t, err)
	require.EqualValues(t, 1, record.RowsAffected())
	require.NotEqual(t, "provider_intents", refusedTable(), "a pre-#1111 decline record is not a payment")

	_, err = w.pool.Exec(t.Context(), w.q(`UPDATE billing.payments SET status = 'completed' WHERE transaction_id LIKE '%_sub_declined:%'`))
	require.NoError(t, err)
	require.Equal(t, "provider_intents", refusedTable(), "a completed payment under a refused enrollment")
	_, err = w.pool.Exec(t.Context(), w.q(`DELETE FROM billing.payments WHERE transaction_id LIKE '%_sub_declined:%'`))
	require.NoError(t, err)
}
