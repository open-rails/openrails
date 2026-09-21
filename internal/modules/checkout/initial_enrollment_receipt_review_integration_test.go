//go:build integration

package checkout

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/intents"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A native paid-now enrollment needs a qualified initial charge. An approved
// schedule identifier alone cannot buy standing access or complete the intent.
func initialEnrollmentMissingChargeCannotActivatePaidAccess(t *testing.T) {
	fx := newSubIntentFixture(t)
	require.Positive(t, fx.payload.AmountMicros)
	require.Empty(t, fx.payload.StartDate)
	require.Nil(t, fx.payload.DelayedStart)
	_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.products SET entitlements_spec='{"initial_paid_access":null}' WHERE id=(SELECT product_id FROM billing.prices WHERE id=$1)`, fx.priceID)
	require.NoError(t, err)
	fx.gateway.txnID = ""
	result := fx.enqueueAndExecute(t)
	var active, payments, access int
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.subscriptions WHERE id=$1 AND status='active'`, fx.payload.LocalSubscriptionID).Scan(&active))
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.payments WHERE subscription_id=$1`, fx.payload.LocalSubscriptionID).Scan(&payments))
	require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.entitlements WHERE source_type='subscription' AND source_id=$1 AND entitlement='initial_paid_access' AND revoked_at IS NULL AND deleted_at IS NULL`, fx.payload.LocalSubscriptionID).Scan(&access))
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
		require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.payments WHERE id=$1 AND amount=$2 AND currency=$3 AND status='completed'`, fx.payload.Terms.PaymentID, fx.payload.AmountMicros, fx.payload.Currency).Scan(&payments))
		require.Equal(t, 1, payments)
		require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.subscriptions WHERE id=$1 AND status='active'`, fx.payload.LocalSubscriptionID).Scan(&active))
		require.Equal(t, 1, active)
		replay := fx.enqueueAndExecute(t)
		require.Equal(t, intents.StatusSucceeded, replay.Status)
		require.EqualValues(t, 1, fx.gateway.createCalls.Load())
	})
	t.Run("paid_now_requires_charge", initialEnrollmentMissingChargeCannotActivatePaidAccess)
	t.Run("provider_decline_has_bound_custody", func(t *testing.T) {
		fx := newSubIntentFixture(t)
		fx.gateway.createMode.Store("decline")
		in := fx.enqueueAndExecute(t)
		require.Equal(t, intents.StatusFailedTerminal, in.Status, string(in.ResultEvidence))
		_, found, err := intents.LoadInitialEnrollmentRefusal(in)
		require.NoError(t, err)
		require.True(t, found)
		require.NoError(t, intents.ValidateInitialEnrollmentTerminal(in))
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
				fx.payload.StartDate, fx.payload.DelayedStart = start.Format("20060102"), &start
			} else {
				fx.payload.AmountMicros = 0
				_, err := fx.db.Pool().Exec(fx.ctx, `UPDATE billing.prices SET amount=0 WHERE id=$1`, fx.priceID)
				require.NoError(t, err)
			}
			fx.gateway.txnID = ""
			result := fx.enqueueAndExecute(t)
			require.Equal(t, intents.StatusSucceeded, result.Status, string(result.ResultEvidence))
			response, err := nmiSubscriptionResponseFromIntent(result)
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
			assert.Equal(t, fx.payload.StartDate, form.Get("start_date"))
			var payments int
			require.NoError(t, fx.db.Pool().QueryRow(fx.ctx, `SELECT count(*) FROM billing.payments WHERE subscription_id=$1`, fx.payload.LocalSubscriptionID).Scan(&payments))
			require.Zero(t, payments)
		})
	}
}

func TestInitialEnrollmentRefusesRawProgressAuthority(t *testing.T) {
	for _, flag := range []string{"declined", "request_refused"} {
		t.Run(flag, func(t *testing.T) {
			fx := newSubIntentFixture(t)
			fx.gateway.txnID = ""
			fx.gateway.createMode.Store("ambiguous500")
			in := fx.enqueueAndExecute(t)
			require.Equal(t, intents.StatusUnknownNeedsVerify, in.Status)
			require.NoError(t, intents.NewStore(fx.db).RecordProgress(fx.ctx, in.ID, map[string]any{flag: true}))
			outcome := NewNMISubscriptionCreateIntentHandler(fx.svc).Verify(fx.ctx, in)
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
	var p NMISubscriptionCreatePayload
	require.NoError(t, json.Unmarshal(in.Payload, &p))
	p.Terms.PeriodEnd = p.Terms.PeriodEnd.Add(270 * 24 * time.Hour)
	p.StartDate = p.Terms.PeriodEnd.Format("20060102")
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	in.Payload = raw
	_, err = decodeNMISubscriptionCreatePayload(in)
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
				require.Error(t, intents.NewStore(fx.db).RecordProgress(fx.ctx, in.ID, map[string]any{"enrollment_submitted": false}))
			case "unknown":
				require.NoError(t, fx.runner.Store.MarkUnknown(fx.ctx, in.ID, time.Now(), "stale diagnostics", map[string]any{"enrollment_submitted": false}))
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
			require.Equal(t, true, progress["enrollment_submitted"])
			if action == "expire" {
				require.Equal(t, intents.StatusFailedRetryable, current.Status)
			} else {
				require.Equal(t, intents.StatusUnknownNeedsVerify, current.Status)
			}
		})
	}
}
