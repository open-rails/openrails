//go:build integration

package riverjobs

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	log "github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #699 integration: the provider-refresh job arms fetchers/probers PER
// MERCHANT from the merchant-secrets store (manifest-seeded shape), with NO
// boot-config rails. Fake provider HTTP servers are the only fakes.

// fakeNMIPullServer serves the pull surface the NMI fetcher touches (v5
// /customers + /subscriptions GETs, classic query.php POST) and records the
// credential presented on every request.
type fakeNMIPullServer struct {
	srv *httptest.Server

	mu   sync.Mutex
	keys []string
}

func newFakeNMIPullServer(t *testing.T) *fakeNMIPullServer {
	t.Helper()
	f := &fakeNMIPullServer{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			// v5: the bare security key is the whole Authorization value.
			f.record(r.Header.Get("Authorization"))
			switch {
			case r.URL.Path == "/subscriptions":
				_, _ = w.Write([]byte(`{"subscriptions":[],"next_cursor":null,"has_more":false,"per_page":100}`))
			case r.URL.Path == "/customers":
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

func (f *fakeNMIPullServer) record(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, strings.TrimSpace(key))
}

func (f *fakeNMIPullServer) seenKeys() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]int{}
	for _, k := range f.keys {
		out[k]++
	}
	return out
}

type dataLinkCredential struct {
	Username, Password, ClientAccNum, ClientSubAcc string
}

// fakeDataLinkServer serves /data/main.cgi (ACTIVEMEMBERS roster + transaction
// exports) and records the credentials of every request. The roster carries
// ONE active member (railSubID, rebilling on rebillDate) so the export parses
// like a real DataLink answer.
type fakeDataLinkServer struct {
	srv       *httptest.Server
	railSubID string

	mu    sync.Mutex
	creds []dataLinkCredential
}

func newFakeDataLinkServer(t *testing.T, railSubID string, rebillDate string) *fakeDataLinkServer {
	t.Helper()
	f := &fakeDataLinkServer{railSubID: railSubID}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		f.mu.Lock()
		f.creds = append(f.creds, dataLinkCredential{
			Username:     r.Form.Get("username"),
			Password:     r.Form.Get("password"),
			ClientAccNum: r.Form.Get("clientAccnum"),
			ClientSubAcc: r.Form.Get("clientSubacc"),
		})
		f.mu.Unlock()
		if r.Form.Get("transactionTypes") == "ACTIVEMEMBERS" {
			_, _ = w.Write([]byte(`"ACTIVEMEMBERS","945281","plan-x","` + railSubID + `","2026-05-03","user","u@example.com","1","` + rebillDate + `","` + rebillDate + `"`))
			return
		}
		// Transaction-type export with no events in the window: empty body.
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDataLinkServer) seenCreds() []dataLinkCredential {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dataLinkCredential(nil), f.creds...)
}

// seedPullMerchant inserts a fresh merchant row and registers full cleanup of
// everything the refresh pass writes for it (self-cleaning suite).
func seedPullMerchant(t *testing.T, dbi *db.DB, slug string) merchant.ID {
	t.Helper()
	ctx := context.Background()
	id := merchant.ID(uuid.New())
	_, err := dbi.Pool().Exec(ctx,
		`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
		id.UUID(), slug)
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, stmt := range []string{
			`DELETE FROM openrails.reconciliation_findings WHERE merchant_id = $1`,
			`DELETE FROM openrails.reconciliation_runs WHERE merchant_id = $1`,
			`DELETE FROM openrails.rail_refresh_watermarks WHERE merchant_id = $1`,
			`DELETE FROM openrails.psps WHERE merchant_id = $1`,
			`DELETE FROM openrails.subscriptions WHERE merchant_id = $1`,
			`DELETE FROM openrails.prices WHERE merchant_id = $1`,
			`DELETE FROM openrails.products WHERE merchant_id = $1`,
			`DELETE FROM openrails.customers WHERE merchant_id = $1`,
			`DELETE FROM openrails.merchants WHERE id = $1`,
		} {
			_, _ = dbi.Pool().Exec(context.Background(), stmt, id.UUID())
		}
	})
	return id
}

// seedLocalCCBillSub seeds one local ACTIVE ccbill subscription matching the
// fake DataLink roster (so the pull observes zero drift). periodEnd must equal
// the roster's rebill date at midnight UTC.
func seedLocalCCBillSub(t *testing.T, dbi *db.DB, mid merchant.ID, railSubID string, periodEnd time.Time) {
	t.Helper()
	ctx := merchant.WithID(context.Background(), mid)
	require.NoError(t, dbi.RunInMerchantConn(ctx, func(ctx context.Context) error {
		cust, prod, price := uuid.New(), uuid.New(), uuid.New()
		subject := cust.String()
		exec := func(sql string, args ...any) {
			_, err := dbi.Qx(ctx).Exec(ctx, sql, args...)
			require.NoError(t, err)
		}
		exec(`INSERT INTO openrails.customers (id, merchant_id, subject) VALUES ($1, $2, $3)`, cust, mid.UUID(), subject)
		exec(`INSERT INTO openrails.products (id, key, display_name, entitlements_spec, merchant_id) VALUES ($1, $2, $2, '{}'::jsonb, $3)`,
			prod, "pull-cc-"+railSubID, mid.UUID())
		exec(`INSERT INTO openrails.prices (id, product_id, amount, currency, merchant_id) VALUES ($1, $2, 5000000, 'usd', $3)`,
			price, prod, mid.UUID())
		exec(`INSERT INTO openrails.subscriptions (id, merchant_id, customer_id, product_id, price_id, status, rail, rail_subscription_id, started_at, current_period_starts_at, current_period_ends_at, entitlements_spec_snapshot)
		      VALUES ($1, $2, $3, $4, $5, 'active', 'ccbill', $6, $7, $7, $8, '{}'::jsonb)`,
			uuid.New(), mid.UUID(), cust, prod, price, railSubID, periodEnd.Add(-30*24*time.Hour), periodEnd)
		return nil
	}))
}

// seedProviderAccount declares one provider account + scoped secrets through
// the SAME service surface the merchant manifest funnels into.
func seedProviderAccount(t *testing.T, svc *merchants.Service, mid merchant.ID, rail, accountID string, credentials map[string]string) {
	t.Helper()
	_, err := svc.UpsertPaymentProviderConfig(context.Background(), mid, rail, merchants.UpsertPaymentProviderConfigRequest{
		Environment: "live",
		AccountID:   accountID,
		Credentials: credentials,
	})
	require.NoError(t, err)
}

func pullTestMerchantsService(t *testing.T, dbi *db.DB) *merchants.Service {
	t.Helper()
	svc, err := merchants.NewService(dbi.DataPool(), merchants.NewMemorySecretStore(), "live")
	require.NoError(t, err)
	return svc
}

func newStorePullWorker(dbi *db.DB, svc *merchants.Service, nmiSrv *fakeNMIPullServer, dlSrv *fakeDataLinkServer, mode string) *ProviderRefreshWorker {
	w := &ProviderRefreshWorker{
		DB:        dbi,
		Config:    &config.Config{Env: "dev", ProviderWriteMode: mode},
		Merchants: svc,
		Clock:     clockwork.NewFakeClockAt(time.Now().UTC()),
		// One window: [now-4h, now-1h].
		Window:          24 * time.Hour,
		SafetyLag:       time.Hour,
		InitialLookback: 3 * time.Hour,
		MaxWindows:      2,
	}
	if nmiSrv != nil {
		w.PullEndpoints.NMIQueryURL = nmiSrv.srv.URL
		w.PullEndpoints.NMIV5BaseURL = nmiSrv.srv.URL
	}
	if dlSrv != nil {
		w.PullEndpoints.CCBillDataLinkBaseURL = dlSrv.srv.URL
	}
	return w
}

// loadPullWatermark returns the SUCCESSFUL watermark: a failure row (last_error
// set, never succeeded) does not count as an advanced watermark.
func loadPullWatermark(t *testing.T, dbi *db.DB, mid merchant.ID, provider string) (time.Time, bool) {
	t.Helper()
	var watermark time.Time
	err := dbi.Pool().QueryRow(context.Background(), `
		SELECT watermark_at FROM openrails.rail_refresh_watermarks
		 WHERE merchant_id = $1 AND rail = $2 AND event_domain = 'events'
		   AND last_succeeded_at IS NOT NULL AND last_error IS NULL`,
		mid.UUID(), provider).Scan(&watermark)
	if err != nil {
		return time.Time{}, false
	}
	return watermark.UTC(), true
}

// Manifest-seeded ccbill+nmi secrets, NO boot rails: the refresh job builds the
// merchant's fetchers from the store, pulls from the fake servers with each
// merchant's OWN credentials, and advances rail_refresh_watermarks. Two
// merchants with different credentials pull inside their own scopes.
func TestProviderRefresh_ArmsFromMerchantStore_NoBootRails(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	svc := pullTestMerchantsService(t, dbi)
	sfx := uuid.NewString()[:8]

	midA := seedPullMerchant(t, dbi, "pull-a-"+sfx)
	midB := seedPullMerchant(t, dbi, "pull-b-"+sfx)

	keyA, keyB := "sec-key-a-"+sfx, "sec-key-b-"+sfx
	seedProviderAccount(t, svc, midA, "nmi", "9911"+sfx, map[string]string{"security_key": keyA})
	seedProviderAccount(t, svc, midA, "ccbill", "945281-0000", map[string]string{
		"datalink_username": "dl-user-" + sfx,
		"datalink_password": "dl-pass-" + sfx,
	})
	seedProviderAccount(t, svc, midB, "nmi", "9922"+sfx, map[string]string{"security_key": keyB})

	// Fake roster member matching a seeded local sub → drift-free pull.
	railSubID := "0125" + sfx
	rebill := time.Now().UTC().AddDate(0, 0, 20).Truncate(24 * time.Hour)
	seedLocalCCBillSub(t, dbi, midA, railSubID, rebill)

	nmiSrv := newFakeNMIPullServer(t)
	dlSrv := newFakeDataLinkServer(t, railSubID, rebill.Format("2006-01-02"))

	// NO boot rails: Rails/NMIClients/CCBillDataLink/SolanaRPC all zero.
	// #719: one job per merchant — the scheduler fans these out in production.
	worker := newStorePullWorker(dbi, svc, nmiSrv, dlSrv, config.ProviderWriteModeLimited)
	for _, mid := range []merchant.ID{midA, midB} {
		require.NoError(t, worker.Work(context.Background(), &river.Job[ProviderRefreshMerchantArgs]{Args: ProviderRefreshMerchantArgs{MerchantID: mid.UUID()}}))
	}

	// Watermarks advanced per merchant+rail.
	horizon := worker.now().Add(-worker.safetyLag())
	for _, tc := range []struct {
		mid      merchant.ID
		provider string
	}{
		{midA, "nmi"}, {midA, "ccbill"}, {midB, "nmi"},
	} {
		wm, ok := loadPullWatermark(t, dbi, tc.mid, tc.provider)
		require.True(t, ok, "watermark row for %s/%s", tc.mid, tc.provider)
		// Postgres stores microseconds — compare within that grain.
		assert.WithinDuration(t, horizon, wm, time.Millisecond, "watermark advanced to horizon for %s/%s", tc.mid, tc.provider)
	}

	// Each merchant pulled with its OWN credentials; nothing else leaked.
	keys := nmiSrv.seenKeys()
	assert.Positive(t, keys[keyA], "fake NMI saw merchant A's security key")
	assert.Positive(t, keys[keyB], "fake NMI saw merchant B's security key")
	for k := range keys {
		assert.Contains(t, []string{keyA, keyB}, k, "no cross-merchant or fabricated NMI credential")
	}
	creds := dlSrv.seenCreds()
	require.NotEmpty(t, creds, "fake DataLink saw merchant A's pulls")
	for _, c := range creds {
		assert.Equal(t, "dl-user-"+sfx, c.Username)
		assert.Equal(t, "dl-pass-"+sfx, c.Password)
		assert.Equal(t, "945281", c.ClientAccNum, "#697 dash account id split into accnum")
		assert.Equal(t, "0000", c.ClientSubAcc)
	}

}

// One secret missing: that rail is absent for the merchant, with ONE WARN
// naming merchant, rail and the missing secret key; the other rail pulls.
func TestProviderRefresh_MissingSecret_RailAbsentWithWarn(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	svc := pullTestMerchantsService(t, dbi)
	sfx := uuid.NewString()[:8]

	mid := seedPullMerchant(t, dbi, "pull-miss-"+sfx)
	// NMI account declared with NO security_key seeded; ccbill fully seeded.
	seedProviderAccount(t, svc, mid, "nmi", "9933"+sfx, nil)
	seedProviderAccount(t, svc, mid, "ccbill", "945282-0000", map[string]string{
		"datalink_username": "dl-user2-" + sfx,
		"datalink_password": "dl-pass2-" + sfx,
	})

	railSubID := "0126" + sfx
	rebill := time.Now().UTC().AddDate(0, 0, 20).Truncate(24 * time.Hour)
	seedLocalCCBillSub(t, dbi, mid, railSubID, rebill)

	nmiSrv := newFakeNMIPullServer(t)
	dlSrv := newFakeDataLinkServer(t, railSubID, rebill.Format("2006-01-02"))

	hook := logrustest.NewGlobal()
	defer hook.Reset()

	worker := newStorePullWorker(dbi, svc, nmiSrv, dlSrv, config.ProviderWriteModeLimited)
	require.NoError(t, worker.Work(context.Background(), &river.Job[ProviderRefreshMerchantArgs]{Args: ProviderRefreshMerchantArgs{MerchantID: mid.UUID()}}))

	// NMI absent: no watermark, no fake-server traffic.
	_, ok := loadPullWatermark(t, dbi, mid, "nmi")
	assert.False(t, ok, "nmi watermark must not exist without credentials")
	assert.Empty(t, nmiSrv.seenKeys(), "no NMI request may be fabricated without the security key")

	// CCBill unaffected.
	_, ok = loadPullWatermark(t, dbi, mid, "ccbill")
	assert.True(t, ok, "ccbill watermark advanced")
	assert.NotEmpty(t, dlSrv.seenCreds())

	// The WARN names merchant, rail and missing secret.
	found := false
	for _, e := range hook.AllEntries() {
		if e.Level != log.WarnLevel || !strings.Contains(e.Message, "merchant secret missing") {
			continue
		}
		if e.Data["merchant_id"] == mid.String() && e.Data["rail"] == "nmi" {
			secret, _ := e.Data["secret"].(string)
			assert.Contains(t, secret, "security_key")
			found = true
		}
	}
	assert.True(t, found, "WARN naming merchant, rail and missing secret key")
}

// provider_write_mode=readonly still skips the refresh job entirely (existing
// behavior pinned): no merchant iteration, no provider traffic, no watermarks.
func TestProviderRefresh_ReadonlySkipsStoreArmedPulls(t *testing.T) {
	dsn := dbtest.SharedPostgresDSN(t)
	dbi := dbtest.OpenAppDB(t, dsn)
	svc := pullTestMerchantsService(t, dbi)
	sfx := uuid.NewString()[:8]

	mid := seedPullMerchant(t, dbi, "pull-ro-"+sfx)
	seedProviderAccount(t, svc, mid, "nmi", "9944"+sfx, map[string]string{"security_key": "sec-ro-" + sfx})

	nmiSrv := newFakeNMIPullServer(t)

	worker := newStorePullWorker(dbi, svc, nmiSrv, nil, config.ProviderWriteModeReadOnly)
	require.NoError(t, worker.Work(context.Background(), &river.Job[ProviderRefreshMerchantArgs]{Args: ProviderRefreshMerchantArgs{MerchantID: mid.UUID()}}))

	assert.Empty(t, nmiSrv.seenKeys(), "readonly mode: pure observer, no pulls")
	_, ok := loadPullWatermark(t, dbi, mid, "nmi")
	assert.False(t, ok, "readonly mode: no watermark rows")
}
