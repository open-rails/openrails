//go:build integration && browser && hyperswitch

package integrationharness

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/authhttp"
	"github.com/open-rails/openrails/config"
	embedauth "github.com/open-rails/openrails/embed/authkit"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/hyperswitch"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

// Selected explicitly against the pinned local vendor, never silently skipped.
// Its card entry happens in the actual pinned vendor iframe, not this Go server.
func TestHyperSwitchActualBrowserCapture(t *testing.T) {
	path := os.Getenv("OPENRAILS_HYPERSWITCH_FIXTURE")
	require.NotEmpty(t, path, "selecting this qualification requires the owned local HyperSwitch fixture")
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var vendor struct {
		APIBaseURL   string `json:"api_base_url"`
		SDKURL       string `json:"sdk_url"`
		MerchantID   string `json:"merchant_id"`
		ProfileID    string `json:"profile_id"`
		PublicAPIKey string `json:"public_api_key"`
		APIKey       string `json:"api_key"`
	}
	require.NoError(t, json.Unmarshal(raw, &vendor))
	ctx := t.Context()
	h := New(t, ctx)
	var delegated billingauth.DelegatedAuthenticator
	surface := h.StartStandalone("USD", WithConfig(func(c *config.Config) {
		c.HyperSwitch = &config.HyperSwitchConfig{APIBaseURL: vendor.APIBaseURL, SDKURL: vendor.SDKURL}
		c.Encryption = &config.EncryptionConfig{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="}
	}), func(c *standaloneConfig) {
		c.delegatedAuthenticator = billingauth.DelegatedAuthenticatorFunc(func(ctx context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
			if delegated == nil {
				return nil, billingauth.ErrUnauthenticated
			}
			return delegated.AuthenticateDelegated(ctx, r)
		})
	})
	owned := surface.ProvisionOwnedMerchant("actual-capture-" + uuid.NewString()[:8])
	core := operator.Get(surface.App()).Core()
	auth, err := authhttp.New(core, authhttp.Config{DirectPeerIP: true})
	require.NoError(t, err)
	t.Cleanup(auth.Close)
	delegated, err = embedauth.NewDelegatedAuthenticator(auth.Verifier(), owned.MerchantID.String())
	require.NoError(t, err)
	user, err := core.CreateUser(ctx, "capture-"+uuid.NewString()+"@example.test", "capture"+uuid.NewString()[:8])
	require.NoError(t, err)
	require.NoError(t, core.MarkEmailVerified(ctx, user.ID))
	token, _, err := core.MintAccessToken(ctx, user.ID, nil)
	require.NoError(t, err)
	psp := dbtest.EnsureTestPSP(ctx, t, h.sharedPool(), owned.MerchantID.UUID(), "nmi")
	custodian := uuid.New()
	settings, _ := json.Marshal(map[string]string{"public_api_key": vendor.PublicAPIKey, "profile_id": vendor.ProfileID})
	_, err = h.sharedPool().Exec(ctx, `INSERT INTO openrails.custodians(id,merchant_id,key,kind,environment,account_id,settings,credential_versions) VALUES($1,$2,$3,'hyperswitch','test',$4,$5,'{"api_key":1}')`, custodian, owned.MerchantID.UUID(), custodian.String(), vendor.MerchantID, settings)
	require.NoError(t, err)
	_, err = h.sharedPool().Exec(ctx, `UPDATE openrails.psps SET custodian_id=$1 WHERE merchant_id=$2 AND id=$3`, custodian, owned.MerchantID.UUID(), psp)
	require.NoError(t, err)
	name, err := merchants.CustodianSecretName("hyperswitch", "test", vendor.MerchantID, "api_key")
	require.NoError(t, err)
	_, err = surface.App().Runtime.Merchants.Secrets().Put(ctx, owned.MerchantID, name, vendor.APIKey)
	require.NoError(t, err)
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Referrer-Policy", "no-referrer")
		_, _ = w.Write([]byte(`<!doctype html><html><body><h1>Local vendor capture qualification</h1><div id="number"></div><div id="expiry"></div><div id="cvc"></div><button id="save">Save card</button></body></html>`))
	}))
	t.Cleanup(page.Close)

	script, err := filepath.Abs("../../tests/browser/hyperswitch-capture.mjs")
	require.NoError(t, err)
	reader, err := hyperswitch.New(hyperswitch.Config{BaseURL: vendor.APIBaseURL, MerchantID: vendor.MerchantID, ProfileID: vendor.ProfileID, APIKey: hyperswitch.Secret(vendor.APIKey), ReadOnly: true})
	require.NoError(t, err)
	for _, store := range []bool{false, true} {
		input, _ := json.Marshal(map[string]string{"page": page.URL, "vendor_api": vendor.APIBaseURL, "vendor_sdk": vendor.SDKURL, "api": surface.BaseURL, "token": token, "psp_id": psp.String(), "email": "capture-browser@example.test", "name": "Capture Browser", "store": strconv.FormatBool(store)})
		command := exec.CommandContext(ctx, "node", script)
		command.Env = append(os.Environ(), "OPENRAILS_BROWSER_FIXTURE="+string(input))
		output, err := command.CombinedOutput()
		t.Log(string(output))
		require.NoError(t, err)
		var proof struct {
			VendorSession  string `json:"vendorSession"`
			VendorCustomer string `json:"vendorCustomer"`
		}
		require.NoError(t, json.Unmarshal(output, &proof))
		session, err := reader.GetSession(ctx, proof.VendorSession, proof.VendorCustomer)
		require.NoError(t, err)
		require.Len(t, session.AssociatedMethods, 1)
		method, err := reader.GetMethod(ctx, session.AssociatedMethods[0].Token.Data, proof.VendorCustomer)
		if store {
			require.NoError(t, err)
			require.Equal(t, "persistent", method.StorageType)
		} else {
			require.ErrorIs(t, err, hyperswitch.ErrBinding)
			require.Equal(t, "volatile", method.StorageType)
		}
		t.Logf("Explicit storage consent=%t; vendor readback storage_type=%s", store, method.StorageType)
	}
	var methods, financial int
	require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT count(*) FROM openrails.payment_methods WHERE merchant_id=$1 AND customer_id=$2`, owned.MerchantID.UUID(), user.ID).Scan(&methods))
	require.Equal(t, 1, methods)
	require.NoError(t, h.sharedPool().QueryRow(ctx, `SELECT (SELECT count(*) FROM openrails.payments WHERE merchant_id=$1 AND customer_id=$2)+(SELECT count(*) FROM openrails.subscriptions WHERE merchant_id=$1 AND customer_id=$2)+(SELECT count(*) FROM openrails.ledger_accounts WHERE merchant_id=$1 AND customer_id=$2)`, owned.MerchantID.UUID(), user.ID).Scan(&financial))
	require.Zero(t, financial)
}
