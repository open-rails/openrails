//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// A webhook URL names a PSP's account but not its money class. A live PSP is
// never declared beside a fake provider or a test seam, so these worlds wire
// none: provider writes stay read-only and no worker runs, so nothing reaches a
// provider, and the clock is the real one. A delivery's effect is the wake it
// queues, which names the PSP it was routed to.
func newAccountClassWorld(t *testing.T, alpha, beta billing.PSPs, configure ...func(*world)) *authorityWorld {
	t.Helper()
	return newAuthorityWorld(t, func(w *world) {
		w.deps.Clock, w.deps.FXTransport = nil, nil
		for slug, psps := range map[string]billing.PSPs{"alpha": alpha, "beta": beta} {
			declaration := cfgMerchant(w.cfg, slug)
			declaration.PSPs = psps
			setCfgMerchant(&w.cfg, slug, declaration)
		}
		for _, change := range configure {
			change(w)
		}
	})
}

// restartWith boots the world's engine again with change applied to its
// configuration, behind a new server the remote clients follow.
func (a *authorityWorld) restartWith(t *testing.T, change func(*openrails.Config)) {
	t.Helper()
	a.server.Close()
	require.NoError(t, a.engine.Close(context.Background()))
	change(&a.cfg)
	var err error
	a.engine, err = openrails.New(t.Context(), a.cfg, a.deps)
	require.NoError(t, err)
	a.alpha, err = declaredMerchant(a.engine, "alpha")
	require.NoError(t, err)
	a.beta, err = declaredMerchant(a.engine, "beta")
	require.NoError(t, err)
	a.serve(t)
	a.alphaRemote = a.remote(t, a.alpha.MerchantID(), a.alphaKey.Secret)
	a.betaRemote = a.remote(t, a.beta.MerchantID(), a.betaKey.Secret)
}

func pspIDs(t *testing.T, c *openrails.Client) map[string]uuid.UUID {
	t.Helper()
	page, err := c.ListPSPs(t.Context(), billing.PSPListParams{})
	require.NoError(t, err)
	out := map[string]uuid.UUID{}
	for _, psp := range page.Items {
		out[psp.Key] = uuid.UUID(psp.ID)
	}
	return out
}

func stripeCatalogNotice(t *testing.T, id, product string, livemode bool) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"id": id, "object": "event", "type": "product.updated", "created": time.Now().Unix(), "livemode": livemode,
		"data": map[string]any{"object": map[string]any{"id": product, "object": "product"}}})
	require.NoError(t, err)
	return body
}

func nmiRecurringNotice(t *testing.T, a *authorityWorld, gateway, id, schedule, secret string) (int, string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"event_id": id, "event_type": "recurring.subscription.update", "event_body": map[string]any{"subscription_id": schedule, "merchant": map[string]string{"id": gateway}}})
	require.NoError(t, err)
	at := fmt.Sprint(time.Now().Unix())
	h := hmac.New(sha256.New, []byte(secret))
	_, _ = h.Write([]byte(at + "."))
	_, _ = h.Write(body)
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.server.URL+"/v1/webhooks/nmi/"+gateway, bytes.NewReader(body))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Webhook-Signature", "t="+at+",s="+hex.EncodeToString(h.Sum(nil)))
	response, err := a.server.Client().Do(r)
	require.NoError(t, err)
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, string(raw)
}

// woken is the PSP each queued wake of kind names, by the object it reads.
func woken(t *testing.T, fleet *river.Client[pgx.Tx], kind string) map[string]uuid.UUID {
	t.Helper()
	jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds(kind).First(100))
	require.NoError(t, err)
	out := map[string]uuid.UUID{}
	for _, job := range jobs.Jobs {
		var args struct {
			PSPID     uuid.UUID `json:"psp_id"`
			Ref       string    `json:"ref"`
			Reference string    `json:"subscription_reference"`
		}
		require.NoError(t, json.Unmarshal(job.EncodedArgs, &args))
		out[args.Ref+args.Reference] = args.PSPID
	}
	return out
}

func TestWebhookAccountClasses(t *testing.T) {
	t.Run("OR-J28-S06/another-merchants-claim-in-the-other-class-is-refused-and-the-owners-webhooks-keep-working", func(t *testing.T) {
		a := newAccountClassWorld(t, billing.PSPs{
			"stripe": openrails.StripePSP{AccountID: "acct_owned", Environment: openrails.EnvironmentLive, SecretKey: "sk_live_owner", WebhookSigningSecret: "whsec_owner"}.PSPConfig(),
		}, nil, func(w *world) {
			declaration := cfgMerchant(w.cfg, "alpha")
			declaration.Custodians = billing.Custodians{"cards": {Kind: "basis_theory", Environment: "live", AccountID: "00000000-0000-4000-8000-0000000000c1",
				Settings: map[string]any{"public_api_key": "public-owner"}, Secrets: map[string]string{"api_key": "private-owner"}}}
			setCfgMerchant(&w.cfg, "alpha", declaration)
		})
		owner := pspIDs(t, a.alpha)["stripe"]
		require.NotEqual(t, uuid.Nil, owner)
		delivered := func(id, product string) {
			t.Helper()
			body := stripeCatalogNotice(t, id, product, true)
			status, raw := callback(t, a, "acct_owned", body, stripeSignature(body, "whsec_owner"))
			require.Equal(t, http.StatusOK, status, raw)
		}
		delivered("evt_before_claims", "prod_before_claims")

		a.restartWith(t, func(cfg *openrails.Config) {
			declaration := cfgMerchant(*cfg, "beta")
			declaration.PSPs = billing.PSPs{"squat": openrails.StripePSP{AccountID: "acct_owned", Environment: openrails.EnvironmentTest, SecretKey: "sk_test_squat", WebhookSigningSecret: "whsec_squat"}.PSPConfig()}
			declaration.Custodians = billing.Custodians{"cards": {Kind: "basis_theory", Environment: "test", AccountID: "00000000-0000-4000-8000-0000000000c1",
				Settings: map[string]any{"public_api_key": "public-squat"}, Secrets: map[string]string{"api_key": "private-squat"}}}
			setCfgMerchant(cfg, "beta", declaration)
		})
		require.Empty(t, pspIDs(t, a.beta), "a file-declared claim of another merchant's account in the other class is not served")
		require.Equal(t, map[string]uuid.UUID{"stripe": owner}, pspIDs(t, a.alpha))
		custodians, err := a.betaRemote.ListCustodians(t.Context())
		require.NoError(t, err)
		require.Empty(t, custodians.Items, "a custodian tenant another merchant holds in the other class is not served")
		custodians, err = a.alphaRemote.ListCustodians(t.Context())
		require.NoError(t, err)
		require.Len(t, custodians.Items, 1)

		fleet := hostFleet(t, a.world)
		delivered("evt_after_claims", "prod_after_claims")
		squat := stripeCatalogNotice(t, "evt_squat", "prod_squat", false)
		status, raw := callback(t, a, "acct_owned", squat, stripeSignature(squat, "whsec_squat"))
		require.Equal(t, http.StatusUnauthorized, status, raw)
		require.Equal(t, map[string]uuid.UUID{"prod_before_claims": owner, "prod_after_claims": owner}, woken(t, fleet, "openrails.provider_object_observe"))
	})
	t.Run("OR-J28-S07/one-merchants-live-and-test-psps-on-one-account-each-receive-their-own-events", func(t *testing.T) {
		t.Run("stripe", func(t *testing.T) {
			a := newAccountClassWorld(t, billing.PSPs{
				"stripe-live": openrails.StripePSP{AccountID: "acct_classes", Environment: openrails.EnvironmentLive, SecretKey: "sk_live_classes", WebhookSigningSecret: "whsec_live_classes"}.PSPConfig(),
				"stripe-test": openrails.StripePSP{AccountID: "acct_classes", Environment: openrails.EnvironmentTest, SecretKey: "sk_test_classes", WebhookSigningSecret: "whsec_test_classes"}.PSPConfig(),
				"one-live":    openrails.StripePSP{AccountID: "acct_one_secret", Environment: openrails.EnvironmentLive, SecretKey: "sk_live_one", WebhookSigningSecret: "whsec_one_secret"}.PSPConfig(),
				"one-test":    openrails.StripePSP{AccountID: "acct_one_secret", Environment: openrails.EnvironmentTest, SecretKey: "sk_test_one", WebhookSigningSecret: "whsec_one_secret"}.PSPConfig(),
			}, nil)
			ids := pspIDs(t, a.alpha)
			require.Len(t, ids, 4)
			fleet := hostFleet(t, a.world)
			for _, d := range []struct {
				account, id, product, secret string
				livemode                     bool
			}{
				{"acct_classes", "evt_live", "prod_live", "whsec_live_classes", true},
				{"acct_classes", "evt_test", "prod_test", "whsec_test_classes", false},
				// One secret verifies both: Stripe's livemode says which.
				{"acct_one_secret", "evt_one_live", "prod_one_live", "whsec_one_secret", true},
				{"acct_one_secret", "evt_one_test", "prod_one_test", "whsec_one_secret", false},
			} {
				body := stripeCatalogNotice(t, d.id, d.product, d.livemode)
				status, raw := callback(t, a, d.account, body, stripeSignature(body, d.secret))
				require.Equal(t, http.StatusOK, status, raw)
			}
			for _, refused := range []struct{ id, secret string }{{"evt_forged", "whsec_foreign"}, {"evt_unsigned", ""}} {
				body := stripeCatalogNotice(t, refused.id, "prod_refused", true)
				signature := ""
				if refused.secret != "" {
					signature = stripeSignature(body, refused.secret)
				}
				status, raw := callback(t, a, "acct_classes", body, signature)
				require.Equal(t, http.StatusUnauthorized, status, raw)
			}
			require.Equal(t, map[string]uuid.UUID{
				"prod_live": ids["stripe-live"], "prod_test": ids["stripe-test"],
				"prod_one_live": ids["one-live"], "prod_one_test": ids["one-test"],
			}, woken(t, fleet, "openrails.provider_object_observe"))
		})
		t.Run("nmi", func(t *testing.T) {
			a := newAccountClassWorld(t, billing.PSPs{
				"nmi-live": openrails.NMIPSP{GatewayID: "classes-gateway", Environment: openrails.EnvironmentLive, SecurityKey: "live-security-key", WebhookSigningSecret: "nmi-live-secret"}.PSPConfig(),
				"nmi-test": openrails.NMIPSP{GatewayID: "classes-gateway", Environment: openrails.EnvironmentTest, SecurityKey: "test-security-key", WebhookSigningSecret: "nmi-test-secret"}.PSPConfig(),
			}, nil)
			ids := pspIDs(t, a.alpha)
			require.Len(t, ids, 2)
			fleet := hostFleet(t, a.world)
			status, raw := nmiRecurringNotice(t, a, "classes-gateway", "evt_nmi_live", "schedule-live", "nmi-live-secret")
			require.Equal(t, http.StatusOK, status, raw)
			status, raw = nmiRecurringNotice(t, a, "classes-gateway", "evt_nmi_test", "schedule-test", "nmi-test-secret")
			require.Equal(t, http.StatusOK, status, raw)
			status, raw = nmiRecurringNotice(t, a, "classes-gateway", "evt_nmi_forged", "schedule-forged", "nmi-foreign-secret")
			require.Equal(t, http.StatusUnauthorized, status, raw)
			require.Equal(t, map[string]uuid.UUID{"schedule-live": ids["nmi-live"], "schedule-test": ids["nmi-test"]}, woken(t, fleet, "openrails.subscription_converge"))
		})
	})
}
