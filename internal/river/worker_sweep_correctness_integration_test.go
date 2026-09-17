//go:build integration

package riverjobs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/riverqueue/river/rivertype"
	"github.com/sendgrid/rest"
	"github.com/sendgrid/sendgrid-go"
	"github.com/stretchr/testify/require"
)

func seedSweepMerchants(t *testing.T, count int) (*db.DB, []uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	database := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	admin := dbtest.SharedSuperuserPGXPool(t)
	prefix := uuid.NewString()[:8]
	ids := make([]uuid.UUID, count)
	for i := range ids {
		ids[i] = uuid.MustParse(fmt.Sprintf("%s-0000-4000-8000-%012x", prefix, i+1))
	}
	_, err := admin.Exec(ctx, `INSERT INTO openrails.merchants(id,slug,status) SELECT id,'sweep-'||id::text,'active' FROM unnest($1::uuid[]) AS id`, ids)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, `DELETE FROM openrails.reconciliation_findings WHERE merchant_id=ANY($1::uuid[])`, ids)
		_, _ = admin.Exec(ctx, `DELETE FROM openrails.psps WHERE merchant_id=ANY($1::uuid[])`, ids)
		_, _ = admin.Exec(ctx, `DELETE FROM openrails.merchants WHERE id=ANY($1::uuid[])`, ids)
	})
	_, err = admin.Exec(ctx, `INSERT INTO openrails.psps(merchant_id,rail,environment,account_id) SELECT id,'stripe','test','acct_'||id::text FROM unnest($1::uuid[]) AS id`, ids)
	require.NoError(t, err)
	return database, ids
}

type sweepRails struct {
	railresolve.Source
	database *db.DB
	seen     map[uuid.UUID]int
	failures map[uuid.UUID]string
}

func (r *sweepRails) Armed(ctx context.Context, rail string) (bool, error) {
	id, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	if rail != string(models.RailStripe) {
		return false, nil
	}
	r.seen[id.UUID()]++
	if query := r.failures[id.UUID()]; query != "" {
		_, err := r.database.Qx(ctx).Exec(ctx, query)
		return false, err
	}
	return false, nil
}

func TestCatalogSweepCoverageAndFailureHealth(t *testing.T) {
	database, ids := seedSweepMerchants(t, 1001)
	ctx := context.Background()
	admin := dbtest.SharedSuperuserPGXPool(t)
	_, err := admin.Exec(ctx, `INSERT INTO openrails.reconciliation_findings(merchant_id,finding_type,subject_key,severity,status,rail,psp_id,openrails_resource_type,openrails_resource_id,external_resource_id)
 SELECT p.merchant_id,'catalog.orphan_in_stripe',jsonb_build_array(p.id::text,'product',p.merchant_id::text,'product_'||p.merchant_id::text,'')::text,'low','reconcile_required','stripe',p.id,'product',p.merchant_id::text,'product_'||p.merchant_id::text
   FROM openrails.psps p WHERE p.merchant_id=ANY($1::uuid[])`, ids)
	require.NoError(t, err)
	source := &sweepRails{database: database, seen: map[uuid.UUID]int{}, failures: map[uuid.UUID]string{
		ids[0]: "SELECT 1/0", // Earlier nonstructural PG error must not mask later drift.
		ids[1]: "SELECT missing_sweep_column FROM openrails.products",
	}}
	worker := CatalogReconciliationPullWorker{DB: database, Config: &config.Config{}, Rails: source}
	job := &rivertype.JobRow{Kind: KindCatalogReconciliationPull}
	health := &WorkerHealthMiddleware{DB: database}
	err = NewStructuralFailureMiddleware().Work(ctx, job, func(ctx context.Context) error {
		return health.Work(ctx, job, func(ctx context.Context) error { return worker.Work(ctx, nil) })
	})
	var refusal *StructuralFailureError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, StructuralReasonSchemaDrift, refusal.Reason)
	for _, id := range ids {
		require.Equal(t, 1, source.seen[id], "merchant %s must be visited once", id)
	}
	var resolved int
	require.NoError(t, admin.QueryRow(ctx, `SELECT count(*) FROM openrails.catalog_drift_events WHERE merchant_id=ANY($1::uuid[]) AND resolved_at IS NOT NULL`, ids).Scan(&resolved))
	require.Zero(t, resolved, "an unarmed or failed account read is never absence proof")
	var lastSuccess *time.Time
	var failures int
	require.NoError(t, admin.QueryRow(ctx, `SELECT last_success_at,consecutive_failures FROM openrails.worker_state WHERE worker_kind=$1`, KindCatalogReconciliationPull).Scan(&lastSuccess, &failures))
	require.Nil(t, lastSuccess)
	require.Equal(t, 1, failures)
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, `DELETE FROM openrails.worker_state WHERE worker_kind=$1`, KindCatalogReconciliationPull)
	})
}

type sweepSecrets struct {
	merchants.MerchantSecretStore
	seen map[merchant.ID]bool
	fail merchant.ID
}

func (s *sweepSecrets) Get(ctx context.Context, id merchant.ID, name string) (merchants.Secret, error) {
	s.seen[id] = true
	if id == s.fail {
		return merchants.Secret{}, fmt.Errorf("controlled secret provider outage")
	}
	return s.MerchantSecretStore.Get(ctx, id, name)
}
func TestStripeWebhookSweepCoverageAndFailure(t *testing.T) {
	database, ids := seedSweepMerchants(t, 1001)
	secrets := &sweepSecrets{MerchantSecretStore: merchants.NewMemorySecretStore(), seen: map[merchant.ID]bool{}, fail: merchant.ID(ids[0])}
	service, err := merchants.NewService(db.WrapPool(database.Pool(), ""), secrets, "test")
	require.NoError(t, err)
	worker := StripeWebhookReconcileWorker{DB: database, Config: &config.Config{APIURL: "https://billing.example.test", ProviderWriteMode: config.ProviderWriteModeFull}, Merchants: service}
	ctx := context.Background()
	err = (&WorkerHealthMiddleware{DB: database}).Work(ctx, &rivertype.JobRow{Kind: KindStripeWebhookReconcile}, func(ctx context.Context) error { return worker.Work(ctx, nil) })
	require.ErrorContains(t, err, "controlled secret provider outage")
	admin := dbtest.SharedSuperuserPGXPool(t)
	var lastSuccess *time.Time
	var failures int
	require.NoError(t, admin.QueryRow(ctx, `SELECT last_success_at,consecutive_failures FROM openrails.worker_state WHERE worker_kind=$1`, KindStripeWebhookReconcile).Scan(&lastSuccess, &failures))
	require.Nil(t, lastSuccess)
	require.Equal(t, 1, failures)
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, `DELETE FROM openrails.worker_state WHERE worker_kind=$1`, KindStripeWebhookReconcile)
	})
	for _, id := range ids {
		require.True(t, secrets.seen[merchant.ID(id)], "merchant %s must be reached beyond the first page", id)
	}
}

type sweepTransport func(*http.Request) (*http.Response, error)

func (f sweepTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNotificationSweepPoisonPageDoesNotStarveReceipt(t *testing.T) {
	ctx := context.Background()
	database, merchants := seedSweepMerchants(t, 1)
	mid := merchants[0]
	mctx := merchant.WithID(ctx, merchant.ID(mid))
	store := merchantconfig.NewStore(database)
	require.NoError(t, store.Upsert(mctx, models.MerchantConfiguration{Profile: models.MerchantProfileConfiguration{FromEmail: "sender@example.test", DisplayName: "Test"}}))
	email, err := subscriptions.NewEmailService(&config.SendGridConfig{APIKey: "test-only-key"}, store)
	require.NoError(t, err)
	sent := 0
	original := sendgrid.DefaultClient
	sendgrid.DefaultClient = &rest.Client{HTTPClient: &http.Client{Transport: sweepTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.sendgrid.com" {
			return nil, fmt.Errorf("unexpected test destination %s", r.URL.Host)
		}
		sent++
		return &http.Response{StatusCode: http.StatusAccepted, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}")), Request: r}, nil
	})}}
	t.Cleanup(func() { sendgrid.DefaultClient = original })
	var customer uuid.UUID
	prefix := uuid.NewString()[:8]
	ids := make([]uuid.UUID, 201)
	for i := range ids {
		ids[i] = uuid.MustParse(fmt.Sprintf("%s-0000-4000-8000-%012x", prefix, i+1))
	}
	require.NoError(t, database.RunInMerchantConn(mctx, func(ctx context.Context) error {
		customer = dbtest.EnsureCustomerIDPgxFor(ctx, t, database.Qx(ctx), mid, uuid.NewString())
		_, err := database.Qx(ctx).Exec(ctx, `INSERT INTO openrails.notifications(id,merchant_id,customer_id,event_type,data,created_at)
 SELECT id,$2,$3,'one_off_purchase_completed',jsonb_build_object('user_email','buyer@example.test','amount',CASE WHEN id=$4 THEN '1000000' ELSE 'invalid' END,'currency','USD'),$5
 FROM unnest($1::uuid[]) AS id`, ids, mid, customer, ids[200], time.Now().Add(-time.Hour))
		return err
	}))
	t.Cleanup(func() {
		_ = database.RunInMerchantConn(mctx, func(ctx context.Context) error {
			_, err := database.Qx(ctx).Exec(ctx, `DELETE FROM openrails.notifications WHERE id=ANY($1::uuid[])`, ids)
			if err != nil {
				return err
			}
			_, err = database.Qx(ctx).Exec(ctx, `DELETE FROM openrails.customers WHERE id=$1`, customer)
			if err != nil {
				return err
			}
			_, err = database.Qx(ctx).Exec(ctx, `DELETE FROM openrails.merchant_configurations WHERE merchant_id=$1`, mid)
			return err
		})
	})
	worker := NotificationEmailSweepWorker{DB: database, Notifications: subscriptions.NewNotificationService(database, email)}
	for range 2 {
		require.Error(t, worker.Work(ctx, nil))
		require.Equal(t, 1, sent, "valid receipt after 200 bad rows must be sent exactly once")
	}
	require.NoError(t, database.RunInMerchantConn(mctx, func(ctx context.Context) error {
		var delivered, pending int
		err := database.Qx(ctx).QueryRow(ctx, `SELECT count(*) FILTER(WHERE emailed_at IS NOT NULL),count(*) FILTER(WHERE emailed_at IS NULL) FROM openrails.notifications WHERE id=ANY($1::uuid[])`, ids).Scan(&delivered, &pending)
		require.NoError(t, err)
		require.Equal(t, 1, delivered)
		require.Equal(t, 200, pending, "failed rows remain visible and retryable")
		return nil
	}))
}
