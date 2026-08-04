//go:build integration

package embed_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/riverqueue/river"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/pkg/embedded"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #699: a doujins-shaped embedded boot — NO Options.PaymentProviders, provider
// credentials declared ONLY through the merchant manifest (UpsertMerchantConfig
// seeds the per-merchant secrets store) — arms the pull plane: worker
// registration builds the merchants service, and the per-merchant builder
// yields fetchers + probers for the seeded rails with the seeded credentials.
func TestEmbeddedPullArming_ManifestSecretsNoPaymentProviders(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	appDB := dbtest.OpenAppDB(t, dsn)

	nano := time.Now().UnixNano()
	slug := fmt.Sprintf("embed-pull-arm-%d", nano)
	nmiAccount := fmt.Sprintf("77%d", nano%1_000_000)
	ccbillAccount := fmt.Sprintf("91%04d-0000", nano%10_000)
	securityKey := fmt.Sprintf("sec-key-%d", nano)

	cfg := &config.Config{Env: "dev", TestMode: config.CredentialPostureLive, DB: &config.DBConfig{URL: dsn}}
	rt, err := embed.New(ctx, embed.Options{
		Options: embedded.Options{Config: cfg, River: embedded.RiverManagedByOpenRails()}, // deliberately NO PaymentProviders
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	m := embed.MerchantConfig{
		DisplayName: slug,
		PSPs: map[string]embed.PSPConfig{
			"mobius": {
				"nmi": {
					Environment: "live",
					AccountID:   nmiAccount,
					Secrets:     map[string]string{"security_key": securityKey},
				},
			},
			"ccbill": {
				"ccbill": {
					Environment: "live",
					AccountID:   ccbillAccount,
					Secrets: map[string]string{
						"datalink_username": "dl-user-" + slug,
						"datalink_password": "dl-pass-" + slug,
					},
				},
			},
		},
	}
	id, err := rt.UpsertMerchantConfig(ctx, slug, m)
	require.NoError(t, err)
	require.False(t, id.IsZero())
	t.Cleanup(func() {
		for _, stmt := range []string{
			`DELETE FROM openrails.merchant_secrets WHERE merchant_id = $1`,
			`DELETE FROM openrails.psps WHERE merchant_id = $1`,
			`DELETE FROM openrails.merchants WHERE id = $1`,
		} {
			_, _ = appDB.Pool().Exec(context.Background(), stmt, id.UUID())
		}
	})

	runtime := rt.Embedded().App().Runtime
	require.NotNil(t, runtime)

	// Worker registration is where hosts fold OpenRails' workers into their
	// River client (doujins RegisterRiverWorkers) — it must arm the merchants
	// service even though no standalone HTTP server ever runs.
	require.NoError(t, runtime.AddBillingWorkersTo(ctx, river.NewWorkers()))
	require.NotNil(t, runtime.Merchants, "#699: worker registration builds the merchants service from the store")

	// The per-merchant pull plane the refresh job builds inside the merchant
	// loop: manifest-seeded credentials arm fetchers AND probers (the
	// unknown-cohort resolution inputs) with no boot-config rails.
	armed := reconcile.MerchantFetcherBuilder{
		Config:    runtime.Config,
		Merchants: runtime.Merchants,
		DB:        runtime.DB,
	}.Build(merchant.WithID(ctx, id), id)

	require.Contains(t, armed.Fetchers, reconcile.ProviderNMI, "NMI fetcher armed from manifest secrets")
	require.Contains(t, armed.Fetchers, reconcile.ProviderCCBill, "CCBill fetcher armed from manifest secrets")

	nmiProber, ok := armed.Probers[reconcile.ProviderNMI].(*reconcile.NMISubscriptionProber)
	require.True(t, ok, "NMI per-subscription prober armed")
	assert.Equal(t, securityKey, nmiProber.Client.SecurityKey, "prober carries the merchant's own key")

	require.Contains(t, armed.Probers, reconcile.ProviderCCBill, "CCBill per-subscription prober armed (#696)")
	require.NotNil(t, armed.CCBillDataLink, "DataLink bulk lane armed")
	assert.Equal(t, "dl-user-"+slug, armed.CCBillDataLink.Username)
	assert.Equal(t, "dl-pass-"+slug, armed.CCBillDataLink.Password)
}

// pullFakeNMI serves the NMI pull surface (v5 /subscriptions + /customers
// GETs, classic query.php POST) and records the credential on every request.
type pullFakeNMI struct {
	srv *httptest.Server

	mu   sync.Mutex
	keys []string
}

func newPullFakeNMI(t *testing.T) *pullFakeNMI {
	t.Helper()
	f := &pullFakeNMI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// v5: the bare security key is the whole Authorization value.
			f.record(r.Header.Get("Authorization"))
			switch r.URL.Path {
			case "/subscriptions":
				_, _ = w.Write([]byte(`{"subscriptions":[],"next_cursor":null,"has_more":false,"per_page":100}`))
			case "/customers":
				_, _ = w.Write([]byte(`{"customers":[],"next_cursor":null,"has_more":false,"per_page":100}`))
			default:
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}
		require.NoError(t, r.ParseForm())
		f.record(r.Form.Get("security_key"))
		_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><nm_response></nm_response>`))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *pullFakeNMI) record(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if k := strings.TrimSpace(key); k != "" {
		f.keys = append(f.keys, k)
	}
}

func (f *pullFakeNMI) sawKey(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range f.keys {
		if k == key {
			return true
		}
	}
	return false
}

// pullCLIManifestMerchant provisions one MODE-1 merchant the doujins way
// (embed.New + UpsertMerchantConfig — DB projections only, secrets in the
// server's memory) and tears the server down, leaving the one-off-CLI shape:
// rows on disk, NO store secrets, manifest as the only credential source.
func pullCLIManifestMerchant(t *testing.T, ctx context.Context, dsn, slug string, m embed.MerchantConfig) merchant.ID {
	t.Helper()
	appDB := dbtest.OpenAppDB(t, dsn)
	cfg := &config.Config{Env: "dev", TestMode: config.CredentialPostureLive, DB: &config.DBConfig{URL: dsn}}
	rt, err := embed.New(ctx, embed.Options{Options: embedded.Options{Config: cfg, River: embedded.RiverManagedByOpenRails()}})
	require.NoError(t, err)
	id, err := rt.UpsertMerchantConfig(ctx, slug, m)
	require.NoError(t, err)
	require.NoError(t, rt.Close(ctx))
	t.Cleanup(func() {
		for _, stmt := range []string{
			`DELETE FROM openrails.reconciliation_findings WHERE merchant_id = $1`,
			`DELETE FROM openrails.reconciliation_runs WHERE merchant_id = $1`,
			`DELETE FROM openrails.merchant_secrets WHERE merchant_id = $1`,
			`DELETE FROM openrails.psps WHERE merchant_id = $1`,
			`DELETE FROM openrails.merchants WHERE id = $1`,
		} {
			_, _ = appDB.Pool().Exec(context.Background(), stmt, id.UUID())
		}
	})
	// STORE-ABSENT precondition: mode 1 never wrote openrails.merchant_secrets.
	var n int
	require.NoError(t, appDB.Pool().QueryRow(ctx, `SELECT count(*) FROM openrails.merchant_secrets WHERE merchant_id = $1`, id.UUID()).Scan(&n))
	require.Zero(t, n, "mode 1 must not persist merchant secrets")
	return id
}

// #723 follow-up: the pull-provider CLI lane in MODE 1 arms fetchers from the
// ON-DISK manifest's ephemeral in-memory plane — the same #699 semantics as
// the server's River pulls — instead of the old WARN + boot-plane-only path.
// The fake NMI server must see the manifest-declared credential.
func TestPullProviderCLI_ManifestModeArmsFromManifestPlane(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)

	nano := time.Now().UnixNano()
	slug := fmt.Sprintf("cli-pull-m1-%d", nano)
	nmiAccount := fmt.Sprintf("78%d", nano%1_000_000)
	securityKey := fmt.Sprintf("cli-sec-key-%d", nano)

	m := embed.MerchantConfig{
		DisplayName: slug,
		PSPs: map[string]embed.PSPConfig{
			"mobius": {
				"nmi": {
					Environment: "live",
					AccountID:   nmiAccount,
					Secrets:     map[string]string{"security_key": securityKey},
				},
			},
		},
	}
	pullCLIManifestMerchant(t, ctx, dsn, slug, m)

	fake := newPullFakeNMI(t)
	var out bytes.Buffer
	require.NoError(t, embedded.PullProvider(ctx, embedded.PullProviderOptions{
		Config:           &config.Config{Env: "dev", TestMode: config.CredentialPostureLive, DB: &config.DBConfig{URL: dsn}},
		MerchantSlug:     slug,
		Providers:        []string{"nmi"},
		LogDir:           t.TempDir(),
		Out:              &out,
		MerchantManifest: &embedded.BillingConfig{Version: 1, Merchants: map[string]embedded.MerchantConfig{slug: m}},
		Endpoints: reconcile.ProviderEndpoints{
			NMIQueryURL:  fake.srv.URL,
			NMIV5BaseURL: fake.srv.URL,
		},
	}))
	require.True(t, fake.sawKey(securityKey),
		"fake NMI must see the manifest-declared credential: the CLI armed from the manifest plane, not boot-config rails")
	require.Contains(t, out.String(), "pull-provider run")
}

// #699/#723: a DECLARED account whose secret is absent from the manifest is a
// rail NOT armed (one loud warn) — never a cross-plane fallback. With no other
// rails the CLI run refuses.
func TestPullProviderCLI_ManifestModeMissingSecretRailNotArmed(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)

	nano := time.Now().UnixNano()
	slug := fmt.Sprintf("cli-pull-m1-miss-%d", nano)
	nmiAccount := fmt.Sprintf("79%d", nano%1_000_000)

	// Declared account, NO security_key anywhere.
	m := embed.MerchantConfig{
		DisplayName: slug,
		PSPs: map[string]embed.PSPConfig{
			"mobius": {
				"nmi": {
					Environment: "live",
					AccountID:   nmiAccount,
				},
			},
		},
	}
	pullCLIManifestMerchant(t, ctx, dsn, slug, m)

	hook := logrustest.NewGlobal()
	defer hook.Reset()
	err := embedded.PullProvider(ctx, embedded.PullProviderOptions{
		Config:           &config.Config{Env: "dev", TestMode: config.CredentialPostureLive, DB: &config.DBConfig{URL: dsn}},
		MerchantSlug:     slug,
		Providers:        []string{"nmi"},
		LogDir:           t.TempDir(),
		MerchantManifest: &embedded.BillingConfig{Version: 1, Merchants: map[string]embedded.MerchantConfig{slug: m}},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no payment providers configured")

	warned := false
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message, "merchant secret missing") {
			warned = true
			break
		}
	}
	require.True(t, warned, "declared-account-missing-secret must warn loudly (#699)")
}
