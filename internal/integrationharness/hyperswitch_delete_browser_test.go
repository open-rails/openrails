//go:build integration && browser && hyperswitch

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/dbtest"
	embedauth "github.com/open-rails/openrails/internal/hostauth"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

// Only the exact native DELETE reply is lost. Browser/PAN traffic stays on the
// vendor edge, and every unrelated HTTP exchange uses the original transport.
type lostNativeDeleteReply struct {
	base    http.RoundTripper
	target  string
	dropped atomic.Bool
}

func (r *lostNativeDeleteReply) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := r.base.RoundTrip(request)
	if err == nil && request.Method == http.MethodDelete && request.URL.String() == r.target && response.StatusCode == http.StatusOK && !r.dropped.Swap(true) {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		return nil, io.ErrUnexpectedEOF
	}
	return response, err
}

// This explicitly selected proof uses only the transferred synthetic vendor
// project. It enters the test card in the pinned SDK iframe, then exercises
// Core's existing authenticated DELETE and a new process's durable recovery.
func TestHyperSwitchActualBrowserDeletion(t *testing.T) {
	type recovery struct {
		DB          *config.DBConfig
		Redis       *config.RedisConfig
		HyperSwitch *config.HyperSwitchConfig
		Encryption  *config.EncryptionConfig
		Merchant    merchant.ID
		Operation   uuid.UUID
	}
	if path := os.Getenv("OPENRAILS_HS_DELETE_RECOVERY"); path != "" {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		var input recovery
		require.NoError(t, json.Unmarshal(raw, &input))
		local, err := embed.New(t.Context(), embed.Options{Config: &config.Config{TestMode: config.CredentialPostureSandbox, MerchantConfigHTTP: true, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: input.DB, Redis: input.Redis, HyperSwitch: input.HyperSwitch, Encryption: input.Encryption}, River: embed.RiverManagedByOpenRails()})
		require.NoError(t, err)
		defer local.Close(context.Background())
		rt := app.HostGraph(local).Runtime
		require.NoError(t, rt.DB.RunInMerchantConn(merchant.WithID(t.Context(), input.Merchant), func(c context.Context) error {
			runner := rt.IntentRunner()
			clock := clockwork.NewFakeClockAt(time.Now().Add(time.Hour))
			runner.Clock = clock
			_, err := runner.VerifyByID(c, input.Operation)
			if err != nil {
				return err
			}
			clock.Advance(time.Hour)
			row, err := runner.ExecuteByID(c, input.Operation)
			require.NoError(t, err)
			require.Equal(t, intents.StatusSucceeded, row.Status)
			return nil
		}))
		return
	}
	fixture := os.Getenv("OPENRAILS_HYPERSWITCH_DELETE_FIXTURE")
	require.NotEmpty(t, fixture, "selecting native erasure proof requires the owned delete fixture")
	raw, err := os.ReadFile(fixture)
	require.NoError(t, err)
	var vendor struct {
		APIBaseURL string `json:"api_base_url"`
		SDKURL     string `json:"sdk_url"`
		Merchant   string `json:"merchant_id"`
		Profile    string `json:"profile_id"`
		Public     string `json:"public_api_key"`
		Key        string `json:"api_key"`
	}
	require.NoError(t, json.Unmarshal(raw, &vendor))
	require.Equal(t, "http://127.0.0.1:33144", vendor.APIBaseURL)
	require.Equal(t, "http://127.0.0.1:33146/HyperLoader.js", vendor.SDKURL)
	const project = "openrails-297-delete-20260921"
	output, err := exec.CommandContext(t.Context(), "docker", "inspect", project+"-router-1", "--format", "{{json .Config.Labels}}").Output()
	require.NoError(t, err)
	var labels map[string]string
	require.NoError(t, json.Unmarshal(output, &labels))
	require.Equal(t, "a0fbc109d734010e3f6b647ae184d7f73a4f5262", labels["org.opencontainers.image.revision"])
	t.Logf("Pinned native-delete router source=%s binary=%s", labels["org.opencontainers.image.revision"], labels["openrails.qualification.binary_sha256"])
	ctx := t.Context()
	h := New(t, ctx)
	var delegated billingauth.DelegatedAuthenticator
	surface := h.StartStandalone("USD", WithConfig(func(c *config.Config) {
		c.HyperSwitch = &config.HyperSwitchConfig{AllowLoopbackHTTP: true, APIBaseURL: vendor.APIBaseURL, SDKURL: vendor.SDKURL}
		c.Encryption = &config.EncryptionConfig{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="}
	}), func(c *standaloneConfig) {
		c.delegatedAuthenticator = billingauth.DelegatedAuthenticatorFunc(func(ctx context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
			if delegated == nil {
				return nil, billingauth.ErrUnauthenticated
			}
			return delegated.AuthenticateDelegated(ctx, r)
		})
	})
	owned := surface.ProvisionOwnedMerchant("actual-delete-" + uuid.NewString()[:8])
	core := operator.Get(surface.App()).Core()
	auth := operator.Get(surface.App()).AuthService()
	delegated, err = embedauth.NewDelegatedAuthenticator(auth.Verifier(), owned.MerchantID.String())
	require.NoError(t, err)
	user, err := core.CreateUser(ctx, "delete-"+uuid.NewString()+"@example.test", "delete"+uuid.NewString()[:8])
	require.NoError(t, err)
	require.NoError(t, core.MarkEmailVerified(ctx, user.ID))
	token, _, err := core.MintAccessToken(ctx, user.ID, nil)
	require.NoError(t, err)
	psp := dbtest.EnsureTestPSP(ctx, t, h.sharedPool(), owned.MerchantID.UUID(), "nmi")
	custodian := uuid.New()
	settings, _ := json.Marshal(map[string]string{"public_api_key": vendor.Public, "profile_id": vendor.Profile})
	_, err = h.sharedPool().Exec(ctx, `INSERT INTO billing.custodians(id,merchant_id,key,kind,environment,account_id,settings,credential_versions) VALUES($1,$2,$3,'hyperswitch','test',$4,$5,'{"api_key":1}')`, custodian, owned.MerchantID.UUID(), custodian.String(), vendor.Merchant, settings)
	require.NoError(t, err)
	_, err = h.sharedPool().Exec(ctx, `UPDATE billing.psps SET custodian_id=$1 WHERE merchant_id=$2 AND id=$3`, custodian, owned.MerchantID.UUID(), psp)
	require.NoError(t, err)
	rt := surface.App().Runtime
	name, err := merchants.CustodianSecretName("hyperswitch", "test", vendor.Merchant, "api_key")
	require.NoError(t, err)
	_, err = rt.Merchants.Secrets().Put(ctx, owned.MerchantID, name, vendor.Key)
	require.NoError(t, err)
	sdkURL, err := url.Parse(vendor.SDKURL)
	require.NoError(t, err)
	sdkOrigin := sdkURL.Scheme + "://" + sdkURL.Host
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self' "+sdkOrigin+"; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src "+surface.BaseURL+" "+vendor.APIBaseURL+"; frame-src "+sdkOrigin+"; base-uri 'none'; object-src 'none'; form-action 'none'")
		_, _ = io.WriteString(w, `<!doctype html><html><body><h1>Local card deletion proof</h1><div id="number"></div><div id="expiry"></div><div id="cvc"></div><button id="save">Save card</button></body></html>`)
	}))
	t.Cleanup(page.Close)
	script, err := filepath.Abs("../../tests/browser/hyperswitch-capture.mjs")
	require.NoError(t, err)
	params, _ := json.Marshal(map[string]string{"page": page.URL, "vendor_api": vendor.APIBaseURL, "vendor_sdk": vendor.SDKURL, "api": surface.BaseURL, "token": token, "psp_id": psp.String(), "email": "delete-browser@example.test", "name": "Delete Browser", "store": "true"})
	private := filepath.Join(t.TempDir(), "browser-secrets.json")
	command := exec.CommandContext(ctx, "node", script)
	command.Env = append(os.Environ(), "OPENRAILS_BROWSER_FIXTURE="+string(params), "OPENRAILS_BROWSER_PRIVATE="+private)
	output, err = command.CombinedOutput()
	require.NoError(t, err, "%s", output)
	var proof struct {
		Method   openrails.PaymentMethodID `json:"method"`
		External int                       `json:"externalHTTPRequests"`
		PAN      int                       `json:"corePANRequests"`
	}
	require.NoError(t, json.Unmarshal(output, &proof))
	require.False(t, proof.Method.IsZero())
	require.Zero(t, proof.External)
	require.Zero(t, proof.PAN)
	var method string
	require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT rail_method_ref FROM billing.payment_methods WHERE id=$1`, proof.Method.UUID()).Scan(&method))
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	sql := func(database, query string) string {
		output, err := exec.CommandContext(ctx, "docker", "exec", project+"-pg-1", "psql", "-U", "postgres", "-d", database, "-At", "-v", "ON_ERROR_STOP=1", "-c", query).CombinedOutput()
		require.NoError(t, err, "vendor count query failed")
		return strings.TrimSpace(string(output))
	}
	locker := sql("hyperswitch", "SELECT locker_id FROM payment_methods WHERE id="+quote(method))
	require.NotEmpty(t, locker)
	count := func() string { return sql("locker", "SELECT count(*) FROM vault WHERE vault_id="+quote(locker)) }
	require.Equal(t, "1", count())
	previous := http.DefaultTransport
	drop := &lostNativeDeleteReply{base: previous, target: vendor.APIBaseURL + "/v2/payment-methods/" + method}
	http.DefaultTransport = drop
	defer func() { http.DefaultTransport = previous }()
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, surface.BaseURL+"/v1/me/payment-methods/"+proof.Method.String(), nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set(merchant.BindingHeader, owned.MerchantID.String())
	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, response.Body)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusAccepted, response.StatusCode)
	require.True(t, drop.dropped.Load())
	require.Equal(t, "0", count(), "physical vault row is gone despite lost caller reply")
	var operation uuid.UUID
	var fence string
	require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT i.id,m.park_reason FROM billing.rail_intents i JOIN billing.payment_methods m ON m.id::text=i.payload->>'payment_method_id' WHERE i.merchant_id=$1 AND i.intent_type='hyperswitch_method_delete' AND m.id=$2`, owned.MerchantID.UUID(), proof.Method.UUID()).Scan(&operation, &fence))
	require.Equal(t, "delete:"+operation.String(), fence)
	path := filepath.Join(t.TempDir(), "delete-recovery.json")
	recoveryJSON, err := json.Marshal(recovery{DB: rt.Config.DB, Redis: rt.Config.Redis, HyperSwitch: rt.Config.HyperSwitch, Encryption: rt.Config.Encryption, Merchant: owned.MerchantID, Operation: operation})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, recoveryJSON, 0600))
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHyperSwitchActualBrowserDeletion$", "-test.v")
	child.Env = append(os.Environ(), "OPENRAILS_HS_DELETE_RECOVERY="+path)
	output, err = child.CombinedOutput()
	require.NoError(t, err, "%s", output)
	var local, financial int
	require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT count(*) FROM billing.payment_methods WHERE id=$1`, proof.Method.UUID()).Scan(&local))
	require.Zero(t, local)
	require.Equal(t, "0", count())
	require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT (SELECT count(*) FROM billing.payments WHERE merchant_id=$1)+(SELECT count(*) FROM billing.invoices WHERE merchant_id=$1)+(SELECT count(*) FROM billing.subscriptions WHERE merchant_id=$1)`, owned.MerchantID.UUID()).Scan(&financial))
	require.Zero(t, financial)
	secrets, err := os.ReadFile(private)
	require.NoError(t, err)
	var protected []string
	require.NoError(t, json.Unmarshal(secrets, &protected))
	protected = append(protected, vendor.Key, "4111111111111111")
	logs, err := exec.CommandContext(ctx, "docker", "logs", project+"-router-1").CombinedOutput()
	require.NoError(t, err)
	for _, value := range protected {
		require.NotEmpty(t, value)
		require.False(t, bytes.Contains(logs, []byte(value)), "protected capture value appeared in vendor logs")
	}
	t.Log("Pinned browser capture, real native vault erasure, lost reply and new-process exact replay passed; zero financial rows, external browser requests or PAN requests to Core")
}
