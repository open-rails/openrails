//go:build integration

package money_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/custodymigration"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchantarchive/contract"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	riverjobs "github.com/open-rails/openrails/internal/river"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// Internal engine fixtures exercise the production fleet/handler/renewal writer
// on the same loopback HyperSwitch and NMI receipt boundaries as invoice tests.
// Public initial enrollment and runtime registration are deliberately absent.
func TestEngineRecurringCollectionExecution(t *testing.T) {
	for _, mode := range []string{"on_time", "missed_periods", "lost_reply", "late_receipt", "declined", "preflight_not_dispatched", "cancel_after_submit", "chargeback_after_submit", "cancel_uncertain", "archived_before_send", "archived_account_recovery", "atomic_completion", "fence_without_dispatch", "native_control"} {
		t.Run(mode, func(t *testing.T) {
			e := newNMIReceiptEnv(t)
			now := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
			clock := clockwork.NewFakeClockAt(now)
			prior := now
			if mode == "missed_periods" || mode == "declined" {
				prior = now.Add(-100 * 24 * time.Hour)
			}
			sub, product, price, custodian := uuid.New(), uuid.New(), uuid.New(), uuid.New()
			*e.custodian = custodian
			account := "engine_" + custodian.String()
			mid := dbtest.TestMerchantID.UUID()
			_, err := e.pool.Exec(e.ctx, `INSERT INTO billing.custodians(id,merchant_id,key,kind,environment,account_id,settings,credential_versions) VALUES($1,$2,$3,'hyperswitch','test',$4,'{"profile_id":"engine_profile","public_api_key":"synthetic"}','{"api_key":1}')`, custodian, mid, custodian.String(), account)
			require.NoError(t, err)
			secret, err := merchants.CustodianSecretName("hyperswitch", "test", account, "api_key")
			require.NoError(t, err)
			_, err = e.merchants.Secrets().Put(e.ctx, dbtest.TestMerchantID, secret, "engine-key")
			require.NoError(t, err)
			t.Cleanup(func() { _ = e.merchants.Secrets().Delete(context.WithoutCancel(e.ctx), dbtest.TestMerchantID, secret) })
			_, err = e.pool.Exec(e.ctx, `UPDATE billing.payment_methods SET custodian='hyperswitch',custodian_id=$2,rail_customer_ref='engine_customer',rail_method_ref='engine_method',stored_credential_recurring_ref='original_recurring',stored_credential_unscheduled_ref='' WHERE id=$1`, e.method, custodian)
			require.NoError(t, err)
			method := e.methodRow(t)
			_, err = e.pool.Exec(e.ctx, `INSERT INTO billing.products(id,merchant_id,key,display_name,entitlements_spec) VALUES($1,$2,$3,'Engine recurring','{"engine":null}')`, product, mid, product.String())
			require.NoError(t, err)
			_, err = e.pool.Exec(e.ctx, `INSERT INTO billing.prices(id,merchant_id,product_id,amount,currency,auto_renew,access_duration_hours) VALUES($1,$2,$3,9990000,'USD',true,720)`, price, mid, product)
			require.NoError(t, err)
			policy, remote := "engine", ""
			if mode == "native_control" {
				policy, remote = "provider", "native-existing"
			}
			_, err = e.pool.Exec(e.ctx, `INSERT INTO billing.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,payment_method_id,rail,collection_policy,rail_subscription_id,status,current_period_starts_at,current_period_ends_at,entitlements_spec_snapshot) VALUES($1,$2,$3,$4,$5,$6,$7,'nmi',$8,$9,'active',$10,$11,'{"engine":null}')`, sub, mid, e.payer.UUID(), product, price, method.PspID, e.method, policy, remote, prior.Add(-30*24*time.Hour), prior)
			require.NoError(t, err)
			t.Cleanup(func() {
				c := context.WithoutCancel(e.ctx)
				_, _ = e.pool.Exec(c, `UPDATE billing.subscriptions SET deleted_at=now() WHERE id=$1`, sub)
				for _, query := range []string{`DELETE FROM billing.host_outbox WHERE payment_id IN (SELECT id FROM billing.payments WHERE subscription_id=$1)`, `DELETE FROM billing.payments WHERE subscription_id=$1`, `DELETE FROM billing.rail_intents WHERE subscription_id=$1`, `DELETE FROM billing.entitlements WHERE source_id=$1`, `DELETE FROM billing.subscriptions WHERE id=$1`} {
					_, _ = e.pool.Exec(c, query, sub)
				}
				_, _ = e.pool.Exec(c, `DELETE FROM billing.prices WHERE id=$1`, price)
				_, _ = e.pool.Exec(c, `DELETE FROM billing.products WHERE id=$1`, product)
			})
			lc := subscriptions.NewSubscriptionLifecycleService(e.db, nil, nil, nil, nil, nil, clock)
			var mu sync.Mutex
			var forms []map[string]string
			repaired := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "api-key=engine-key", r.Header.Get("Authorization"))
				require.Equal(t, "engine_profile", r.Header.Get("x-profile-id"))
				w.Header().Set("Content-Type", "application/json")
				mu.Lock()
				fixed := repaired
				mu.Unlock()
				if r.URL.Path == "/v2/payment-methods/engine_method" {
					if mode == "preflight_not_dispatched" && !fixed {
						http.Error(w, "unavailable", 503)
						return
					}
					_, _ = fmt.Fprintf(w, `{"id":"engine_method","merchant_id":%q,"customer_id":"engine_customer","storage_type":"persistent","payment_method_data":{"card":{"last4_digits":"1111","expiry_month":"12","expiry_year":"2030"}}}`, account)
					return
				}
				require.Equal(t, "/v2/proxy", r.URL.Path)
				if r.Method == http.MethodGet {
					_, _ = fmt.Fprintf(w, `{"contract":"openrails-nmi-form-v2","strict":true,"max_response_bytes":65536,"routes":[{"destination_url":%q,"method":"POST","response_profile":"nmi_classic"}]}`, e.plane.Endpoints.NMIDirectPostURL)
					return
				}
				var body struct {
					Token string            `json:"token"`
					Form  map[string]string `json:"request_body"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
				require.Equal(t, "engine_method", body.Token)
				require.Equal(t, "merchant", body.Form["initiated_by"])
				require.Equal(t, "recurring", body.Form["billing_method"])
				require.Equal(t, "used", body.Form["stored_credential_indicator"])
				require.Equal(t, "original_recurring", body.Form["initial_transaction_id"])
				require.Equal(t, "9.99", body.Form["amount"])
				for _, key := range []string{"recurring", "subscription_id", "plan_id", "customer_vault_id", "billing_id"} {
					require.Empty(t, body.Form[key])
				}
				mu.Lock()
				forms = append(forms, body.Form)
				n := len(forms)
				mu.Unlock()
				if mode == "declined" && n == 1 {
					fmt.Fprint(w, `{"response":{"response":"2","response_code":"200","responsetext":"Declined"},"status_code":200,"response_headers":{}}`)
					return
				}
				txn := "engine_sale_" + sub.String()
				e.gateway.orderSale(body.Form["orderid"], txn)
				if mode != "late_receipt" {
					e.gateway.payment(txn, "", body.Form["amount"], "USD")
				}
				if mode == "cancel_after_submit" || mode == "chargeback_after_submit" {
					cancelType := models.CancelTypeMerchant
					if mode == "chargeback_after_submit" {
						cancelType = models.CancelTypeChargeback
					}
					require.NoError(t, lc.CancelMembership(e.ctx, &subscriptions.CancelMembershipParams{SubscriptionID: &sub, CancelType: cancelType, RevokeAccess: true}))
				}
				if mode == "archived_account_recovery" {
					_, err := e.pool.Exec(e.ctx, `UPDATE billing.psps SET archived=true WHERE id=$1`, method.PspID)
					require.NoError(t, err)
					_, err = e.pool.Exec(e.ctx, `UPDATE billing.custodians SET archived=true WHERE id=$1`, custodian)
					require.NoError(t, err)
				}
				if mode == "lost_reply" || mode == "late_receipt" || mode == "cancel_uncertain" || mode == "archived_account_recovery" {
					conn, _, err := w.(http.Hijacker).Hijack()
					require.NoError(t, err)
					_ = conn.Close()
					return
				}
				_, _ = fmt.Fprintf(w, `{"response":{"response":"1","response_code":"100","responsetext":"Approved","transactionid":%q},"status_code":200,"response_headers":{}}`, txn)
			}))
			t.Cleanup(server.Close)
			e.plane.Config.HyperSwitch = &config.HyperSwitchConfig{APIBaseURL: server.URL}
			require.NoError(t, e.svc.SetHyperSwitchDeployment(server.URL))
			handler := money.NewSubscriptionCollectionHandler(e.db, e.plane, e.plane.Config, clock)
			runner := func() *intents.Runner {
				return &intents.Runner{Store: intents.NewStore(e.db), Registry: intents.NewRegistry(money.NewSubscriptionCollectionHandler(e.db, e.plane, e.plane.Config, clock)), Config: e.plane.Config, Clock: clock}
			}
			worker := riverjobs.DunningWorker{DB: e.db, Config: e.plane.Config, Clock: clock, NMIResolver: e.plane, EngineCollections: e.svc}
			var wg sync.WaitGroup
			errs := make([]error, 2)
			for i := range 2 {
				wg.Go(func() { errs[i] = worker.Work(context.Background(), &river.Job[riverjobs.DunningArgs]{}) })
			}
			wg.Wait()
			for _, err := range errs {
				require.NoError(t, err)
			}
			var count int
			require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT count(*) FROM billing.rail_intents WHERE subscription_id=$1 AND intent_type='subscription_collection'`, sub).Scan(&count))
			if mode == "native_control" {
				require.Zero(t, count)
				mu.Lock()
				require.Empty(t, forms)
				mu.Unlock()
				return
			}
			require.Equal(t, 1, count)
			require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT id FROM billing.rail_intents WHERE subscription_id=$1 AND intent_type='subscription_collection'`, sub).Scan(&e.op))
			get := func() gen.OpenrailsRailIntent {
				row, err := intents.NewStore(e.db).Get(e.ctx, e.op)
				require.NoError(t, err)
				return row
			}
			accepted := get()
			terms, err := subscriptions.DecodeSubscriptionCollectionPayload(accepted)
			require.NoError(t, err)
			expectedStart := prior
			if !prior.Add(30 * 24 * time.Hour).After(now) {
				expectedStart = now
			}
			require.True(t, terms.Renewal.PeriodStart.Equal(expectedStart))
			if mode == "archived_before_send" {
				_, err = e.pool.Exec(e.ctx, `UPDATE billing.psps SET archived=true WHERE id=$1`, method.PspID)
				require.NoError(t, err)
				_, err = e.pool.Exec(e.ctx, `UPDATE billing.custodians SET archived=true WHERE id=$1`, custodian)
				require.NoError(t, err)
			}
			if mode == "on_time" {
				store := intents.NewStore(e.db)
				_, err = e.pool.Exec(e.ctx, `UPDATE billing.rail_intents SET expires_at=$2 WHERE id=$1`, e.op, clock.Now().Add(-time.Hour))
				require.NoError(t, err)
				require.NoError(t, store.MarkSuperseded(e.ctx, e.op, "stale sweep"))
				changed, err := store.SupersedeBySubject(e.ctx, subscriptions.TypeSubscriptionCollection, sub, "stale resume")
				require.NoError(t, err)
				require.Zero(t, changed)
				_, err = store.ExpireOverdue(e.ctx, clock.Now())
				require.NoError(t, err)
				require.Equal(t, intents.StatusPending, get().Status)
				modified := terms
				modified.Renewal.Amount++
				replay, err := store.Enqueue(e.ctx, intents.EnqueueParams{MerchantID: mid, Provider: "nmi", IntentType: subscriptions.TypeSubscriptionCollection, SubscriptionID: &sub, PspID: method.PspID, CustodianID: custodian, PriceID: &price, Payload: modified, IdempotencyKey: accepted.IdempotencyKey, NextAttemptAt: clock.Now(), Origin: intents.OriginSystem})
				require.NoError(t, err)
				require.JSONEq(t, string(accepted.Payload), string(replay.Payload))
				_, err = store.Enqueue(e.ctx, intents.EnqueueParams{MerchantID: mid, Provider: "nmi", IntentType: subscriptions.TypeSubscriptionCollection, SubscriptionID: &sub, PspID: method.PspID, CustodianID: custodian, PriceID: &price, Payload: terms, IdempotencyKey: accepted.IdempotencyKey + ":other", NextAttemptAt: clock.Now(), Origin: intents.OriginSystem})
				require.Error(t, err)
			}
			trigger := ""
			if mode == "atomic_completion" {
				trigger = "engine_terminal_" + uuid.NewString()[:8]
				admin := dbtest.SharedSuperuserPGXPool(t)
				_, err = admin.Exec(e.ctx, fmt.Sprintf(`CREATE FUNCTION billing.%s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id='%s'::uuid AND NEW.status='succeeded' THEN RAISE EXCEPTION 'injected terminal failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER %s BEFORE UPDATE ON billing.rail_intents FOR EACH ROW EXECUTE FUNCTION billing.%s()`, trigger, e.op, trigger, trigger))
				require.NoError(t, err)
				t.Cleanup(func() {
					_, _ = admin.Exec(context.WithoutCancel(e.ctx), "DROP FUNCTION IF EXISTS billing."+trigger+"() CASCADE")
				})
			}
			if mode == "fence_without_dispatch" {
				store := intents.NewStore(e.db)
				claimed, ok, err := store.ClaimByID(e.ctx, e.op, clock.Now(), clock.Now().Add(intents.DefaultLease))
				require.NoError(t, err)
				require.True(t, ok)
				_, fresh, err := store.BeginCollectedPayment(e.ctx, claimed, clock.Now())
				require.NoError(t, err)
				require.True(t, fresh)
				require.NoError(t, store.Park(e.ctx, e.op, clock.Now(), "stale no-send result"))
				require.Equal(t, intents.StatusInFlight, get().Status)
				require.Error(t, store.MarkFailedRetryable(e.ctx, e.op, clock.Now(), "stale retry"))
				clock.Advance(intents.DefaultLease + time.Second)
			}
			for i := range 2 {
				wg.Go(func() { _, errs[i] = runner().RunExecuteOnce(e.ctx) })
			}
			wg.Wait()
			for _, err := range errs {
				require.NoError(t, err)
			}
			current := get()
			if mode == "fence_without_dispatch" {
				require.Equal(t, intents.StatusUnknownNeedsVerify, current.Status)
				clock.Advance(90 * 24 * time.Hour)
				_, err = runner().RunVerifyOnce(e.ctx)
				require.NoError(t, err)
				require.Equal(t, intents.StatusUnknownNeedsVerify, get().Status)
				require.JSONEq(t, string(accepted.Payload), string(get().Payload))
				replay, err := e.svc.AdmitDueSubscriptionCollection(e.ctx, sub, clock.Now())
				require.NoError(t, err)
				require.Equal(t, e.op, replay.ID)
				mu.Lock()
				require.Empty(t, forms)
				mu.Unlock()
				return
			}

			if mode == "declined" || mode == "preflight_not_dispatched" {
				require.Equal(t, intents.StatusFailedTerminal, current.Status)
				require.NoError(t, intents.ValidateSubscriptionCollectionTerminal(current))
				requireEngineArchiveValues(t, e, current.ID)
				var next *time.Time
				require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT next_retry_at FROM billing.subscriptions WHERE id=$1`, sub).Scan(&next))
				if next != nil {
					clock.Advance(next.Sub(clock.Now()) + time.Second)
				} else {
					clock.Advance(time.Hour)
				}
				mu.Lock()
				repaired = true
				mu.Unlock()
				require.NoError(t, worker.Work(context.Background(), &river.Job[riverjobs.DunningArgs]{}))
				nextOp, err := dbtest.Queries(e.pool).GetUnresolvedSubscriptionCollection(e.ctx, gen.GetUnresolvedSubscriptionCollectionParams{MerchantID: mid, SubscriptionID: sub})
				require.NoError(t, err)
				require.NotEqual(t, e.op, nextOp.ID)
				e.op = nextOp.ID
				terms, err = subscriptions.DecodeSubscriptionCollectionPayload(nextOp)
				require.NoError(t, err)
				require.Equal(t, 1, terms.Attempt)
				if mode == "declined" {
					require.True(t, terms.Renewal.PeriodStart.Equal(clock.Now()))
					require.True(t, terms.PreviousPeriodEnd.Equal(prior))
				}
				_, err = runner().RunExecuteOnce(e.ctx)
				require.NoError(t, err)
				current = get()
			}
			lateGrants := -1
			if current.Status == intents.StatusUnknownNeedsVerify {
				if mode == "cancel_uncertain" {
					require.NoError(t, lc.CancelMembership(e.ctx, &subscriptions.CancelMembershipParams{SubscriptionID: &sub, CancelType: models.CancelTypeUser}))
				}
				if mode == "cancel_uncertain" {
					store := intents.NewStore(e.db)
					require.Error(t, store.MarkSucceeded(e.ctx, e.op, clock.Now(), nil))
					require.Error(t, store.MarkFailedTerminal(e.ctx, e.op, "stale refusal", nil))
					require.NoError(t, store.MarkSuperseded(e.ctx, e.op, "stale cancel"))
					require.Error(t, store.MarkFailedRetryable(e.ctx, e.op, clock.Now(), "stale retry"))
					require.Error(t, store.RetainCollectionNonexecution(e.ctx, current, intents.CollectionNonexecutionProof{}, "not_dispatched", "forged missing response"))
					pm, err := paymentmethods.NewPaymentMethodRepo(e.db).GetByID(e.ctx, e.method)
					require.NoError(t, err)
					deleteRails := &paymentmethods.RailPaymentMethodService{DB: e.db, Config: e.plane.Config, MerchantSecrets: e.merchants.Secrets()}
					deleter := &intents.PaymentMethodDeleteThrough{Runner: &intents.Runner{Store: store, Registry: intents.NewRegistry(intents.NewHyperSwitchMethodDeleteHandler(e.db, deleteRails, clock)), Config: e.plane.Config, Clock: clock}}
					_, err = deleter.ExecutePaymentMethodDelete(e.ctx, pm)
					require.Error(t, err, "cancelled uncertain charge must still pin its saved card")
					target := uuid.New()
					key := "engine-remap-" + target.String()
					_, err = e.pool.Exec(e.ctx, `INSERT INTO billing.custodians(id,merchant_id,key,kind,environment,account_id) VALUES($1,$2,$3,'basis_theory','test',$3)`, target, mid, key)
					require.NoError(t, err)
					t.Cleanup(func() {
						_, _ = e.pool.Exec(context.WithoutCancel(e.ctx), `DELETE FROM billing.custodians WHERE id=$1`, target)
					})
					migration, err := custodymigration.Migrate(e.ctx, custodymigration.Options{PGXPool: e.pool, Config: e.plane.Config, MerchantID: dbtest.TestMerchantID, Apply: true, Export: custodymigration.VaultExport{ExportedAt: time.Now().UTC(), SourceRail: "nmi", SourcePSPID: method.PspID, Custodian: key, Tokens: []custodymigration.ImportedToken{{SourceRailCustomerRef: "engine_customer", SourceRailMethodRef: "engine_method", Token: "replacement_engine_token"}}}})
					require.NoError(t, err)
					require.Equal(t, 1, migration.Counts[custodymigration.OutcomeBlocked])
					require.Equal(t, e.methodRow(t).RailMethodRef, "engine_method")
					require.JSONEq(t, string(current.Payload), string(get().Payload))
					require.JSONEq(t, string(current.ResultEvidence), string(get().ResultEvidence))
				}
				if mode == "atomic_completion" {
					require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT count(*) FROM billing.payments WHERE subscription_id=$1`, sub).Scan(&count))
					require.Zero(t, count)
					_, err = dbtest.SharedSuperuserPGXPool(t).Exec(e.ctx, "DROP FUNCTION billing."+trigger+"() CASCADE")
					require.NoError(t, err)
					e.gateway.mu.Lock()
					e.gateway.payments = map[string]map[string]any{}
					e.gateway.saleForOrder = map[string]string{}
					e.gateway.mu.Unlock()
				}
				if mode == "late_receipt" {
					require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT count(*) FROM billing.grants WHERE customer_id=$1`, e.payer.UUID()).Scan(&lateGrants))
					clock.Advance(90 * 24 * time.Hour)
					_, err = runner().RunVerifyOnce(e.ctx)
					require.NoError(t, err)
					require.Equal(t, intents.StatusUnknownNeedsVerify, get().Status)
					require.JSONEq(t, string(accepted.Payload), string(get().Payload))
					e.gateway.payment("engine_sale_"+sub.String(), "", "9.99", "USD")
				} else {
					clock.Advance(2 * time.Minute)
				}
				if next := get().NextAttemptAt; !next.Before(clock.Now()) {
					clock.Advance(next.Sub(clock.Now()) + time.Second)
				}
				_, err = runner().RunVerifyOnce(e.ctx)
				require.NoError(t, err)
				current = get()
			}
			require.Equal(t, intents.StatusSucceeded, current.Status, string(current.ResultEvidence))
			require.NoError(t, intents.ValidateSubscriptionCollectionTerminal(current))
			requireEngineArchiveValues(t, e, current.ID)
			var grantsBefore, grantsAfter int
			require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT count(*) FROM billing.grants WHERE customer_id=$1`, e.payer.UUID()).Scan(&grantsBefore))
			if lateGrants >= 0 {
				require.Equal(t, lateGrants, grantsBefore, "late receipt cannot mint a new paid period or grant")
			}
			preserved := current
			require.NoError(t, intents.NewStore(e.db).PruneSucceeded(e.ctx, e.op, nil, false, false))
			require.JSONEq(t, string(preserved.Payload), string(get().Payload))
			require.JSONEq(t, string(preserved.ResultEvidence), string(get().ResultEvidence))
			require.Equal(t, intents.OutcomeSucceeded, handler.Execute(e.ctx, current).Class, "terminal replay uses retained receipt without another provider call")
			require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT count(*) FROM billing.grants WHERE customer_id=$1`, e.payer.UUID()).Scan(&grantsAfter))
			require.Equal(t, grantsBefore, grantsAfter)
			require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT count(*) FROM billing.payments WHERE subscription_id=$1 AND status='completed'`, sub).Scan(&count))
			require.Equal(t, 1, count)
			require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT count(*) FROM billing.host_outbox WHERE payment_id IN (SELECT id FROM billing.payments WHERE subscription_id=$1 AND status='completed')`, sub).Scan(&count))
			require.Equal(t, 1, count)
			var state, token string
			var start, end time.Time
			require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT status,current_period_starts_at,current_period_ends_at FROM billing.subscriptions WHERE id=$1`, sub).Scan(&state, &start, &end))
			require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT token_type FROM billing.payments WHERE subscription_id=$1 AND status='completed'`, sub).Scan(&token))
			require.Equal(t, "pan_via_proxy", token)
			terminal := strings.HasPrefix(mode, "cancel_") || mode == "chargeback_after_submit"
			if terminal {
				require.Equal(t, "cancelled", state)
				var review string
				require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT metadata->>'refund_review' FROM billing.payments WHERE subscription_id=$1 AND status='completed'`, sub).Scan(&review))
				require.NotEmpty(t, review)
			} else {
				require.Equal(t, "active", state)
				require.True(t, start.Equal(terms.Renewal.PeriodStart))
				require.True(t, end.Equal(terms.Renewal.PeriodEnd))
			}
			entitled, err := entitlements.NewEntitlementService(e.db, clock).IsEntitled(e.ctx, e.payer.String(), "engine", clock.Now())
			require.NoError(t, err)
			require.Equal(t, !terminal, entitled)
			mu.Lock()
			want := 1
			if mode == "declined" {
				want = 2
			}
			require.Len(t, forms, want)
			mu.Unlock()
			e.gateway.mu.Lock()
			require.Zero(t, e.gateway.sends, "no native vault sale or recurring schedule request")
			e.gateway.mu.Unlock()
		})
	}
}

func requireEngineArchiveValues(t *testing.T, e nmiReceiptEnv, id uuid.UUID) {
	t.Helper()
	for _, profile := range contract.Profiles {
		if profile.Name == "rail_intents" {
			fields := make([]string, len(profile.Columns))
			for i, c := range profile.Columns {
				fields[i] = `"` + c.Name + `"::text`
			}
			var values []*string
			require.NoError(t, e.pool.QueryRow(e.ctx, "SELECT ARRAY["+strings.Join(fields, ",")+"] FROM billing.rail_intents WHERE id=$1", id).Scan(&values))
			require.NoError(t, contract.ValidateValues(profile, values))
			return
		}
	}
	t.Fatal("rail intent archive profile missing")
}
