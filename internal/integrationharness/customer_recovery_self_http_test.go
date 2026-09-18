//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/testauth"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// selfCall issues one authenticated self-route request as the bound
// customer, with extra headers.
type selfCall func(method, path string, headers map[string]string, body any) (int, []byte)

// TestCustomerRecoverySelfRoutes pins the customer's own pay-now / retry-now
// routes on the embedded and standalone self surfaces: payer ownership (a
// customer reaches only their own invoice and subscription; anything else is
// 404), the canonical Idempotency-Key, the recovery flags on every self read,
// the 402 decline contract, the rail refusal, and one response shape on both
// surfaces.
func TestCustomerRecoverySelfRoutes(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	pool := h.sharedPool()
	dbtest.EnsureTestMerchant(ctx, t, pool)
	mid := dbtest.TestMerchantID
	gateway := NewFakeNMIGateway(t)
	sandbox := func(cfg *config.Config) {
		cfg.ProviderSandbox = &config.ProviderSandboxConfig{NMIGatewayURL: gateway.URL}
	}
	standalone := h.StartStandalone("usd", WithConfig(sandbox))

	cfg := &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: h.DSN}}
	sandbox(cfg)
	rt, err := embed.New(ctx, embed.Options{Config: cfg, Redis: h.Redis, River: embed.RiverManagedByOpenRails()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	type surface struct {
		runtime *app.Runtime
		bind    func(customer uuid.UUID)
		call    selfCall
	}
	surfaces := map[string]surface{}

	var embeddedSubject string
	authn := billingauth.DelegatedAuthenticatorFunc(func(ctx context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			return nil, billingauth.ErrUnauthenticated
		}
		return &billingauth.DelegatedPrincipal{MerchantID: mid.String(), MerchantSlug: dbtest.TestMerchantSlug, SubjectID: embeddedSubject, Issuer: "embedded-host"}, nil
	})
	handler, err := rt.Handler(embed.MountOptions{RouteSets: []embed.RouteSet{embed.RouteSetCustomer}, Gate: httproutes.NewGate(httproutes.GateOptions{DelegatedAuthenticator: authn}), DelegatedAuthenticator: authn})
	require.NoError(t, err)
	surfaces["embedded"] = surface{
		runtime: app.HostGraph(rt).Runtime,
		bind:    func(customer uuid.UUID) { embeddedSubject = customer.String() },
		call: func(method, path string, headers map[string]string, body any) (int, []byte) {
			var buf bytes.Buffer
			if body != nil {
				require.NoError(t, json.NewEncoder(&buf).Encode(body))
			}
			req := httptest.NewRequest(method, path, &buf)
			req.Header.Set("Authorization", "Bearer host-session")
			req.Header.Set("Content-Type", "application/json")
			for k, v := range headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			return rec.Code, rec.Body.Bytes()
		},
	}

	var standaloneToken string
	surfaces["standalone"] = surface{
		runtime: standalone.App().Runtime,
		bind: func(customer uuid.UUID) {
			caller := standalone.RegisterDelegatedCaller("recovery-"+strings.ReplaceAll(uuid.NewString(), "-", "")[:8], dbtest.TestMerchantSlug, customer.String(), nil)
			standaloneToken = caller.Token
		},
		call: func(method, path string, headers map[string]string, body any) (int, []byte) {
			var buf bytes.Buffer
			if body != nil {
				require.NoError(t, json.NewEncoder(&buf).Encode(body))
			}
			req, err := http.NewRequestWithContext(ctx, method, standalone.BaseURL+path, &buf)
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			for k, v := range headers {
				req.Header.Set(k, v)
			}
			require.NoError(t, testauth.Authorize(req, standaloneToken))
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			return resp.StatusCode, raw
		},
	}

	keysOf := func(raw []byte) []string {
		var m map[string]any
		require.NoError(t, json.Unmarshal(raw, &m), string(raw))
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys
	}
	shapes := map[string]map[string][]string{}
	for name, s := range surfaces {
		t.Run(name, func(t *testing.T) {
			gateway.SetMode(NMISaleApprove)
			gateway.SetVisible(true)
			sales := gateway.SaleCount()
			mine := h.SeedPastDueInvoice(s.runtime, merchant.ID(mid), "USD", 5_000_000)
			other := h.SeedPastDueInvoice(s.runtime, merchant.ID(mid), "USD", 5_000_000)
			sub := h.SeedPastDueSubscription(s.runtime, merchant.ID(mid), SubscriptionForCustomer(mine.Customer))
			stripeSub := h.SeedPastDueSubscription(s.runtime, merchant.ID(mid), SubscriptionForCustomer(mine.Customer), SubscriptionOnRail(models.RailStripe))
			otherSub := h.SeedPastDueSubscription(s.runtime, merchant.ID(mid), SubscriptionForCustomer(other.Customer))
			s.bind(mine.Customer)
			key := func() map[string]string { return map[string]string{"Idempotency-Key": uuid.NewString()} }
			invoicePath := "/v1/me/invoices/" + mine.Invoice.String()
			subPath := "/v1/me/subscriptions/sub_" + sub.Subscription.String()

			// Recovery flags on the self reads.
			status, body := s.call(http.MethodGet, invoicePath, nil, nil)
			require.Equal(t, http.StatusOK, status, string(body))
			var invoice openrails.InvoiceDTO
			require.NoError(t, json.Unmarshal(body, &invoice))
			require.NotNil(t, invoice.Recovery, string(body))
			require.True(t, invoice.Recovery.Retryable)
			require.Contains(t, invoice.Recovery.CompatiblePaymentMethodIDs, openrails.PaymentMethodID(mine.Method))
			require.Contains(t, invoice.Recovery.CompatiblePaymentMethodIDs, openrails.PaymentMethodID(sub.Method), "every vaulted NMI method of the payer")
			require.Len(t, invoice.Recovery.CompatiblePaymentMethodIDs, 2, "the Stripe method is not a recovery method")
			status, body = s.call(http.MethodGet, "/v1/me/invoices", nil, nil)
			require.Equal(t, http.StatusOK, status, string(body))
			require.Contains(t, string(body), `"retryable":true`)
			status, body = s.call(http.MethodGet, subPath, nil, nil)
			require.Equal(t, http.StatusOK, status, string(body))
			var subscription openrails.Subscription
			require.NoError(t, json.Unmarshal(body, &subscription))
			require.NotNil(t, subscription.Recovery, string(body))
			require.True(t, subscription.Recovery.Retryable, "%+v", subscription.Recovery)
			require.Equal(t, 1, subscription.Recovery.AttemptCount)
			require.NotNil(t, subscription.Recovery.NextAttemptAt, "the scheduled retry is the engine's own next attempt")
			require.Equal(t, []openrails.PaymentMethodID{openrails.PaymentMethodID(sub.Method)}, subscription.Recovery.CompatiblePaymentMethodIDs)
			status, body = s.call(http.MethodGet, "/v1/me/subscriptions/sub_"+stripeSub.Subscription.String(), nil, nil)
			require.Equal(t, http.StatusOK, status, string(body))
			require.NoError(t, json.Unmarshal(body, &subscription))
			require.False(t, subscription.Recovery.Retryable)
			require.Equal(t, openrails.RecoveryBlockedRailUnsupported, subscription.Recovery.BlockedReason)

			// The canonical key is required.
			status, body = s.call(http.MethodPost, invoicePath+"/pay-now", nil, map[string]string{"payment_method_id": "pm_" + mine.Method.String()})
			require.Equal(t, http.StatusBadRequest, status, string(body))
			require.Contains(t, string(body), "Idempotency-Key")
			require.Equal(t, sales, gateway.SaleCount())

			// Another customer's invoice and subscription do not exist here.
			status, body = s.call(http.MethodPost, "/v1/me/invoices/"+other.Invoice.String()+"/pay-now", key(), map[string]string{"payment_method_id": "pm_" + other.Method.String()})
			require.Equal(t, http.StatusNotFound, status, string(body))
			status, body = s.call(http.MethodGet, "/v1/me/invoices/"+other.Invoice.String()+"/payments", nil, nil)
			require.Equal(t, http.StatusNotFound, status, string(body))
			status, body = s.call(http.MethodPost, "/v1/me/subscriptions/sub_"+otherSub.Subscription.String()+"/retry-now", key(), nil)
			require.Equal(t, http.StatusNotFound, status, string(body))
			status, body = s.call(http.MethodPost, invoicePath+"/pay-now", key(), map[string]string{"payment_method_id": "pm_" + other.Method.String()})
			require.Equal(t, http.StatusBadRequest, status, string(body))
			require.Contains(t, string(body), openrails.CodeCollectionPaymentMethodInvalid)
			require.Equal(t, sales, gateway.SaleCount(), "no provider traffic")
			require.Equal(t, "past_due", h.SubscriptionState(otherSub.Subscription).Status)

			// A provider-managed rail is refused with its code before any provider traffic.
			status, body = s.call(http.MethodPost, "/v1/me/subscriptions/sub_"+stripeSub.Subscription.String()+"/retry-now", key(), nil)
			require.Equal(t, http.StatusConflict, status, string(body))
			require.Contains(t, string(body), openrails.CodePaymentRecoveryRailUnsupported)
			require.Equal(t, sales, gateway.SaleCount())
			require.Zero(t, h.RebillOperations(stripeSub.Subscription))

			// Decline: the 402 contract carries the attempt.
			gateway.SetMode(NMISaleDecline)
			declineKey := key()
			status, body = s.call(http.MethodPost, invoicePath+"/pay-now", declineKey, map[string]string{"payment_method_id": "pm_" + mine.Method.String()})
			require.Equal(t, http.StatusPaymentRequired, status, string(body))
			var envelope struct {
				Error openrails.ErrorDetails `json:"error"`
			}
			require.NoError(t, json.Unmarshal(body, &envelope))
			require.Equal(t, openrails.CodeCardDeclined, envelope.Error.Code)
			require.Equal(t, mine.Invoice.String(), envelope.Error.Metadata["invoice_id"])
			require.NotEmpty(t, envelope.Error.Metadata["attempt_id"])
			require.Equal(t, true, envelope.Error.Metadata["retryable"])
			shapes[name] = map[string][]string{"decline": keysOf(body)}
			status, body = s.call(http.MethodPost, invoicePath+"/pay-now", declineKey, map[string]string{"payment_method_id": "pm_" + mine.Method.String()})
			require.Equal(t, http.StatusPaymentRequired, status, string(body))
			require.Contains(t, string(body), `"replayed":true`)
			status, body = s.call(http.MethodGet, invoicePath+"/payments", nil, nil)
			require.Equal(t, http.StatusOK, status, string(body))
			var attempts openrails.Page[openrails.InvoicePaymentAttemptDTO]
			require.NoError(t, json.Unmarshal(body, &attempts))
			require.EqualValues(t, 1, attempts.Total)
			require.Equal(t, "failed", attempts.Data[0].Status)
			require.Equal(t, sales, gateway.SaleCount())

			// Success: the invoice is paid, the subscription renewed.
			gateway.SetMode(NMISaleApprove)
			status, body = s.call(http.MethodPost, invoicePath+"/pay-now", key(), map[string]string{"payment_method_id": "pm_" + mine.Method.String()})
			require.Equal(t, http.StatusOK, status, string(body))
			var paid openrails.InvoicePayNowResult
			require.NoError(t, json.Unmarshal(body, &paid))
			require.Equal(t, "paid", paid.Invoice.Status)
			require.Equal(t, "settled", paid.Attempt.Status)
			require.Equal(t, "succeeded", paid.Operation.Status)
			require.False(t, paid.Invoice.Recovery.Retryable)
			shapes[name]["pay_now"] = keysOf(body)
			status, body = s.call(http.MethodPost, subPath+"/retry-now", key(), map[string]string{"payment_method_id": "pm_" + sub.Method.String()})
			require.Equal(t, http.StatusOK, status, string(body))
			var renewed openrails.SubscriptionRetryNowResult
			require.NoError(t, json.Unmarshal(body, &renewed))
			require.Equal(t, "active", renewed.Subscription.Status)
			require.Nil(t, renewed.Subscription.NextRetryAt)
			require.NotNil(t, renewed.Payment)
			require.Equal(t, "succeeded", renewed.Operation.Status)
			require.Equal(t, openrails.RecoveryBlockedNotDue, renewed.Subscription.Recovery.BlockedReason)
			shapes[name]["retry_now"] = keysOf(body)
			require.Equal(t, sales+2, gateway.SaleCount())
			status, body = s.call(http.MethodGet, invoicePath+"/payments", nil, nil)
			require.Equal(t, http.StatusOK, status, string(body))
			require.NoError(t, json.Unmarshal(body, &attempts))
			require.EqualValues(t, 2, attempts.Total, "history is append-only")
			require.Equal(t, "settled", attempts.Data[0].Status)
			require.Equal(t, "failed", attempts.Data[1].Status)

			// R4: the recovery block names each state that refuses a retry.
			blocked := func(f SubscriptionFixture) openrails.PaymentRecovery {
				t.Helper()
				status, body := s.call(http.MethodGet, "/v1/me/subscriptions/sub_"+f.Subscription.String(), nil, nil)
				require.Equal(t, http.StatusOK, status, string(body))
				var view openrails.Subscription
				require.NoError(t, json.Unmarshal(body, &view))
				require.NotNil(t, view.Recovery, string(body))
				require.False(t, view.Recovery.Retryable, string(body))
				return *view.Recovery
			}
			remapped := h.SeedPastDueSubscription(s.runtime, merchant.ID(mid), SubscriptionForCustomer(mine.Customer))
			h.ReattributeToAnotherPSP(remapped)
			require.Equal(t, openrails.RecoveryBlockedPSPMismatch, blocked(remapped).BlockedReason)
			status, body = s.call(http.MethodPost, "/v1/me/subscriptions/sub_"+remapped.Subscription.String()+"/retry-now", key(), nil)
			require.Equal(t, http.StatusConflict, status, string(body))
			require.Contains(t, string(body), openrails.CodePaymentMethodPSPMismatch)
			stale := h.SeedPastDueSubscription(s.runtime, merchant.ID(mid), SubscriptionForCustomer(mine.Customer))
			h.AgePastDunningWindow(stale)
			require.Equal(t, openrails.RecoveryBlockedWindowExpired, blocked(stale).BlockedReason)
			leased := h.SeedPastDueSubscription(s.runtime, merchant.ID(mid), SubscriptionForCustomer(mine.Customer))
			h.SkewedWorkerLease(leased)
			leasedView := blocked(leased)
			require.Equal(t, openrails.RecoveryBlockedInProgress, leasedView.BlockedReason)
			require.Nil(t, leasedView.NextAttemptAt, "a lease is not a schedule")

			// One key, one request: reusing it on another subscription conflicts.
			reused := key()
			status, body = s.call(http.MethodPost, "/v1/me/subscriptions/sub_"+stale.Subscription.String()+"/retry-now", reused, nil)
			require.Equal(t, http.StatusConflict, status, string(body))
			require.Contains(t, string(body), openrails.CodeSubscriptionNotRetryable, "a refused request binds nothing")
			fresh := h.SeedPastDueSubscription(s.runtime, merchant.ID(mid), SubscriptionForCustomer(mine.Customer))
			status, body = s.call(http.MethodPost, "/v1/me/subscriptions/sub_"+fresh.Subscription.String()+"/retry-now", reused, nil)
			require.Equal(t, http.StatusOK, status, string(body))
			status, body = s.call(http.MethodPost, "/v1/me/subscriptions/sub_"+leased.Subscription.String()+"/retry-now", reused, nil)
			require.Equal(t, http.StatusConflict, status, string(body))
			require.Contains(t, string(body), openrails.CodeSubscriptionRetryIdempotencyConflict)
		})
	}
	require.Equal(t, shapes["embedded"], shapes["standalone"], "one response shape on both surfaces")
}
