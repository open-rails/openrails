//go:build integration

package merchants_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/bootstrap"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	vaultint "github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/integrations/vault/vaulttest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/merchantsecrets"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

type purgeAllowed struct{}

func (purgeAllowed) AllowDestructive(context.Context, uuid.UUID) (bool, string) { return true, "test" }

type vaultPurgeFixture struct {
	pool          *db.Pool
	database      *db.DB
	store         *merchantsecrets.Store
	service       *merchants.Service
	id            merchant.ID
	slug          string
	root          *vaultint.KVv2Adapter
	cfg           *config.Config
	failDeletes   atomic.Bool
	blockWrite    atomic.Bool
	writeReached  chan struct{}
	inventoryRead chan struct{}
	inventoryOnce sync.Once
	releaseWrite  chan struct{}
	once          sync.Once
}

func newVaultPurgeFixture(t *testing.T) *vaultPurgeFixture {
	t.Helper()
	ctx := context.Background()
	addr, token := vaulttest.Addr(t)
	target, err := url.Parse(addr)
	require.NoError(t, err)
	f := &vaultPurgeFixture{writeReached: make(chan struct{}), inventoryRead: make(chan struct{}), releaseWrite: make(chan struct{})}
	proxy := httputil.NewSingleHostReverseProxy(target)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.failDeletes.Load() && r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/metadata/") {
			http.Error(w, "controlled delete outage", http.StatusServiceUnavailable)
			return
		}
		if f.blockWrite.Load() && (r.Method == http.MethodPost || r.Method == http.MethodPut) && strings.Contains(r.URL.Path, "/data/") {
			f.once.Do(func() { close(f.writeReached) })
			select {
			case <-f.releaseWrite:
			case <-r.Context().Done():
				return
			}
		}
		proxy.ServeHTTP(w, r)
		if f.blockWrite.Load() && (r.Method == "LIST" || r.URL.Query().Get("list") == "true") && strings.Contains(r.URL.Path, "/metadata/") {
			f.inventoryOnce.Do(func() { close(f.inventoryRead) })
		}
	}))
	t.Cleanup(server.Close)
	// A one-connection app pool proves a request-pinned Vault write does not
	// acquire another pool connection and deadlock itself.
	pc, err := pgxpool.ParseConfig(dbtest.SharedPostgresDSN(t))
	require.NoError(t, err)
	pc.MaxConns = 1
	raw, err := pgxpool.NewWithConfig(ctx, pc)
	require.NoError(t, err)
	t.Cleanup(raw.Close)
	f.pool = db.WrapPool(raw, config.DefaultSchema)
	f.database, err = db.NewWithPGXPool(raw, config.DefaultSchema)
	require.NoError(t, err)
	f.id = merchant.ID(uuid.New())
	f.slug = "vault-purge-" + uuid.NewString()[:8]
	_, err = f.pool.Exec(ctx, `INSERT INTO openrails.merchants(id,slug,status) VALUES($1,$2,'active')`, f.id.UUID(), f.slug)
	require.NoError(t, err)
	f.cfg = &config.Config{Env: "production", MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendVault, Vault: &config.VaultConfig{Enabled: true, Address: server.URL, AuthMethod: "token", Token: token}}
	f.store, err = merchantsecrets.Build(ctx, f.cfg, f.pool)
	require.NoError(t, err)
	f.service, err = merchants.NewService(f.pool, f.store.Secrets, "test")
	require.NoError(t, err)
	f.service.WithDestructivePolicy(purgeAllowed{})
	f.root = vaultint.NewKVv2Adapter(vaulttest.RootClient(t), "secret")
	t.Cleanup(func() {
		names, _ := f.store.Secrets.List(context.Background(), f.id)
		for _, name := range names {
			_ = f.root.DeleteSecret(context.Background(), "secret/openrails/merchants/"+f.id.String()+"/"+name)
		}
	})
	return f
}

func TestVaultUUIDPathsSurviveRenameAndReclaim(t *testing.T) {
	f := newVaultPurgeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	manifest := bootstrap.MerchantConfig{Custodians: map[string]bootstrap.CustodianConfig{
		"vault-test": {"basistheory": bootstrap.CustodianAccountConfig{AccountID: "bt-tenant-test", Secrets: map[string]string{"api_key": "private-bootstrap-key"}}},
	}}
	require.NoError(t, f.database.RunInMerchantScope(ctx, f.id, "bootstrap seed", func(ctx context.Context) error {
		return bootstrap.SeedMerchantManifestSecretPlane(ctx, f.cfg, f.id, manifest, f.store.Secrets, nil)
	}))
	name, err := merchants.PSPSecretName("stripe", "test", "acct_uuid", "webhook_signing_secret")
	require.NoError(t, err)
	require.NoError(t, f.database.RunInMerchantScope(ctx, f.id, "test write", func(ctx context.Context) error {
		_, err := f.service.RotateCredential(ctx, f.id, name, "whsec_original")
		return err
	}))
	old := f.slug
	_, err = f.pool.Exec(ctx, `UPDATE openrails.merchants SET slug=$2 WHERE id=$1`, f.id.UUID(), old+"-renamed")
	require.NoError(t, err)
	second := merchant.ID(uuid.New())
	_, err = f.pool.Exec(ctx, `INSERT INTO openrails.merchants(id,slug,status) VALUES($1,$2,'active')`, second.UUID(), old)
	require.NoError(t, err)
	restarted, err := merchantsecrets.Build(ctx, f.cfg, f.pool)
	require.NoError(t, err)
	got, err := restarted.Secrets.Get(ctx, f.id, name)
	require.NoError(t, err)
	require.Equal(t, "whsec_original", got.Value)
	_, err = restarted.Secrets.Get(ctx, second, name)
	require.ErrorIs(t, err, merchants.ErrSecretNotFound)
	data, _, err := f.root.ReadSecret(ctx, "secret/openrails/merchants/"+f.id.String()+"/"+name)
	require.NoError(t, err)
	require.Equal(t, "whsec_original", data["value"])
}

func TestVaultPurgeCleanupSurvivesFailureAndRestart(t *testing.T) {
	f := newVaultPurgeFixture(t)
	ctx := context.Background()
	name, _ := merchants.PSPSecretName("stripe", "test", "acct_purge", "webhook_signing_secret")
	_, err := f.store.Secrets.Put(ctx, f.id, name, "whsec_one")
	require.NoError(t, err)
	_, err = f.store.Secrets.Put(ctx, f.id, name, "whsec_two")
	require.NoError(t, err)
	inventory, err := f.service.TakePurgeInventory(ctx, f.id)
	require.NoError(t, err)
	f.failDeletes.Store(true)
	err = f.service.Delete(ctx, f.id, merchants.DeleteOptions{ConfirmPhrase: merchants.PurgeConfirmPhrase(f.slug), ExpectRows: &inventory.TotalRows, Actor: "test"})
	require.ErrorIs(t, err, merchants.ErrSecretCleanupPending)
	admin := dbtest.SharedSuperuserPGXPool(t)
	var run uuid.UUID
	var status string
	var finished *time.Time
	require.NoError(t, admin.QueryRow(ctx, `SELECT id,status,finished_at FROM openrails.destructive_runs WHERE merchant_id=$1 AND kind='merchant_purge'`, f.id.UUID()).Scan(&run, &status, &finished))
	require.Equal(t, "failed", status)
	require.Nil(t, finished)
	var deleted bool
	require.NoError(t, admin.QueryRow(ctx, `SELECT deleted_at IS NOT NULL FROM openrails.merchants WHERE id=$1`, f.id.UUID()).Scan(&deleted))
	require.True(t, deleted)
	data, version, err := f.root.ReadSecret(ctx, "secret/openrails/merchants/"+f.id.String()+"/"+name)
	require.NoError(t, err)
	require.Equal(t, "whsec_two", data["value"])
	require.Equal(t, 2, version)
	// A different configured backend must not make this cleanup appear complete.
	wrong, err := merchants.NewService(f.pool, merchants.NewMemorySecretStore(), "test")
	require.NoError(t, err)
	require.ErrorContains(t, wrong.RetrySecretCleanup(ctx, f.id, run), "no longer matches")
	f.failDeletes.Store(false)
	restarted, err := merchantsecrets.Build(ctx, f.cfg, f.pool)
	require.NoError(t, err)
	service, err := merchants.NewService(f.pool, restarted.Secrets, "test")
	require.NoError(t, err)
	worker := riverjobs.MerchantSecretCleanupWorker{DB: f.database, Merchants: service}
	require.NoError(t, worker.Work(ctx, nil))
	require.NoError(t, admin.QueryRow(ctx, `SELECT status,finished_at FROM openrails.destructive_runs WHERE id=$1`, run).Scan(&status, &finished))
	require.Equal(t, "completed", status)
	require.NotNil(t, finished)
	metadata, err := vaulttest.RootClient(t).Logical().ReadWithContext(ctx, "secret/metadata/openrails/merchants/"+f.id.String()+"/"+name)
	require.NoError(t, err)
	require.Nil(t, metadata, "all versions and metadata must be destroyed")
	_, err = restarted.Secrets.Put(ctx, f.id, name, "whsec_late")
	require.ErrorIs(t, err, merchants.ErrMerchantNotFound)
	require.NoError(t, service.RetrySecretCleanup(ctx, f.id, run), "completed cleanup is idempotent")
}

func TestVaultPurgeIncludesWriteCommittedAfterInventory(t *testing.T) {
	f := newVaultPurgeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var release sync.Once
	defer release.Do(func() { close(f.releaseWrite) })
	inventory, err := f.service.TakePurgeInventory(ctx, f.id)
	require.NoError(t, err)
	name, _ := merchants.PSPSecretName("stripe", "test", "acct_race", "webhook_signing_secret")
	f.blockWrite.Store(true)
	writeResult := make(chan error, 1)
	go func() {
		writeResult <- f.database.RunInMerchantScope(ctx, f.id, "rotate before purge", func(ctx context.Context) error {
			_, err := f.service.RotateCredential(ctx, f.id, name, "whsec_late")
			return err
		})
	}()
	select {
	case <-f.writeReached:
	case <-ctx.Done():
		t.Fatal("Vault write was not reached")
	}
	// Use a separate service pool for the purge, as another worker/node would.
	raw, err := pgxpool.New(ctx, dbtest.SharedPostgresDSN(t))
	require.NoError(t, err)
	defer raw.Close()
	service, err := merchants.NewService(db.WrapPool(raw, ""), f.store.Secrets, "test")
	require.NoError(t, err)
	service.WithDestructivePolicy(purgeAllowed{})
	purged := make(chan error, 1)
	go func() {
		purged <- service.Delete(ctx, f.id, merchants.DeleteOptions{ConfirmPhrase: merchants.PurgeConfirmPhrase(f.slug), ExpectRows: &inventory.TotalRows})
	}()
	// Wait for the purge's inventory to read the root while the new write is
	// blocked. The retry must use a fresh listing, not only captured names.
	select {
	case <-f.inventoryRead:
	case <-ctx.Done():
		t.Fatal("purge inventory was not read")
	}
	release.Do(func() { close(f.releaseWrite) })
	require.NoError(t, <-writeResult)
	require.NoError(t, <-purged)
	names, err := f.store.Secrets.List(ctx, f.id)
	require.NoError(t, err)
	require.Empty(t, names)
	_, err = f.store.Secrets.Get(ctx, f.id, name)
	require.ErrorIs(t, err, merchants.ErrSecretNotFound, "same-node cache must be cleared")
	_, err = f.store.Secrets.Put(ctx, f.id, name, "whsec_after")
	require.ErrorIs(t, err, merchants.ErrMerchantNotFound)
}

func TestVaultCleanupCannotRunBeforeDatabaseCommit(t *testing.T) {
	f := newVaultPurgeFixture(t)
	ctx := context.Background()
	name, _ := merchants.PSPSecretName("stripe", "test", "acct_rollback", "webhook_signing_secret")
	_, err := f.store.Secrets.Put(ctx, f.id, name, "whsec_keep")
	require.NoError(t, err)
	require.ErrorIs(t, f.service.RetrySecretCleanup(ctx, f.id, uuid.New()), merchants.ErrSecretCleanupPending)
	inventory, err := f.service.TakePurgeInventory(ctx, f.id)
	require.NoError(t, err)
	// Delete the matching inventory before applying: the transaction must fail
	// without a committed cleanup run, and must leave Vault untouched.
	admin := dbtest.SharedSuperuserPGXPool(t)
	_, err = admin.Exec(ctx, `DELETE FROM openrails.merchant_purge_inventories WHERE merchant_id=$1`, f.id.UUID())
	require.NoError(t, err)
	err = f.service.Delete(ctx, f.id, merchants.DeleteOptions{ConfirmPhrase: merchants.PurgeConfirmPhrase(f.slug), ExpectRows: &inventory.TotalRows})
	var stale *merchants.ErrPurgeInventoryStale
	require.ErrorAs(t, err, &stale)
	got, err := f.store.Secrets.Get(ctx, f.id, name)
	require.NoError(t, err)
	require.Equal(t, "whsec_keep", got.Value)
	rows, err := gen.New(f.pool).ListPendingMerchantSecretCleanups(ctx, gen.ListPendingMerchantSecretCleanupsParams{PageLimit: 100})
	require.NoError(t, err)
	for _, row := range rows {
		require.NotEqual(t, f.id.UUID(), *row.MerchantID)
	}
}
