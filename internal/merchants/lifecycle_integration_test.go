//go:build integration

package merchants

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"
	"github.com/open-rails/openrails/pkg/merchant"
)

// allowAllDestructive stands in for the #836/#835 gate in tests that are about
// merchant ROUTING and tombstoning, not about the gate. The gate's own
// behaviour on the purge path is pinned against the real migrated schema in
// purge_integration_test.go.
type allowAllDestructive struct{}

func (allowAllDestructive) AllowDestructive(context.Context, uuid.UUID) (bool, string) {
	return true, ""
}

// applyDirectoryFunctionMigration replays the REAL baseline definitions of the
// #824 cross-merchant directory functions onto this package's hand-written
// schema, so they cannot drift from what production runs. schemaDDL below is a
// deliberate minimal subset of the migrated schema; a hand-copied function body
// here would be exactly the divergence that let #824 hide behind
// routes_merchant_webhook_integration_test.go's bespoke openrails.psps.
func applyDirectoryFunctionMigration(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	// The functions GRANT to openrails_app, which only the baseline creates.
	_, err := pool.Exec(ctx, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'openrails_app') THEN
			CREATE ROLE openrails_app NOLOGIN NOBYPASSRLS;
		END IF;
	END $$;`)
	require.NoError(t, err)
	sql, err := postgresmigrations.BaselineObjects(
		"current_merchant_id",
		"assert_cross_merchant_reader",
		"psp_owner_by_identity",
		"customer_merchant_ids_for_subject",
	)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, sql)
	require.NoError(t, err)
}

// schemaDDL stands up the minimal openrails.* schema the merchant service touches:
// the merchant directory (with #225 columns), one representative merchant-owned table
// (entitlements) so export/delete have rows to purge, and the #225 control-plane
// tables. The columns under test mirror the baseline's shapes.
const schemaDDL = `
CREATE SCHEMA IF NOT EXISTS openrails;

CREATE TABLE IF NOT EXISTS openrails.merchants (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug             TEXT NOT NULL UNIQUE,
    status           TEXT NOT NULL DEFAULT 'active',
    permission_group_id  TEXT,
    display_name     TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    deleted_at       TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS openrails.entitlements (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id      UUID,
    customer_id UUID
);

CREATE TABLE IF NOT EXISTS openrails.customers (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id UUID NOT NULL,
    -- #824: the subject-first directory lookup is replayed from the baseline.
    subject     TEXT
);

CREATE TABLE IF NOT EXISTS openrails.merchant_secrets (
    merchant_id UUID NOT NULL,
    name       TEXT NOT NULL,
    value      TEXT NOT NULL,
    version    INTEGER NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    PRIMARY KEY (merchant_id, name)
);


CREATE TABLE IF NOT EXISTS openrails.custodians (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    merchant_id uuid NOT NULL,
    key text NOT NULL,
    kind text NOT NULL,
    environment text DEFAULT 'live' NOT NULL,
    account_id text NOT NULL,
    settings jsonb DEFAULT '{}'::jsonb NOT NULL,
    credential_versions jsonb DEFAULT '{}'::jsonb NOT NULL,
    archived boolean DEFAULT false NOT NULL,
    created_at timestamptz DEFAULT current_timestamp NOT NULL,
    updated_at timestamptz DEFAULT current_timestamp NOT NULL,
    PRIMARY KEY (id),
    UNIQUE (id, merchant_id),
    UNIQUE (kind, environment, account_id)
);

CREATE TABLE IF NOT EXISTS openrails.psps (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    merchant_id uuid NOT NULL,
    custodian_id uuid REFERENCES openrails.custodians(id),
    rail text NOT NULL,
    environment text DEFAULT 'live' NOT NULL,
    account_id text NOT NULL,
	key text,
    archived boolean DEFAULT false NOT NULL,
    evidence jsonb,
    first_seen_at timestamptz DEFAULT current_timestamp NOT NULL,
    last_verified_at timestamptz,
    replaced_at timestamptz,
    created_at timestamptz DEFAULT current_timestamp NOT NULL,
    updated_at timestamptz DEFAULT current_timestamp NOT NULL,
    PRIMARY KEY (id),
    UNIQUE (rail, environment, account_id)
);

CREATE TABLE IF NOT EXISTS openrails.probe_verdicts (
    rail text NOT NULL,
    key_hash text NOT NULL,
    verdict text NOT NULL,
    checked_at timestamptz DEFAULT current_timestamp NOT NULL,
    PRIMARY KEY (rail, key_hash)
);

CREATE TABLE IF NOT EXISTS openrails.subscriptions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id uuid NOT NULL,
    rail text NOT NULL,
    psp_id uuid NOT NULL,
    status text NOT NULL
);

CREATE TABLE IF NOT EXISTS openrails.payments (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id uuid NOT NULL,
    rail text NOT NULL,
    psp_id uuid,
    status text NOT NULL,
    CONSTRAINT payments_psp_required_on_rail CHECK (((psp_id IS NOT NULL) OR (rail = ANY (ARRAY['manual'::text, 'admin'::text]))))
);

CREATE TABLE IF NOT EXISTS openrails.rail_intents (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id uuid NOT NULL,
    rail text NOT NULL,
    psp_id uuid,
    custodian_id uuid,
    status text NOT NULL,
    CONSTRAINT rail_intents_addressed CHECK (((psp_id IS NOT NULL) OR (custodian_id IS NOT NULL)))
);

CREATE TABLE IF NOT EXISTS openrails.merchant_purge_inventories (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id  UUID NOT NULL,
    status       TEXT NOT NULL DEFAULT 'completed',
    manifest     JSONB,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT current_timestamp,
    completed_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS openrails.destructive_runs (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    merchant_id   UUID NOT NULL,
    psp_id        UUID,
    kind          TEXT NOT NULL,
    actor         TEXT NOT NULL,
    started_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at   TIMESTAMPTZ,
    dry_run       BOOLEAN NOT NULL DEFAULT false,
    coverage      JSONB,
    expected_rows BIGINT,
    affected      JSONB,
    reversed_at   TIMESTAMPTZ,
    reversed_by   TEXT,
    status        TEXT NOT NULL DEFAULT 'running',
    note          TEXT
);
`

func compactLifecycleSQL(sql string) string {
	return strings.Join(strings.Fields(strings.ToLower(sql)), " ")
}

func lifecycleFixtureObjectDDL(name string) string {
	marker := "CREATE TABLE IF NOT EXISTS openrails." + name + " ("
	start := strings.Index(schemaDDL, marker)
	if start < 0 {
		return ""
	}
	ddl := schemaDDL[start:]
	end := strings.Index(ddl, "\n);")
	if end < 0 {
		return ""
	}
	return ddl[:end+3]
}

func TestLifecycleFixtureProviderProvenanceMatchesBaseline(t *testing.T) {
	t.Parallel()

	cases := []struct {
		object string
		clause string
	}{
		{"subscriptions", "psp_id uuid not null"},
		{"payments", "constraint payments_psp_required_on_rail check (((psp_id is not null) or (rail = any (array['manual'::text, 'admin'::text]))))"},
		{"rail_intents", "constraint rail_intents_addressed check (((psp_id is not null) or (custodian_id is not null)))"},
	}
	for _, tc := range cases {
		t.Run(tc.object, func(t *testing.T) {
			baseline, err := postgresmigrations.BaselineObjects(tc.object)
			require.NoError(t, err)
			fixture := lifecycleFixtureObjectDDL(tc.object)
			require.NotEmpty(t, fixture, "minimal lifecycle fixture must define the object")
			require.Contains(t, compactLifecycleSQL(baseline), tc.clause, "production baseline provenance contract changed")
			require.Contains(t, compactLifecycleSQL(fixture), tc.clause, "minimal lifecycle fixture drifted from the production provenance contract")
		})
	}
}

func TestLifecycleFixtureEnforcesProviderProvenance(t *testing.T) {
	ctx := context.Background()
	pool := newTestPool(t)
	merchantID := uuid.New()

	_, err := pool.Exec(ctx, `
		INSERT INTO openrails.subscriptions (merchant_id, rail, status)
		VALUES ($1, 'nmi', 'active')
	`, merchantID)
	require.Error(t, err, "provider-bound subscriptions require a PSP")

	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.payments (merchant_id, rail, status)
		VALUES ($1, 'nmi', 'completed')
	`, merchantID)
	require.Error(t, err, "on-rail payments require a PSP")
	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.payments (merchant_id, rail, status)
		VALUES ($1, 'manual', 'completed')
	`, merchantID)
	require.NoError(t, err, "off-rail manual payments have no PSP")

	_, err = pool.Exec(ctx, `
		INSERT INTO openrails.rail_intents (merchant_id, rail, status)
		VALUES ($1, 'nmi', 'pending')
	`, merchantID)
	require.Error(t, err, "outbound intents must address a PSP or custodian")
}

func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	if dsn := strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_DSN")); dsn != "" {
		pool := newExternalTenancyTestPool(t, ctx, dsn)
		_, err := pool.Exec(ctx, schemaDDL)
		require.NoError(t, err)
		applyDirectoryFunctionMigration(t, ctx, pool)
		return pool
	}

	container, err := postgres.Run(ctx,
		"postgres:18-alpine",
		postgres.WithDatabase("openrails"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		dbtest.WithPostgresLimits(),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	require.NoError(t, err)
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	_, err = pool.Exec(ctx, schemaDDL)
	require.NoError(t, err)
	applyDirectoryFunctionMigration(t, ctx, pool)
	return pool
}

func newExternalTenancyTestPool(t *testing.T, ctx context.Context, adminDSN string) *pgxpool.Pool {
	t.Helper()
	adminCfg, err := pgxpool.ParseConfig(adminDSN)
	require.NoError(t, err)
	adminCfg.ConnConfig.Config.Database = "postgres"
	adminPool, err := pgxpool.NewWithConfig(ctx, adminCfg)
	require.NoError(t, err)

	dbName := fmt.Sprintf("openrails_tenancy_%d", time.Now().UnixNano())
	_, err = adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{dbName}.Sanitize())
	require.NoError(t, err)

	testCfg, err := pgxpool.ParseConfig(adminDSN)
	require.NoError(t, err)
	testCfg.ConnConfig.Config.Database = dbName
	pool, err := pgxpool.NewWithConfig(ctx, testCfg)
	require.NoError(t, err)

	t.Cleanup(func() {
		pool.Close()
		_, _ = adminPool.Exec(context.Background(), "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()", dbName)
		_, _ = adminPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{dbName}.Sanitize())
		adminPool.Close()
	})
	return pool
}

func newSvc(t *testing.T) *Service {
	pool := newTestPool(t)
	svc, err := NewService(db.WrapPool(pool, ""), NewMemorySecretStore(), "live")
	require.NoError(t, err)
	return svc
}

func seedPSP(t *testing.T, svc *Service, merchantID merchant.ID, rail, environment, accountID string) {
	t.Helper()
	_, err := svc.pool.Exec(context.Background(), `
		INSERT INTO openrails.psps (merchant_id, rail, environment, account_id, archived)
		VALUES ($1::uuid, lower($2), $3, $4, false)
		ON CONFLICT (rail, environment, account_id) DO NOTHING
	`, merchantID.String(), rail, environment, accountID)
	require.NoError(t, err)
}

func seedArchivedPSP(t *testing.T, svc *Service, merchantID merchant.ID, rail, environment, accountID string) {
	t.Helper()
	_, err := svc.pool.Exec(context.Background(), `
		INSERT INTO openrails.psps (merchant_id, rail, environment, account_id, archived)
		VALUES ($1::uuid, lower($2), $3, $4, true)
		ON CONFLICT (rail, environment, account_id) DO UPDATE SET archived = true
	`, merchantID.String(), rail, environment, accountID)
	require.NoError(t, err)
}

func TestArchivedPSPRejectsNewWorkButResolvesByAccountID(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t)
	tn, _, err := svc.Provision(ctx, ProvisionRequest{Slug: "archived-rail", PermissionGroupID: "group-archived-rail"})
	require.NoError(t, err)

	const accountID = "archived-nmi-account"
	seedArchivedPSP(t, svc, tn.ID, "nmi", "live", accountID)
	secretName, err := PSPSecretName("nmi", "live", accountID, "webhook_signing_secret")
	require.NoError(t, err)
	_, err = svc.PutCredential(ctx, tn.ID, secretName, "archived-webhook-secret")
	require.NoError(t, err)

	_, ok, err := svc.ActivePSPSecretName(ctx, tn.ID, "nmi", "live", "security_key")
	require.ErrorIs(t, err, ErrNoActivePSP)
	require.False(t, ok)

	got, ok, err := svc.LoadNMIWebhookSigningSecretForAccount(ctx, tn.ID, accountID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "archived-webhook-secret", got)
}

func TestArchivedPSPDrainState(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t)
	tn, _, err := svc.Provision(ctx, ProvisionRequest{Slug: "archived-drain", PermissionGroupID: "group-archived-drain"})
	require.NoError(t, err)

	const accountID = "draining-nmi-account"
	seedArchivedPSP(t, svc, tn.ID, "nmi", "live", accountID)

	items, err := svc.ListPaymentProviderConfigs(ctx, tn.ID, "nmi", "live", "archived")
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.True(t, items[0].Archived)
	require.True(t, items[0].Drained)
	require.Equal(t, int64(0), items[0].OpenObligations)

	_, err = svc.pool.Exec(ctx, `
		INSERT INTO openrails.subscriptions (merchant_id, rail, psp_id, status)
		VALUES ($1::uuid, 'nmi', $2::uuid, 'active')
	`, tn.ID.String(), items[0].ID)
	require.NoError(t, err)

	items, err = svc.ListPaymentProviderConfigs(ctx, tn.ID, "nmi", "live", "archived")
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.False(t, items[0].Drained)
	require.Equal(t, int64(1), items[0].OpenObligations)
}

func TestProvision_Idempotent(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t)

	// #500: provisioning a merchant namespace requires the caller to provide the
	// durable AuthKit permission group id. The lifecycle service does not mint it.
	req := ProvisionRequest{Slug: "acme", PermissionGroupID: "group-acme"}
	first, created, err := svc.Provision(ctx, req)
	require.NoError(t, err)
	require.True(t, created, "first provision inserts the row")
	require.Equal(t, "acme", first.Slug)
	require.Equal(t, "group-acme", first.PermissionGroupID, "explicit permission-group link recorded (never auto-minted)")

	// Re-provision: same merchant id, no duplicate row (idempotent), created=false.
	second, created, err := svc.Provision(ctx, req)
	require.NoError(t, err)
	require.False(t, created, "re-provision must not report a fresh insert")
	require.Equal(t, first.ID, second.ID)

	var count int
	require.NoError(t, svc.pool.QueryRow(ctx, `SELECT count(*) FROM openrails.merchants WHERE slug='acme'`).Scan(&count))
	require.Equal(t, 1, count, "provision must not create a duplicate merchant row")

	_, _, err = svc.Provision(ctx, ProvisionRequest{Slug: "noown"})
	require.ErrorIs(t, err, ErrPermissionGroupRequired, "control-plane provisioning must not create an ownerless merchant")
}

func TestSetDisplayName(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t)
	tn, _, err := svc.Provision(ctx, ProvisionRequest{Slug: "named", PermissionGroupID: "group-named"})
	require.NoError(t, err)

	require.NoError(t, svc.SetDisplayName(ctx, tn.ID, "  Named Merchant  "))
	var displayName *string
	require.NoError(t, svc.pool.QueryRow(ctx,
		`SELECT display_name FROM openrails.merchants WHERE id = $1::uuid`, tn.ID.String()).Scan(&displayName))
	require.NotNil(t, displayName)
	require.Equal(t, "Named Merchant", *displayName)

	require.NoError(t, svc.SetDisplayName(ctx, tn.ID, "Repaired Name"))
	require.NoError(t, svc.SetDisplayName(ctx, tn.ID, "  "))
	require.NoError(t, svc.pool.QueryRow(ctx,
		`SELECT display_name FROM openrails.merchants WHERE id = $1::uuid`, tn.ID.String()).Scan(&displayName))
	require.NotNil(t, displayName)
	require.Equal(t, "Repaired Name", *displayName)

	require.ErrorIs(t, svc.SetDisplayName(ctx, merchant.ID(uuid.New()), "Missing"), ErrMerchantNotFound)
	_, err = svc.pool.Exec(ctx,
		`UPDATE openrails.merchants SET status = 'deleted' WHERE id = $1::uuid`, tn.ID.String())
	require.NoError(t, err)
	require.ErrorIs(t, svc.SetDisplayName(ctx, tn.ID, "Deleted"), ErrMerchantNotFound)
	_, err = svc.pool.Exec(ctx,
		`UPDATE openrails.merchants SET status = 'active', deleted_at = current_timestamp WHERE id = $1::uuid`, tn.ID.String())
	require.NoError(t, err)
	require.ErrorIs(t, svc.SetDisplayName(ctx, tn.ID, "Soft Deleted"), ErrMerchantNotFound)
}

func TestListDirectoryRefs(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t)
	named, _, err := svc.Provision(ctx, ProvisionRequest{Slug: "refs-named", PermissionGroupID: "group-refs-named"})
	require.NoError(t, err)
	require.NoError(t, svc.SetDisplayName(ctx, named.ID, "Refs Named"))
	_, _, err = svc.Provision(ctx, ProvisionRequest{Slug: "refs-unnamed", PermissionGroupID: "group-refs-unnamed"})
	require.NoError(t, err)

	refs, err := svc.ListDirectoryRefs(ctx, []string{"  REFS-NAMED  ", "refs-named", "refs-unnamed", "refs-missing", ""})
	require.NoError(t, err)
	require.Equal(t, []DirectoryRef{
		{Slug: "refs-named", DisplayName: "Refs Named"},
		{Slug: "refs-unnamed", DisplayName: ""},
	}, refs, "slugs normalize and dedupe; a merchant without a name still resolves; unknown slugs are omitted")

	empty, err := svc.ListDirectoryRefs(ctx, nil)
	require.NoError(t, err)
	require.Empty(t, empty)

	oversized := make([]string, maxDirectoryRefSlugs+1)
	for i := range oversized {
		oversized[i] = fmt.Sprintf("refs-bulk-%d", i)
	}
	_, err = svc.ListDirectoryRefs(ctx, oversized)
	require.Error(t, err, "an oversized slug list must error rather than silently truncate")

	_, err = svc.pool.Exec(ctx,
		`UPDATE openrails.merchants SET deleted_at = current_timestamp WHERE id = $1::uuid`, named.ID.String())
	require.NoError(t, err)
	refs, err = svc.ListDirectoryRefs(ctx, []string{"refs-named", "refs-unnamed"})
	require.NoError(t, err)
	require.Equal(t, []DirectoryRef{{Slug: "refs-unnamed"}}, refs, "soft-deleted merchants must not surface")
}

func TestDelete_RequiresExport(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t)
	tn, _, err := svc.Provision(ctx, ProvisionRequest{Slug: "acme", PermissionGroupID: "group-acme"})
	require.NoError(t, err)

	// Seed a merchant-owned row + a secret so the purge has something to remove.
	_, err = svc.pool.Exec(ctx, `
		WITH subject AS (
			INSERT INTO openrails.customers (merchant_id)
			VALUES ($1::uuid)
			RETURNING id
		)
		INSERT INTO openrails.entitlements (merchant_id, customer_id)
		SELECT $1::uuid, id FROM subject
	`, tn.ID.String())
	require.NoError(t, err)
	_, err = svc.secrets.Put(ctx, tn.ID, "psps/stripe/live/acct_884_test/secret_key", "sk")
	require.NoError(t, err)

	svc.WithDestructivePolicy(allowAllDestructive{})

	// Unconfirmed -> refused, and the refusal states the true blast radius.
	err = svc.Delete(ctx, tn.ID, DeleteOptions{})
	var notConfirmed *ErrPurgeNotConfirmed
	require.ErrorAs(t, err, &notConfirmed)
	require.Equal(t, 1, notConfirmed.TotalRows)

	one := 1
	confirmed := DeleteOptions{ConfirmPhrase: PurgeConfirmPhrase(tn.Slug), ExpectRows: &one}

	// Confirmed but with NO purge inventory -> refused.
	err = svc.Delete(ctx, tn.ID, confirmed)
	var stale *ErrPurgeInventoryStale
	require.ErrorAs(t, err, &stale, "delete must require a matching purge inventory, got %v", err)

	// Take the inventory, then the purge proceeds.
	inv, err := svc.TakePurgeInventory(ctx, tn.ID)
	require.NoError(t, err)
	require.NotEmpty(t, inv.ID)
	require.False(t, inv.IsBackup())
	require.Equal(t, 1, inv.RowCounts["entitlements"])
	require.NotEmpty(t, inv.NotCaptured)

	require.NoError(t, svc.Delete(ctx, tn.ID, confirmed))

	var entCount int
	require.NoError(t, svc.pool.QueryRow(ctx, `SELECT count(*) FROM openrails.entitlements WHERE merchant_id=$1::uuid`, tn.ID.String()).Scan(&entCount))
	require.Equal(t, 0, entCount, "delete must purge merchant-owned rows")

	// The directory row is tombstoned (no longer resolvable as active).
	_, err = svc.Get(ctx, tn.ID)
	require.True(t, errors.Is(err, ErrMerchantNotFound))
}

func TestCredentialRotation_LoadsPSPScopedSecret(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t)
	tn, _, err := svc.Provision(ctx, ProvisionRequest{Slug: "acme", PermissionGroupID: "group-acme"})
	require.NoError(t, err)

	seedPSP(t, svc, tn.ID, "stripe", "live", "acct_test")
	secretName, err := PSPSecretName("stripe", "live", "acct_test", "secret_key")
	require.NoError(t, err)
	sec, err := svc.secrets.Put(ctx, tn.ID, secretName, "sk_1")
	require.NoError(t, err)
	require.Equal(t, 1, sec.Version)

	sec, err = svc.secrets.Put(ctx, tn.ID, secretName, "sk_2")
	require.NoError(t, err)
	require.Equal(t, 2, sec.Version)

	// Loaded credentials reflect the rotated value.
	creds, err := svc.LoadStripeCredentials(ctx, tn.ID)
	require.NoError(t, err)
	require.Equal(t, "sk_2", creds.SecretKey)

}

func TestWebhookRouting_ResolvesThenCallerVerifies(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t)
	tn, _, err := svc.Provision(ctx, ProvisionRequest{Slug: "acme", PermissionGroupID: "group-acme"})
	require.NoError(t, err)

	// Resolve by slug.
	route, err := svc.ResolveBySlug(ctx, "acme")
	require.NoError(t, err)
	require.Equal(t, tn.ID, route.MerchantID)

	// Unknown slug is unresolved (caller must reject; never default-fallback).
	_, err = svc.ResolveBySlug(ctx, "nope")
	require.True(t, errors.Is(err, ErrMerchantRouteUnresolved))

	// After resolution the caller loads THAT merchant's signing secret (the trust
	// boundary), which is namespaced to the merchant.
	seedPSP(t, svc, tn.ID, "stripe", "live", "acct_acme")
	secretName, err := PSPSecretName("stripe", "live", "acct_acme", "webhook_signing_secret")
	require.NoError(t, err)
	_, err = svc.secrets.Put(ctx, tn.ID, secretName, "whsec_acme")
	require.NoError(t, err)
	creds, err := svc.LoadStripeCredentials(ctx, route.MerchantID)
	require.NoError(t, err)
	require.Equal(t, "whsec_acme", creds.WebhookSigningSecret)

	// A deleted merchant no longer resolves.
	svc.WithDestructivePolicy(allowAllDestructive{})
	inv, err := svc.TakePurgeInventory(ctx, tn.ID)
	require.NoError(t, err)
	require.NotEmpty(t, inv.ID)
	require.NoError(t, svc.Delete(ctx, tn.ID, DeleteOptions{
		ConfirmPhrase: PurgeConfirmPhrase(tn.Slug), ExpectRows: &inv.TotalRows}))
	_, err = svc.ResolveBySlug(ctx, "acme")
	require.ErrorIs(t, err, ErrMerchantRouteUnresolved)
}
