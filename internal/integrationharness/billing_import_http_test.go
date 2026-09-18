//go:build integration

package integrationharness

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/billingimport"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
)

// #737 HTTP door: POST /v1/import/billing on the real standalone server —
// authenticated owner key lands a small declared book (per-SourceID results,
// rows in openrails.* in micros), re-post is idempotent, unauthenticated and
// under-privileged callers are rejected, a wrong-merchant credential cannot
// bind the book (its RLS scope sees no price), and a bad/missing as_of is a
// 400 in the Stripe error envelope.
func TestBillingImportHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
	pool := h.Pool()

	sfx := strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	merchantID := dbtest.TestMerchantID.UUID()
	prod, price := uuid.New(), uuid.New()
	cActive, cCancelled := uuid.New(), uuid.New()

	_, err := pool.Exec(ctx,
		`INSERT INTO openrails.products (id,key,display_name,entitlements_spec,merchant_id) VALUES ($1,$2,$2,'{"premium":null}'::jsonb,$3)`,
		prod, "imphttp-"+sfx, merchantID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx,
		`INSERT INTO openrails.prices (id,product_id,amount,currency,access_duration_hours,auto_renew,merchant_id) VALUES ($1,$2,23000000,'USD',720,true,$3)`,
		price, prod, merchantID)
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, c := range []uuid.UUID{cActive, cCancelled} {
			_, _ = pool.Exec(ctx, `DELETE FROM openrails.entitlements WHERE customer_id=$1`, c)
			_, _ = pool.Exec(ctx, `DELETE FROM openrails.grants WHERE customer_id=$1`, c)
			_, _ = pool.Exec(ctx, `DELETE FROM openrails.payments WHERE customer_id=$1`, c)
			_, _ = pool.Exec(ctx, `DELETE FROM openrails.subscriptions WHERE customer_id=$1`, c)
		}
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.prices WHERE id=$1`, price)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.products WHERE id=$1`, prod)
	})

	day := 24 * time.Hour
	asOf := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	paidThrough := asOf.Add(20 * day)
	cancelAt := asOf.Add(-10 * day)
	subActive, subCancelled := "sub-a-"+sfx, "sub-c-"+sfx

	// or#893: every declared provider row must attribute a PSP.
	dbtest.EnsureTestPSP(ctx, t, pool, merchantID, "nmi")
	dbtest.EnsureTestPSP(ctx, t, pool, merchantID, "ccbill")

	book := billingimport.DeclaredBilling{
		AsOf:       asOf,
		DefaultPSP: billingimport.PSPRef{Key: "nmi"},
		Customers:  []billingimport.DeclaredCustomer{{Customer: openrails.CustomerID(cActive)}, {Customer: openrails.CustomerID(cCancelled)}},
		Subscriptions: []billingimport.DeclaredSubscription{
			{
				SourceID: "runway-" + sfx, Customer: openrails.CustomerID(cActive), Price: openrails.PriceID(price), Rail: "nmi",
				RailSubscriptionID: subActive, StartedAt: asOf.Add(-100 * day), PaidThrough: &paidThrough,
			},
			{
				SourceID: "usercancel-" + sfx, Customer: openrails.CustomerID(cCancelled), Price: openrails.PriceID(price), Rail: "ccbill",
				RailSubscriptionID: subCancelled, StartedAt: asOf.Add(-90 * day),
				PSP:    billingimport.PSPRef{Key: "ccbill"},
				Cancel: billingimport.CancelEvidence{Kind: "user_cancelled", At: cancelAt},
			},
		},
		Transactions: []billingimport.DeclaredTransaction{
			// cents → micros pin at the wire: 2300 amount_cents → 23_000_000.
			{RailSubscriptionID: subActive, TransactionID: "tx-" + sfx, Success: true, AmountCents: 2300, Currency: "USD", OccurredAt: asOf.Add(-10 * day)},
		},
	}
	sourceIDs := []string{"runway-" + sfx, "usercancel-" + sfx}
	importURL := surface.BaseURL + "/v1/import/billing"

	type importResult struct {
		Imported []string          `json:"imported"`
		Skipped  []string          `json:"skipped"`
		Blocked  []string          `json:"blocked"`
		Reasons  map[string]string `json:"reasons"`
	}

	t.Run("PAN refusal is a client error and persists no book", func(t *testing.T) {
		for _, evidence := range []json.RawMessage{
			json.RawMessage(`{"card":"4111111111111111"}`),
			json.RawMessage(`{"card":"\u0034111111111111111"}`),
			json.RawMessage(`{"card":4.111111111111111e15}`),
		} {
			customer := uuid.New()
			candidate := billingimport.DeclaredBilling{
				AsOf: asOf, DefaultPSP: billingimport.PSPRef{Key: "nmi"},
				Customers: []billingimport.DeclaredCustomer{{Customer: openrails.CustomerID(customer)}},
				Subscriptions: []billingimport.DeclaredSubscription{{
					SourceID: "pan-" + customer.String(), Customer: openrails.CustomerID(customer), Price: openrails.PriceID(price), Rail: "nmi",
					RailSubscriptionID: "pan-" + customer.String(), StartedAt: asOf.Add(-day),
					PaidThrough: &paidThrough, Evidence: evidence,
				}},
			}
			status, body := requestJSON(t, http.MethodPost, importURL, surface.Token, candidate)
			require.Equalf(t, http.StatusBadRequest, status, "PAN import refusal: %s", body)
			var envelope struct {
				Error struct{ Type, Code, Message string }
			}
			require.NoError(t, json.Unmarshal(body, &envelope))
			require.Equal(t, "invalid_request_error", envelope.Error.Type)
			require.Equal(t, "invalid_param", envelope.Error.Code)
			require.Contains(t, envelope.Error.Message, "card-number-shaped")
			require.NotContains(t, string(body), "4111111111111111")
			var rows int
			require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM openrails.customers WHERE merchant_id=$1 AND id=$2`, merchantID, customer).Scan(&rows))
			require.Zero(t, rows, "the whole book is refused before customer creation")
		}
	})

	t.Run("authenticated import lands the book", func(t *testing.T) {
		status, body := requestJSON(t, http.MethodPost, importURL, surface.Token, book)
		require.Equalf(t, http.StatusOK, status, "import: %s", string(body))
		var res importResult
		require.NoError(t, json.Unmarshal(body, &res))
		require.ElementsMatch(t, sourceIDs, res.Imported, "reasons: %v", res.Reasons)
		require.Empty(t, res.Blocked)

		var st string
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT status::text FROM openrails.subscriptions WHERE merchant_id=$1 AND rail_subscription_id=$2`,
			merchantID, subActive).Scan(&st))
		require.Equal(t, "active", st, "paid-through beyond AsOf adopts as active")
		var ct string
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT COALESCE(cancel_type::text,'') FROM openrails.subscriptions WHERE merchant_id=$1 AND rail_subscription_id=$2`,
			merchantID, subCancelled).Scan(&ct))
		require.Equal(t, "user", ct, "explicit cancel evidence written faithfully")
		var amt int64
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT amount FROM openrails.payments WHERE merchant_id=$1 AND transaction_id=$2`,
			merchantID, "tx-"+sfx).Scan(&amt))
		require.EqualValues(t, 23_000_000, amt, "amount_cents lands as ledger micros")
	})

	t.Run("re-import is idempotent", func(t *testing.T) {
		status, body := requestJSON(t, http.MethodPost, importURL, surface.Token, book)
		require.Equalf(t, http.StatusOK, status, "re-import: %s", string(body))
		var res importResult
		require.NoError(t, json.Unmarshal(body, &res))
		require.ElementsMatch(t, sourceIDs, res.Skipped, "reasons: %v", res.Reasons)
		require.Empty(t, res.Imported)
		var n int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM openrails.payments WHERE merchant_id=$1 AND transaction_id=$2`,
			merchantID, "tx-"+sfx).Scan(&n))
		require.Equal(t, 1, n)
	})

	t.Run("unauthenticated is rejected", func(t *testing.T) {
		status, _ := requestJSON(t, http.MethodPost, importURL, "", book)
		require.Equal(t, http.StatusUnauthorized, status)
	})

	t.Run("read-only credential is rejected", func(t *testing.T) {
		ro := surface.MintAPIKey(dbtest.TestMerchantSlug, "imp-ro-"+sfx,
			[]string{controlplane.PermMerchantSubscriptionsRead, controlplane.PermMerchantCatalogRead})
		status, body := requestJSON(t, http.MethodPost, importURL, ro, book)
		require.Equalf(t, http.StatusForbidden, status, "read-only key: %s", string(body))
	})

	t.Run("wrong-merchant credential cannot bind the book", func(t *testing.T) {
		b := surface.ProvisionOwnedMerchant("bimp" + sfx)
		// Customer UUIDs can be shared, but B cannot bind A's PSP account.
		// That attribution failure rolls back the entire import.
		status, body := requestJSON(t, http.MethodPost, importURL, b.APIKey, book)
		require.Equalf(t, http.StatusBadRequest, status, "wrong merchant import: %s", string(body))
		require.Contains(t, string(body), "invalid_psp_reference")
		var n int
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM openrails.subscriptions WHERE merchant_id=$1`, b.MerchantID.UUID()).Scan(&n))
		require.Zero(t, n, "no rows may land under the wrong merchant")
		require.NoError(t, pool.QueryRow(ctx,
			`SELECT count(*) FROM openrails.subscriptions WHERE merchant_id=$1 AND rail_subscription_id IN ($2,$3)`,
			merchantID, subActive, subCancelled).Scan(&n))
		require.Equal(t, 2, n, "merchant A's book untouched")
	})

	t.Run("malformed as_of is a 400", func(t *testing.T) {
		status, _ := requestJSON(t, http.MethodPost, importURL, surface.Token,
			json.RawMessage(`{"as_of":"not-a-timestamp","subscriptions":[]}`))
		require.Equal(t, http.StatusBadRequest, status)
	})

	t.Run("missing as_of is a 400 in the Stripe envelope", func(t *testing.T) {
		status, body := requestJSON(t, http.MethodPost, importURL, surface.Token,
			json.RawMessage(`{"subscriptions":[]}`))
		require.Equal(t, http.StatusBadRequest, status)
		var envelope struct {
			Error struct {
				Type string `json:"type"`
				Code string `json:"code"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(body, &envelope))
		require.Equal(t, "invalid_request_error", envelope.Error.Type)
		require.Equal(t, "as_of_required", envelope.Error.Code)
	})
}
