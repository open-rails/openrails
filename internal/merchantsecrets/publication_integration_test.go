//go:build integration

package merchantsecrets

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/integrations/vault/vaulttest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

type interruptedKV struct {
	*vault.KVv2Adapter
	afterWrite func()
	loseReply  bool
}

func (k *interruptedKV) WriteSecretCAS(ctx context.Context, path string, data map[string]string, expected int) (int, error) {
	version, err := k.KVv2Adapter.WriteSecretCAS(ctx, path, data, expected)
	if err == nil && k.afterWrite != nil {
		k.afterWrite()
		k.afterWrite = nil
	}
	if err == nil && k.loseReply {
		k.loseReply = false
		return 0, errors.New("synthetic lost reply")
	}
	return version, err
}

func TestVaultPublicationRecoveryAndExactReferences(t *testing.T) {
	pool, ctx := startSecretsPostgres(t)
	id, _ := registerMerchant(t, ctx, pool, "publication")
	client := vaulttest.RootClient(t)
	kv := &interruptedKV{KVv2Adapter: vault.NewKVv2Adapter(client, "secret")}
	backend := merchants.NewVaultSecretStore("secret", kv)
	cache := merchants.NewCachedSecretStore(backend, time.Hour)
	svc, err := merchants.NewService(pool, cache, "live")
	require.NoError(t, err)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response></nm_response>`))
	}))
	defer provider.Close()
	svc.SetCredentialProbeEndpointsForIntegration(provider.URL, "")
	account := "publication-" + uuid.NewString()
	makeRequest := func(revision int64, key string) merchants.UpsertPaymentProviderConfigRequest {
		return merchants.UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: &revision, AccountID: account, Credentials: map[string]string{"security_key": key}}
	}
	read := func() merchants.Secret {
		ref, ok, err := svc.ActivePSPSecretRef(ctx, id, "nmi", "live", "security_key")
		require.NoError(t, err)
		require.True(t, ok)
		secret, err := merchants.ReadSecretRef(ctx, cache, id, ref)
		require.NoError(t, err)
		return secret
	}
	first := makeRequest(0, "synthetic-published-one")
	kv.loseReply = true
	result, err := svc.UpsertPaymentProviderConfig(ctx, id, "nmi", first)
	require.NoError(t, err)
	require.EqualValues(t, 1, result.Revision)
	require.Equal(t, "synthetic-published-one", read().Value)
	// Failure after the durable external write and before SQL publication leaves
	// the old published reference active. Retrying recovers the candidate by ID.
	second := makeRequest(1, "synthetic-published-two")
	interrupted, cancel := context.WithCancel(ctx)
	defer cancel()
	kv.afterWrite = cancel
	_, err = svc.UpsertPaymentProviderConfig(interrupted, id, "nmi", second)
	require.Error(t, err)
	require.Equal(t, "synthetic-published-one", read().Value)
	result, err = svc.UpsertPaymentProviderConfig(ctx, id, "nmi", second)
	require.NoError(t, err)
	require.EqualValues(t, 2, result.Revision)
	active := read()
	require.Equal(t, "synthetic-published-two", active.Value)
	// A newer direct backend value is unpublished; fresh/cache readers retain
	// the exact validated version, not an arbitrary newer version.
	_, err = kv.WriteSecret(ctx, "secret/openrails/merchants/"+id.String()+"/"+active.Name, map[string]string{"value": "synthetic-unpublished"})
	require.NoError(t, err)
	require.Equal(t, "synthetic-published-two", read().Value)
	conflicting := makeRequest(1, "synthetic-stale-rotation")
	_, err = svc.UpsertPaymentProviderConfig(ctx, id, "nmi", conflicting)
	require.ErrorIs(t, err, merchants.ErrCredentialOperationConflict)
	require.Equal(t, "synthetic-published-two", read().Value)
	// A known published reference still consults backend authority after warming.
	oldToken := client.Token()
	client.SetToken("invalid-owned-test-token")
	ref, ok, err := svc.ActivePSPSecretRef(ctx, id, "nmi", "live", "security_key")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = merchants.ReadSecretRef(ctx, cache, id, ref)
	require.ErrorIs(t, err, merchants.ErrSecretBackendUnavailable)
	client.SetToken(oldToken)
	// Raw durable receipts never carry submitted credential material.
	var receipt string
	require.NoError(t, pool.QueryRow(merchant.WithID(ctx, id), `SELECT string_agg(request_metadata::text||COALESCE(result::text,''),'') FROM billing.credential_publications WHERE merchant_id=$1`, id.UUID()).Scan(&receipt))
	for _, value := range []string{"synthetic-published-one", "synthetic-published-two", "synthetic-stale-rotation"} {
		require.False(t, strings.Contains(receipt, value))
	}
}

func TestEncryptedDBPublicationAndCompetingRotations(t *testing.T) {
	pool, ctx := startSecretsPostgres(t)
	id, _ := registerMerchant(t, ctx, pool, "db-publication")
	backend, err := Build(ctx, &config.Config{SecretBackend: config.SecretBackendDB, Encryption: &config.EncryptionConfig{MasterKey: testMasterKey(t)}}, pool)
	require.NoError(t, err)
	defer backend.Close()
	service, err := merchants.NewService(pool, backend.Secrets, "live")
	require.NoError(t, err)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response></nm_response>`))
	}))
	defer provider.Close()
	service.SetCredentialProbeEndpointsForIntegration(provider.URL, "")
	account := "db-publication-" + uuid.NewString()
	initial := int64(0)
	req := merchants.UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: &initial, AccountID: account, Credentials: map[string]string{"security_key": "synthetic-db-original"}}
	result, err := service.UpsertPaymentProviderConfig(ctx, id, "nmi", req)
	require.NoError(t, err)
	require.EqualValues(t, 1, result.Revision)
	result, err = service.UpsertPaymentProviderConfig(ctx, id, "nmi", req)
	require.NoError(t, err)
	require.EqualValues(t, 1, result.Revision)
	revision := int64(1)
	results := make(chan error, 2)
	for _, value := range []string{"synthetic-db-rotation-a", "synthetic-db-rotation-b"} {
		go func(value string) {
			_, err := service.UpsertPaymentProviderConfig(ctx, id, "nmi", merchants.UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: &revision, AccountID: account, Credentials: map[string]string{"security_key": value}})
			results <- err
		}(value)
	}
	successes, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			successes++
		} else {
			require.ErrorIs(t, err, merchants.ErrCredentialOperationConflict)
			conflicts++
		}
	}
	require.Equal(t, 1, successes)
	require.Equal(t, 1, conflicts)
	var persisted string
	require.NoError(t, pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT string_agg(value,'') FROM billing.merchant_secrets WHERE merchant_id=$1`, id.UUID()).Scan(&persisted)
	}))
	require.NotContains(t, persisted, "synthetic-db-")
	other, _ := registerMerchant(t, ctx, pool, "db-cross-merchant")
	req.OperationID = uuid.New()
	_, err = service.UpsertPaymentProviderConfig(ctx, other, "nmi", req)
	require.Error(t, err)
}

func TestSnapshotNonPersistenceAndBorrowedVaultLifecycle(t *testing.T) {
	pool, ctx := startSecretsPostgres(t)
	id, _ := registerMerchant(t, ctx, pool, "snapshot-publication")
	snapshot := merchants.NewManifestSecretStore()
	name, _ := merchants.PSPSecretName("nmi", "live", "snapshot-account", "security_key")
	_, err := snapshot.Seeder().Put(ctx, id, name, "synthetic-host-owned")
	require.NoError(t, err)
	client := vaulttest.RootClient(t)
	token := client.Token()
	backend, err := Build(ctx, &config.Config{SecretBackend: config.SecretBackendSnapshot}, pool, BuildOptions{Snapshot: snapshot, VaultClient: client})
	require.NoError(t, err)
	require.False(t, backend.SecretWrite)
	require.False(t, merchants.CanStageCredentials(backend.Secrets))
	_, err = backend.Secrets.Put(ctx, id, name, "must-not-persist")
	require.Error(t, err)
	secret, err := backend.Secrets.Get(ctx, id, name)
	require.NoError(t, err)
	require.Equal(t, "synthetic-host-owned", secret.Value)
	var count int
	require.NoError(t, pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM billing.merchant_secrets WHERE merchant_id=$1`, id.UUID()).Scan(&count)
	}))
	require.Zero(t, count)
	backend.Close()
	require.Equal(t, token, client.Token())
	_, err = client.Auth().Token().LookupSelf()
	require.NoError(t, err)
}
