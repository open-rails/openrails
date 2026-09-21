//go:build integration

package checkout

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

type initialMembershipFixtureSecrets struct {
	owner merchant.ID
	name  string
}

func (s initialMembershipFixtureSecrets) Get(_ context.Context, owner merchant.ID, name string) (merchants.Secret, error) {
	if owner != s.owner || name != s.name {
		return merchants.Secret{}, merchants.ErrSecretNotFound
	}
	return merchants.Secret{Name: name, Value: "synthetic-custody-key", Version: 1}, nil
}

func TestHyperSwitchInitialMembershipAtomicWorkflow(t *testing.T) {
	for _, mode := range []string{"paid", "declined", "lost response", "wrong destination", "park before fence", "profile before fence"} {
		t.Run(mode, func(t *testing.T) {
			fx := newSubIntentFixture(t)
			_, seedErr := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.products SET entitlements_spec='{"initial_engine_access":null}' WHERE id=(SELECT product_id FROM billing.prices WHERE id=$1)`, fx.priceID)
			require.NoError(t, seedErr)
			fx.prepare(t)
			terms := fx.payload.Terms
			terms.CollectionPolicy = models.CollectionPolicyEngine
			_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.psps SET rail='nmi' WHERE id=$1`, terms.PSPID)
			require.NoError(t, err)
			custody := uuid.New()
			vendor := "vendor_" + uuid.NewString()
			mid, err := merchant.Require(fx.ctx)
			require.NoError(t, err)
			_, err = fx.db.Pool().Exec(fx.ctx, `INSERT INTO billing.custodians(id,merchant_id,key,kind,environment,account_id,settings,credential_versions) VALUES($1,$2,$3,'hyperswitch','test',$4,'{"profile_id":"profile_initial","public_api_key":"public_initial"}','{"api_key":1}')`, custody, mid.UUID(), custody.String(), vendor)
			require.NoError(t, err)
			_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE billing.payment_methods SET custodian='hyperswitch',custodian_id=$2,rail_customer_ref='customer_initial',rail_method_ref='method_initial',charge_via='pan_proxy' WHERE id=$1`, terms.PaymentMethodID, custody)
			require.NoError(t, err)
			gateway, err := fx.svc.resolveNMIClient(fx.ctx, "mobius")
			require.NoError(t, err)
			var posts atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "api-key=synthetic-custody-key", r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v2/payment-methods/method_initial" {
					fmt.Fprintf(w, `{"id":"method_initial","merchant_id":%q,"customer_id":"customer_initial","storage_type":"persistent","payment_method_data":{"card":{"last4_digits":"1111","expiry_month":"12","expiry_year":"2030"}}}`, vendor)
					return
				}
				require.Equal(t, "/v2/proxy", r.URL.Path)
				if r.Method == http.MethodGet {
					destination := gateway.DirectPostURL
					if mode == "wrong destination" {
						destination += "/unaccepted"
					}
					if mode == "park before fence" {
						_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.payment_methods SET park_reason='fixture revoked',parked_at=now() WHERE id=$1`, terms.PaymentMethodID)
						require.NoError(t, err)
					}
					if mode == "profile before fence" {
						_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.custodians SET settings=jsonb_set(settings,'{profile_id}','"changed_profile"') WHERE id=$1`, custody)
						require.NoError(t, err)
					}
					fmt.Fprintf(w, `{"contract":"openrails-nmi-form-v2","strict":true,"max_response_bytes":65536,"routes":[{"destination_url":%q,"method":"POST","response_profile":"nmi_classic"}]}`, destination)
					return
				}
				posts.Add(1)
				var input struct {
					Form map[string]string `json:"request_body"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
				require.Equal(t, "recurring", input.Form["billing_method"])
				require.Equal(t, "customer", input.Form["initiated_by"])
				require.Empty(t, input.Form["recurring"])
				require.Equal(t, "9.99", input.Form["amount"])
				if mode == "declined" {
					fmt.Fprint(w, `{"response":{"response":"2","response_code":"200","responsetext":"Declined"},"status_code":200,"response_headers":{}}`)
					return
				}
				form := url.Values{}
				for key, value := range input.Form {
					form.Set(key, value)
				}
				fx.gateway.createForm.Store(form)
				fx.gateway.charged.Store(true)
				if mode == "lost response" {
					conn, _, err := w.(http.Hijacker).Hijack()
					require.NoError(t, err)
					_ = conn.Close()
					return
				}
				fmt.Fprintf(w, `{"response":{"response":"1","response_code":"100","responsetext":"Approved","transactionid":%q},"status_code":200,"response_headers":{}}`, fx.gateway.txnID)
			}))
			defer server.Close()
			fx.svc.Config = &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull, HyperSwitch: &config.HyperSwitchConfig{APIBaseURL: server.URL}}
			name, err := merchants.CustodianSecretName("hyperswitch", "test", vendor, "api_key")
			require.NoError(t, err)
			fx.svc.MerchantSecrets = initialMembershipFixtureSecrets{mid, name}
			method, err := fx.db.Gen(fx.ctx).GetPaymentMethodByID(fx.ctx, gen.GetPaymentMethodByIDParams{MerchantID: mid.UUID(), ID: terms.PaymentMethodID})
			require.NoError(t, err)
			_, err = charge.FreezeHyperSwitchBinding(fx.ctx, fx.db.Gen(fx.ctx), method, server.URL)
			require.NoError(t, err, "fixture account binding")
			fx.svc.Intents = fx.runner
			fx.runner.Registry = intents.NewRegistry(NewInitialMembershipIntentHandler(fx.svc, upgradeReceiptResolver{gateway}))
			principal := billingauth.DelegatedPrincipal{CredentialClass: billingauth.CredentialClassUserSession, MerchantID: mid.String(), SubjectID: terms.CustomerID.String()}
			key := "initial-engine-" + uuid.NewString()
			_, confirmationErr := fx.svc.ConfirmInitialMembership(fx.ctx, terms, key, principal)
			t.Logf("confirmation: %v", confirmationErr)
			in, err := intents.NewStore(fx.db).GetByIdempotencyKey(fx.ctx, InitialMembershipIdempotencyKey(key))
			require.NoError(t, err)
			if mode == "wrong destination" || mode == "park before fence" || mode == "profile before fence" {
				require.NotEqual(t, intents.StatusSucceeded, in.Status)
				var progress map[string]any
				if len(in.ResultEvidence) > 0 {
					require.NoError(t, json.Unmarshal(in.ResultEvidence, &progress))
				}
				require.NotContains(t, progress, "initial_submitted")
				require.Zero(t, posts.Load())
				require.Zero(t, fx.gateway.createCalls.Load())
				var payments, subscriptions, access int
				require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.payments WHERE customer_id=$1`, terms.CustomerID).Scan(&payments))
				require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.subscriptions WHERE customer_id=$1`, terms.CustomerID).Scan(&subscriptions))
				require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.entitlements WHERE customer_id=$1`, terms.CustomerID).Scan(&access))
				require.Zero(t, payments)
				require.Zero(t, subscriptions)
				require.Zero(t, access)
				return
			}
			if mode == "lost response" {
				in, err = fx.runner.VerifyByID(fx.ctx, in.ID)
				require.NoError(t, err)
			}
			want := intents.StatusSucceeded
			if mode == "declined" {
				want = intents.StatusFailedTerminal
			}
			if in.LastFailureReason != nil {
				t.Logf("durable error: %s", *in.LastFailureReason)
			}
			require.Equal(t, want, in.Status, string(in.ResultEvidence))
			require.NoError(t, intents.ValidateInitialMembershipTerminal(in))
			pmt, err := fx.db.Gen(fx.ctx).GetPaymentByID(fx.ctx, gen.GetPaymentByIDParams{MerchantID: mid.UUID(), ID: terms.PaymentID})
			require.NoError(t, err)
			require.NotNil(t, pmt.TokenType)
			require.Equal(t, charge.TokenTypePANViaProxy, *pmt.TokenType)
			var count int
			require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.subscriptions WHERE id=$1 AND collection_policy='engine' AND rail_subscription_id=''`, terms.SubscriptionID).Scan(&count))
			if mode == "declined" {
				require.Zero(t, count)
			} else {
				require.Equal(t, 1, count)
			}
			method, err = fx.db.Gen(fx.ctx).GetPaymentMethodByID(fx.ctx, gen.GetPaymentMethodByIDParams{MerchantID: mid.UUID(), ID: terms.PaymentMethodID})
			require.NoError(t, err)
			var access, events int
			require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.entitlements WHERE customer_id=$1 AND entitlement='initial_engine_access' AND revoked_at IS NULL`, terms.CustomerID).Scan(&access))
			require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.host_outbox WHERE payment_id=$1`, terms.PaymentID).Scan(&events))
			if mode == "declined" {
				require.Zero(t, access)
				require.Zero(t, events)
				require.Empty(t, method.StoredCredentialRecurringRef)
			} else {
				require.Equal(t, 1, access)
				require.Equal(t, 1, events)
				require.Equal(t, fx.gateway.txnID, method.StoredCredentialRecurringRef)
			}
			require.Empty(t, method.StoredCredentialUnscheduledRef)
			_, _ = fx.svc.ConfirmInitialMembership(fx.ctx, terms, key, principal)
			require.EqualValues(t, 1, posts.Load())
			require.Zero(t, fx.gateway.createCalls.Load())
		})
	}
}
