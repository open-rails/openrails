//go:build integration

package bootstrap

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

type declarationLoopbackTransport struct{ base http.RoundTripper }

func (r declarationLoopbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if ip := net.ParseIP(req.URL.Hostname()); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("declaration fixture refuses non-loopback HTTP host %q", req.URL.Hostname())
	}
	return r.base.RoundTrip(req)
}

// A host snapshot applies one complete declaration, converges two replicas,
// exports metadata without credential values, and keeps credentials in memory.
func TestMerchantDeclarationLifecycle(t *testing.T) {
	ctx := t.Context()
	originalTransport := http.DefaultTransport
	http.DefaultTransport = declarationLoopbackTransport{base: originalTransport}
	t.Cleanup(func() { http.DefaultTransport = originalTransport })
	refused, err := http.NewRequest(http.MethodPost, "https://sandbox.nmi.com/payments/auth", nil)
	require.NoError(t, err)
	_, err = http.DefaultTransport.RoundTrip(refused)
	require.ErrorContains(t, err, "refuses non-loopback")
	gateway := nmiProbeArmTestServer(t, "1")
	t.Cleanup(gateway.Close)
	pool := newMerchantManifestTestPool(t)
	cp := newMerchantManifestControlPlane(t, pool)
	cfg := sandboxModeReconcileConfig()
	snapshot := merchants.NewManifestSecretStore()
	threshold, floor := int64(75_000_000), int64(2_000_000)
	mt := MerchantConfig{
		DisplayName:                        "Host Three",
		Profile:                            MerchantProfileConfig{DisplayName: "Host Three Billing", LogoURL: "https://cdn.example/logo.png", FromEmail: "billing@example.com", SupportURL: "https://example.com/support"},
		Invoice:                            &InvoiceConfig{CollectionThreshold: &threshold, MonthlyFloor: &floor, BillingPeriodBoundary: "calendar_month"},
		DelegatedInvokerWastedSpendWindows: []BudgetWindowConfig{{Key: "burst", Window: "15m", Limit: 5_000_000}, {Key: "sustained", Window: "5h", Limit: 20_000_000}},
		PSPs: map[string]PSPConfig{
			"stripe":           {"stripe": {AccountID: "acct_test_123", Secrets: map[string]string{"secret_key": "sk_test_bootstrap"}}},
			"mobius":           {"nmi": {AccountID: "100001", Settings: map[string]any{"tokenization_url": "https://secure.networkmerchants.com/token/Collect.js", "tokenization_key": "public-token"}, Secrets: map[string]string{"security_key": "active-security"}}},
			"mobius-secondary": {"nmi": {AccountID: "100002", Archived: true, Secrets: map[string]string{"security_key": "archived-security"}}},
			"ccbill":           {"ccbill": {AccountID: "945280-0000", Secrets: map[string]string{"salt": "flexform-salt", "datalink_username": "merchant-user", "datalink_password": "merchant-pass"}}},
		},
	}
	manifest := &BillingConfig{Version: 1, Merchants: map[string]MerchantConfig{"host-three": mt}}
	apply := MerchantManifestReconcileOptions{Insert: true, Overwrite: true, SecretStore: snapshot.Seeder(), NMIProbeV5BaseURL: gateway.URL}
	require.NoError(t, ReconcileMerchantManifestData(ctx, cfg, cp, manifest, apply))
	var id merchant.ID
	var group string
	require.NoError(t, pool.QueryRow(ctx, `SELECT id,permission_group_id FROM billing.merchants WHERE slug='host-three'`).Scan(&id, &group))
	wantGroup, err := cp.Core().ResolveGroupIDForSlug(ctx, controlplane.MerchantGroup("host-three"))
	require.NoError(t, err)
	require.Equal(t, wantGroup, group)

	// Both replicas enter together. Results, merchant identity and PSP count
	// prove serialization rather than merely a lack of errors.
	start, results := make(chan struct{}), make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- ReconcileMerchantManifestData(ctx, cfg, cp, manifest, apply)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	var again merchant.ID
	var count int
	require.NoError(t, pool.QueryRow(ctx, `SELECT id,permission_group_id FROM billing.merchants WHERE slug='host-three'`).Scan(&again, &group))
	require.Equal(t, id, again)
	require.Equal(t, wantGroup, group)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM billing.merchants`).Scan(&count))
	require.Equal(t, 1, count)
	var configJSON []byte
	require.NoError(t, pool.QueryRow(ctx, `SELECT config FROM billing.merchant_configurations WHERE merchant_id=$1`, id).Scan(&configJSON))
	var stored struct {
		Profile   map[string]string `json:"profile"`
		Boundary  string            `json:"billing_period_boundary"`
		Threshold int64             `json:"collection_threshold"`
		Floor     int64             `json:"monthly_floor"`
	}
	require.NoError(t, json.Unmarshal(configJSON, &stored))
	require.Equal(t, mt.Profile.DisplayName, stored.Profile["display_name"])
	require.Equal(t, mt.Profile.LogoURL, stored.Profile["logo_url"])
	require.Equal(t, mt.Profile.FromEmail, stored.Profile["from_email"])
	require.Equal(t, mt.Profile.SupportURL, stored.Profile["support_url"])
	require.Equal(t, "calendar_month", stored.Boundary)
	require.Equal(t, threshold, stored.Threshold)
	require.Equal(t, floor, stored.Floor)

	for key, rails := range mt.PSPs {
		for rail, declared := range rails {
			var gotKey, environment, account string
			var archived bool
			require.NoError(t, pool.QueryRow(ctx, `SELECT key,environment,account_id,archived FROM billing.psps WHERE merchant_id=$1 AND rail=$2 AND account_id=$3`, id, rail, declared.AccountID).Scan(&gotKey, &environment, &account, &archived))
			require.Equal(t, key, gotKey)
			require.Equal(t, "test", environment)
			require.Equal(t, declared.AccountID, account)
			require.Equal(t, declared.Archived, archived)
			for secretKey, value := range declared.Secrets {
				name, err := merchants.PSPSecretName(rail, "test", account, secretKey)
				require.NoError(t, err)
				secret, err := snapshot.Get(ctx, id, name)
				require.NoError(t, err)
				require.Equal(t, value, secret.Value)
			}
		}
	}
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM billing.psps WHERE merchant_id=$1`, id).Scan(&count))
	require.Equal(t, 4, count)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM billing.merchant_secrets WHERE merchant_id=$1`, id).Scan(&count))
	require.Zero(t, count, "snapshot credentials never persist")
	svc, err := merchants.NewService(cp.Pool(), snapshot, "test")
	require.NoError(t, err)
	tokenization, err := svc.LoadNMITokenizationConfig(ctx, id, "nmi")
	require.NoError(t, err)
	require.Equal(t, "public-token", tokenization.TokenizationKey)
	require.Equal(t, "https://secure.networkmerchants.com/token/Collect.js", tokenization.CollectJSURL)

	dumped, err := DumpMerchantConfig(ctx, cfg, cp, "host-three", DumpMerchantConfigOptions{})
	require.NoError(t, err)
	require.Len(t, dumped.Merchants, 1)
	d := dumped.Merchants["host-three"]
	require.Equal(t, mt.DisplayName, d.DisplayName)
	require.Equal(t, mt.Profile, d.Profile)
	require.Equal(t, mt.Invoice, d.Invoice)
	windows := map[string]BudgetWindowConfig{}
	for _, w := range d.DelegatedInvokerWastedSpendWindows {
		windows[w.Key] = w
	}
	require.Len(t, windows, len(mt.DelegatedInvokerWastedSpendWindows))
	for _, w := range mt.DelegatedInvokerWastedSpendWindows {
		got := windows[w.Key]
		require.Equal(t, w.Window, got.Window)
		require.Equal(t, w.Limit, got.Limit)
	}
	require.Len(t, d.PSPs, len(mt.PSPs))
	for key, rails := range mt.PSPs {
		require.Len(t, d.PSPs[key], 1)
		for rail, expected := range rails {
			actual := d.PSPs[key][rail]
			require.Equal(t, expected.AccountID, actual.AccountID)
			require.Equal(t, expected.Archived, actual.Archived)
			require.Equal(t, expected.Settings, actual.Settings)
			require.Empty(t, actual.LegacyEnvironment)
			require.Empty(t, actual.Secrets)
		}
	}
	encoded, err := MarshalMerchantManifest(dumped)
	require.NoError(t, err)
	require.Contains(t, string(encoded), "psps:")
	require.NotContains(t, string(encoded), "rail_merchant_accounts:")
	for _, rails := range mt.PSPs {
		for _, psp := range rails {
			for _, value := range psp.Secrets {
				require.NotContains(t, string(encoded), value)
			}
		}
	}
	reparsed, err := ParseMerchantConfigManifest(encoded)
	require.NoError(t, err)
	require.Equal(t, dumped, reparsed)

	// Reapplying preserves PSP metadata. Runtime credential writes are refused;
	// managed rotation is exercised by the credential publication workflow.
	seed, err := ResolvePushMerchantConfigOptions(cfg, true, false, false, false)
	require.NoError(t, err)
	require.Equal(t, MerchantManifestReconcileOptions{Insert: true}, seed)
	seed.NMIProbeV5BaseURL = gateway.URL
	seed.SecretStore = snapshot.Seeder()
	scope, armed, err := svc.ActivePSPScope(ctx, id, "stripe", "test")
	require.NoError(t, err)
	require.True(t, armed)
	require.Equal(t, "acct_test_123", scope.AccountID)
	creds, err := svc.LoadStripeCredentials(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "sk_test_bootstrap", creds.SecretKey)
	name, err := merchants.PSPSecretName("stripe", "test", "acct_test_123", "secret_key")
	require.NoError(t, err)
	_, err = snapshot.Put(ctx, id, name, "sk_test_rotated")
	require.ErrorIs(t, err, merchants.ErrManifestSecretsReadOnly)
	var updated time.Time
	require.NoError(t, pool.QueryRow(ctx, `SELECT max(updated_at) FROM billing.psps WHERE merchant_id=$1`, id).Scan(&updated))
	require.NoError(t, ReconcileMerchantManifestData(ctx, cfg, cp, manifest, seed))
	creds, err = svc.LoadStripeCredentials(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "sk_test_bootstrap", creds.SecretKey)
	var updatedAfter time.Time
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*),max(updated_at) FROM billing.psps WHERE merchant_id=$1`, id).Scan(&count, &updatedAfter))
	require.Equal(t, 4, count)
	require.True(t, updated.Equal(updatedAfter))
}

// Managed credentials are published through Client operations; startup cannot
// bypass their revision and credential validation boundaries.
func TestMerchantDeclarationRefusesManagedProviderBootstrap(t *testing.T) {
	for _, backendName := range []string{config.SecretBackendDB, config.SecretBackendVault} {
		t.Run(backendName, func(t *testing.T) {
			ctx := t.Context()
			pool := newMerchantManifestTestPool(t)
			cp := newMerchantManifestControlPlane(t, pool)
			cfg := sandboxModeReconcileConfig()
			cfg.SecretBackend = backendName
			cfg.Encryption = &config.EncryptionConfig{MasterKey: base64.StdEncoding.EncodeToString(make([]byte, 32))}
			// The provisioning guard rejects managed declarations before contacting
			// credential storage. Successful DB/Vault publication has separate tests.
			manifest := nmiManifestWithSecurityKey("must-not-publish")
			_, err := ProvisionMerchant(ctx, ProvisionMerchantRequest{Config: cfg, ControlPlane: cp, Slug: "host-three", Merchant: manifest.Merchants["host-three"], Options: MerchantManifestReconcileOptions{Insert: true}})
			require.ErrorContains(t, err, "managed provider declarations require explicit Client publication operations")
			var count int
			require.NoError(t, pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM billing.merchants)+(SELECT count(*) FROM billing.psps)+(SELECT count(*) FROM billing.merchant_secrets)`).Scan(&count))
			require.Zero(t, count, "a refused bootstrap must not create identity, provider, or credential state")
		})
	}
}
