//go:build integration

package merchantsecrets

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
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

func TestExplicitCredentialCustodyTransitionPreservesSource(t *testing.T) {
	pool, ctx := startSecretsPostgres(t)
	id, _ := registerMerchant(t, ctx, pool, "custody-transition")
	source := merchants.NewManifestSecretStore()
	account := "custody-" + uuid.NewString()
	canonical, _ := merchants.PSPSecretName("nmi", "live", account, "security_key")
	_, err := source.Seeder().Put(ctx, id, canonical, "synthetic-custody-key")
	require.NoError(t, err)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response></nm_response>`))
	}))
	defer provider.Close()
	configure := func(store merchants.MerchantSecretStore) *merchants.Service {
		svc, err := merchants.NewService(pool, store, "live")
		require.NoError(t, err)
		svc.SetCredentialProbeEndpointsForIntegration(provider.URL, "")
		return svc
	}
	snapshotService := configure(source)
	zero := int64(0)
	row, err := snapshotService.UpsertPaymentProviderConfig(ctx, id, "nmi", merchants.UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: &zero, AccountID: account})
	require.NoError(t, err)
	dbBackend, err := Build(ctx, &config.Config{SecretBackend: config.SecretBackendDB, Encryption: &config.EncryptionConfig{MasterKey: testMasterKey(t)}}, pool)
	require.NoError(t, err)
	defer dbBackend.Close()
	dbService := configure(dbBackend.Secrets)
	_, err = dbService.UpsertPaymentProviderConfig(ctx, id, "nmi", merchants.UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: &row.Revision, AccountID: account, Credentials: map[string]string{"security_key": "synthetic-custody-key"}})
	require.ErrorIs(t, err, merchants.ErrCredentialCustodyTransitionRequired)
	transition := merchants.CredentialTransitionRequest{OperationID: uuid.New(), ExpectedRevision: row.Revision, AccountID: account}
	moved, err := dbService.TransitionProviderCredentials(ctx, id, "nmi", transition, source)
	require.NoError(t, err)
	require.Equal(t, row.Revision+1, moved.Revision)
	replay, err := dbService.TransitionProviderCredentials(ctx, id, "nmi", transition, source)
	require.NoError(t, err)
	require.Equal(t, moved.Revision, replay.Revision)
	old, err := source.Get(ctx, id, canonical)
	require.NoError(t, err)
	require.Equal(t, "synthetic-custody-key", old.Value)
	ref, ok, err := dbService.ActivePSPSecretRef(ctx, id, "nmi", "live", "security_key")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = merchants.ReadSecretRef(ctx, source, id, ref)
	require.ErrorIs(t, err, merchants.ErrCredentialCustodyTransitionRequired)
	vaultBackend, err := Build(ctx, &config.Config{SecretBackend: config.SecretBackendVault}, pool, BuildOptions{VaultClient: vaulttest.RootClient(t)})
	require.NoError(t, err)
	defer vaultBackend.Close()
	vaultService := configure(vaultBackend.Secrets)
	movedAgain, err := vaultService.TransitionProviderCredentials(ctx, id, "nmi", merchants.CredentialTransitionRequest{OperationID: uuid.New(), ExpectedRevision: moved.Revision, AccountID: account}, dbBackend.Secrets)
	require.NoError(t, err)
	require.Equal(t, moved.Revision+1, movedAgain.Revision)
	// Historical source custody is retained even after successful publication.
	retained, err := merchants.ReadSecretRef(ctx, dbBackend.Secrets, id, ref)
	require.NoError(t, err)
	require.Equal(t, "synthetic-custody-key", retained.Value)
	next, ok, err := vaultService.ActivePSPSecretRef(ctx, id, "nmi", "live", "security_key")
	require.NoError(t, err)
	require.True(t, ok)
	final, err := merchants.ReadSecretRef(ctx, vaultBackend.Secrets, id, next)
	require.NoError(t, err)
	require.Equal(t, "synthetic-custody-key", final.Value)
	// Returning custody to a host snapshot is explicit: the target must already
	// contain the verified values and retain a stable identity across restarts.
	snapshotID := uuid.NewString()
	targetSnapshot, err := merchants.NewManifestSecretStoreWithIdentity(snapshotID)
	require.NoError(t, err)
	targetService := configure(targetSnapshot)
	reverse := merchants.CredentialTransitionRequest{OperationID: uuid.New(), ExpectedRevision: movedAgain.Revision, AccountID: account}
	_, err = targetService.TransitionProviderCredentials(ctx, id, "nmi", reverse, vaultBackend.Secrets)
	require.ErrorIs(t, err, merchants.ErrSecretNotFound)
	_, err = targetSnapshot.Seeder().Put(ctx, id, canonical, "synthetic-custody-key")
	require.NoError(t, err)
	returned, err := targetService.TransitionProviderCredentials(ctx, id, "nmi", reverse, vaultBackend.Secrets)
	require.NoError(t, err)
	require.Equal(t, movedAgain.Revision+1, returned.Revision)
	snapshotRef, ok, err := targetService.ActivePSPSecretRef(ctx, id, "nmi", "live", "security_key")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, canonical, snapshotRef.Name)
	restarted, err := merchants.NewManifestSecretStoreWithIdentity(snapshotID)
	require.NoError(t, err)
	_, err = merchants.ReadSecretRef(ctx, restarted, id, snapshotRef)
	require.ErrorIs(t, err, merchants.ErrSecretNotFound)
	_, err = restarted.Seeder().Put(ctx, id, canonical, "synthetic-custody-key")
	require.NoError(t, err)
	_, err = merchants.ReadSecretRef(ctx, restarted, id, snapshotRef)
	require.NoError(t, err)
	_, err = merchants.ReadSecretRef(ctx, vaultBackend.Secrets, id, snapshotRef)
	require.ErrorIs(t, err, merchants.ErrCredentialCustodyTransitionRequired)
	// Reverse publication retains the source candidate as well.
	_, err = merchants.ReadSecretRef(ctx, vaultBackend.Secrets, id, next)
	require.NoError(t, err)

}

type snapshotStripeTransport func(*http.Request) (*http.Response, error)

func (f snapshotStripeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestManagedToSnapshotRequiresExactWebhookOverlap(t *testing.T) {
	pool, ctx := startSecretsPostgres(t)
	id, _ := registerMerchant(t, ctx, pool, "snapshot-overlap")
	backend, err := Build(ctx, &config.Config{SecretBackend: config.SecretBackendDB, Encryption: &config.EncryptionConfig{MasterKey: testMasterKey(t)}}, pool)
	require.NoError(t, err)
	defer backend.Close()
	account := "acct_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	configure := func(store merchants.MerchantSecretStore) *merchants.Service {
		svc, err := merchants.NewService(pool, store, "test")
		require.NoError(t, err)
		svc.StripeClients = stripeapi.NewFactory(snapshotStripeTransport(func(r *http.Request) (*http.Response, error) {
			require.Equal(t, "api.stripe.com", r.URL.Host)
			require.Equal(t, http.MethodGet, r.Method)
			body := `{"object":"balance","livemode":false}`
			switch r.URL.Path {
			case "/v1/account":
				body = `{"object":"account","id":"` + account + `"}`
			case "/v1/balance":
			default:
				t.Fatalf("unexpected provider path %s", r.URL.Path)
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
		}))
		return svc
	}
	source := configure(backend.Secrets)
	zero := int64(0)
	row, err := source.UpsertPaymentProviderConfig(ctx, id, "stripe", merchants.UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: &zero, AccountID: account, Credentials: map[string]string{"secret_key": "sk_test_snapshot", "webhook_signing_secret": "whsec_original"}})
	require.NoError(t, err)
	// An explicitly supplied previous secret must not discard the effective
	// current secret that the rotation must retain for webhook overlap.
	_, err = source.UpsertPaymentProviderConfig(ctx, id, "stripe", merchants.UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: &row.Revision, AccountID: account, Credentials: map[string]string{"webhook_signing_secret": "whsec_current", "webhook_signing_secret_previous": "whsec_wrong"}})
	require.ErrorIs(t, err, merchants.ErrCredentialOperationConflict)
	row, err = source.UpsertPaymentProviderConfig(ctx, id, "stripe", merchants.UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: &row.Revision, AccountID: account, Credentials: map[string]string{"webhook_signing_secret": "whsec_current"}})
	require.NoError(t, err)
	// Neither replacing previous alone nor supplying it during another
	// rotation can retire the existing overlap without explicit retirement.
	_, err = source.UpsertPaymentProviderConfig(ctx, id, "stripe", merchants.UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: &row.Revision, AccountID: account, Credentials: map[string]string{"webhook_signing_secret_previous": "whsec_wrong"}})
	require.ErrorIs(t, err, merchants.ErrCredentialOperationConflict)
	_, err = source.UpsertPaymentProviderConfig(ctx, id, "stripe", merchants.UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: &row.Revision, AccountID: account, Credentials: map[string]string{"webhook_signing_secret": "whsec_next", "webhook_signing_secret_previous": "whsec_current"}})
	require.ErrorContains(t, err, "explicit retirement")
	target, err := merchants.NewManifestSecretStoreWithIdentity(uuid.NewString())
	require.NoError(t, err)
	destination := configure(target)
	seed := func(key, value string) {
		name, err := merchants.PSPSecretName("stripe", "test", account, key)
		require.NoError(t, err)
		_, err = target.Seeder().Put(ctx, id, name, value)
		require.NoError(t, err)
	}
	seed("secret_key", "sk_test_snapshot")
	seed("webhook_signing_secret", "whsec_current")
	request := merchants.CredentialTransitionRequest{OperationID: uuid.New(), ExpectedRevision: row.Revision, AccountID: account}
	_, err = destination.TransitionProviderCredentials(ctx, id, "stripe", request, backend.Secrets)
	require.ErrorIs(t, err, merchants.ErrSecretNotFound)
	seed("webhook_signing_secret_previous", "whsec_wrong")
	_, err = destination.TransitionProviderCredentials(ctx, id, "stripe", request, backend.Secrets)
	require.ErrorIs(t, err, merchants.ErrCredentialOperationConflict)
	seed("webhook_signing_secret_previous", "whsec_original")
	result, err := destination.TransitionProviderCredentials(ctx, id, "stripe", request, backend.Secrets)
	require.NoError(t, err)
	require.Equal(t, row.Revision+1, result.Revision)
	replay, err := destination.TransitionProviderCredentials(ctx, id, "stripe", request, backend.Secrets)
	require.NoError(t, err)
	require.Equal(t, result.Revision, replay.Revision)
	ref, ok, err := destination.ActivePSPSecretRef(ctx, id, "stripe", "test", "webhook_signing_secret_previous")
	require.NoError(t, err)
	require.True(t, ok)
	secret, err := merchants.ReadSecretRef(ctx, target, id, ref)
	require.NoError(t, err)
	require.Equal(t, "whsec_original", secret.Value)
	wrongEnvironment, err := merchants.NewService(pool, target, "live")
	require.NoError(t, err)
	_, err = wrongEnvironment.TransitionProviderCredentials(ctx, id, "stripe", request, backend.Secrets)
	require.ErrorIs(t, err, merchants.ErrCredentialOperationConflict)
}

func TestCommittedCredentialReplayDoesNotNeedProvider(t *testing.T) {
	pool, ctx := startSecretsPostgres(t)
	id, _ := registerMerchant(t, ctx, pool, "receipt-provider-down")
	backend, err := Build(ctx, &config.Config{SecretBackend: config.SecretBackendDB, Encryption: &config.EncryptionConfig{MasterKey: testMasterKey(t)}}, pool)
	require.NoError(t, err)
	defer backend.Close()
	service, err := merchants.NewService(pool, backend.Secrets, "live")
	require.NoError(t, err)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response></nm_response>`))
	}))
	service.SetCredentialProbeEndpointsForIntegration(provider.URL, "")
	req := merchants.UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: new(int64), AccountID: "replay-" + uuid.NewString(), Credentials: map[string]string{"security_key": "synthetic-replay-key"}}
	committed, err := service.UpsertPaymentProviderConfig(ctx, id, "nmi", req)
	require.NoError(t, err)
	provider.Close()
	// Treat the first successful response as lost. A later retry cannot depend
	// on another provider read or a newly available write privilege.
	reader, err := merchants.NewService(pool, merchants.NewReadOnlySecretStore(backend.Secrets), "live")
	require.NoError(t, err)
	reader.SetCredentialProbeEndpointsForIntegration(provider.URL, "")
	replay, err := reader.UpsertPaymentProviderConfig(ctx, id, "nmi", req)
	require.NoError(t, err)
	require.Equal(t, committed.Revision, replay.Revision)
	require.Equal(t, committed.ID, replay.ID)
	req.Credentials["security_key"] = "synthetic-changed-key"
	_, err = reader.UpsertPaymentProviderConfig(ctx, id, "nmi", req)
	require.ErrorIs(t, err, merchants.ErrCredentialOperationConflict)
	req.Credentials["security_key"] = "synthetic-replay-key"
	req.PublicConfig = map[string]string{"tokenization_key": "public-changed"}
	_, err = reader.UpsertPaymentProviderConfig(ctx, id, "nmi", req)
	require.ErrorIs(t, err, merchants.ErrCredentialOperationConflict)
}
