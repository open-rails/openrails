//go:build e2e && integration

package ci_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
)

// resourceSpend is the customer's USD usage report grouped by resource, as
// the customer reads it: each resource's total amount.
func resourceSpend(t *testing.T, bill *openrails.Engine, customer billing.CustomerID) map[string]int64 {
	t.Helper()
	auth := &authtest.Fake{}
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, bill, openrails.Routes{Auth: auth}))
	req := httptest.NewRequest(http.MethodGet, "/v1/me/usage?currency=USD&group_by=resource", nil)
	req.Header.Set("Authorization", "Bearer "+auth.Person(customer.String()))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var report billing.Usage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &report))
	out := map[string]int64{}
	for _, row := range report.Rows {
		out[row.Key] = row.Amount
	}
	return out
}

// A recorded usage event keeps its resource in its resource column, which
// the usage report and the metrics read; the caller's metadata stays its own.
func TestRecordedUsageKeepsItsResource(t *testing.T) {
	f := newFixture(t)
	bill, client := f.runtimeWithStripe(t, "resource-"+uuid.NewString()[:8], nil)
	ctx := t.Context()
	customer := billing.CustomerID(uuid.New())
	_, err := createCreditGrant(ctx, client, customer, billing.CreateCreditGrantParams{Currency: "USD", Amount: 1_000_000, Source: "support", SourceID: "seed"})
	require.NoError(t, err)
	usage := func(sourceID, resource string, amount int64, metadata map[string]any) billing.RecordUsageParams {
		return billing.RecordUsageParams{CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: "gpu", Amount: amount,
			Resource: resource, Metadata: metadata, Source: "worker", SourceID: sourceID}
	}
	results, err := client.RecordUsage(ctx, []billing.RecordUsageParams{
		usage("u-1", "gpu:a100", 300, nil),
		usage("u-2", "gpu:a100", 200, nil),
		usage("u-3", "  cpu  ", 100, nil),
		usage("u-4", "", 50, nil),
		usage("u-5", "gpu:h100", 400, map[string]any{"resource": "the caller's own"}),
	})
	require.NoError(t, err)
	for _, r := range results {
		require.Equal(t, http.StatusCreated, r.Status, "%+v", r.Error)
	}
	resource := func(i int) *string { return results[i].Event.Resource }
	require.Equal(t, "gpu:a100", *resource(0))
	require.NotContains(t, results[0].Event.Metadata, "resource", "the resource is not copied into metadata")
	require.Equal(t, "cpu", *resource(2), "trimmed")
	require.Nil(t, resource(3), "no resource is NULL")
	require.Equal(t, "gpu:h100", *resource(4))
	require.Equal(t, "the caller's own", results[4].Event.Metadata["resource"])

	replay, err := recordUsage(ctx, client, usage("u-1", "gpu:a100", 300, nil))
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	require.Equal(t, "gpu:a100", *replay.Resource, "a replay answers the stored resource")

	require.Equal(t, map[string]int64{"gpu:h100": 400, "gpu:a100": 500, "cpu": 100, "": 50}, resourceSpend(t, bill, customer))

	from, to := results[0].Event.OccurredAt.Add(-time.Hour).Format(time.RFC3339), results[0].Event.OccurredAt.Add(time.Hour).Format(time.RFC3339)
	metrics, err := client.QueryMetrics(ctx, billing.MetricsQuery{Measures: []string{"usage_revenue"}, By: []string{"sku"},
		Range: &billing.MetricsRange{From: from, To: to}, Filters: map[string][]string{"customer": {customer.String()}}})
	require.NoError(t, err)
	bySKU := map[string]string{}
	for _, row := range metricsRows(metrics) {
		bySKU[row["sku"]] = row["usage_revenue"]
	}
	require.Equal(t, map[string]string{"gpu:h100": "400", "gpu:a100": "500", "cpu": "100", "": "50"}, bySKU)
}

// The migration that moves a recorded event's resource from its metadata to
// its column applies over events recorded before it: a non-blank string
// resource moves, trimmed; anything else, and a column already set, is left.
func TestUsageResourceBackfill(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_DSN"))
	require.NotEmpty(t, dsn, "OPENRAILS_E2E_DSN must point at a disposable PostgreSQL database")
	pool := openPool(t, dsn)
	f := &fixture{pool: pool, schema: "e2e_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{f.schema}.Sanitize()+" CASCADE")
	})
	files, err := fs.Glob(postgresmigrations.FS, "*.up.sql")
	require.NoError(t, err)
	before := fstest.MapFS{}
	for _, name := range files {
		if strings.HasSuffix(name, "_usage_event_resource.up.sql") {
			break
		}
		body, err := fs.ReadFile(postgresmigrations.FS, name)
		require.NoError(t, err)
		before[name] = &fstest.MapFile{Data: body}
	}
	require.Less(t, len(before), len(files), "the migration is in this build's chain")
	cfg := f.config()
	cfg.DB = &openrails.DBConfig{URL: dsn}
	application, err := app.BootstrapWithOptions(t.Context(), &cfg, &app.BootstrapOptions{PGXPool: pool, Migrations: before})
	require.NoError(t, err)
	require.NoError(t, application.Close(t.Context()))

	table := func(name string) string { return pgx.Identifier{f.schema, name}.Sanitize() }
	text := func(s string) *string { return &s }
	slug := "backfill-" + uuid.NewString()[:8]
	var merchantID uuid.UUID
	require.NoError(t, pool.QueryRow(t.Context(), `INSERT INTO `+table("merchants")+` (slug, status) VALUES ($1, 'active') RETURNING id`, slug).Scan(&merchantID))
	customer := uuid.New()
	_, err = pool.Exec(t.Context(), `INSERT INTO `+table("customers")+` (merchant_id, id) VALUES ($1, $2)`, merchantID, customer)
	require.NoError(t, err)
	stored := map[string]struct {
		resource *string
		metadata string
	}{
		"moved":      {nil, `{"resource": "gpu:a100"}`},
		"trimmed":    {nil, `{"resource": "  cpu  "}`},
		"blank":      {nil, `{"resource": "   "}`},
		"not text":   {nil, `{"resource": 7}`},
		"none":       {nil, `{"tier": "pro"}`},
		"no meta":    {nil, ``},
		"column set": {text("gpu:h100"), `{"resource": "the caller's own"}`},
	}
	for sourceID, row := range stored {
		var metadata *string
		if row.metadata != "" {
			metadata = &row.metadata
		}
		_, err := pool.Exec(t.Context(), `INSERT INTO `+table("usage_events")+` (merchant_id, customer_id, invoker_id, currency, resource, event_type,
			dimensions, amount, source, source_id, pricing_authority, metadata, occurred_at, outcome, forgiven_amount, charges)
			VALUES ($1, $2, $3, 'USD', $4, 'gpu', '{}', 10, 'worker', $5, 'host', $6::jsonb, now(), 'succeeded', 0, '[]'::jsonb)`,
			merchantID, customer, customer.String(), row.resource, sourceID, metadata)
		require.NoError(t, err, sourceID)
	}

	bill, _ := f.runtimeWithStripe(t, slug, nil)
	got := map[string]*string{}
	rows, err := pool.Query(t.Context(), `SELECT source_id, resource FROM `+table("usage_events")+` WHERE merchant_id = $1`, merchantID)
	require.NoError(t, err)
	for rows.Next() {
		var sourceID string
		var resource *string
		require.NoError(t, rows.Scan(&sourceID, &resource))
		got[sourceID] = resource
	}
	require.NoError(t, rows.Err())
	require.Equal(t, map[string]*string{
		"moved": text("gpu:a100"), "trimmed": text("cpu"), "blank": nil, "not text": nil, "none": nil, "no meta": nil, "column set": text("gpu:h100"),
	}, got)
	require.Equal(t, map[string]int64{"gpu:a100": 10, "cpu": 10, "gpu:h100": 10, "": 40}, resourceSpend(t, bill, billing.CustomerID(customer)))
}
