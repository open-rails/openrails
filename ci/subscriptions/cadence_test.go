//go:build greenfield && integration

package subscriptions_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/cadence"
)

// cadencePrice creates an auto-renew product and a price at hours with the
// default (omitted) key.
func (w *world) cadencePrice(tp topology, productKey, entitlement string, amount int64, hours int) *openrails.Price {
	w.t.Helper()
	client := w.client[tp]
	product, err := client.Products.RetrieveByKey(w.t.Context(), productKey)
	if err != nil {
		product, err = client.Products.Create(w.t.Context(), &openrails.ProductCreateParams{Key: productKey, DisplayName: "Cadence " + productKey, EntitlementsSpec: map[string]*int{entitlement: nil}})
	}
	require.NoError(w.t, err)
	price, err := client.Prices.Create(w.t.Context(), &openrails.PriceCreateParams{ProductID: product.ID, UnitAmount: amount, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours})
	require.NoError(w.t, err)
	return price
}

func (w *world) sql(query string) string {
	return strings.ReplaceAll(query, "openrails.", pgx.Identifier{w.schema}.Sanitize()+".")
}

// Default price keys are exact per cadence: neighbouring cadences never share
// a key, so creating one never archives another; a default key held by a
// price on another cadence is refused with a typed error.
func TestCadencePriceKeys(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			product := "keys-" + string(tp)
			want := map[int]string{1: "1h", 12: "12h", 24: "1d", 36: "36h", 720: "monthly", 744: "31d"}
			ids := map[int]string{}
			for _, hours := range []int{1, 12, 24, 36, 720, 744} {
				price := w.cadencePrice(tp, product, "content:keys", 1_000_000+int64(hours), hours)
				require.Equal(t, product+"-"+want[hours], price.Key, "%dh", hours)
				ids[hours] = price.ID
			}
			for hours, id := range ids {
				price, err := w.client[tp].Prices.Retrieve(t.Context(), id)
				require.NoError(t, err)
				require.False(t, price.Archived, "%dh price stays current", hours)
				current, err := w.client[tp].Prices.RetrieveByKey(t.Context(), product+"-"+want[hours])
				require.NoError(t, err)
				require.Equal(t, id, current.ID, "%dh key names its own price", hours)
			}

			products, err := w.client[tp].Products.RetrieveByKey(t.Context(), product)
			require.NoError(t, err)
			explicit := 36
			held, err := w.client[tp].Prices.Create(t.Context(), &openrails.PriceCreateParams{ProductID: products.ID, Key: product + "-2d", UnitAmount: 5_000_000, Currency: "USD", AutoRenew: true, AccessDurationHours: &explicit})
			require.NoError(t, err)
			twoDays := 48
			_, err = w.client[tp].Prices.Create(t.Context(), &openrails.PriceCreateParams{ProductID: products.ID, UnitAmount: 6_000_000, Currency: "USD", AutoRenew: true, AccessDurationHours: &twoDays})
			require.ErrorIs(t, err, openrails.ErrPriceKeyCadenceConflict)
			require.ErrorIs(t, err, openrails.ErrConflict)
			var status *openrails.StatusError
			if errors.As(err, &status) {
				require.Equal(t, "price_key_cadence_conflict", status.Code)
			}
			still, err := w.client[tp].Prices.RetrieveByKey(t.Context(), product+"-2d")
			require.NoError(t, err)
			require.Equal(t, held.ID, still.ID)
			require.False(t, still.Archived)
		})
	}

	// The schema trigger's label is the Go label for every cadence up to a
	// leap year.
	rows, err := w.pool.Query(t.Context(), w.sql(`SELECT h, openrails.price_interval_label(h, true), openrails.price_interval_label(h, false) FROM generate_series(1, 8784) h`))
	require.NoError(t, err)
	seen := map[string]int{}
	for rows.Next() {
		var hours int
		var renew, once string
		require.NoError(t, rows.Scan(&hours, &renew, &once))
		require.Equal(t, cadence.PriceIntervalLabel(&hours, true), renew, "%dh", hours)
		require.Equal(t, "onetime", once)
		prev, dup := seen[renew]
		require.False(t, dup, "%dh and %dh share label %q", prev, hours, renew)
		seen[renew] = hours
	}
	require.NoError(t, rows.Err())
}

// renewedReceipts counts the customer's renewal receipts through /v1/me.
func (c *customer) renewedReceipts() []map[string]any {
	c.w.t.Helper()
	out := c.must(http.MethodGet, "/notifications?limit=100", "", nil)
	items, _ := out["data"].([]any)
	var receipts []map[string]any
	for _, item := range items {
		n := item.(map[string]any)
		if n["event_type"] == "premium_renewed" {
			receipts = append(receipts, n)
		}
	}
	return receipts
}

// Renewal receipts are spaced by the merchant's policy (default: one per
// subscription per day). Hourly and daily members get at most one per day;
// a monthly member gets exactly one per renewal.
func TestCadenceRenewalReceipts(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		hours, renewals, receipts int
	}{
		{hours: 1, renewals: 49, receipts: 2},
		{hours: 24, renewals: 3, receipts: 3},
		{hours: 720, renewals: 3, receipts: 3},
	} {
		t.Run(fmt.Sprintf("%dh", row.hours), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			price := w.cadencePrice(embedded, "receipts", "content:receipts", 1_990_000, row.hours)
			c := w.newCustomer()
			sub := c.subscribe(embedded, "stripe", price.ID, "content:receipts", c.saveCard("stripe", visa))
			started := *w.subscription(embedded, sub).CurrentPeriodStartsAt
			for i := 1; i <= row.renewals; i++ {
				end := *w.subscription(embedded, sub).CurrentPeriodEndsAt
				w.advance(end.Sub(w.clock.Now()) + time.Second)
				w.runRenewals()
				require.True(t, w.subscription(embedded, sub).CurrentPeriodEndsAt.After(end), "renewal %d applied", i)
			}
			require.Len(t, completed(w.payments(embedded, c.id)), row.renewals+1, "every renewal charged")
			receipts := c.renewedReceipts()
			require.Equal(t, row.receipts, len(receipts), "renewal receipts")
			perDay := map[int]int{}
			for _, r := range receipts {
				data := r["data"].(map[string]any)
				require.Equal(t, sub.String(), data["subscription_id"])
				start, err := time.Parse(time.RFC3339Nano, data["period_start"].(string))
				require.NoError(t, err)
				perDay[int(start.Sub(started)/(24*time.Hour))]++
			}
			for day, n := range perDay {
				if row.hours < 720 {
					require.LessOrEqual(t, n, 1, "day %d", day)
				}
			}
		})
	}
}

// staffJSON posts a JSON body to a merchant route with the staff credential.
func (w *world) staffJSON(method, path string, body any) (int, map[string]any) {
	w.t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(w.t, err)
	req, err := http.NewRequestWithContext(w.t.Context(), method, w.server.URL+mountPrefix+path, bytes.NewReader(raw))
	require.NoError(w.t, err)
	req.Header.Set("Authorization", "Bearer "+w.auth.token(w.t, "staff"))
	req.Header.Set("X-OpenRails-Merchant-Slug", w.slug)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	require.NoError(w.t, err)
	defer res.Body.Close()
	out, err := io.ReadAll(res.Body)
	require.NoError(w.t, err)
	var decoded map[string]any
	require.NoError(w.t, json.Unmarshal(out, &decoded), "%s", out)
	return res.StatusCode, decoded
}

func number(t *testing.T, v any) int64 {
	t.Helper()
	switch n := v.(type) {
	case float64:
		return int64(n)
	case string:
		var out int64
		_, err := fmt.Sscan(n, &out)
		require.NoError(t, err)
		return out
	}
	t.Fatalf("not a number: %T %v", v, v)
	return 0
}

// The dashboard labels each cadence exactly and its MRR equals the fleet MRR
// after every cadence is added.
func TestCadenceMetrics(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	type cadenceRow struct {
		hours  int
		label  string
		amount int64
	}
	rows := []cadenceRow{
		{1, "hourly", 10_000},
		{24, "daily", 1_000_000},
		{168, "weekly", 2_300_000},
		{720, "monthly", 10_000_000},
		{8760, "annual", 96_000_000},
	}
	var expected int64
	for _, row := range rows {
		price := w.cadencePrice(embedded, fmt.Sprintf("metrics-%d", row.hours), "content:metrics", row.amount, row.hours)
		c := w.newCustomer()
		c.subscribe(embedded, "stripe", price.ID, "content:metrics", c.saveCard("stripe", visa))

		var norm int64
		require.NoError(t, w.pool.QueryRow(t.Context(), w.sql(`SELECT openrails.monthly_normalized_amount($1, $2)`), row.amount, row.hours).Scan(&norm))
		expected += norm

		status, res := w.staffJSON(http.MethodPost, "/v1/merchant/metrics/query", map[string]any{
			"measures": []string{"mrr", "subscriptions"}, "by": []string{"billing_cycle"},
			"filters": map[string][]string{"status": {"active"}}, "range": map[string]string{"last": "1d"},
		})
		require.Equal(t, http.StatusOK, status, "%v", res)
		columns := map[string]int{}
		for i, col := range res["columns"].([]any) {
			columns[col.(map[string]any)["name"].(string)] = i
		}
		byCycle := map[string]int64{}
		var dashboard int64
		for _, r := range res["rows"].([]any) {
			cells := r.([]any)
			mrr := number(t, cells[columns["mrr"]])
			byCycle[cells[columns["billing_cycle"]].(string)] += mrr
			dashboard += mrr
		}
		require.Equal(t, norm, byCycle[row.label], "%dh is %s with its normalised MRR: %v", row.hours, row.label, byCycle)

		var fleet int64
		require.NoError(t, w.pool.QueryRow(t.Context(), w.sql(`SELECT monthly_amount FROM openrails.fleet_mrr_by_currency(NULL) WHERE currency = 'USD'`)).Scan(&fleet))
		require.Equal(t, expected, dashboard, "dashboard MRR after %dh", row.hours)
		require.Equal(t, dashboard, fleet, "fleet MRR equals dashboard MRR after %dh", row.hours)
	}
}

// Sub-two-day periods are shown with their time and zone; longer periods by
// date. Sub-day remaining time is in hours.
func TestCadenceNoticeText(t *testing.T) {
	start := time.Date(2026, 9, 23, 14, 0, 0, 0, time.UTC)
	hourly := subscriptions.RenderSubscriptionRenewalEmail("Store", subscriptions.SubscriptionEmailData{SubscriptionID: uuid.New(), PeriodStart: start, PeriodEnd: start.Add(time.Hour)})
	require.Contains(t, hourly.Plain, "New Period: Sep 23, 2026 14:00 UTC to Sep 23, 2026 15:00 UTC")
	require.Contains(t, hourly.HTML, "Sep 23, 2026 14:00 UTC to Sep 23, 2026 15:00 UTC")
	confirmed := subscriptions.RenderSubscriptionConfirmationEmail("Store", subscriptions.SubscriptionEmailData{PeriodStart: start, PeriodEnd: start.Add(time.Hour)})
	require.Contains(t, confirmed.Plain, "Current Period: Sep 23, 2026 14:00 UTC to Sep 23, 2026 15:00 UTC")

	monthly := subscriptions.RenderSubscriptionRenewalEmail("Store", subscriptions.SubscriptionEmailData{PeriodStart: start, PeriodEnd: start.Add(720 * time.Hour)})
	require.Contains(t, monthly.Plain, "New Period: Sep 23, 2026 to Oct 23, 2026")
	require.NotContains(t, monthly.Plain, "UTC")

	require.Equal(t, "5 hours", cadence.FormatRemaining(5*time.Hour+10*time.Minute))
	require.Equal(t, "1 hour", cadence.FormatRemaining(time.Hour))
	require.Equal(t, "30 minutes", cadence.FormatRemaining(30*time.Minute))
	require.Equal(t, "3 days", cadence.FormatRemaining(80*time.Hour))
}
