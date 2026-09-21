//go:build integration

package integrationharness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	orauthkit "github.com/open-rails/openrails/embed/authkit"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	embcp "github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// Engine rows are explicit internal fixtures: public engine enrollment and a
// collection executor are not enabled. Customer requests use the actual Client,
// HTTP authentication, durable River job and production lifecycle worker.
func TestEngineSubscriptionLifecycleHTTP(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	var receiptMu sync.Mutex
	receiptOrder, receiptTxn := "", ""
	var receiptReads atomic.Int64
	var deleteCalls atomic.Int64
	var heldMu sync.Mutex
	heldMethod := ""
	deleteEntered, deleteRelease := make(chan struct{}, 1), make(chan struct{})
	var releaseOnce sync.Once
	releaseDelete := func() { releaseOnce.Do(func() { close(deleteRelease) }) }
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "api-key=lifecycle-delete-key", r.Header.Get("Authorization"))
		require.Equal(t, "synthetic-engine", r.Header.Get("x-profile-id"))
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/v2/proxy" {
			fmt.Fprint(w, `{"contract":"openrails-nmi-form-v2","native_vault_delete_contract":"openrails-native-vault-delete-v1"}`)
			return
		}
		require.Equal(t, http.MethodDelete, r.Method)
		id := strings.TrimPrefix(r.URL.Path, "/v2/payment-methods/")
		deleteCalls.Add(1)
		heldMu.Lock()
		hold := heldMethod == id
		heldMu.Unlock()
		if hold {
			deleteEntered <- struct{}{}
			<-deleteRelease
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
	}))
	defer hs.Close()
	defer releaseDelete()
	var sends atomic.Int64
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receiptMu.Lock()
		order, txn := receiptOrder, receiptTxn
		receiptMu.Unlock()
		if txn != "" && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/payments/"+txn) {
			receiptReads.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "transaction", "id": txn, "amount": "9.99", "currency": "USD", "response": "1", "customer_vault_id": "", "actions": []map[string]any{{"id": txn + "-a", "type": "sale", "amount": "9.99", "success": true, "response": "1"}}})
			return
		}
		require.NoError(t, r.ParseForm())
		if order != "" && r.Form.Get("order_id") == order && r.Form.Get("type") != "sale" {
			receiptReads.Add(1)
			_, _ = fmt.Fprintf(w, `<nm_response><transaction><transaction_id>%s</transaction_id><order_id>%s</order_id><action><action_type>sale</action_type><success>1</success></action></transaction></nm_response>`, txn, order)
			return
		}
		sends.Add(1)
		http.Error(w, "unexpected provider request", 500)
	}))
	defer gateway.Close()
	h := New(t, t.Context())
	// These are explicit merchant-admin deletion requests against a disposable
	// fixture; enable its ordinary maintenance gate and restore it afterward.
	var previousSwitch bool
	require.NoError(t, h.Pool().QueryRow(t.Context(), `SELECT enabled FROM billing.destructive_action_switch`).Scan(&previousSwitch))
	_, switchErr := h.Pool().Exec(t.Context(), `UPDATE billing.destructive_action_switch SET enabled=true`)
	require.NoError(t, switchErr)
	t.Cleanup(func() {
		_, err := h.Pool().Exec(context.Background(), `UPDATE billing.destructive_action_switch SET enabled=$1`, previousSwitch)
		require.NoError(t, err)
	})

	var host billingauth.DelegatedAuthenticator
	surface := h.StartStandalone("USD", WithClock(clockwork.NewFakeClockAt(now)), WithConfig(func(c *config.Config) {
		c.ProviderSandbox = &config.ProviderSandboxConfig{NMIGatewayURL: gateway.URL}
		c.ProviderWriteMode = config.ProviderWriteModeFull
		c.HyperSwitch = &config.HyperSwitchConfig{APIBaseURL: hs.URL, SDKURL: hs.URL + "/sdk.js"}
		c.Encryption = &config.EncryptionConfig{MasterKey: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="}
	}), func(c *standaloneConfig) {
		c.delegatedAuthenticator = billingauth.DelegatedAuthenticatorFunc(func(ctx context.Context, r *http.Request) (*billingauth.DelegatedPrincipal, error) {
			return host.AuthenticateDelegated(ctx, r)
		})
	})
	owned := surface.ProvisionOwnedMerchant("engine-life-" + uuid.NewString()[:8])
	cp, rt := embcp.Get(surface.App()), surface.App().Runtime
	var err error
	host, err = orauthkit.NewDelegatedAuthenticator(cp.AuthService().Verifier(), owned.MerchantID.String())
	require.NoError(t, err)
	owner := surface.Client(openrails.WithAPIKey(owned.APIKey), openrails.WithMerchantID(owned.MerchantID))
	psp := h.ArmLoopbackNMI(rt, owned.MerchantID)
	product, err := owner.CreateProduct(t.Context(), openrails.CreateProductRequest{Key: uuid.NewString(), DisplayName: "Lifecycle", EntitlementsSpec: map[string]*int{"engine_access": nil}})
	require.NoError(t, err)
	hours := 720
	price, err := owner.CreatePrice(t.Context(), openrails.CreatePriceRequest{ProductID: product.ID, UnitAmount: 9_990_000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours})
	require.NoError(t, err)
	pool := h.MerchantPool(owned.MerchantID.UUID())
	ctx := merchant.WithID(t.Context(), owned.MerchantID)
	custodian := uuid.New()
	_, err = pool.Exec(ctx, `INSERT INTO billing.custodians(id,merchant_id,key,kind,environment,account_id,settings,credential_versions) VALUES($1,$2,$3,'hyperswitch','test',$3,'{"profile_id":"synthetic-engine","public_api_key":"synthetic"}','{"api_key":1}')`, custodian, owned.MerchantID.UUID(), custodian.String())
	require.NoError(t, err)
	secret, err := merchants.CustodianSecretName("hyperswitch", "test", custodian.String(), "api_key")
	require.NoError(t, err)
	_, err = rt.Merchants.Secrets().Put(ctx, owned.MerchantID, secret, "lifecycle-delete-key")
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup := context.Background()
		_, err := pool.Exec(cleanup, `DELETE FROM public.river_job WHERE args->>'merchant_id'=$1`, owned.MerchantID.String())
		require.NoError(t, err)
		_, err = pool.Exec(cleanup, `DELETE FROM billing.rail_mutation_logs WHERE merchant_id=$1`, owned.MerchantID.UUID())
		require.NoError(t, err)
		_, err = pool.Exec(cleanup, `DELETE FROM billing.rail_intents WHERE merchant_id=$1`, owned.MerchantID.UUID())
		require.NoError(t, err)
		_, err = pool.Exec(cleanup, `UPDATE billing.subscriptions SET deleted_at=now() WHERE merchant_id=$1`, owned.MerchantID.UUID())
		require.NoError(t, err)
	})
	newCustomer := func() (*openrails.Client, uuid.UUID, string) {
		suffix := uuid.NewString()[:8]
		user, err := cp.Core().CreateUser(t.Context(), suffix+"@example.test", "engine"+suffix)
		require.NoError(t, err)
		token, _, err := cp.Core().MintAccessToken(t.Context(), user.ID, nil)
		require.NoError(t, err)
		customer := uuid.MustParse(user.ID)
		_, err = owner.EnsureCustomer(t.Context(), openrails.CustomerID(customer))
		require.NoError(t, err)
		c, err := openrails.NewRemote(surface.BaseURL, openrails.WithMerchantID(owned.MerchantID), openrails.WithTokenProvider(func(context.Context) (string, error) { return token, nil }))
		require.NoError(t, err)
		return c, customer, token
	}
	_, _, foreignToken := newCustomer()
	cancelWorker := riverjobs.CancelSubscriptionWorker{DB: rt.DB, Config: rt.Config, SubscriptionService: rt.SubscriptionService, SubscriptionLifecycleService: rt.SubscriptionLifecycleService, UserSubscriptionService: rt.UserSubscriptionService}
	resumeWorker := riverjobs.ResumeSubscriptionWorker{DB: rt.DB, Config: rt.Config, Clock: rt.Clock, SubscriptionService: rt.SubscriptionService, SubscriptionLifecycleService: rt.SubscriptionLifecycleService, EntitlementService: rt.EntitlementService}
	runCancel := func(id uuid.UUID) {
		var raw []byte
		require.NoError(t, pool.QueryRow(ctx, `SELECT args FROM public.river_job WHERE kind=$1 AND args->>'subscription_id'=$2 ORDER BY id DESC LIMIT 1`, riverjobs.KindSubscriptionCancel, id.String()).Scan(&raw))
		var args riverjobs.CancelSubscriptionArgs
		require.NoError(t, json.Unmarshal(raw, &args))
		require.NoError(t, cancelWorker.Work(t.Context(), &river.Job[riverjobs.CancelSubscriptionArgs]{Args: args}))
	}
	runResume := func(id uuid.UUID) {
		var raw []byte
		require.NoError(t, pool.QueryRow(ctx, `SELECT args FROM public.river_job WHERE kind=$1 AND args->>'subscription_id'=$2 ORDER BY id DESC LIMIT 1`, riverjobs.KindSubscriptionResume, id.String()).Scan(&raw))
		var args riverjobs.ResumeSubscriptionArgs
		require.NoError(t, json.Unmarshal(raw, &args))
		require.NoError(t, resumeWorker.Work(t.Context(), &river.Job[riverjobs.ResumeSubscriptionArgs]{Args: args}))
	}
	for _, scenario := range []string{"engine", "engine_expired", "engine_chargeback", "engine_past_due_customer", "engine_past_due_merchant", "engine_maintenance", "engine_resume_then_delete", "engine_delete_then_resume", "engine_resolved_charge_delete", "provider"} {
		t.Run(scenario, func(t *testing.T) {
			maintenance := scenario == "engine_maintenance"
			resolvedDelete := scenario == "engine_resolved_charge_delete"
			pastDue := scenario == "engine_past_due_customer" || scenario == "engine_past_due_merchant" || resolvedDelete
			policy := "engine"
			if scenario == "provider" {
				policy = "provider"
			}
			customer, user, token := newCustomer()
			id := uuid.New()
			end := now.Add(20 * 24 * time.Hour)
			if scenario == "engine_expired" || pastDue || maintenance {
				end = now.Add(-100 * 24 * time.Hour)
			}
			remote := ""
			if policy == "provider" {
				remote = "native-" + id.String()
			}
			method := uuid.New()
			_, err := pool.Exec(ctx, `INSERT INTO billing.payment_methods(id,merchant_id,customer_id,psp_id,rail,custodian,custodian_id,rail_customer_ref,rail_method_ref,stored_credential_recurring_ref,initial_transaction_id) VALUES($1,$2,$3,$4,'nmi','hyperswitch',$5,$6,$7,'synthetic-recurring','synthetic-initial')`, method, owned.MerchantID.UUID(), user, psp, custodian, "customer-"+method.String(), "method-"+method.String())
			require.NoError(t, err)
			_, err = pool.Exec(ctx, `INSERT INTO billing.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,rail,collection_policy,rail_subscription_id,status,current_period_starts_at,current_period_ends_at,entitlements_spec_snapshot,payment_method_id) VALUES($1,$2,$3,$4,$5,$6,'nmi',$7,$8,'active',$9,$10,'{"engine_access":null}',$11)`, id, owned.MerchantID.UUID(), user, product.ID.UUID(), price.ID.UUID(), psp, policy, remote, end.Add(-720*time.Hour), end, method)
			require.NoError(t, err)
			_, err = rt.EntitlementService.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{UserID: user.String(), Entitlement: "engine_access", Indefinite: true, SourceType: models.EntitlementSourceSubscription, SourceID: id})
			require.NoError(t, err)
			var operation uuid.UUID
			var payload []byte
			if pastDue || maintenance {
				_, err = pool.Exec(ctx, `UPDATE billing.subscriptions SET status='past_due',next_retry_at=$2,retry_attempts=1 WHERE id=$1`, id, now)
				require.NoError(t, err)
				accepted, err := rt.MoneyService.AdmitDueSubscriptionCollection(ctx, id, now)
				require.NoError(t, err)
				operation, payload = accepted.ID, accepted.Payload
				// No executor exists: uncertainty is explicitly synthetic.
				_, err = pool.Exec(ctx, `UPDATE billing.rail_intents SET status='unknown_needs_verify',result_evidence='{"submission":"synthetic uncertainty"}' WHERE id=$1`, operation)
				require.NoError(t, err)
			}
			if maintenance {
				engine := converge.NewConvergeEngine(rt.DB)
				engine.Now = func() time.Time { return now }
				fetcher := &engineMaintenanceFetcher{}
				for _, state := range []string{"active", "past_due", "unknown"} {
					for _, grace := range []time.Time{now.Add(-time.Hour), now.Add(time.Hour)} {
						_, err = pool.Exec(ctx, `UPDATE billing.subscriptions SET status=$2,next_retry_at=NULL,grace_ends_at=$3 WHERE id=$1`, id, state, grace)
						require.NoError(t, err)
						require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(c context.Context) error {
							if _, err := engine.Converge(c, converge.Scope{Merchant: owned.MerchantID, Customer: &user}); err != nil {
								return err
							}
							_, err := reconcile.ReconcileUnknownCohort(c, rt.DB, rt.SubscriptionLifecycleService, map[reconcile.Provider]reconcile.RailFetcher{reconcile.ProviderNMI: fetcher}, nil, owned.MerchantID, now, reconcile.UnknownReconcileOptions{})
							return err
						}))
						var observed string
						var next *time.Time
						require.NoError(t, pool.QueryRow(ctx, `SELECT status,next_retry_at FROM billing.subscriptions WHERE id=$1`, id).Scan(&observed, &next))
						require.Equal(t, state, observed)
						require.Nil(t, next)
						replay, err := rt.MoneyService.AdmitDueSubscriptionCollection(ctx, id, now)
						require.NoError(t, err)
						require.Equal(t, operation, replay.ID)
						require.Equal(t, "unknown_needs_verify", replay.Status)
						require.JSONEq(t, string(payload), string(replay.Payload))
					}
				}
				require.Zero(t, fetcher.calls.Load(), "engine-only cohort must not read the native provider")
				return
			}
			sid := openrails.SubscriptionID(id)
			path := surface.BaseURL + "/v1/me/subscriptions/" + sid.String()
			status, raw := requestJSON(t, http.MethodPost, path+"/cancel", foreignToken, map[string]any{"feedback": "not my subscription"})
			require.Equal(t, 404, status, string(raw))
			status, raw = requestJSON(t, http.MethodPost, path+"/cancel", token, map[string]any{"feedback": "taking a break"})
			require.Equal(t, 202, status, string(raw))
			if scenario == "engine_past_due_merchant" {
				require.NoError(t, owner.CancelSubscription(t.Context(), sid, openrails.CancelSubscriptionRequest{Reason: "stop declined renewals"}))
			}
			if scenario == "engine_chargeback" {
				require.NoError(t, rt.SubscriptionLifecycleService.CancelMembership(ctx, &subscriptions.CancelMembershipParams{SubscriptionID: &id, CancelType: models.CancelTypeChargeback, RevokeAccess: true}))
			}
			runCancel(id)
			cancelled, err := customer.GetMySubscription(t.Context(), sid)
			require.NoError(t, err)
			require.Equal(t, policy, cancelled.CollectionPolicy)
			require.Equal(t, scenario == "engine" || scenario == "provider" || scenario == "engine_resume_then_delete" || scenario == "engine_delete_then_resume", cancelled.Resumable)
			if scenario == "engine_delete_then_resume" || resolvedDelete {
				if resolvedDelete {
					accepted, err := intents.NewStore(rt.DB).Get(ctx, operation)
					require.NoError(t, err)
					terms, err := subscriptions.DecodeSubscriptionCollectionPayload(accepted)
					require.NoError(t, err)
					_, err = pool.Exec(ctx, `UPDATE billing.rail_intents SET result_evidence=jsonb_build_object('submitted_at',$2::text) WHERE id=$1`, operation, now.Format(time.RFC3339Nano))
					require.NoError(t, err)
					receiptMu.Lock()
					receiptOrder, receiptTxn = terms.OrderReference, "lifecycle_late_"+id.String()
					receiptMu.Unlock()
					outcome := money.NewSubscriptionCollectionHandler(rt.DB, rt.CollectionResolver, rt.Config, rt.Clock).Verify(ctx, accepted)
					require.Equal(t, intents.OutcomeSucceeded, outcome.Class, outcome.Reason)
					var paid int
					require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM billing.payments WHERE subscription_id=$1 AND status='completed'`, id).Scan(&paid))
					require.Equal(t, 1, paid)
				}
				if !resolvedDelete {
					heldMu.Lock()
					heldMethod = "method-" + method.String()
					heldMu.Unlock()
					t.Cleanup(releaseDelete)
					done := make(chan error, 1)
					go func() {
						result, err := owner.DeletePaymentMethod(t.Context(), openrails.CustomerID(user), openrails.PaymentMethodID(method))
						if err == nil && result.Pending {
							err = fmt.Errorf("deletion remains pending")
						}
						done <- err
					}()
					select {
					case <-deleteEntered:
					case err := <-done:
						require.NoError(t, err)
						t.Fatal("deletion did not reach the held vendor request")
					}
					status, raw = requestJSON(t, http.MethodPost, path+"/resume", token, nil)
					require.Equal(t, 202, status, string(raw))
					var resumeRaw []byte
					require.NoError(t, pool.QueryRow(ctx, `SELECT args FROM public.river_job WHERE kind=$1 AND args->>'subscription_id'=$2 ORDER BY id DESC LIMIT 1`, riverjobs.KindSubscriptionResume, id.String()).Scan(&resumeRaw))
					var resumeArgs riverjobs.ResumeSubscriptionArgs
					require.NoError(t, json.Unmarshal(resumeRaw, &resumeArgs))
					require.Error(t, resumeWorker.Work(t.Context(), &river.Job[riverjobs.ResumeSubscriptionArgs]{Args: resumeArgs}))
					var state string
					require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM billing.subscriptions WHERE id=$1`, id).Scan(&state))
					require.Equal(t, "cancelled", state)
					releaseDelete()
					require.NoError(t, <-done)
				} else {
					result, err := owner.DeletePaymentMethod(t.Context(), openrails.CustomerID(user), openrails.PaymentMethodID(method))
					require.NoError(t, err)
					require.False(t, result.Pending)
				}
				var methodID *uuid.UUID
				require.NoError(t, pool.QueryRow(ctx, `SELECT payment_method_id FROM billing.subscriptions WHERE id=$1`, id).Scan(&methodID))
				require.Nil(t, methodID)
				observed, err := customer.GetMySubscription(t.Context(), sid)
				require.NoError(t, err)
				require.False(t, observed.Resumable)
				status, raw = requestJSON(t, http.MethodPost, path+"/resume", token, nil)
				require.Equal(t, 400, status, string(raw))
				return
			}
			var state string
			var marker *time.Time
			var count int
			require.NoError(t, pool.QueryRow(ctx, `SELECT status,deletion_scheduled_at FROM billing.subscriptions WHERE id=$1`, id).Scan(&state, &marker))
			require.Equal(t, "cancelled", state)
			require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM billing.rail_intents WHERE subscription_id=$1 AND intent_type='nmi_delete_subscription'`, id).Scan(&count))
			if policy == "engine" {
				require.Nil(t, marker)
				require.Zero(t, count)
			} else {
				require.NotNil(t, marker)
				require.Equal(t, 1, count)
			}
			if pastDue {
				var next, grace *time.Time
				require.NoError(t, pool.QueryRow(ctx, `SELECT next_retry_at,grace_ends_at FROM billing.subscriptions WHERE id=$1`, id).Scan(&next, &grace))
				require.Nil(t, next)
				require.Nil(t, grace)
				replay, err := rt.MoneyService.AdmitDueSubscriptionCollection(ctx, id, now.Add(24*time.Hour))
				require.NoError(t, err)
				require.Equal(t, operation, replay.ID)
				require.Equal(t, "unknown_needs_verify", replay.Status)
				require.JSONEq(t, string(payload), string(replay.Payload))
				require.JSONEq(t, `{"submission":"synthetic uncertainty"}`, string(replay.ResultEvidence))
				require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM billing.rail_intents WHERE subscription_id=$1`, id).Scan(&count))
				require.Equal(t, 1, count)
			}
			access, err := owner.HasEntitlement(t.Context(), openrails.CustomerID(user), "engine_access", end)
			require.NoError(t, err)
			require.False(t, access)
			if scenario == "engine_expired" || scenario == "engine_chargeback" || pastDue {
				status, raw = requestJSON(t, http.MethodPost, path+"/resume", token, nil)
				require.Equal(t, 400, status, string(raw))
				_, err = rt.SubscriptionLifecycleService.ResumeMembership(ctx, &subscriptions.ResumeMembershipParams{SubscriptionID: id})
				require.Error(t, err)
				access, err = owner.HasEntitlement(t.Context(), openrails.CustomerID(user), "engine_access", now)
				require.NoError(t, err)
				require.False(t, access)
				return
			}
			status, raw = requestJSON(t, http.MethodPost, path+"/resume", foreignToken, nil)
			require.Equal(t, 404, status, string(raw))
			status, raw = requestJSON(t, http.MethodPost, path+"/resume", token, nil)
			require.Equal(t, 202, status, string(raw))
			// Merchant Client addresses the same durable resume workflow.
			require.NoError(t, owner.ResumeSubscription(t.Context(), sid))
			runResume(id)
			require.NoError(t, pool.QueryRow(ctx, `SELECT status,deletion_scheduled_at FROM billing.subscriptions WHERE id=$1`, id).Scan(&state, &marker))
			require.Equal(t, "active", state)
			require.Nil(t, marker)
			access, err = owner.HasEntitlement(t.Context(), openrails.CustomerID(user), "engine_access", now)
			require.NoError(t, err)
			require.True(t, access)
			if scenario == "engine_resume_then_delete" {
				before := deleteCalls.Load()
				_, err := owner.DeletePaymentMethod(t.Context(), openrails.CustomerID(user), openrails.PaymentMethodID(method))
				require.Error(t, err)
				require.Equal(t, before, deleteCalls.Load())
				return
			}
			if policy == "engine" {
				// Merchant revocation wins even over an already accepted customer resume.
				require.NoError(t, owner.CancelSubscription(t.Context(), sid, openrails.CancelSubscriptionRequest{Reason: "revoked by merchant", RevokeAccess: true}))
				runCancel(id)
				runResume(id)
				status, raw = requestJSON(t, http.MethodPost, path+"/resume", token, nil)
				require.Equal(t, 400, status, string(raw))
				var apiErr *openrails.StatusError
				require.ErrorAs(t, owner.ResumeSubscription(t.Context(), sid), &apiErr)
				require.Equal(t, 400, apiErr.Status)
				require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM billing.subscriptions WHERE id=$1`, id).Scan(&state))
				require.Equal(t, "cancelled", state)
				access, err = owner.HasEntitlement(t.Context(), openrails.CustomerID(user), "engine_access", now)
				require.NoError(t, err)
				require.False(t, access)
			}
		})
	}
	require.Zero(t, sends.Load())
	require.EqualValues(t, 2, deleteCalls.Load())
	require.EqualValues(t, 2, receiptReads.Load())
}

// A native operational reader remains supplied so filtering, not missing
// configuration, is what prevents an engine-only unknown cohort from probing.
type engineMaintenanceFetcher struct{ calls atomic.Int64 }

func (*engineMaintenanceFetcher) Name() string { return "nmi" }
func (*engineMaintenanceFetcher) Capabilities() reconcile.Capabilities {
	return reconcile.Capabilities{Subscriptions: true}
}
func (f *engineMaintenanceFetcher) Fetch(context.Context, reconcile.FetchParams) (*reconcile.RemoteSnapshot, error) {
	f.calls.Add(1)
	return &reconcile.RemoteSnapshot{Provider: reconcile.ProviderNMI}, nil
}
