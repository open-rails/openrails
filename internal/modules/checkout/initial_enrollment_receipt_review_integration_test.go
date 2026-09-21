//go:build integration

package checkout

import (
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/webhooks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sync/atomic"
)

// A native paid-now enrollment needs a qualified initial charge. An approved
// schedule identifier alone cannot buy standing access or complete the intent.
func initialEnrollmentMissingChargeCannotActivatePaidAccess(t *testing.T) {
	fx := newSubIntentFixture(t)
	require.Positive(t, fx.payload.Terms.Amount)
	require.Empty(t, fx.payload.NativeSchedule.StartDate)
	require.Nil(t, fx.payload.DelayedStart())
	_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.products SET entitlements_spec='{"initial_paid_access":null}' WHERE id=(SELECT product_id FROM billing.prices WHERE id=$1)`, fx.priceID)
	require.NoError(t, err)
	fx.gateway.txnID = ""
	result := fx.enqueueAndExecute(t)
	var active, payments, access int
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.subscriptions WHERE id=$1 AND status='active'`, fx.payload.Terms.SubscriptionID).Scan(&active))
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.payments WHERE subscription_id=$1`, fx.payload.Terms.SubscriptionID).Scan(&payments))
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.entitlements WHERE source_type='subscription' AND source_id=$1 AND entitlement='initial_paid_access' AND revoked_at IS NULL AND deleted_at IS NULL`, fx.payload.Terms.SubscriptionID).Scan(&access))
	t.Logf("one approved enrollment without a charge identifier: intent=%s active_subscriptions=%d payments=%d access_windows=%d", result.Status, active, payments, access)
	assert.Equal(t, intents.StatusUnknownNeedsVerify, result.Status)
	assert.Zero(t, active, "a paid-now subscription cannot activate from schedule-only evidence")
	assert.Zero(t, access, "no paid entitlement may be granted without its exact initial payment")
	require.Zero(t, payments, "missing provider money must not be fabricated")
	require.EqualValues(t, 1, fx.gateway.createCalls.Load())
}

// The optional outer local runner installs its fail-closed proxy before process
// startup. Every fixture also validates its own client URLs as loopback-only.
func TestInitialEnrollmentAcceptedModes(t *testing.T) {
	if guard := os.Getenv("OPENRAILS_LOCAL_HTTP_GUARD"); guard != "" {
		proxy, err := http.ProxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: "https", Host: "secure.nmi.com"}})
		require.NoError(t, err)
		require.NotNil(t, proxy)
		require.Equal(t, guard, proxy.String())
	}
	t.Run("paid_now_exact_receipts", func(t *testing.T) {
		fx := newSubIntentFixture(t)
		result := fx.enqueueAndExecute(t)
		require.Equal(t, intents.StatusSucceeded, result.Status, string(result.ResultEvidence))
		var payments, active int
		require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.payments WHERE id=$1 AND amount=$2 AND currency=$3 AND status='completed'`, fx.payload.Terms.PaymentID, fx.payload.Terms.Amount, fx.payload.Terms.Currency).Scan(&payments))
		require.Equal(t, 1, payments)
		require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.subscriptions WHERE id=$1 AND status='active'`, fx.payload.Terms.SubscriptionID).Scan(&active))
		require.Equal(t, 1, active)
		replay := fx.enqueueAndExecute(t)
		require.Equal(t, intents.StatusSucceeded, replay.Status)
		require.EqualValues(t, 1, fx.gateway.createCalls.Load())
		another := fx.payload
		another.CheckoutIdempotencyKey = "second-session-" + uuid.NewString()
		another.Terms.SubscriptionID, another.Terms.PaymentID = uuid.New(), uuid.New()
		_, err := fx.runner.Store.Enqueue(fx.ctx, intents.EnqueueParams{MerchantID: result.MerchantID, Provider: result.Rail, PspID: another.Terms.PSPID, IntentType: TypeInitialMembership, PriceID: &another.Terms.PriceID, Payload: another, IdempotencyKey: InitialMembershipIdempotencyKey(another.CheckoutIdempotencyKey), NextAttemptAt: another.Terms.AcceptedAt, Origin: intents.OriginUser, OriginReason: "second session"})
		require.ErrorContains(t, err, "already has a subscription")
		require.EqualValues(t, 1, fx.gateway.createCalls.Load())
	})
	t.Run("paid_now_requires_charge", initialEnrollmentMissingChargeCannotActivatePaidAccess)
	t.Run("provider_decline_has_bound_custody", func(t *testing.T) {
		fx := newSubIntentFixture(t)
		fx.gateway.createMode.Store("decline")
		in := fx.enqueueAndExecute(t)
		require.Equal(t, intents.StatusFailedTerminal, in.Status, string(in.ResultEvidence))
		_, found, err := intents.LoadInitialMembershipRefusal(in)
		require.NoError(t, err)
		require.True(t, found)
		require.NoError(t, intents.ValidateInitialMembershipTerminal(in))
		for _, mutation := range []map[string]any{{"declined": false, "request_refused": true}, {"not_executed": true}, {"response_code": 299}, {"localization_id": "another-refusal"}, {"transaction_id": "another-payment"}} {
			var altered map[string]any
			require.NoError(t, json.Unmarshal(in.ResultEvidence, &altered))
			for key, value := range mutation {
				altered[key] = value
			}
			forged := in
			forged.ResultEvidence, err = json.Marshal(altered)
			require.NoError(t, err)
			require.Error(t, intents.ValidateInitialMembershipTerminal(forged), "terminal flags must match sealed refusal")
		}

		var attempts int
		require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.payments WHERE id=$1 AND status='failed' AND money_movement='none'`, fx.payload.Terms.PaymentID).Scan(&attempts))
		require.Equal(t, 1, attempts)
		replay := fx.enqueueAndExecute(t)
		require.Equal(t, intents.StatusFailedTerminal, replay.Status)
		require.EqualValues(t, 1, fx.gateway.createCalls.Load())
	})
	for _, mode := range []string{"covered_delay", "free_recurring"} {
		t.Run(mode, func(t *testing.T) {
			fx := newSubIntentFixture(t)
			if mode == "covered_delay" {
				start := time.Now().UTC().AddDate(0, 0, 3).Truncate(24 * time.Hour)
				fx.payload.NativeSchedule.StartDate = start.Format("20060102")
				fx.payload.Terms.Pending = true
				fx.payload.Terms.PeriodStart = start
			} else {
				fx.payload.Terms.Amount = 0
				_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.prices SET amount=0 WHERE id=$1`, fx.priceID)
				require.NoError(t, err)
			}
			fx.gateway.txnID = ""
			result := fx.enqueueAndExecute(t)
			require.Equal(t, intents.StatusSucceeded, result.Status, string(result.ResultEvidence))
			response, err := initialMembershipResponseFromIntent(result)
			require.NoError(t, err)
			if mode == "covered_delay" {
				require.Equal(t, "pending", response.Status)
			} else {
				require.Equal(t, "success", response.Status)
			}
			form, ok := fx.gateway.createForm.Load().(url.Values)
			require.True(t, ok)
			assert.Equal(t, "add_subscription", form.Get("recurring"))
			assert.NotContains(t, form, "type", "a no-charge enrollment must not dispatch a sale")
			assert.NotContains(t, form, "amount", "recurring schedule amount is not an initial charge")
			assert.NotContains(t, form, "stored_credential_indicator", "no transaction established a recurring credential anchor")
			assert.Equal(t, fx.payload.NativeSchedule.StartDate, form.Get("start_date"))
			var payments int
			require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.payments WHERE subscription_id=$1`, fx.payload.Terms.SubscriptionID).Scan(&payments))
			require.Zero(t, payments)
		})
	}
}

func TestInitialEnrollmentRefusesRawProgressAuthority(t *testing.T) {
	t.Run("decline_cannot_rebind_another_envelope", func(t *testing.T) {
		fx := newSubIntentFixture(t)
		fx.gateway.txnID = ""
		fx.gateway.createMode.Store("ambiguous500")
		in := fx.enqueueAndExecute(t)
		var payload map[string]any
		require.NoError(t, json.Unmarshal(in.Payload, &payload))
		payload["email"] = "different@example.invalid"
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		in.Payload = raw
		err = intents.NewStore(fx.db).RetainInitialMembershipDecline(fx.ctx, in, &nmi.CustomerVaultError{ResponseCode: 200, RawResponse: "response=2&response_code=200"})
		require.Error(t, err, "provider refusal cannot be rebound to different accepted terms")
	})

	for _, flag := range []string{"declined", "request_refused"} {
		t.Run(flag, func(t *testing.T) {
			fx := newSubIntentFixture(t)
			fx.gateway.txnID = ""
			fx.gateway.createMode.Store("ambiguous500")
			in := fx.enqueueAndExecute(t)
			require.Equal(t, intents.StatusUnknownNeedsVerify, in.Status)
			require.NoError(t, intents.NewStore(fx.db).RecordProgress(fx.ctx, in.ID, map[string]any{flag: true}))
			outcome := NewInitialMembershipIntentHandler(fx.svc).Verify(fx.ctx, in)
			require.NotEqual(t, intents.OutcomeTerminal, outcome.Class, "raw progress cannot prove provider refusal")
			current, err := fx.runner.Store.Get(fx.ctx, in.ID)
			require.NoError(t, err)
			require.NotEqual(t, intents.StatusFailedTerminal, current.Status)
		})
	}
}

func TestInitialEnrollmentRejectsInflatedAcceptedPeriod(t *testing.T) {
	fx := newSubIntentFixture(t)
	in := fx.enqueueAndExecute(t)
	var p InitialMembershipPayload
	require.NoError(t, json.Unmarshal(in.Payload, &p))
	p.Terms.PeriodEnd = p.Terms.PeriodEnd.Add(270 * 24 * time.Hour)
	p.NativeSchedule.StartDate = p.Terms.PeriodEnd.Format("20060102")
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	in.Payload = raw
	_, err = decodeInitialMembershipPayload(in)
	require.Error(t, err, "a 30-day provider plan cannot establish 300 days of accepted access")
}

func TestSubmittedInitialEnrollmentStaysInUnresolvedCensus(t *testing.T) {
	for _, action := range []string{"retry", "expire", "supersede", "progress", "unknown"} {
		t.Run(action, func(t *testing.T) {
			fx := newSubIntentFixture(t)
			fx.gateway.txnID = ""
			fx.gateway.createMode.Store("ambiguous500")
			in := fx.enqueueAndExecute(t)
			require.Equal(t, intents.StatusUnknownNeedsVerify, in.Status)
			switch action {
			case "progress":
				require.Error(t, intents.NewStore(fx.db).RecordProgress(fx.ctx, in.ID, map[string]any{"initial_submitted": false}))
			case "unknown":
				require.NoError(t, fx.runner.Store.MarkUnknown(fx.ctx, in.ID, time.Now(), "stale diagnostics", map[string]any{"initial_submitted": false}))
			case "retry":
				require.Error(t, fx.runner.Store.MarkFailedRetryable(fx.ctx, in.ID, time.Now(), "stale executor"))
			case "supersede":
				require.NoError(t, fx.runner.Store.MarkSuperseded(fx.ctx, in.ID, "stale relevance"))
			case "expire":
				_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.rail_intents SET status='failed_retryable',expires_at=$2 WHERE id=$1`, in.ID, time.Now().Add(-time.Hour))
				require.NoError(t, err)
				_, err = fx.runner.Store.ExpireOverdue(fx.ctx, time.Now())
				require.NoError(t, err)
			}
			current, err := fx.runner.Store.Get(fx.ctx, in.ID)
			require.NoError(t, err)
			var progress map[string]any
			require.NoError(t, json.Unmarshal(current.ResultEvidence, &progress))
			require.Equal(t, true, progress["initial_submitted"])
			if action == "expire" {
				require.Equal(t, intents.StatusFailedRetryable, current.Status)
			} else {
				require.Equal(t, intents.StatusUnknownNeedsVerify, current.Status)
			}
		})
	}
}

func TestNMIObservationBeforeInitialCompletionIsHarmless(t *testing.T) {
	fx := newSubIntentFixture(t)
	ctx := db.WithPSPID(fx.ctx, dbtest.TestPSPID(dbtest.TestMerchantID.UUID(), "mobius"))
	client, err := fx.svc.resolveNMIClient(ctx, "mobius")
	require.NoError(t, err)
	observer := webhooks.NMIConvergeService{DB: fx.db, Clock: fx.svc.Clock(), Rail: "nmi", NMIClient: client, SubscriptionService: fx.svc.SubscriptionService, PaymentService: fx.svc.PurchaseService.PaymentService, PriceService: fx.svc.PriceService, SubscriptionLifecycleService: fx.svc.Lifecycle}
	var observed atomic.Bool
	fx.gateway.beforeResponse = func() error {
		customer, err := observer.Converge(ctx, fx.gateway.subID)
		if err != nil {
			return err
		}
		if customer != uuid.Nil {
			return fmt.Errorf("early provider observation unexpectedly established a membership")
		}
		observed.Store(true)
		return nil
	}
	in := fx.enqueueAndExecute(t)
	require.Equal(t, intents.StatusSucceeded, in.Status, string(in.ResultEvidence))
	require.True(t, observed.Load())
	require.EqualValues(t, 1, fx.gateway.createCalls.Load())
}

func TestDeferredNativeFirstPaymentUsesAcceptedTerms(t *testing.T) {
	fx := newSubIntentFixture(t)
	accepted := time.Now().UTC().Add(-70 * 24 * time.Hour).Truncate(24 * time.Hour)
	fx.svc.SetClock(clockwork.NewFakeClockAt(accepted))
	_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.products SET entitlements_spec='{"deferred_original":null}' WHERE id=(SELECT product_id FROM billing.prices WHERE id=$1)`, fx.priceID)
	require.NoError(t, err)

	start := accepted.Add(3 * 24 * time.Hour)
	fx.payload.Terms.Pending = true
	fx.payload.Terms.PeriodStart = start
	fx.payload.NativeSchedule.StartDate = start.Format("20060102")
	fx.gateway.txnID = ""
	in := fx.enqueueAndExecute(t)
	require.Equal(t, intents.StatusSucceeded, in.Status, string(in.ResultEvidence))
	require.Zero(t, fx.payload.Terms.Amount)
	require.Equal(t, uuid.Nil, fx.payload.Terms.PaymentID)
	_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE billing.prices SET amount=25000000,access_duration_hours=24 WHERE id=$1`, fx.priceID)
	require.NoError(t, err)
	_, err = fx.db.Pool().Exec(fx.ctx, `UPDATE billing.products SET entitlements_spec='{"changed":null}' WHERE id=(SELECT product_id FROM billing.prices WHERE id=$1)`, fx.priceID)
	require.NoError(t, err)
	fx.gateway.txnID = "scheduled-" + uuid.NewString()
	fx.gateway.observedAmount = "9.99"
	fx.gateway.observedAt = start.Add(time.Hour)
	fx.gateway.charged.Store(true)
	ctx := db.WithPSPID(fx.ctx, dbtest.TestPSPID(dbtest.TestMerchantID.UUID(), "mobius"))
	client, err := fx.svc.resolveNMIClient(ctx, "mobius")
	require.NoError(t, err)
	observer := webhooks.NMIConvergeService{DB: fx.db, Clock: clockwork.NewRealClock(), Rail: "nmi", NMIClient: client, SubscriptionService: fx.svc.SubscriptionService, PaymentService: fx.svc.PurchaseService.PaymentService, PriceService: fx.svc.PriceService, SubscriptionLifecycleService: fx.svc.Lifecycle}
	_, err = observer.Converge(ctx, fx.gateway.subID)
	require.NoError(t, err)
	sub, err := fx.svc.SubscriptionService.GetByID(ctx, fx.payload.Terms.SubscriptionID)
	require.NoError(t, err)
	require.Equal(t, models.StatusActive, sub.Status)
	require.WithinDuration(t, fx.payload.Terms.PeriodStart, *sub.CurrentPeriodStartsAt, time.Microsecond)
	require.WithinDuration(t, fx.payload.Terms.PeriodEnd, *sub.CurrentPeriodEndsAt, time.Microsecond)
	var amount int64
	require.NoError(t, fx.db.Pool().QueryRow(ctx, `SELECT amount FROM billing.payments WHERE subscription_id=$1 AND transaction_id=$2`, sub.ID, fx.gateway.txnID).Scan(&amount))
	require.EqualValues(t, 9990000, amount)
	var originalGrants int
	require.NoError(t, fx.db.Pool().QueryRow(ctx, `SELECT count(*) FROM billing.grants WHERE source_id=$1 AND source_type='subscription' AND event='grant' AND spec_snapshot->'entitlements' ? 'deferred_original' AND starts_at=$2 AND ends_at=$3`, sub.ID.String(), fx.payload.Terms.PeriodStart, fx.payload.Terms.PeriodEnd).Scan(&originalGrants))
	require.Equal(t, 1, originalGrants)
	var recurringAnchor string
	require.NoError(t, fx.db.Pool().QueryRow(ctx, `SELECT stored_credential_recurring_ref FROM billing.payment_methods WHERE id=$1`, fx.payload.Terms.PaymentMethodID).Scan(&recurringAnchor))
	require.Empty(t, recurringAnchor, "observed provider payment alone does not establish recurring CIT consent")

	original, err := fx.runner.Store.Get(ctx, in.ID)
	require.NoError(t, err)
	require.NoError(t, intents.ValidateInitialMembershipTerminal(original))
	var terms InitialMembershipPayload
	require.NoError(t, json.Unmarshal(original.Payload, &terms))
	require.Zero(t, terms.Terms.Amount)
	require.Equal(t, uuid.Nil, terms.Terms.PaymentID)
	require.EqualValues(t, 1, fx.gateway.createCalls.Load(), "observing the provider's first collection never resubmits enrollment")
}

func TestDeferredNativeFreePhaseDoesNotInventPayment(t *testing.T) {
	fx := newSubIntentFixture(t)
	accepted := time.Now().UTC().Add(-40 * 24 * time.Hour).Truncate(24 * time.Hour)
	fx.svc.SetClock(clockwork.NewFakeClockAt(accepted))
	_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.prices SET amount=0 WHERE id=$1`, fx.priceID)
	require.NoError(t, err)
	start := accepted.Add(3 * 24 * time.Hour)
	fx.payload.Terms.Pending = true
	fx.payload.Terms.PeriodStart = start
	fx.payload.NativeSchedule.StartDate = start.Format("20060102")
	fx.payload.Terms.Amount = 0
	fx.gateway.txnID = ""
	in := fx.enqueueAndExecute(t)
	require.Equal(t, intents.StatusSucceeded, in.Status, string(in.ResultEvidence))
	ctx := db.WithPSPID(fx.ctx, dbtest.TestPSPID(dbtest.TestMerchantID.UUID(), "mobius"))
	client, err := fx.svc.resolveNMIClient(ctx, "mobius")
	require.NoError(t, err)
	observer := webhooks.NMIConvergeService{DB: fx.db, Clock: clockwork.NewRealClock(), Rail: "nmi", NMIClient: client, SubscriptionService: fx.svc.SubscriptionService, PaymentService: fx.svc.PurchaseService.PaymentService, PriceService: fx.svc.PriceService, SubscriptionLifecycleService: fx.svc.Lifecycle}
	_, err = observer.Converge(ctx, fx.gateway.subID)
	require.NoError(t, err)
	sub, err := fx.svc.SubscriptionService.GetByID(ctx, fx.payload.Terms.SubscriptionID)
	require.NoError(t, err)
	require.Equal(t, models.StatusActive, sub.Status)
	var payments int
	require.NoError(t, fx.db.Pool().QueryRow(ctx, `SELECT count(*) FROM billing.payments WHERE subscription_id=$1`, sub.ID).Scan(&payments))
	require.Zero(t, payments)
	original, err := fx.runner.Store.Get(ctx, in.ID)
	require.NoError(t, err)
	require.NoError(t, intents.ValidateInitialMembershipTerminal(original))
}

func TestInitialMembershipNestedMethodFenceRefusesDeletion(t *testing.T) {
	fx := newSubIntentFixture(t)
	fx.gateway.txnID = ""
	fx.gateway.createMode.Store("ambiguous500")
	in := fx.enqueueAndExecute(t)
	require.Equal(t, intents.StatusUnknownNeedsVerify, in.Status)
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(in.Payload, &raw))
	require.NotContains(t, raw, "payment_method_id", "one canonical nested method identity")
	count, err := fx.db.Gen(fx.ctx).CountUnresolvedOperationsNamingPaymentMethod(fx.ctx, gen.CountUnresolvedOperationsNamingPaymentMethodParams{MerchantID: dbtest.TestMerchantID.UUID(), PaymentMethodID: fx.payload.Terms.PaymentMethodID})
	require.NoError(t, err)
	require.Positive(t, count)
	_, err = intents.NewStore(fx.db).Enqueue(fx.ctx, intents.EnqueueParams{MerchantID: dbtest.TestMerchantID.UUID(), Provider: "nmi", PspID: fx.payload.Terms.PSPID, IntentType: intents.TypeNMIPaymentMethodDelete, Payload: intents.NMIPaymentMethodDeletePayload{UserID: fx.payload.Terms.CustomerID.String(), PaymentMethodID: fx.payload.Terms.PaymentMethodID, RailCustomerRef: fx.payload.Instrument.RailCustomerRef, RailMethodRef: fx.payload.Instrument.RailMethodRef}, IdempotencyKey: intents.NMIPaymentMethodDeleteIdempotencyKey(fx.payload.Terms.PaymentMethodID), NextAttemptAt: time.Now(), Origin: intents.OriginUser})
	require.Error(t, err, "accepted initial membership pins its actual method against destructive admission")
	require.EqualValues(t, 1, fx.gateway.createCalls.Load())
}
