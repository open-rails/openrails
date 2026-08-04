//go:build integration

package embed_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/embedded"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/pkg/service"
)

// #723 MODE 1 conformance: merchant_source=manifest — the boot YAML (+ mounted
// secret files) is the truth, held in memory; the DB carries projections only.

func manifestModeConfig(dsn string) *config.Config {
	return &config.Config{
		Env:      "dev",
		TestMode: config.CredentialPostureLive,
		// Explicit default (#723): manifest-is-truth.
		MerchantSource: config.MerchantSourceManifest,
		// full: the loop test executes a (fake-provider) charge.
		ProviderWriteMode: config.ProviderWriteModeFull,
		DB:                &config.DBConfig{URL: dsn},
	}
}

func manifestModeManifestYAML(slug, gatewayID string) []byte {
	return []byte(fmt.Sprintf(`version: 1
merchants:
  %s:
    display_name: %s
    psps:
      mobius:
        nmi:
          environment: live
          account_id: %q
`, slug, slug, gatewayID))
}

func manifestModeCatalogYAML(slug string, amount int64) []byte {
	return []byte(fmt.Sprintf(`version: 1
catalogs:
  - merchant: %s
    products:
      - key: pro
        display_name: Pro
        entitlements: [pro-access]
        prices:
          - currency: usd
            unit_amount: %d
            duration: 30d
            auto_renew: true
`, slug, amount))
}

// bootManifestRuntime is one "pod boot": parse the manifest bytes (secret-file
// overlay applied via VAULT_SECRETS_PATH), start the engine, apply the merchant
// manifest, converge the catalog.
func bootManifestRuntime(t *testing.T, ctx context.Context, dsn, slug, nmiV5BaseURL string, manifestRaw, catalogRaw []byte) (*embed.Runtime, merchant.ID) {
	t.Helper()
	cfg := manifestModeConfig(dsn)
	manifest, err := embed.LoadMerchantConfigManifest(manifestRaw)
	require.NoError(t, err)
	rt, err := embed.New(ctx, embed.Options{Options: embedded.Options{Config: cfg, River: embedded.RiverManagedByOpenRails()}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	id, err := rt.UpsertMerchantConfig(ctx, slug, manifest.Merchants[slug])
	require.NoError(t, err)
	require.False(t, id.IsZero())
	runtime := rt.Embedded().App().Runtime
	require.NotNil(t, runtime.SolanaPlanService,
		"embedded provisioning arms recurring Solana services")
	runtime.CollectionResolver = &money.MerchantCollectionAdapterBuilder{
		Config:      runtime.Config,
		DB:          runtime.DB,
		MerchantsFn: func() *merchants.Service { return runtime.Merchants },
		Endpoints:   money.CollectionEndpoints{NMIV5BaseURL: nmiV5BaseURL},
	}
	require.NoError(t, embedded.PushMerchantCatalog(ctx, embedded.CatalogPushOptions{
		Runtime:  rt.Embedded(),
		Manifest: catalogRaw,
		Insert:   true, Overwrite: true, Prune: true,
	}))
	return rt, id
}

// inMerchantScope runs fn on a connection whose app.merchant_id is pinned for the
// duration of ONE transaction — the read shape production uses. The pin is
// transaction-local on purpose: a session-level set_config would ride the pooled
// connection back out and silently scope somebody else's query.
func inMerchantScope(t *testing.T, pool *pgxpool.Pool, ctx context.Context, id merchant.ID, fn func(tx pgx.Tx)) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = tx.Exec(ctx, "SELECT set_config('app.merchant_id', $1, true)", id.UUID().String())
	require.NoError(t, err)
	fn(tx)
}

func activePriceAmounts(t *testing.T, pool *pgxpool.Pool, ctx context.Context, id merchant.ID) []int64 {
	t.Helper()
	var out []int64
	inMerchantScope(t, pool, ctx, id, func(tx pgx.Tx) {
		rows, err := tx.Query(ctx, `SELECT amount FROM openrails.prices WHERE merchant_id = $1 AND NOT archived ORDER BY amount`, id.UUID())
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var amount int64
			require.NoError(t, rows.Scan(&amount))
			out = append(out, amount)
		}
		require.NoError(t, rows.Err())
	})
	return out
}

// merchantSecretRowCount counts under the merchant's OWN scope. openrails.merchant_secrets
// is RLS-forced, so an unpinned count on the openrails_app role is always 0 — which
// would make MODE 1's "wrote no secret" assertion vacuous and MODE 2's "persisted a
// secret" assertion impossible.
func merchantSecretRowCount(t *testing.T, pool *pgxpool.Pool, ctx context.Context, id merchant.ID) int {
	t.Helper()
	var n int
	inMerchantScope(t, pool, ctx, id, func(tx pgx.Tx) {
		require.NoError(t, tx.QueryRow(ctx, `SELECT count(*) FROM openrails.merchant_secrets WHERE merchant_id = $1`, id.UUID()).Scan(&n))
	})
	return n
}

// fakeNMI serves the v5 sale endpoint and records the Authorization (security
// key) each charge arrived with.
func fakeNMI(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/plans":
			_, _ = w.Write([]byte(`{"plans":[],"next_cursor":null,"has_more":false}`))
		case r.Method == http.MethodPost && r.URL.Path == "/payments/sale":
			keys = append(keys, r.Header.Get("Authorization"))
			_, _ = w.Write([]byte(`{"object":"transaction","id":"txn_manifest_mode","response":"1","response_text":"SUCCESS","response_code":"100"}`))
		default:
			http.Error(w, "unexpected NMI request", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server, &keys
}

// chargeViaStorePlane executes one production-path NMI sale: the #725
// per-merchant resolver arms the client from the merchants service (which in
// mode 1 reads the in-memory manifest plane), pointed at the fake gateway.
func chargeViaStorePlane(t *testing.T, ctx context.Context, rt *embed.Runtime, id merchant.ID, baseURL string) error {
	t.Helper()
	runtime := rt.Embedded().App().Runtime
	require.NotNil(t, runtime.Merchants, "mode 1 arms Runtime.Merchants at UpsertMerchantConfig")
	builder := &money.MerchantCollectionAdapterBuilder{
		Config:      runtime.Config,
		DB:          runtime.DB,
		MerchantsFn: func() *merchants.Service { return runtime.Merchants },
		Endpoints:   money.CollectionEndpoints{NMIV5BaseURL: baseURL},
	}
	mctx := merchant.WithID(ctx, id)
	client, ok, err := builder.ResolveNMIClient(mctx, id.UUID(), nil)
	if err != nil {
		return err
	}
	require.True(t, ok, "declared+seeded NMI account must resolve a client")
	_, err = client.RunSale(ctx, nmi.SaleParams{
		CustomerVaultID: "vault-manifest-mode",
		Amount:          moneyutil.Cents(500),
		Currency:        "USD",
	})
	return err
}

// TestManifestMode_Loop is the #723 conformance loop: boot from manifest bytes
// + secret FILES → rails armed, catalog converged, a charge executes with the
// file-provided credential, an entitlement lands → rotate the secret + change
// the price in the files → REBOOT (new embed.New, same DB) → new price live,
// the NEXT charge carries the rotated key, no store residue → a third boot is
// an idempotent no-op.
func TestManifestMode_Loop(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	appDB := dbtest.OpenAppDB(t, dsn)
	pool := appDB.Pool()

	nano := time.Now().UnixNano()
	slug := fmt.Sprintf("mloop%d", nano)
	gatewayID := fmt.Sprintf("57%d", nano%1_000_000)
	keyV1 := fmt.Sprintf("sec-key-v1-%d", nano)
	keyV2 := fmt.Sprintf("sec-key-v2-%d", nano)

	// Operator-mounted secret files: filename = env-var name (#723 precedence
	// yaml < files < env).
	secretsDir := t.TempDir()
	secretFile := filepath.Join(secretsDir, fmt.Sprintf("BILLING_MERCHANTS_%s_PSPS_MOBIUS_NMI_SECRETS_SECURITY_KEY", strings.ToUpper(slug)))
	require.NoError(t, os.WriteFile(secretFile, []byte(keyV1+"\n"), 0o600))
	t.Setenv("VAULT_SECRETS_PATH", secretsDir)

	manifestRaw := manifestModeManifestYAML(slug, gatewayID)
	server, seenKeys := fakeNMI(t)

	// ---- Boot 1.
	rt1, id := bootManifestRuntime(t, ctx, dsn, slug, server.URL, manifestRaw, manifestModeCatalogYAML(slug, 5_000_000))
	t.Cleanup(func() {
		for _, stmt := range []string{
			`DELETE FROM openrails.entitlements WHERE merchant_id = $1`,
			`DELETE FROM openrails.prices WHERE merchant_id = $1`,
			`DELETE FROM openrails.products WHERE merchant_id = $1`,
			`DELETE FROM openrails.merchant_configurations WHERE merchant_id = $1`,
			`DELETE FROM openrails.psps WHERE merchant_id = $1`,
			`DELETE FROM openrails.merchants WHERE id = $1`,
		} {
			_, _ = pool.Exec(context.Background(), stmt, id.UUID())
		}
	})

	// Rails armed from the manifest plane (#699 semantics).
	runtime := rt1.Embedded().App().Runtime
	armed := reconcile.MerchantFetcherBuilder{
		Config:    runtime.Config,
		Merchants: runtime.Merchants,
		DB:        runtime.DB,
	}.Build(merchant.WithID(ctx, id), id)
	require.Contains(t, armed.Fetchers, reconcile.ProviderNMI, "NMI fetcher armed from the in-memory manifest plane")
	prober, ok := armed.Probers[reconcile.ProviderNMI].(*reconcile.NMISubscriptionProber)
	require.True(t, ok)
	assert.Equal(t, keyV1, prober.Client.SecurityKey, "prober carries the secret-FILE credential")

	// Catalog converged; the DB rows are a projection for FKs.
	require.Equal(t, []int64{5_000_000}, activePriceAmounts(t, pool, ctx, id))

	// MODE 1 invariant: nothing was written to the persistent secret store.
	require.Zero(t, merchantSecretRowCount(t, pool, ctx, id), "manifest mode must never write openrails.merchant_secrets")

	// A charge executes through the production per-merchant resolver with the
	// file-provided key.
	require.NoError(t, chargeViaStorePlane(t, ctx, rt1, id, server.URL))
	require.Equal(t, []string{keyV1}, *seenKeys)

	// An entitlement (the converged product's entitlement string) lands.
	svc := rt1.Service()
	mctx := merchant.WithID(ctx, id)
	userID := uuid.NewString()
	_, err := svc.EntitlementGrantEntitlement(mctx, "manifest-loop-admin", service.EntitlementGrantEntitlementRequest{
		UserID:      userID,
		Entitlement: "pro-access",
		Reason:      "#723 conformance",
	})
	require.NoError(t, err)
	// The read runs inside a merchant-scoped connection because it is a plain
	// RLS-scoped SELECT (no MerchantTx of its own): in production the HTTP layer
	// pins it, and this test stands in for that layer. Without the pin it reads
	// the base pool and the openrails_app role answers with an empty list.
	var ents []string
	require.NoError(t, runtime.DB.RunInMerchantConn(mctx, func(ctx context.Context) error {
		var err error
		ents, err = svc.ListActiveEntitlements(ctx, userID, time.Now().UTC())
		return err
	}))
	require.Contains(t, ents, "pro-access")

	// Runtime writes against the plane are refused: rotation is file+reboot.
	_, err = runtime.Merchants.Secrets().Put(mctx, id, "rail_merchant_accounts/nmi/live/"+gatewayID+"/security_key", "sneaky")
	require.ErrorIs(t, err, merchants.ErrManifestSecretsReadOnly)

	// ---- Rotate: new secret value in the file, new price in the catalog.
	require.NoError(t, os.WriteFile(secretFile, []byte(keyV2+"\n"), 0o600))
	require.NoError(t, rt1.Close(ctx))

	// ---- Boot 2 (reboot: fresh runtime, same DB).
	rt2, id2 := bootManifestRuntime(t, ctx, dsn, slug, server.URL, manifestRaw, manifestModeCatalogYAML(slug, 7_000_000))
	require.Equal(t, id, id2, "reboot binds the same merchant")

	require.Equal(t, []int64{7_000_000}, activePriceAmounts(t, pool, ctx, id), "changed price is live after reboot; the old one is archived")
	require.NoError(t, chargeViaStorePlane(t, ctx, rt2, id, server.URL))
	require.Equal(t, []string{keyV1, keyV2}, *seenKeys, "the charge after reboot uses the ROTATED key")
	require.Zero(t, merchantSecretRowCount(t, pool, ctx, id), "no stale residue: still zero persistent secret rows")

	// ---- Boot 3: unchanged inputs are an idempotent no-op.
	require.NoError(t, rt2.Close(ctx))
	rt3, id3 := bootManifestRuntime(t, ctx, dsn, slug, server.URL, manifestRaw, manifestModeCatalogYAML(slug, 7_000_000))
	require.Equal(t, id, id3)
	require.Equal(t, []int64{7_000_000}, activePriceAmounts(t, pool, ctx, id))
	require.NoError(t, chargeViaStorePlane(t, ctx, rt3, id, server.URL))
	require.Equal(t, []string{keyV1, keyV2, keyV2}, *seenKeys)
	require.Zero(t, merchantSecretRowCount(t, pool, ctx, id))
}

// allowAllGate satisfies billingauth.Gate for mount tests: the mode-1 405
// choke point must answer BEFORE authorization work.
type allowAllGate struct{ id merchant.ID }

func (g allowAllGate) Authorize(context.Context, *http.Request, string) (billingauth.Principal, error) {
	return billingauth.Principal{MerchantID: g.id}, nil
}

// TestManifestMode_MutationRoutesRejected405: PUT/DELETE payment-providers and
// the catalog mutation routes answer 405 with the machine code
// `manifest_driven` in mode 1; reads keep working.
func TestManifestMode_MutationRoutesRejected405(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)

	nano := time.Now().UnixNano()
	slug := fmt.Sprintf("m405%d", nano)
	cfg := manifestModeConfig(dsn)
	rt, err := embed.New(ctx, embed.Options{Options: embedded.Options{Config: cfg, River: embedded.RiverManagedByOpenRails()}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	id, err := rt.UpsertMerchantConfig(ctx, slug, embed.MerchantConfig{DisplayName: slug})
	require.NoError(t, err)

	handler, err := embedded.MountHandler(rt.Embedded(), embedded.MountOptions{
		RouteSets:      []embed.RouteSet{embed.RouteSetCatalog, embed.RouteSetPaymentProviders},
		Gate:           allowAllGate{id: id},
		ProviderRoutes: &embedded.ProviderRoutes{Webhooks: true},
	})
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	do := func(method, path, body string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(raw)
	}

	assertManifestDriven := func(status int, body string) {
		t.Helper()
		require.Equal(t, http.StatusMethodNotAllowed, status, body)
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &envelope), body)
		require.Equal(t, "manifest_driven", envelope.Error.Code)
		require.Contains(t, envelope.Error.Message, "reboot")
	}

	// Provider-config writes.
	assertManifestDriven(do(http.MethodPut, "/v1/merchant/payment-providers/stripe", `{"environment":"live","account_id":"acct_x"}`))
	assertManifestDriven(do(http.MethodDelete, "/v1/merchant/payment-providers/stripe", ""))
	// Catalog mutations — including plan-only publish (documented: the plan is
	// computed at boot from the YAML; the CLI dry-run remains available).
	assertManifestDriven(do(http.MethodPost, "/v1/merchant/catalog/products", `{"key":"x","display_name":"X"}`))
	assertManifestDriven(do(http.MethodPost, "/v1/merchant/catalog/publish", `{"catalog":{"version":1}}`))

	// Reads stay served (list providers; empty is fine — not 405).
	status, body := do(http.MethodGet, "/v1/merchant/payment-providers", "")
	require.Equal(t, http.StatusOK, status, body)
}

// TestAPIMode_MutationRoutesWork: the same PUT works against the existing
// store path when merchant_source=api (quick mode-2 smoke; the full api-mode
// HTTP behavior is covered by the standalone integrationharness suites).
func TestAPIMode_MutationRoutesWork(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	appDB := dbtest.OpenAppDB(t, dsn)

	nano := time.Now().UnixNano()
	slug := fmt.Sprintf("mapi%d", nano)
	cfg := &config.Config{
		Env:               "dev",
		TestMode:          config.CredentialPostureLive,
		MerchantSource:    config.MerchantSourceAPI,
		ProviderWriteMode: config.ProviderWriteModeFull,
		DB:                &config.DBConfig{URL: dsn},
	}
	rt, err := embed.New(ctx, embed.Options{Options: embedded.Options{Config: cfg, River: embedded.RiverManagedByOpenRails()}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	// API mode still allows the bare identity bind.
	id, err := rt.UpsertMerchantConfig(ctx, slug, embed.MerchantConfig{DisplayName: slug})
	require.NoError(t, err)
	// Arm the store-backed merchants service the way worker registration does.
	rt.Embedded().App().Runtime.EnsureMerchantsService(ctx)
	require.NotNil(t, rt.Embedded().App().Runtime.Merchants)
	t.Cleanup(func() {
		for _, stmt := range []string{
			`DELETE FROM openrails.merchant_secrets WHERE merchant_id = $1`,
			`DELETE FROM openrails.psps WHERE merchant_id = $1`,
			`DELETE FROM openrails.merchants WHERE id = $1`,
		} {
			_, _ = appDB.Pool().Exec(context.Background(), stmt, id.UUID())
		}
	})

	handler, err := embedded.MountHandler(rt.Embedded(), embedded.MountOptions{
		RouteSets:      []embed.RouteSet{embed.RouteSetPaymentProviders},
		Gate:           allowAllGate{id: id},
		ProviderRoutes: &embedded.ProviderRoutes{Webhooks: true},
	})
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	payload := fmt.Sprintf(`{"environment":"live","account_id":"acct_%d","credentials":{"webhook_signing_secret":"whsec_%d"}}`, nano, nano)
	req, err := http.NewRequest(http.MethodPut, server.URL+"/v1/merchant/payment-providers/stripe", strings.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotZero(t, merchantSecretRowCount(t, appDB.Pool(), ctx, id), "api mode persists to the store")
}

// TestManifestMode_APIModeRefusesManifestTruth: matrix row — api mode +
// manifest bytes passed refuses (two truths). The bare bind stays legal.
func TestManifestMode_APIModeRefusesManifestTruth(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)

	nano := time.Now().UnixNano()
	slug := fmt.Sprintf("mrefuse%d", nano)
	cfg := &config.Config{
		Env:            "dev",
		TestMode:       config.CredentialPostureLive,
		MerchantSource: config.MerchantSourceAPI,
		DB:             &config.DBConfig{URL: dsn},
	}
	rt, err := embed.New(ctx, embed.Options{Options: embedded.Options{Config: cfg, River: embedded.RiverManagedByOpenRails()}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	_, err = rt.UpsertMerchantConfig(ctx, slug, embed.MerchantConfig{
		DisplayName: slug,
		PSPs: map[string]embed.PSPConfig{
			"mobius": {"nmi": {AccountID: "579145", Secrets: map[string]string{"security_key": "k"}}},
		},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "two truths")
}

// TestManifestMode_MalformedManifestRefusesBoot: matrix row — manifest mode
// with unresolvable declared truth refuses.
func TestManifestMode_MalformedManifestRefusesBoot(t *testing.T) {
	_, err := embed.LoadMerchantConfigManifest([]byte("version: 1\nmerchants: {}\n"))
	require.Error(t, err, "a manifest declaring no merchants is refused")
	_, err = embed.LoadMerchantConfigManifest([]byte("merchants:\n  x:\n    acounts: {}\n"))
	require.Error(t, err, "a typo'd manifest field is refused, never dropped")
}

// TestManifestMode_MissingSecretFailsClosed: a declared account whose secret is
// absent from files/env arms nothing for that rail (loud WARN upstream), boot
// proceeds, and a charge on the rail fails closed.
func TestManifestMode_MissingSecretFailsClosed(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	appDB := dbtest.OpenAppDB(t, dsn)

	nano := time.Now().UnixNano()
	slug := fmt.Sprintf("mmiss%d", nano)
	gatewayID := fmt.Sprintf("58%d", nano%1_000_000)
	cfg := manifestModeConfig(dsn)

	manifest, err := embed.LoadMerchantConfigManifest(manifestModeManifestYAML(slug, gatewayID))
	require.NoError(t, err)
	rt, err := embed.New(ctx, embed.Options{Options: embedded.Options{Config: cfg, River: embedded.RiverManagedByOpenRails()}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	// Boot proceeds: the account row reconciles with NO secret anywhere.
	id, err := rt.UpsertMerchantConfig(ctx, slug, manifest.Merchants[slug])
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, stmt := range []string{
			`DELETE FROM openrails.psps WHERE merchant_id = $1`,
			`DELETE FROM openrails.merchants WHERE id = $1`,
		} {
			_, _ = appDB.Pool().Exec(context.Background(), stmt, id.UUID())
		}
	})

	runtime := rt.Embedded().App().Runtime
	armed := reconcile.MerchantFetcherBuilder{
		Config:    runtime.Config,
		Merchants: runtime.Merchants,
		DB:        runtime.DB,
	}.Build(merchant.WithID(ctx, id), id)
	require.NotContains(t, armed.Fetchers, reconcile.ProviderNMI, "declared account with a missing secret arms nothing")

	// Charge fails CLOSED (#725: declared-account-with-missing-secret never
	// falls back to the boot plane).
	err = chargeViaStorePlane(t, ctx, rt, id, "http://127.0.0.1:0")
	require.Error(t, err)
	require.True(t, errors.Is(err, merchants.ErrSecretNotFound) || strings.Contains(err.Error(), "security_key"),
		"fail-closed error names the missing credential: %v", err)
}

// TestManifestMode_ReadSideBindKeepsWorking: hentai0's shape — an empty
// MerchantConfig upsert binds to an existing merchant, declares no truth, and
// stays legal in manifest mode.
func TestManifestMode_ReadSideBindKeepsWorking(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	appDB := dbtest.OpenAppDB(t, dsn)

	nano := time.Now().UnixNano()
	slug := fmt.Sprintf("mbind%d", nano)

	// Writer host (doujins shape) declares the merchant.
	writerCfg := manifestModeConfig(dsn)
	writer, err := embed.New(ctx, embed.Options{Options: embedded.Options{Config: writerCfg, River: embedded.RiverManagedByOpenRails()}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = writer.Close(context.Background()) })
	id, err := writer.UpsertMerchantConfig(ctx, slug, embed.MerchantConfig{DisplayName: slug})
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = appDB.Pool().Exec(context.Background(), `DELETE FROM openrails.merchants WHERE id = $1`, id.UUID())
	})

	// Reader host (hentai0 shape): empty MerchantConfig — pure bind.
	readerCfg := manifestModeConfig(dsn)
	reader, err := embed.New(ctx, embed.Options{Options: embedded.Options{Config: readerCfg, River: embedded.RiverManagedByOpenRails()}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = reader.Close(context.Background()) })
	boundID, err := reader.UpsertMerchantConfig(ctx, slug, embed.MerchantConfig{})
	require.NoError(t, err)
	require.Equal(t, id, boundID, "the empty upsert binds to the same merchant")
	require.Equal(t, boundID, reader.Embedded().App().Runtime.ConfiguredMerchant())
}

// bootManifestRuntimeWithRailAccounts boots a MODE-1 runtime declaring
// railAccounts through UpsertMerchantConfig directly (no manifest bytes/YAML,
// no catalog) — the same doujins/hentai0 shape as
// TestEmbeddedPullArming_ManifestSecretsNoPaymentProviders. Options.PaymentProviders
// is deliberately unset, so Runtime.Rails (the legacy boot-config bridge) stays
// empty while Runtime.Merchants arms from the DB-projected accounts (#775).
func bootManifestRuntimeWithRailAccounts(t *testing.T, ctx context.Context, dsn, slug string, railAccounts map[string]embed.PSPConfig) (*embed.Runtime, merchant.ID) {
	t.Helper()
	appDB := dbtest.OpenAppDB(t, dsn)
	cfg := manifestModeConfig(dsn)
	rt, err := embed.New(ctx, embed.Options{Options: embedded.Options{Config: cfg, River: embedded.RiverManagedByOpenRails()}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	id, err := rt.UpsertMerchantConfig(ctx, slug, embed.MerchantConfig{
		DisplayName: slug,
		PSPs:        railAccounts,
	})
	require.NoError(t, err)
	require.False(t, id.IsZero())
	t.Cleanup(func() {
		for _, stmt := range []string{
			`DELETE FROM openrails.psps WHERE merchant_id = $1`,
			`DELETE FROM openrails.merchants WHERE id = $1`,
		} {
			_, _ = appDB.Pool().Exec(context.Background(), stmt, id.UUID())
		}
	})
	return rt, id
}

// TestManifestMode_CheckoutPreGateAcceptsDBArmedRail is #775's RED/GREEN pin:
// before the fix, checkoutRailConfigured only consulted the DB for NMI, so a
// MODE-1 host with a manifest-armed ccbill account (and no
// embedded.Options.PaymentProviders, since manifest hosts deliberately don't
// use it) 400s "unsupported rail" on POST /v1/me/checkout with rail=ccbill —
// even though the deeper checkout service would resolve the account fine.
//
// A deliberately-nonexistent price_id is used: the point of this test is the
// PRE-GATE boundary, not a full checkout — proving the request reaches
// price-lookup validation (a DIFFERENT 400 message) instead of being rejected
// by the rail pre-gate is exactly what distinguishes the two behaviors.
func TestManifestMode_CheckoutPreGateAcceptsDBArmedRail(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)

	nano := time.Now().UnixNano()
	slug := fmt.Sprintf("mccbill%d", nano)
	ccbillAccount := fmt.Sprintf("92%04d-0000", nano%10_000)

	rt, id := bootManifestRuntimeWithRailAccounts(t, ctx, dsn, slug, map[string]embed.PSPConfig{
		"ccbill": {
			"ccbill": {
				AccountID: ccbillAccount,
				Secrets: map[string]string{
					"datalink_username": "dl-user-" + slug,
					"datalink_password": "dl-pass-" + slug,
				},
			},
		},
	})

	authn := billingauth.DelegatedAuthenticatorFunc(func(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
		return &billingauth.DelegatedPrincipal{MerchantID: id.UUID().String(), SubjectID: uuid.NewString()}, nil
	})
	handler, err := embedded.MountHandler(rt.Embedded(), embedded.MountOptions{
		RouteSets:              []embed.RouteSet{embed.RouteSetCustomer},
		DelegatedAuthenticator: authn,
	})
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	body := fmt.Sprintf(`{"price_id":%q,"payment":{"rail":"ccbill"}}`, api.FormatPriceID(uuid.New()))
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/me/checkout", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer host-credential")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(t, http.StatusBadRequest, resp.StatusCode, string(raw))
	require.NotContains(t, string(raw), "unsupported rail", "the ccbill pre-gate must pass: an active manifest/DB-armed ccbill account exists")
	require.Contains(t, string(raw), "price not found", "past the pre-gate, the request fails deeper on the fabricated price_id")
}

// TestManifestMode_ProviderRoutesDeriveWebhooksFromDBArmedAccounts is #775's
// other RED/GREEN pin: ProviderRoutesForRuntime derived the mounted provider
// surface from the empty Rails bridge alone, silently dropping the webhook
// routes for a MODE-1 host with a manifest-armed rail account. With the fix,
// an armed ccbill account is enough to mount the merchant webhook route: the
// request reaches the ccbill handler (403 IP-allowlist, since this isn't a
// real CCBill source) instead of 404 (route never registered).
func TestManifestMode_ProviderRoutesDeriveWebhooksFromDBArmedAccounts(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)

	nano := time.Now().UnixNano()
	slug := fmt.Sprintf("mwebhook%d", nano)
	ccbillAccount := fmt.Sprintf("93%04d-0000", nano%10_000)

	rt, _ := bootManifestRuntimeWithRailAccounts(t, ctx, dsn, slug, map[string]embed.PSPConfig{
		"ccbill": {
			"ccbill": {
				AccountID: ccbillAccount,
				Secrets: map[string]string{
					"datalink_username": "dl-user-" + slug,
					"datalink_password": "dl-pass-" + slug,
				},
			},
		},
	})

	// RouteSetWebhooks alone needs neither Gate nor Authenticator
	// (validateAuthBoundary only requires them for checkout/customer/merchant-admin
	// route sets) — and ProviderRoutes is left nil, so MountHandler must derive it
	// via ProviderRoutesForRuntime.
	handler, err := embedded.MountHandler(rt.Embedded(), embedded.MountOptions{
		RouteSets: []embed.RouteSet{embed.RouteSetWebhooks},
	})
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/merchants/"+slug+"/webhooks/ccbill", strings.NewReader("{}"))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.NotEqual(t, http.StatusNotFound, resp.StatusCode, string(raw))
	require.Equal(t, http.StatusForbidden, resp.StatusCode, string(raw))
	require.Contains(t, string(raw), "Unauthorized webhook source", "the route must be mounted and resolve the merchant slug to reach the ccbill IP-allowlist check")
}
