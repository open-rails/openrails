//go:build integration

package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

type initialStripeResolver struct{ service *subscriptions.StripeService }

func (r initialStripeResolver) ResolveNMIClient(context.Context, uuid.UUID, *uuid.UUID) (*nmi.NMIClient, bool, error) {
	return nil, false, errors.New("NMI must not execute Stripe")
}
func (r initialStripeResolver) ResolveStripeEngineService(_ context.Context, mid uuid.UUID, psp *uuid.UUID) (*subscriptions.StripeService, bool, error) {
	owner, account := r.service.AccountIdentity()
	if owner != mid || psp == nil || *psp != account {
		return nil, false, errors.New("wrong Stripe account")
	}
	return r.service, true, nil
}

type initialStripeWire func(*http.Request) (*http.Response, error)

func (f initialStripeWire) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func initialStripeResponse(v any) *http.Response {
	raw, _ := json.Marshal(v)
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw)))}
}

func TestStripeInitialMembershipOwnedWorkflow(t *testing.T) {
	for _, mode := range []string{"paid", "lost response", "authentication", "declined", "wrong method"} {
		t.Run(mode, func(t *testing.T) {
			fx := newSubIntentFixture(t)
			fx.prepare(t)
			terms := fx.payload.Terms
			terms.CollectionPolicy = models.CollectionPolicyEngine
			mid, err := merchant.Require(fx.ctx)
			require.NoError(t, err)
			terms.PSPID = dbtest.EnsureTestPSP(fx.ctx, t, fx.db.Pool(), mid.UUID(), "stripe")
			_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE billing.payment_methods SET rail='stripe',psp_id=$2,rail_customer_ref='cus_initial',rail_method_ref='pm_initial' WHERE id=$1`, terms.PaymentMethodID, terms.PSPID)
			require.NoError(t, err)
			cfg := &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull}
			fx.svc.Config = cfg
			fx.runner.Config = cfg
			fx.svc.Intents = fx.runner
			service := subscriptions.NewAccountStripeService(cfg, mid.UUID(), terms.PSPID, "acct_initial", "sk_test_initial")
			resolver := initialStripeResolver{service}
			fx.runner.Registry = intents.NewRegistry(NewInitialMembershipIntentHandler(fx.svc, resolver))
			var mu sync.Mutex
			var pi map[string]any
			posts, cancels := 0, 0
			release := stripeapi.InstallBaseTransport(initialStripeWire(func(r *http.Request) (*http.Response, error) {
				mu.Lock()
				defer mu.Unlock()
				require.Equal(t, stripeapi.APIVersion, r.Header.Get(stripeapi.VersionHeader))
				switch {
				case r.Method == "POST" && r.URL.Path == "/v1/payment_intents":
					posts++
					b, _ := io.ReadAll(r.Body)
					v, _ := url.ParseQuery(string(b))
					require.Equal(t, "999", v.Get("amount"))
					require.Equal(t, "off_session", v.Get("setup_future_usage"))
					require.Equal(t, "false", v.Get("off_session"))
					metadata := map[string]string{}
					for k, values := range v {
						if strings.HasPrefix(k, "metadata[") {
							metadata[strings.TrimSuffix(strings.TrimPrefix(k, "metadata["), "]")] = values[0]
						}
					}
					pi = map[string]any{"id": "pi_initial", "status": "succeeded", "customer": "cus_initial", "payment_method": "pm_initial", "amount": 999, "amount_received": 999, "currency": "usd", "setup_future_usage": "off_session", "capture_method": "automatic", "confirmation_method": "automatic", "latest_charge": "ch_initial", "metadata": metadata, "livemode": false, "client_secret": "pi_initial_secret_sensitive"}
					if mode == "authentication" {
						pi["status"] = "requires_action"
					}
					if mode == "declined" {
						pi["status"] = "requires_payment_method"
						pi["last_payment_error"] = map[string]any{"code": "card_declined", "decline_code": "insufficient_funds"}
					}
					if mode == "lost response" {
						return nil, errors.New("simulated lost accepted response")
					}
					return initialStripeResponse(pi), nil
				case r.Method == "POST" && r.URL.Path == "/v1/payment_intents/pi_initial/cancel":
					cancels++
					pi["status"] = "canceled"
					return initialStripeResponse(pi), nil
				case r.Method == "GET" && r.URL.Path == "/v1/payment_intents":
					return initialStripeResponse(map[string]any{"data": []any{pi}, "has_more": false}), nil
				case r.Method == "GET" && r.URL.Path == "/v1/payment_intents/pi_initial":
					return initialStripeResponse(pi), nil
				case r.Method == "GET" && r.URL.Path == "/v1/charges/ch_initial":
					method := "pm_initial"
					if mode == "wrong method" {
						method = "pm_other"
					}
					return initialStripeResponse(map[string]any{"id": "ch_initial", "amount": 999, "amount_captured": 999, "currency": "usd", "customer": "cus_initial", "payment_method": method, "payment_intent": "pi_initial", "paid": true, "captured": true, "status": "succeeded"}), nil
				default:
					t.Errorf("unexpected Stripe route %s %s", r.Method, r.URL.Path)
					return nil, errors.New("unexpected Stripe route")
				}
			}))
			defer release()
			principal := billingauth.DelegatedPrincipal{CredentialClass: billingauth.CredentialClassUserSession, MerchantID: mid.String(), SubjectID: terms.CustomerID.String()}
			key := "stripe-initial-" + uuid.NewString()
			_, confirmErr := fx.svc.ConfirmInitialMembership(fx.ctx, terms, key, principal)
			op, err := intents.NewStore(fx.db).GetByIdempotencyKey(fx.ctx, InitialMembershipIdempotencyKey(key))
			require.NoError(t, err)
			if mode == "lost response" {
				require.ErrorIs(t, confirmErr, ErrCheckoutProcessing)
				op, err = fx.runner.VerifyByID(fx.ctx, op.ID)
				require.NoError(t, err)
			}
			if mode == "authentication" {
				require.ErrorIs(t, confirmErr, ErrCheckoutProcessing)
				require.Equal(t, intents.StatusUnknownNeedsVerify, op.Status)
				recovery, err := fx.svc.StripePaymentAuthentication(fx.ctx, op.ID, principal, resolver)
				require.NoError(t, err)
				require.Equal(t, "pi_initial_secret_sensitive", recovery.ClientSecret)
				bad := principal
				bad.SubjectID = uuid.NewString()
				_, err = fx.svc.StripePaymentAuthentication(fx.ctx, op.ID, bad, resolver)
				require.Error(t, err)
				bad = principal
				bad.CredentialClass = "api_key"
				_, err = fx.svc.StripePaymentAuthentication(fx.ctx, op.ID, bad, resolver)
				require.Error(t, err)
				mu.Lock()
				pi["status"] = "succeeded"
				mu.Unlock()
				_, err = fx.svc.ConfirmStripePaymentAuthentication(fx.ctx, op.ID, principal)
				require.NoError(t, err)
				op, err = intents.NewStore(fx.db).Get(fx.ctx, op.ID)
				require.NoError(t, err)
			}
			if mode == "wrong method" {
				require.Equal(t, intents.StatusUnknownNeedsVerify, op.Status)
			} else if mode == "declined" {
				require.Equal(t, intents.StatusFailedTerminal, op.Status)
				require.Equal(t, 1, cancels)
				require.NoError(t, intents.ValidateInitialMembershipTerminal(op))
			} else {
				require.Equal(t, intents.StatusSucceeded, op.Status)
				require.NoError(t, intents.ValidateInitialMembershipTerminal(op))
				method, err := fx.db.Gen(fx.ctx).GetPaymentMethodByID(fx.ctx, gen.GetPaymentMethodByIDParams{MerchantID: mid.UUID(), ID: terms.PaymentMethodID})
				require.NoError(t, err)
				require.Equal(t, "pi_initial", method.StoredCredentialRecurringRef)
				sub, err := fx.svc.SubscriptionService.GetByID(fx.ctx, terms.SubscriptionID)
				require.NoError(t, err)
				require.Equal(t, models.CollectionPolicyEngine, sub.CollectionPolicy)
				require.Empty(t, sub.RailSubscriptionID)
			}
			var successCount int
			require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.payments WHERE customer_id=$1 AND status='completed'`, terms.CustomerID).Scan(&successCount))
			if mode == "declined" || mode == "wrong method" {
				require.Zero(t, successCount)
			} else {
				require.Equal(t, 1, successCount)
			}
			_, _ = fx.svc.ConfirmInitialMembership(fx.ctx, terms, key, principal)
			require.Equal(t, 1, posts, "replayed original operation must never create another PI")
		})
	}
}
