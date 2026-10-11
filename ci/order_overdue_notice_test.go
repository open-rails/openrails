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

// An overdue notice queued before the repair named its order by a bare id,
// which no reader decodes: the migration names it ord_<uuid>, and the
// customer reads their notices again.
func TestOverdueNoticeOrderIDRepair(t *testing.T) {
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
		if strings.HasSuffix(name, "_order_overdue_notice_ids.up.sql") {
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
	slug := "overdue-" + uuid.NewString()[:8]
	var merchantID uuid.UUID
	require.NoError(t, pool.QueryRow(t.Context(), `INSERT INTO `+table("merchants")+` (slug, status) VALUES ($1, 'active') RETURNING id`, slug).Scan(&merchantID))
	customer := uuid.New()
	_, err = pool.Exec(t.Context(), `INSERT INTO `+table("customers")+` (merchant_id, id) VALUES ($1, $2)`, merchantID, customer)
	require.NoError(t, err)
	bare, typed := uuid.New(), uuid.New()
	for _, orderID := range []string{bare.String(), billing.OrderID(typed).String()} {
		_, err := pool.Exec(t.Context(), `INSERT INTO `+table("notifications")+` (merchant_id, customer_id, event_type, data, emailed_at)
			VALUES ($1, $2, 'order_overdue', jsonb_build_object('order_id', $3::text, 'amount_due', '50000000', 'currency', 'USD'), now())`,
			merchantID, customer, orderID)
		require.NoError(t, err)
	}

	bill, _ := f.runtimeWithStripe(t, slug, nil)
	auth := &authtest.Fake{}
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, bill, openrails.Routes{Auth: auth}))
	req := httptest.NewRequest(http.MethodGet, "/v1/me/notifications", nil)
	req.Header.Set("Authorization", "Bearer "+auth.Person(customer.String()))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var page billing.ListPage[billing.Notification]
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page))
	var orders []billing.OrderID
	for _, n := range page.Items {
		require.Equal(t, "order_overdue", n.EventType)
		orders = append(orders, n.Data.OrderID)
	}
	require.ElementsMatch(t, []billing.OrderID{billing.OrderID(bare), billing.OrderID(typed)}, orders)
}
