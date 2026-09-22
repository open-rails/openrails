//go:build integration

package checkout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"

	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments"
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
	for _, mode := range []string{"paid", "lost response", "authentication", "declined", "wrong method", "setup", "not dispatched", "refunded", "disputed", "verify readonly", "verify limited", "verify full", "verify lost cancel", "opaque valid", "opaque malformed", "bound missing", "bound wrong owner", "bound changed quote", "bound deleted", "bound paid"} {
		t.Run(mode, func(t *testing.T) {
			fx := newSubIntentFixtureForMerchant(t, merchant.ID(uuid.New()))
			_, seedErr := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.products SET entitlements_spec='{"stripe_engine_access":null}' WHERE id=(SELECT product_id FROM billing.prices WHERE id=$1)`, fx.priceID)
			require.NoError(t, seedErr)
			fx.prepare(t)
			terms := fx.payload.Terms
			terms.CollectionPolicy = models.CollectionPolicyEngine
			mid, err := merchant.Require(fx.ctx)
			require.NoError(t, err)
			terms.PSPID = uuid.New()
			_, err = fx.db.Pool().Exec(fx.ctx, `INSERT INTO billing.psps(id,merchant_id,rail,environment,account_id,key) VALUES($1,$2,'stripe','test',$3,$3)`, terms.PSPID, mid.UUID(), "stripe-"+uuid.NewString())
			require.NoError(t, err)
			_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE billing.payment_methods SET rail='stripe',psp_id=$2,rail_customer_ref='cus_initial',rail_method_ref='pm_initial' WHERE id=$1`, terms.PaymentMethodID, terms.PSPID)
			require.NoError(t, err)
			cfg := &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull}
			fx.svc.Config = cfg
			fx.runner.Config = cfg
			fx.svc.Intents = fx.runner
			serviceConfig := cfg
			if mode == "not dispatched" {
				copy := *cfg
				copy.ProviderWriteMode = config.ProviderWriteModeReadOnly
				serviceConfig = &copy
			}
			service := subscriptions.NewAccountStripeService(serviceConfig, mid.UUID(), terms.PSPID, "acct_initial", "sk_test_initial")
			resolver := initialStripeResolver{service}
			fx.runner.Registry = intents.NewRegistry(NewInitialMembershipIntentHandler(fx.svc, resolver))
			var mu sync.Mutex
			var pi map[string]any
			var setup map[string]any
			setupStatus := "requires_payment_method"
			posts, cancels := 0, 0
			release := stripeapi.InstallBaseTransport(initialStripeWire(func(r *http.Request) (*http.Response, error) {
				mu.Lock()
				defer mu.Unlock()
				require.Equal(t, stripeapi.APIVersion, r.Header.Get(stripeapi.VersionHeader))
				switch {
				case r.Method == "GET" && r.URL.Path == "/v1/customers/search":
					return initialStripeResponse(map[string]any{"data": []any{}}), nil
				case r.Method == "POST" && r.URL.Path == "/v1/customers":
					return initialStripeResponse(map[string]any{"id": "cus_setup"}), nil
				case r.Method == "POST" && r.URL.Path == "/v1/setup_intents":
					b, _ := io.ReadAll(r.Body)
					v, _ := url.ParseQuery(string(b))
					require.Equal(t, "off_session", v.Get("usage"))
					require.Equal(t, "cus_setup", v.Get("customer"))
					metadata := map[string]string{}
					for k, values := range v {
						if strings.HasPrefix(k, "metadata[") {
							metadata[strings.TrimSuffix(strings.TrimPrefix(k, "metadata["), "]")] = values[0]
						}
					}
					setup = map[string]any{"id": "seti_setup", "status": setupStatus, "customer": "cus_setup", "payment_method": "pm_setup", "usage": "off_session", "livemode": false, "payment_method_types": []string{"card"}, "metadata": metadata, "client_secret": "seti_setup_secret_private"}
					return initialStripeResponse(setup), nil
				case r.Method == "GET" && r.URL.Path == "/v1/setup_intents/seti_setup":
					setup["status"] = setupStatus
					return initialStripeResponse(setup), nil
				case r.Method == "GET" && r.URL.Path == "/v1/payment_methods/pm_setup":
					return initialStripeResponse(map[string]any{"id": "pm_setup", "customer": "cus_setup", "type": "card", "livemode": false, "card": map[string]any{"last4": "4242", "brand": "visa", "exp_month": 12, "exp_year": 2035}}), nil
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
					pi = map[string]any{"id": "pi_initial", "status": "succeeded", "customer": v.Get("customer"), "payment_method": v.Get("payment_method"), "amount": 999, "amount_received": 999, "currency": "usd", "setup_future_usage": "off_session", "capture_method": "automatic", "confirmation_method": "automatic", "latest_charge": "ch_initial", "metadata": metadata, "livemode": false, "client_secret": "pi_initial_secret_sensitive"}
					if strings.HasPrefix(mode, "verify ") {
						pi["status"] = "processing"
					}
					if mode == "authentication" {
						pi["status"] = "requires_action"
					}
					if mode == "declined" {
						pi["status"] = "requires_payment_method"
						pi["last_payment_error"] = map[string]any{"code": "card_declined", "decline_code": "insufficient_funds"}
					}
					if mode == "lost response" || mode == "refunded" || mode == "disputed" {
						return nil, errors.New("simulated lost accepted response")
					}
					return initialStripeResponse(pi), nil
				case r.Method == "POST" && r.URL.Path == "/v1/payment_intents/pi_initial/cancel":
					cancels++
					pi["status"] = "canceled"
					if mode == "verify lost cancel" {
						return nil, errors.New("lost cancellation response")
					}
					return initialStripeResponse(pi), nil
				case r.Method == "GET" && r.URL.Path == "/v1/payment_intents":
					return initialStripeResponse(map[string]any{"data": []any{pi}, "has_more": false}), nil
				case r.Method == "GET" && r.URL.Path == "/v1/payment_intents/pi_initial":
					return initialStripeResponse(pi), nil
				case r.Method == "GET" && r.URL.Path == "/v1/charges/ch_initial":
					method := "pm_initial"
					customer := "cus_initial"
					if mode == "setup" {
						method = "pm_setup"
						customer = "cus_setup"
					}
					if mode == "wrong method" {
						method = "pm_other"
					}
					ch := map[string]any{"id": "ch_initial", "amount": 999, "amount_captured": 999, "currency": "usd", "customer": customer, "payment_method": method, "payment_intent": "pi_initial", "paid": true, "captured": true, "status": "succeeded"}
					if mode == "refunded" {
						ch["refunded"] = true
						ch["amount_refunded"] = 999
					}
					if mode == "disputed" {
						ch["disputed"] = true
					}
					return initialStripeResponse(ch), nil
				default:
					t.Errorf("unexpected Stripe route %s %s", r.Method, r.URL.Path)
					return nil, errors.New("unexpected Stripe route")
				}
			}))
			defer release()
			principal := billingauth.DelegatedPrincipal{CredentialClass: billingauth.CredentialClassUserSession, MerchantID: mid.String(), SubjectID: terms.CustomerID.String()}
			key := "stripe-initial-" + uuid.NewString()
			if mode == "setup" {
				fx.svc.RailCustomerService = payments.NewRailCustomerService(fx.db)
				setupKey := "setup-key-" + uuid.NewString()
				action, err := fx.svc.CreateStripeMethodSetup(fx.ctx, terms.PSPID, setupKey, principal, resolver)
				require.NoError(t, err)
				require.Equal(t, "seti_setup_secret_private", action.ClientSecret)
				again, err := fx.svc.CreateStripeMethodSetup(fx.ctx, terms.PSPID, setupKey, principal, resolver)
				require.NoError(t, err)
				require.Equal(t, action.ID, again.ID)
				_, err = fx.svc.ConfirmStripeMethodSetup(fx.ctx, action.ID.UUID(), principal, resolver)
				require.Error(t, err)
				var before int
				require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.payments WHERE customer_id=$1`, terms.CustomerID).Scan(&before))
				require.Zero(t, before)
				stranger := principal
				stranger.SubjectID = uuid.NewString()
				_, err = fx.svc.StripeMethodSetup(fx.ctx, action.ID.UUID(), stranger, resolver)
				require.Error(t, err)
				mu.Lock()
				setupStatus = "succeeded"
				mu.Unlock()
				completed, err := fx.svc.ConfirmStripeMethodSetup(fx.ctx, action.ID.UUID(), principal, resolver)
				require.NoError(t, err)
				require.NotNil(t, completed.PaymentMethodID)
				require.Empty(t, completed.ClientSecret)
				terms.PaymentMethodID = completed.PaymentMethodID.UUID()
				stored, err := NewCheckoutSessionRepo(fx.db).GetByID(fx.ctx, action.ID.UUID())
				require.NoError(t, err)
				raw, _ := json.Marshal(stored.RailState)
				require.NotContains(t, string(raw), "secret")
			}
			var sessionID *uuid.UUID
			if mode == "opaque valid" {
				key = "checkout_session:" + uuid.NewString()
			}
			if mode == "opaque malformed" {
				key = "checkout_session:not-a-uuid"
			}
			if strings.HasPrefix(mode, "bound ") || mode == "refunded" || mode == "disputed" {
				id := uuid.New()
				sessionID = &id
				if mode != "bound missing" {
					quote := terms
					if mode == "bound changed quote" {
						quote.Entitlements = map[string]*int{"changed": nil}
					}
					raw, err := json.Marshal(quote)
					require.NoError(t, err)
					expires := fx.svc.now().Add(time.Hour)
					session := &models.CheckoutSession{ID: id, CustomerID: terms.CustomerID, PriceID: &terms.PriceID, PspID: terms.PSPID, Mode: models.CheckoutSessionModeSubscription, Rail: models.RailStripe, Status: models.CheckoutSessionStatusRequiresAction, Amount: &terms.Amount, Currency: &terms.Currency, ExpiresAt: &expires, RailState: map[string]any{initialMembershipQuoteKey: string(raw)}}
					require.NoError(t, NewCheckoutSessionRepo(fx.db).Create(fx.ctx, session))
					if mode == "bound deleted" {
						_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE billing.checkout_sessions SET deleted_at=now() WHERE id=$1`, id)
						require.NoError(t, err)
					}
					if mode == "bound wrong owner" {
						other := uuid.New()
						_, err = fx.db.Pool().Exec(fx.ctx, `INSERT INTO billing.customers(id,merchant_id) VALUES($1,$2)`, other, mid.UUID())
						require.NoError(t, err)
						_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE billing.checkout_sessions SET customer_id=$2 WHERE id=$1`, id, other)
						require.NoError(t, err)
					}
				}
			}
			cfg.EngineAdmissionHold = true
			_, heldErr := fx.svc.ConfirmInitialMembership(fx.ctx, terms, key, principal, sessionID)
			require.Error(t, heldErr)
			require.Zero(t, posts, "hold refuses fresh payment before provider I/O")
			cfg.EngineAdmissionHold = false
			_, confirmErr := fx.svc.ConfirmInitialMembership(fx.ctx, terms, key, principal, sessionID)
			if mode == "bound missing" || mode == "bound wrong owner" || mode == "bound changed quote" || mode == "bound deleted" {
				require.Error(t, confirmErr)
				require.Zero(t, posts, "invalid session binding refuses before provider I/O")
				_, lookupErr := intents.NewStore(fx.db).GetByIdempotencyKey(fx.ctx, InitialMembershipIdempotencyKey(key))
				require.True(t, db.IsNotFound(lookupErr), "invalid session cannot admit a durable payment operation")
				return
			}
			op, err := intents.NewStore(fx.db).GetByIdempotencyKey(fx.ctx, InitialMembershipIdempotencyKey(key))
			require.NoError(t, err)
			if sessionID != nil {
				before := posts
				changed := uuid.New()
				_, err = fx.svc.ConfirmInitialMembership(fx.ctx, terms, key, principal, &changed)
				require.Error(t, err, "canonical replay cannot change session binding")
				_, err = fx.svc.ConfirmInitialMembership(fx.ctx, terms, key, principal, nil)
				require.Error(t, err, "canonical replay cannot erase session binding")
				require.Equal(t, before, posts)
				if mode == "refunded" || mode == "disputed" {
					_, err = fx.db.Gen(fx.ctx).ExpireCheckoutSessions(fx.ctx, gen.ExpireCheckoutSessionsParams{MerchantID: mid.UUID(), Now: fx.svc.now().Add(2 * time.Hour), RowLimit: 100})
					require.NoError(t, err)
					expired, err := NewCheckoutSessionRepo(fx.db).GetByID(fx.ctx, *sessionID)
					require.NoError(t, err, "expiry retains the accepted session row")
					require.Equal(t, models.CheckoutSessionStatusExpired, expired.Status)
				}
			}
			if strings.HasPrefix(mode, "verify ") {
				require.ErrorIs(t, confirmErr, ErrCheckoutProcessing)
				mu.Lock()
				pi["status"] = "requires_payment_method"
				pi["last_payment_error"] = map[string]any{"code": "card_declined", "decline_code": "insufficient_funds"}
				mu.Unlock()
				if mode == "verify readonly" {
					cfg.ProviderWriteMode = config.ProviderWriteModeReadOnly
				}
				if mode == "verify limited" {
					cfg.ProviderWriteMode = config.ProviderWriteModeLimited
				}
				op, err = fx.runner.VerifyByID(fx.ctx, op.ID)
				require.NoError(t, err)
				require.Equal(t, intents.StatusFailedRetryable, op.Status)
				require.Zero(t, cancels, "Verify never cancels")
				require.Equal(t, 1, posts)
				op, err = fx.runner.ExecuteByID(fx.ctx, op.ID)
				require.NoError(t, err)
				if mode == "verify readonly" {
					require.Zero(t, cancels)
					cfg.ProviderWriteMode = config.ProviderWriteModeFull
					op, err = fx.runner.VerifyByID(fx.ctx, op.ID)
					require.NoError(t, err)
					op, err = fx.runner.ExecuteByID(fx.ctx, op.ID)
					require.NoError(t, err)
				}
				if mode == "verify lost cancel" {
					require.Equal(t, intents.StatusUnknownNeedsVerify, op.Status)
					op, err = fx.runner.VerifyByID(fx.ctx, op.ID)
					require.NoError(t, err)
				}
				require.Equal(t, 1, posts, "cancellation recovery cannot create another PI")
				require.Equal(t, intents.StatusFailedTerminal, op.Status)
				_, err = fx.runner.ExecuteByID(fx.ctx, op.ID)
				require.NoError(t, err)
				_, err = fx.runner.VerifyByID(fx.ctx, op.ID)
				require.NoError(t, err)
				require.Equal(t, 1, cancels)
				require.Equal(t, 1, posts, "same original PI throughout cancellation")
				require.NoError(t, intents.ValidateInitialMembershipTerminal(op))
				return
			}
			if mode == "lost response" || mode == "refunded" || mode == "disputed" {
				cfg.EngineAdmissionHold = true // reconciliation survives an admission hold
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
			if mode == "not dispatched" {
				require.Equal(t, intents.StatusFailedTerminal, op.Status)
				require.NoError(t, intents.ValidateInitialMembershipTerminal(op))
				require.Error(t, intents.NewStore(fx.db).RetainInitialMembershipNonexecution(fx.ctx, op, intents.InitialMembershipNonexecutionProof{}))
			} else if mode == "wrong method" {
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
				if mode == "refunded" || mode == "disputed" {
					require.Equal(t, models.StatusCancelled, sub.Status)
					var grants, notices int
					require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.grants WHERE source_id=$1 AND event='grant'`, sub.ID.String()).Scan(&grants))
					require.Zero(t, grants)
					require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.notifications WHERE customer_id=$1`, sub.CustomerID).Scan(&notices))
					require.Zero(t, notices)
					receipt, paid, err := intents.LoadCollectedReceipt(op)
					require.NoError(t, err)
					require.True(t, paid)
					require.NotEmpty(t, receipt.ReversalKind())
					// Drain acknowledged host effects, then move the actual authored book.
					_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE billing.host_outbox SET delivered_at=now() WHERE merchant_id=$1`, mid.UUID())
					require.NoError(t, err)
					var artifact bytes.Buffer
					err = merchantarchive.Export(t.Context(), fx.db, mid, &artifact)
					require.NoError(t, err, "%+v", err)
					super, app := dbtest.SharedRLSPostgres(t)
					schema := "reversal_" + strings.ReplaceAll(uuid.NewString(), "-", "")
					dbtest.ApplyPostgresMigrations(t, super, app, schema)
					target, err := db.NewDB(t.Context(), &config.DBConfig{URL: app, Schema: schema})
					require.NoError(t, err)
					t.Cleanup(func() { _ = target.Close() })
					_, err = target.Qx(t.Context()).Exec(t.Context(), `INSERT INTO openrails.merchants(id,slug) VALUES($1,$2)`, mid.UUID(), mid.String())
					require.NoError(t, err)
					_, err = merchantarchive.Restore(t.Context(), target, mid, bytes.NewReader(artifact.Bytes()))
					require.NoError(t, err, "%+v", err)
					var restored bytes.Buffer
					require.NoError(t, merchantarchive.Export(t.Context(), target, mid, &restored))
					require.Equal(t, artifact.Bytes(), restored.Bytes(), "actual initial reversal book survives restore")
					_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE billing.rail_intents SET payload=jsonb_set(payload,'{checkout_session_id}',to_jsonb($2::text)) WHERE id=$1`, op.ID, uuid.NewString())
					require.NoError(t, err)
					require.Error(t, merchantarchive.Export(t.Context(), fx.db, mid, io.Discard), "typed but missing session binding cannot export")
					_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE billing.rail_intents SET payload=$2 WHERE id=$1`, op.ID, op.Payload)
					require.NoError(t, err)

				}
			}
			if sessionID != nil {
				projected, err := NewCheckoutSessionRepo(fx.db).GetByID(fx.ctx, *sessionID)
				require.NoError(t, err)
				require.Equal(t, models.CheckoutSessionStatusSucceeded, projected.Status)
				require.Nil(t, projected.ExpiresAt)
			}
			var successCount int
			require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.payments WHERE customer_id=$1 AND status='completed'`, terms.CustomerID).Scan(&successCount))
			if mode == "declined" || mode == "wrong method" || mode == "not dispatched" {
				require.Zero(t, successCount)
			} else {
				require.Equal(t, 1, successCount)
			}
			_, _ = fx.svc.ConfirmInitialMembership(fx.ctx, terms, key, principal, sessionID)
			if mode == "not dispatched" {
				require.Zero(t, posts)
			} else {
				require.Equal(t, 1, posts, "replayed original operation must never create another PI")
			}
		})
	}
}
