package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/payment"
	"github.com/open-rails/openrails/internal/vaulttest"
	"github.com/open-rails/openrails/server/internal/hostconfig"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

type commandFixture struct {
	ctx context.Context
	cfg *config.Config
	db  *db.DB
	mid billing.MerchantID
}

func newCommandFixture(t *testing.T) commandFixture {
	t.Helper()
	dsn := os.Getenv("OPENRAILS_E2E_DSN")
	if dsn == "" {
		t.Skip("OPENRAILS_E2E_DSN must name a disposable database")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	schema := "cli_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	authSchema := schema + "_auth"
	t.Cleanup(func() {
		for _, name := range []string{authSchema, schema} {
			_, err := pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{name}.Sanitize()+" CASCADE")
			require.NoError(t, err)
		}
		pool.Close()
	})
	cfg := &config.Config{Database: config.DatabaseConfig{Schema: schema, RiverSchema: schema},
		DB: &config.DBConfig{URL: dsn}, ProviderWriteMode: config.ProviderWriteModeReadOnly,
		Logger: &config.LoggerConfig{Level: "info"}, Redis: &config.RedisConfig{URL: ":invalid-unused-redis"}}
	require.NoError(t, engine.Migrate(t.Context(), pool, *cfg))
	database, err := db.NewWithPGXPool(pool, schema)
	require.NoError(t, err)
	directory, err := merchants.NewDirectoryService(database.DataPool())
	require.NoError(t, err)
	mid := billing.MerchantID(uuid.New())
	_, err = directory.Provision(t.Context(), merchants.ProvisionRequest{ID: mid, Slug: "fixture", PermissionGroupID: mid.String()}, nil)
	require.NoError(t, err)
	h := &hostconfig.Config{Config: cfg, Auth: authkit.Config{
		DefaultTenantID: iam.TenantID(uuid.New()),
		Token:           authkit.TokenConfig{Issuer: "https://issuer.example/" + schema},
		Keys:            authkit.KeysConfig{Path: t.TempDir(), AllowEphemeralDevKeys: true},
		Database:        authkit.DatabaseConfig{Schema: authSchema, RiverSchema: schema},
		Registration:    authkit.RegistrationConfig{AllowMissingSenders: true},
		HTTP:            &authkit.HTTPConfig{DirectPeerIP: true},
	}}
	ctx := hostconfig.NewContext(context.WithValue(t.Context(), config.ConfigContextKey, cfg), h)
	return commandFixture{ctx: ctx, cfg: cfg, db: database, mid: mid}
}

type failCommandHTTP struct{ calls *int }

func (f failCommandHTTP) RoundTrip(*http.Request) (*http.Response, error) {
	*f.calls++
	return nil, errors.New("unexpected command HTTP request")
}

func TestOneOffCommandsDoNotStartTheServer(t *testing.T) {
	f := newCommandFixture(t)
	t.Chdir(t.TempDir())
	t.Setenv("DB_URL", f.cfg.DB.URL)
	t.Setenv("VAULT_SECRETS_PATH", t.TempDir())
	for _, key := range []string{"PROVIDER_WRITE_MODE", "AUTH_TOKEN_ISSUER", "OPENRAILS_CONFIG", "BILLING_CONFIG"} {
		t.Setenv(key, "")
		require.NoError(t, os.Unsetenv(key))
	}
	configPath := filepath.Join(t.TempDir(), "database.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(fmt.Sprintf("database:\n  schema: %s\n  river_schema: %s\n", f.cfg.Database.Schema, f.cfg.Database.RiverSchema)), 0o600))
	var logs bytes.Buffer
	oldOutput, oldLevel := log.StandardLogger().Out, log.GetLevel()
	log.SetOutput(&logs)
	log.SetLevel(log.InfoLevel)
	t.Cleanup(func() { log.SetOutput(oldOutput); log.SetLevel(oldLevel) })
	calls := 0
	oldTransport := http.DefaultTransport
	http.DefaultTransport = failCommandHTTP{calls: &calls}
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	parentDeadline, parentHasDeadline := f.ctx.Deadline()
	databaseCmd := &cobra.Command{}
	databaseCmd.SetContext(f.ctx)
	require.NoError(t, withCLIDB(databaseCmd, func(ctx context.Context, database *db.DB) error {
		deadline, hasDeadline := ctx.Deadline()
		require.Equal(t, parentHasDeadline, hasDeadline)
		require.Equal(t, parentDeadline, deadline, "the ten-second connection budget does not limit the operation")
		return database.Pool().Ping(ctx)
	}))

	for _, args := range [][]string{{"merchants", "list", "--json"}, {"merchants", "get", f.mid.String()}} {
		out, err := execute(newRootCmd(), append(args, "--config", configPath)...)
		require.NoError(t, err)
		require.True(t, json.Valid([]byte(out)), out)
		require.Contains(t, out, f.mid.String())
	}
	out, err := execute(newRootCmd(), "workers", "--json", "--config", configPath)
	require.NoError(t, err)
	require.True(t, json.Valid([]byte(out)), out)

	resources, err := openCLIControlPlane(f.ctx)
	require.NoError(t, err)
	require.NoError(t, resources.loadMerchants(f.ctx))
	created, err := resources.ProvisionMerchant(f.ctx, billing.ProvisionMerchantParams{Slug: "created-from-cli"})
	require.NoError(t, err)
	renamed, err := resources.RenameMerchant(f.ctx, created.MerchantID, billing.RenameMerchantParams{Name: "renamed-from-cli"})
	require.NoError(t, err)
	require.Equal(t, "renamed-from-cli", renamed.Name)
	resources.close()
	require.Zero(t, calls, "no provider, FX, HTTP or console startup")
	require.Empty(t, logs.String(), "success prints command data, not startup logs")
}

type commandDeparture struct {
	mid   billing.MerchantID
	calls int
	err   error
}

func (d *commandDeparture) MerchantLeft(_ context.Context, mid billing.MerchantID) error {
	d.mid, d.calls = mid, d.calls+1
	return d.err
}

func TestOneOffMerchantDeletionRetainsNetworkDeparture(t *testing.T) {
	f := newCommandFixture(t)
	resources, err := openCLIControlPlane(f.ctx)
	require.NoError(t, err)
	defer resources.close()
	departure := &commandDeparture{err: errors.New("fixture pending link cleanup")}
	resources.network = departure
	_, err = resources.DeleteMerchant(f.ctx, f.mid)
	require.ErrorIs(t, err, departure.err)
	require.Equal(t, f.mid, departure.mid)
	require.Equal(t, 1, departure.calls)
	deleted, err := resources.GetMerchant(f.ctx, f.mid)
	require.NoError(t, err)
	require.Equal(t, billing.MerchantDeleted, deleted.Status)
	departure.err = nil
	_, err = resources.DeleteMerchant(f.ctx, f.mid)
	require.NoError(t, err, "retry still finishes the network departure")
	require.Equal(t, 2, departure.calls)
	restored, err := resources.RestoreMerchant(f.ctx, f.mid)
	require.NoError(t, err)
	require.Equal(t, billing.MerchantActive, restored.Status)
}

func TestOneOffDirectoryKeepsVaultDisplayNames(t *testing.T) {
	vault := vaulttest.New(t)
	f := newCommandFixture(t)
	f.cfg.Vault = vault.Config()
	vault.Put(t, "openrails/merchants/"+f.mid.String()+"/merchant", merchantdocs.Merchant{DisplayName: "Fixture display name"})
	for _, args := range [][]string{{"list", "--json"}, {"get", f.mid.String()}} {
		cmd := newMerchantsCmd()
		cmd.SetContext(f.ctx)
		out, err := execute(cmd, args...)
		require.NoError(t, err)
		require.True(t, json.Valid([]byte(out)))
		require.Contains(t, out, "Fixture display name")
	}
}

func TestOneOffSolanaReviewNeedsOnlyTheLedger(t *testing.T) {
	f := newCommandFixture(t)
	var out bytes.Buffer
	err := runSolanaPayResolve(f.ctx, f.cfg, "fixture", "unknown-signature", "reviewed", &out)
	require.ErrorIs(t, err, payment.ErrOrderAttemptNotFound)
	require.Empty(t, out.String())
}

func TestOneOffMigratePreparesTheConfiguredAccountTenant(t *testing.T) {
	f := newCommandFixture(t)
	host := hostconfig.FromContext(f.ctx)
	require.NoError(t, applyStandaloneMigrations(f.ctx, host))
	require.NoError(t, applyStandaloneMigrations(f.ctx, host), "the explicit adoption is idempotent")
	host.Auth.Database.SkipMigrations = true
	resources, err := openCLIControlPlane(f.ctx)
	require.NoError(t, err)
	t.Cleanup(resources.close)
	view, err := resources.cp.Core().ForTenant(f.ctx, host.Auth.DefaultTenantID)
	require.NoError(t, err)
	user, err := view.CreateUser(f.ctx, iam.NewUser{Username: "migration_fixture"})
	require.NoError(t, err)
	require.Equal(t, host.Auth.DefaultTenantID, user.TenantID)
}

func TestOneOffHoldClearKeepsAdmissionChecksAndEvents(t *testing.T) {
	f := newCommandFixture(t)
	runtime := &app.Runtime{DB: f.db, Config: f.cfg}
	require.NoError(t, runtime.WireOneOffDestructiveAdmissions(f.ctx))
	defer runtime.Close(context.Background())
	gate := runtime.DestructiveAdmissions
	actor := destructive.Actor{Issuer: "https://staff.example", ID: "operator", Credential: "key:fixture"}
	var trip *destructive.HeldError
	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("fixture-%d", i)
		action, err := gate.Admit(f.ctx, destructive.Command{MerchantID: f.mid.UUID(), Kind: destructive.Refund,
			Target: key, Key: key, Digest: destructive.Digest(key), Units: 1, Actor: actor})
		if errors.As(err, &trip) {
			break
		}
		require.NoError(t, err)
		_, err = gate.Start(f.ctx, f.mid.UUID(), action.ID)
		require.NoError(t, err)
		require.NoError(t, gate.Finish(f.ctx, f.mid.UUID(), action.ID, destructive.OutcomeSucceeded, "fixture"))
	}
	require.NotNil(t, trip)
	args := []string{"clear", f.mid.String(), billing.DestructiveHoldID(trip.Hold.ID).String(), "--operator-issuer", actor.Issuer, "--operator", actor.ID, "--reason", "reviewed"}
	cmd := newDestructiveHoldsCmd()
	cmd.SetContext(f.ctx)
	_, err := execute(cmd, args...)
	require.ErrorIs(t, err, destructive.ErrSelfClear)
	args[len(args)-3] = "reviewer"
	cmd = newDestructiveHoldsCmd()
	cmd.SetContext(f.ctx)
	out, err := execute(cmd, args...)
	require.NoError(t, err)
	require.True(t, json.Valid([]byte(out)), out)
	hold, err := gate.GetHold(f.ctx, f.mid.UUID(), trip.Hold.ID)
	require.NoError(t, err)
	require.NotNil(t, hold.ClearedAt)
	events, err := f.db.Gen(merchant.WithID(f.ctx, f.mid)).ListHostEvents(f.ctx, gen.ListHostEventsParams{
		MerchantID: f.mid.UUID(), EventType: string(billing.HostEventDestructiveHoldCleared), RowLimit: 10})
	require.NoError(t, err)
	require.Len(t, events, 1, "clearing retains its transactional host event")
}
