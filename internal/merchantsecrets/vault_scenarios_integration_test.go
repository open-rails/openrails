//go:build integration

// #724 MODE 2 scenario table: the Vault-backed merchant-secret store proven
// against a REAL Vault (vaulttest container) + real Postgres. No Vault mocks —
// mocked-Vault tests assert our guesses about Vault, not Vault.
package merchantsecrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	vaultapi "github.com/hashicorp/vault/api"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	vaultint "github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/integrations/vault/vaulttest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// registerMerchant inserts a merchant row (the vault store resolves slugs from
// openrails.merchants). Slugs carry a per-run suffix so an externally-reused
// Vault (VAULT_ADDR override) never sees colliding paths across runs.
func registerMerchant(t *testing.T, ctx context.Context, pool *db.Pool, slugPrefix string) (merchant.ID, string) {
	t.Helper()
	slug := fmt.Sprintf("%s-%s", slugPrefix, uuid.New().String()[:8])
	id := uuid.New()
	_, err := pool.Exec(ctx,
		`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`, id, slug)
	require.NoError(t, err)
	return merchant.ID(id), slug
}

// vaultMerchantPath is the full KV logical path the store derives for (slug, name).
func vaultMerchantPath(slug, name string) string {
	return path.Join("secret", "openrails", "merchants", slug, name)
}

// kvCurrentVersion reads the REAL KV-v2 metadata version for a full logical path.
func kvCurrentVersion(t *testing.T, client *vaultapi.Client, fullPath string) int {
	t.Helper()
	meta := strings.Replace(fullPath, "secret/", "secret/metadata/", 1)
	sec, err := client.Logical().ReadWithContext(context.Background(), meta)
	require.NoError(t, err, "read kv metadata %s", meta)
	require.NotNil(t, sec, "kv metadata %s must exist", meta)
	n, ok := sec.Data["current_version"].(json.Number)
	require.True(t, ok, "current_version type: %T", sec.Data["current_version"])
	v, err := n.Int64()
	require.NoError(t, err)
	return int(v)
}

func buildVaultStore(t *testing.T, ctx context.Context, pool *db.Pool, token string) *Store {
	t.Helper()
	addr, _ := vaulttest.Addr(t)
	store, err := Build(ctx, vaultBackedConfig("production", addr, token), pool)
	require.NoError(t, err)
	return store
}

func scopedName(t *testing.T, rail, environment, accountID, key string) string {
	t.Helper()
	name, err := merchants.RailMerchantAccountSecretName(rail, environment, accountID, key)
	require.NoError(t, err)
	return name
}

// --- Store cycle: Put/Get/Delete/status-list, KV-v2 versioning, isolation ------

func TestVaultStoreCycle_VersioningAndTwoMerchantIsolation(t *testing.T) {
	pool, ctx := startSecretsPostgres(t)
	_, rootToken := vaulttest.Addr(t)
	store := buildVaultStore(t, ctx, pool, rootToken)
	root := vaulttest.RootClient(t)

	midA, slugA := registerMerchant(t, ctx, pool, "vtc-a")
	midB, slugB := registerMerchant(t, ctx, pool, "vtc-b")
	acctA, acctB := "vtc-acct-"+slugA, "vtc-acct-"+slugB
	nameA := scopedName(t, "nmi", "live", acctA, "security_key")
	nameB := scopedName(t, "nmi", "live", acctB, "security_key")

	// Absent before any write: terminal ErrSecretNotFound (not "unavailable").
	_, err := store.Secrets.Get(ctx, midA, nameA)
	require.ErrorIs(t, err, merchants.ErrSecretNotFound)

	// Put/Get round trip under merchant A.
	_, err = store.Secrets.Put(ctx, midA, nameA, "sk-A-1")
	require.NoError(t, err)
	got, err := store.Secrets.Get(ctx, midA, nameA)
	require.NoError(t, err)
	require.Equal(t, "sk-A-1", got.Value)

	// Merchant B: own namespace, own value; A's name is invisible to B.
	_, err = store.Secrets.Put(ctx, midB, nameB, "sk-B-1")
	require.NoError(t, err)
	_, err = store.Secrets.Get(ctx, midB, nameA)
	require.ErrorIs(t, err, merchants.ErrSecretNotFound, "merchant B must never see A's secret")

	namesA, err := store.Secrets.List(ctx, midA)
	require.NoError(t, err)
	require.Contains(t, namesA, nameA)
	require.NotContains(t, namesA, nameB, "cross-namespace leak: A lists B's secret name")
	namesB, err := store.Secrets.List(ctx, midB)
	require.NoError(t, err)
	require.Contains(t, namesB, nameB)
	require.NotContains(t, namesB, nameA)

	// KV-v2 overwrite versioning is real: rotate → current_version bumps to 2,
	// and the store SURFACES it (Secret.Version mirrors KV-v2, not a constant).
	require.Equal(t, 1, kvCurrentVersion(t, root, vaultMerchantPath(slugA, nameA)))
	require.Equal(t, 1, got.Version, "store must surface KV-v2 version on read")
	put2, err := store.Secrets.Put(ctx, midA, nameA, "sk-A-2")
	require.NoError(t, err)
	require.Equal(t, 2, put2.Version, "store must surface the new KV-v2 version on write")
	got, err = store.Secrets.Get(ctx, midA, nameA)
	require.NoError(t, err)
	require.Equal(t, "sk-A-2", got.Value, "rotation must be live immediately")
	require.Equal(t, 2, got.Version)
	require.Equal(t, 2, kvCurrentVersion(t, root, vaultMerchantPath(slugA, nameA)))

	// Rotating A never touches B.
	gotB, err := store.Secrets.Get(ctx, midB, nameB)
	require.NoError(t, err)
	require.Equal(t, "sk-B-1", gotB.Value)

	// Status-list (registry + configured view) through the merchants service.
	svc, err := merchants.NewService(pool, store.Secrets, "live")
	require.NoError(t, err)
	statuses, err := svc.ListSecretStatuses(ctx, midA)
	require.NoError(t, err)
	var configured bool
	for _, st := range statuses {
		if st.Name == nameA {
			require.True(t, st.Configured)
			configured = true
		}
	}
	require.True(t, configured, "status-list must surface the scoped vault-backed secret %s", nameA)

	// Delete: purges all versions; absence is terminal; idempotent; B unaffected.
	require.NoError(t, store.Secrets.Delete(ctx, midA, nameA))
	_, err = store.Secrets.Get(ctx, midA, nameA)
	require.ErrorIs(t, err, merchants.ErrSecretNotFound)
	require.NoError(t, store.Secrets.Delete(ctx, midA, nameA), "delete must be idempotent")
	gotB, err = store.Secrets.Get(ctx, midB, nameB)
	require.NoError(t, err)
	require.Equal(t, "sk-B-1", gotB.Value)
}

// --- Full stack: PUT payment-providers → KV path → arming read → rotate --------

func TestVaultFullStack_PaymentProviderConfigRotationAndIsolation(t *testing.T) {
	pool, ctx := startSecretsPostgres(t)
	_, rootToken := vaulttest.Addr(t)
	store := buildVaultStore(t, ctx, pool, rootToken)
	root := vaulttest.RootClient(t)
	rootKV := vaultint.NewKVv2Adapter(root, "secret")

	svc, err := merchants.NewService(pool, store.Secrets, "live")
	require.NoError(t, err)

	midA, slugA := registerMerchant(t, ctx, pool, "vfs-a")
	midB, slugB := registerMerchant(t, ctx, pool, "vfs-b")
	acctA, acctB := "vfs-nmi-"+slugA, "vfs-nmi-"+slugB

	// PUT /v1/merchant/payment-providers equivalent (service level; the HTTP
	// handler delegates here): declares the account and stores credentials.
	cfgA, err := svc.UpsertPaymentProviderConfig(ctx, midA, "nmi", merchants.UpsertPaymentProviderConfigRequest{
		Environment: "live",
		AccountID:   acctA,
		Credentials: map[string]string{
			"security_key":           "sk-full-A-1",
			"webhook_signing_secret": "wh-full-A-1",
		},
	})
	require.NoError(t, err)
	require.True(t, cfgA.Credentials["security_key"].Configured,
		"API response must show the vault-held credential as configured")

	// The KV holds the value at the canonical RailMerchantAccountSecretName path.
	keyName := scopedName(t, "nmi", "live", acctA, "security_key")
	kvData, _, err := rootKV.ReadSecret(ctx, vaultMerchantPath(slugA, keyName))
	require.NoError(t, err)
	require.Equal(t, "sk-full-A-1", kvData["value"],
		"credential must live in Vault KV at the canonical path")

	// Arming/charge-path read: resolver → store.Get (the pair rail arming uses).
	resolved, ok, err := svc.ActiveRailMerchantAccountSecretName(ctx, midA, "nmi", "live", "security_key")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, keyName, resolved)
	sec, err := store.Secrets.Get(ctx, midA, resolved)
	require.NoError(t, err)
	require.Equal(t, "sk-full-A-1", sec.Value)

	// Second merchant configured independently.
	_, err = svc.UpsertPaymentProviderConfig(ctx, midB, "nmi", merchants.UpsertPaymentProviderConfigRequest{
		Environment: "live",
		AccountID:   acctB,
		Credentials: map[string]string{"security_key": "sk-full-B-1"},
	})
	require.NoError(t, err)

	// Rotate A via a second PUT: new value live immediately, KV version bumps.
	_, err = svc.UpsertPaymentProviderConfig(ctx, midA, "nmi", merchants.UpsertPaymentProviderConfigRequest{
		Environment: "live",
		AccountID:   acctA,
		Credentials: map[string]string{"security_key": "sk-full-A-2"},
	})
	require.NoError(t, err)
	sec, err = store.Secrets.Get(ctx, midA, resolved)
	require.NoError(t, err)
	require.Equal(t, "sk-full-A-2", sec.Value, "rotation must be live immediately (write-through)")
	require.Equal(t, 2, kvCurrentVersion(t, root, vaultMerchantPath(slugA, keyName)))

	// The second merchant is untouched by A's rotation.
	keyNameB := scopedName(t, "nmi", "live", acctB, "security_key")
	secB, err := store.Secrets.Get(ctx, midB, keyNameB)
	require.NoError(t, err)
	require.Equal(t, "sk-full-B-1", secB.Value)
	require.Equal(t, 1, kvCurrentVersion(t, root, vaultMerchantPath(slugB, keyNameB)))
}

// --- Capability gating (#661) with REAL Vault policies --------------------------

func TestVaultCapabilityGating_RealPolicies(t *testing.T) {
	pool, ctx := startSecretsPostgres(t)
	addr, rootToken := vaulttest.Addr(t)

	t.Run("kv read-only token serves reads, refuses writes, gates SecretWrite", func(t *testing.T) {
		roToken := vaulttest.TokenWithPolicy(t, "or-ms-kv-ro", vaulttest.PolicyKVReadOnly)
		store, err := Build(ctx, vaultBackedConfig("production", addr, roToken), pool)
		require.NoError(t, err, "read-only KV still serves the declared vault backend")
		require.True(t, store.Capabilities.KVRead)
		require.False(t, store.Capabilities.KVWrite)
		require.False(t, store.SecretWrite, "route gate: writes/config-push must be off")

		mid, slug := registerMerchant(t, ctx, pool, "vcg-ro")
		name := scopedName(t, "nmi", "live", "vcg-acct-"+slug, "security_key")

		// Seed through root, read through the read-only store.
		rootStore := buildVaultStore(t, ctx, pool, rootToken)
		_, err = rootStore.Secrets.Put(ctx, mid, name, "ro-visible")
		require.NoError(t, err)
		got, err := store.Secrets.Get(ctx, mid, name)
		require.NoError(t, err)
		require.Equal(t, "ro-visible", got.Value)

		// A write through the read-only token fails as a REAL Vault 403, surfaced
		// as the retryable backend-unavailable class (never silent success).
		_, err = store.Secrets.Put(ctx, mid, name, "ro-write-attempt")
		require.Error(t, err)
		require.ErrorIs(t, err, merchants.ErrSecretBackendUnavailable)
		got, err = rootStore.Secrets.Get(ctx, mid, name)
		require.NoError(t, err)
		require.Equal(t, "ro-visible", got.Value, "failed write must not change the stored value")
	})

	t.Run("transit-only token + secret_backend=vault refuses boot", func(t *testing.T) {
		toToken := vaulttest.TokenWithPolicy(t, "or-ms-transit-only", vaulttest.PolicyTransitOnly)
		_, err := Build(ctx, vaultBackedConfig("production", addr, toToken), pool)
		require.Error(t, err, "vault-declared secrets with no KV read must never boot (would run silently empty)")
		require.Contains(t, err.Error(), "cannot read the KV mount")
	})

	t.Run("transit-only token + secret_backend=db degrades to DB store", func(t *testing.T) {
		toToken := vaulttest.TokenWithPolicy(t, "or-ms-transit-only", vaulttest.PolicyTransitOnly)
		cfg := &config.Config{
			Env:            "production",
			MerchantSource: config.MerchantSourceAPI,
			SecretBackend:  config.SecretBackendDB,
			Encryption:     &config.EncryptionConfig{MasterKey: testMasterKey(t)},
			Vault:          &config.VaultConfig{Enabled: true, Address: addr, AuthMethod: "token", Token: toToken},
		}
		store, err := Build(ctx, cfg, pool)
		require.NoError(t, err)
		require.False(t, store.Capabilities.KVRead)
		require.False(t, store.Capabilities.KVWrite)
		require.True(t, store.SolanaCanSign, "vault connection enables the signing surface")
		require.True(t, store.SecretWrite, "DB-backed writes stay on")
		require.NotNil(t, store.SolanaTransit)

		// Secrets round-trip through Postgres, not Vault.
		mid, _ := registerMerchant(t, ctx, pool, "vcg-db") // merchant_deks FK needs a real merchant
		_, err = store.Secrets.Put(ctx, mid, merchants.SecretNMISecurityKey, "db-backed")
		require.NoError(t, err)
		got, err := store.Secrets.Get(ctx, mid, merchants.SecretNMISecurityKey)
		require.NoError(t, err)
		require.Equal(t, "db-backed", got.Value)
	})
}

// --- Outage semantics: pause/unpause a dedicated container ----------------------

func TestVaultOutage_FailClosedAtBootAndRead_RecoverWithoutRestart(t *testing.T) {
	pool, ctx := startSecretsPostgres(t)
	d := vaulttest.StartDedicated(t)
	mid, slug := registerMerchant(t, ctx, pool, "vout")

	cfg := vaultBackedConfig("production", d.Addr, d.RootToken)

	// Unreachable at boot ⇒ Build fails loudly (vault declared means vault required).
	d.Pause(t)
	bootCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_, err := Build(bootCtx, cfg, pool)
	cancel()
	require.Error(t, err, "boot with unreachable Vault must fail loudly, never degrade")
	require.Contains(t, err.Error(), "vault capability probe")
	d.Unpause(t)

	// Healthy boot; seed nameA through the store (it lands in the read cache too).
	store, err := Build(ctx, cfg, pool)
	require.NoError(t, err)
	nameA := scopedName(t, "nmi", "live", "vout-a-"+slug, "security_key")
	_, err = store.Secrets.Put(ctx, mid, nameA, "vA")
	require.NoError(t, err)

	// Seed nameB directly in Vault (never read through this process yet), so the
	// outage read below cannot be served by the in-process cache.
	rootKV := vaultint.NewKVv2Adapter(d.Client(t, d.RootToken), "secret")
	nameB := scopedName(t, "nmi", "live", "vout-b-"+slug, "security_key")
	func() {
		_, werr := rootKV.WriteSecret(ctx, vaultMerchantPath(slug, nameB), map[string]string{"value": "vB"})
		require.NoError(t, werr)
	}()

	// Unreachable at read time ⇒ typed fail-closed error: retryable backend
	// unavailability, NEVER "secret absent" and never a fabricated value.
	d.Pause(t)
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	_, err = store.Secrets.Get(readCtx, mid, nameB)
	cancel()
	require.Error(t, err)
	require.ErrorIs(t, err, merchants.ErrSecretBackendUnavailable)
	require.False(t, errors.Is(err, merchants.ErrSecretNotFound),
		"an outage must never read as terminal absence (would disable webhook verification / cancel work)")

	// Pinned by design: a value this process already read/wrote stays served from
	// the in-process TTL cache during the outage (DefaultSecretCacheTTL) — the
	// cache holds real previously-authoritative values, never fabricated ones.
	cached, err := store.Secrets.Get(ctx, mid, nameA)
	require.NoError(t, err)
	require.Equal(t, "vA", cached.Value)

	// Unpause ⇒ recovery WITHOUT a process restart; pre-outage data intact.
	d.Unpause(t)
	got, err := store.Secrets.Get(ctx, mid, nameB)
	require.NoError(t, err, "recovery after unpause must not need a new Build/restart")
	require.Equal(t, "vB", got.Value)
}

// --- Backend parity: same cycle/rotation/isolation table on vault AND DB --------

func TestBackendParity_CycleRotationIsolation(t *testing.T) {
	pool, ctx := startSecretsPostgres(t)
	addr, rootToken := vaulttest.Addr(t)

	backends := []struct {
		name  string
		build func(t *testing.T) *Store
	}{
		{"vault", func(t *testing.T) *Store {
			store, err := Build(ctx, vaultBackedConfig("production", addr, rootToken), pool)
			require.NoError(t, err)
			return store
		}},
		{"db-encrypted", func(t *testing.T) *Store {
			store, err := Build(ctx, &config.Config{
				Env:            "production",
				MerchantSource: config.MerchantSourceAPI,
				SecretBackend:  config.SecretBackendDB,
				Encryption:     &config.EncryptionConfig{MasterKey: testMasterKey(t)},
			}, pool)
			require.NoError(t, err)
			return store
		}},
	}

	// configuredSets collects the status-list configured names per backend so the
	// two stores cannot drift on the status surface either.
	configuredSets := map[string][]string{}

	for _, be := range backends {
		be := be
		t.Run(be.name, func(t *testing.T) {
			store := be.build(t)
			midA, _ := registerMerchant(t, ctx, pool, "vpar-a")
			midB, _ := registerMerchant(t, ctx, pool, "vpar-b")

			// Identical name strings across backends → comparable configured sets.
			nameShared := scopedName(t, "nmi", "live", "vpar-shared", "security_key")
			nameOnlyA := scopedName(t, "nmi", "live", "vpar-only-a", "webhook_signing_secret")

			// Absent ⇒ terminal not-found.
			_, err := store.Secrets.Get(ctx, midA, nameShared)
			require.ErrorIs(t, err, merchants.ErrSecretNotFound)

			// Cycle.
			put1, err := store.Secrets.Put(ctx, midA, nameShared, "v1")
			require.NoError(t, err)
			require.GreaterOrEqual(t, put1.Version, 1)
			got, err := store.Secrets.Get(ctx, midA, nameShared)
			require.NoError(t, err)
			require.Equal(t, "v1", got.Value)

			// Rotation: new value live immediately; version strictly increases
			// on BOTH backends (vault surfaces KV-v2, db increments).
			put2, err := store.Secrets.Put(ctx, midA, nameShared, "v2")
			require.NoError(t, err)
			require.Greater(t, put2.Version, put1.Version)
			got, err = store.Secrets.Get(ctx, midA, nameShared)
			require.NoError(t, err)
			require.Equal(t, "v2", got.Value)

			// Isolation: same name under B is an independent slot.
			_, err = store.Secrets.Put(ctx, midB, nameShared, "vB")
			require.NoError(t, err)
			gotA, err := store.Secrets.Get(ctx, midA, nameShared)
			require.NoError(t, err)
			require.Equal(t, "v2", gotA.Value, "B's write must not clobber A")
			_, err = store.Secrets.Get(ctx, midB, nameOnlyA)
			require.ErrorIs(t, err, merchants.ErrSecretNotFound)

			// List: full canonical names, scoped to the merchant.
			_, err = store.Secrets.Put(ctx, midA, nameOnlyA, "wh1")
			require.NoError(t, err)
			namesA, err := store.Secrets.List(ctx, midA)
			require.NoError(t, err)
			require.Contains(t, namesA, nameShared)
			require.Contains(t, namesA, nameOnlyA)
			namesB, err := store.Secrets.List(ctx, midB)
			require.NoError(t, err)
			require.Contains(t, namesB, nameShared)
			require.NotContains(t, namesB, nameOnlyA)

			// Status-list surface.
			svc, err := merchants.NewService(pool, store.Secrets, "live")
			require.NoError(t, err)
			statuses, err := svc.ListSecretStatuses(ctx, midA)
			require.NoError(t, err)
			var configured []string
			for _, st := range statuses {
				if st.Configured {
					configured = append(configured, st.Name)
				}
			}
			configuredSets[be.name] = configured

			// Delete: terminal absence, idempotent, other merchant untouched.
			require.NoError(t, store.Secrets.Delete(ctx, midA, nameOnlyA))
			_, err = store.Secrets.Get(ctx, midA, nameOnlyA)
			require.ErrorIs(t, err, merchants.ErrSecretNotFound)
			require.NoError(t, store.Secrets.Delete(ctx, midA, nameOnlyA))
			gotB, err := store.Secrets.Get(ctx, midB, nameShared)
			require.NoError(t, err)
			require.Equal(t, "vB", gotB.Value)
		})
	}

	require.ElementsMatch(t, configuredSets["vault"], configuredSets["db-encrypted"],
		"the two backends drifted on the status-list configured view")
}
