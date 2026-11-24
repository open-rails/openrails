//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/migrate"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	"github.com/open-rails/openrails/internal/modules/dashboard"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// Each refusal is stored with what caused it: the customer, the issuer's
// discretion, suspected fraud, the merchant or gateway, or processing.
func TestDeclineCauses(t *testing.T) {
	t.Run("OR-J45-S01/each-refusal-is-stored-with-its-outcome-cause-and-advice", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			refuse  func(*providerLedger)
			outcome billing.AttemptOutcome
			cause   billing.DeclineCause
			advice  stripemock.Advice
		}{
			{"insufficient-funds-is-the-customers", func(p *providerLedger) { p.core.SetDecline("4242", "insufficient_funds") }, billing.AttemptDeclined, billing.DeclineCauseCustomer,
				stripemock.Advice{Code: "try_again_later", NetworkAdviceCode: "02", NetworkDeclineCode: "51"}},
			{"do-not-honor-is-the-issuers-discretion", func(p *providerLedger) { p.core.SetDecline("4242", "do_not_honor") }, billing.AttemptDeclined, billing.DeclineCauseIssuer, stripemock.Advice{}},
			{"a-stolen-card-is-fraud", func(p *providerLedger) { p.core.SetDecline("4242", "stolen_card") }, billing.AttemptDeclined, billing.DeclineCauseFraud, stripemock.Advice{}},
			{"a-radar-block-is-refused-by-the-merchant", func(p *providerLedger) { p.core.SetRadarBlock("4242", "rule") }, billing.AttemptRefused, billing.DeclineCauseMerchant, stripemock.Advice{}},
			{"a-processing-error-is-an-error", func(p *providerLedger) { p.core.SetDecline("4242", "processing_error") }, billing.AttemptError, billing.DeclineCauseProcessing, stripemock.Advice{}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				a, p, sub := purchasedMembership(t, 7*24)
				tc.refuse(p)
				p.core.SetAdvice("4242", tc.advice)
				a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
				fleet := startFleet(t, a.world)
				runRegistered(t, fleet, "openrails.billing_pass", 1)
				rebill := awaitAttempt(t, a, sub.ID, "rebill")
				require.Equal(t, tc.outcome, rebill.Outcome)
				require.Equal(t, tc.cause, rebill.DeclineCause)
				require.Equal(t, tc.advice.Code, rebill.AdviceCode)
				require.Equal(t, tc.advice.NetworkAdviceCode, rebill.NetworkAdviceCode)
				initial := awaitAttempt(t, a, sub.ID, "initial")
				require.Equal(t, billing.AttemptApproved, initial.Outcome)
				require.Empty(t, initial.DeclineCause, "an approval has no cause")
				// The list filters by outcome and cause.
				for _, filter := range []billing.PaymentAttemptListParams{
					{SubscriptionID: sub.ID, Outcome: []billing.AttemptOutcome{tc.outcome}},
					{SubscriptionID: sub.ID, DeclineCause: []billing.DeclineCause{tc.cause}},
				} {
					found, err := a.alpha.ListPaymentAttempts(t.Context(), filter)
					require.NoError(t, err)
					require.Len(t, found.Items, 1)
					require.Equal(t, rebill.ID, found.Items[0].ID)
				}
			})
		}
	})
	t.Run("OR-J45-S09/the-migration-classifies-existing-attempts-and-history-from-their-stored-codes", func(t *testing.T) {
		ctx := t.Context()
		pool, err := pgxpool.New(ctx, os.Getenv("OPENRAILS_CRITICAL_DSN"))
		require.NoError(t, err)
		defer pool.Close()
		schema := "backfill_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		defer func() {
			_, err := pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
			require.NoError(t, err)
		}()
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		// The schema as it stood before decline causes.
		before := fstest.MapFS{}
		entries, err := fs.ReadDir(postgresmigrations.FS, ".")
		require.NoError(t, err)
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), "_decline_causes.up.sql") {
				raw, err := fs.ReadFile(postgresmigrations.FS, e.Name())
				require.NoError(t, err)
				before[e.Name()] = &fstest.MapFile{Data: raw}
			}
		}
		require.NoError(t, migrate.Apply(ctx, pool, migrate.Options{Schema: schema, Chain: before, Logger: logger}))
		table := func(name string) string { return pgx.Identifier{schema, name}.Sanitize() }
		// Rows without their parents: the foreign keys are not under test.
		_, err = pool.Exec(ctx, `DO $$ DECLARE c record; BEGIN
			FOR c IN SELECT conrelid::regclass AS t, conname FROM pg_constraint WHERE contype = 'f' AND connamespace = '`+schema+`'::regnamespace
				AND conrelid::regclass::text LIKE ANY (ARRAY['%payment_attempts', '%nmi_history_months', '%dashboard_configs'])
			LOOP EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I', c.t, c.conname); END LOOP; END $$`)
		require.NoError(t, err)
		merchant, customer, psp := uuid.New(), uuid.New(), uuid.New()
		type old struct{ rail, category, reason, action, code, issuer string }
		rows := map[string]old{
			"approved":        {"stripe", "approved", "", "", "", ""},
			"nmi-461":         {"nmi", "issuer_hard", "card_not_supported", "fix_payment_method", "461", ""},
			"stripe-new-code": {"stripe", "unknown", "unknown", "retry", "new_account_information_available", ""},
			"issuer-51":       {"nmi", "issuer_soft", "generic_decline", "retry", "200", "51"},
			"issuer-05":       {"nmi", "issuer_soft", "do_not_honor", "retry", "201", "05"},
			"processing":      {"stripe", "system_error", "processing_error", "retry", "processing_error", ""},
			// A postal-code mismatch read as the cause of NMI's 200; the issuer's
			// code says what it was, unless it says no more than do not honor.
			"zip-issuer-51": {"nmi", "card_data", "incorrect_zip", "fix_payment_method", "200", "51"},
			"zip-issuer-05": {"nmi", "card_data", "incorrect_zip", "fix_payment_method", "200", "05"},
		}
		ids := map[string]uuid.UUID{}
		for step, r := range rows {
			ids[step] = uuid.New()
			_, err := pool.Exec(ctx, `INSERT INTO `+table("payment_attempts")+` (id, merchant_id, customer_id, psp_id, rail, kind, owner, card_entry, source, observed_via,
				category, reason, action, response_code, issuer_code, amount, currency, attempted_at, step)
				VALUES ($1, $2, $3, $4, $5, 'initial', 'none', 'new', 'openrails', 'response', $6, NULLIF($7, ''), NULLIF($8, ''), NULLIF($9, ''), NULLIF($10, ''), 1000000, 'USD', now(), $11)`,
				ids[step], merchant, customer, psp, r.rail, r.category, r.reason, r.action, r.code, r.issuer, step)
			require.NoError(t, err)
		}
		month := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		for _, h := range []struct {
			kind, category, reason string
			n                      int
		}{{"one_off_sale", "approved", "", 5}, {"one_off_sale", "issuer_soft", "generic_decline", 2}, {"one_off_sale", "issuer_hard", "generic_decline", 3}, {"scheduled_rebill", "issuer_hard", "card_not_supported", 1}} {
			_, err := pool.Exec(ctx, `INSERT INTO `+table("nmi_history_months")+` (merchant_id, psp_id, month_at, kind, category, reason, authorizations) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				merchant, psp, month, h.kind, h.category, h.reason, h.n)
			require.NoError(t, err)
		}
		// Saved dashboards naming a dimension decline_cause renamed, or
		// filtering on what declined meant before refused and error.
		widget := func(id string, q billing.QueryMetricsParams) billing.DashboardWidget {
			return billing.DashboardWidget{ID: id, Title: id, Viz: "line", Grid: billing.DashboardGrid{W: 6, H: 4}, Query: q}
		}
		kept := widget("cash", billing.QueryMetricsParams{Measures: []string{"cash"}})
		failed := func(filters map[string][]string) billing.DashboardWidget {
			return widget("failed", billing.QueryMetricsParams{Measures: []string{"attempts"}, Filters: filters})
		}
		firstFailed := func(id string, filters map[string][]string) billing.DashboardWidget {
			return widget(id, billing.QueryMetricsParams{Measures: []string{"rebills"}, Filters: filters})
		}
		emptied := uuid.New()
		for owner, widgets := range map[uuid.UUID][]billing.DashboardWidget{
			merchant: {widget("by-category", billing.QueryMetricsParams{Measures: []string{"attempts"}, By: []string{"category"}}), kept,
				widget("first-category", billing.QueryMetricsParams{Measures: []string{"rebills"}, Filters: map[string][]string{"first_failure_category": {"issuer_hard"}}}),
				failed(map[string][]string{"outcome": {"declined"}, "psp": {"psp_a"}}),
				firstFailed("first-failed", map[string][]string{"first_outcome": {"declined", "error"}}),
				firstFailed("first-errored", map[string][]string{"first_outcome": {"approved", "error"}}),
				firstFailed("first-declined", map[string][]string{"first_outcome": {"declined"}, "first_failure_reason": {"do_not_honor", "invalid_request"}}),
				firstFailed("both", map[string][]string{"outcome": {"declined"}, "first_outcome": {"declined", "error"}})},
			emptied: {widget("by-first-category", billing.QueryMetricsParams{Measures: []string{"rebills"}, By: []string{"time", "first_failure_category"}})},
		} {
			layout, err := json.Marshal(widgets)
			require.NoError(t, err)
			_, err = pool.Exec(ctx, `INSERT INTO `+table("dashboard_configs")+` (merchant_id, layout) VALUES ($1, $2::jsonb)`, owner, string(layout))
			require.NoError(t, err)
		}

		require.NoError(t, migrate.Apply(ctx, pool, migrate.Options{Schema: schema, Logger: logger}))
		var layout []byte
		require.NoError(t, pool.QueryRow(ctx, `SELECT layout FROM `+table("dashboard_configs")+` WHERE merchant_id = $1`, merchant).Scan(&layout))
		var left []billing.DashboardWidget
		require.NoError(t, json.Unmarshal(layout, &left))
		// Declined and error together are every failure; either alone keeps its
		// reasons: error the old system errors, three of them refused now.
		errored := []string{"", "communication_error", "duplicate_transaction", "invalid_request", "issuer_unavailable", "merchant_config", "processing_error"}
		require.Equal(t, []billing.DashboardWidget{kept,
			failed(map[string][]string{"outcome": {"declined", "error", "refused"}, "psp": {"psp_a"}}),
			firstFailed("first-failed", map[string][]string{"first_outcome": {"declined", "error", "refused"}}),
			firstFailed("first-errored", map[string][]string{"first_outcome": {"approved", "error", "refused"}, "first_failure_reason": errored}),
			firstFailed("first-declined", map[string][]string{"first_outcome": {"declined", "refused"}, "first_failure_reason": {"do_not_honor"}}),
			firstFailed("both", map[string][]string{"outcome": {"declined", "error", "refused"}, "first_outcome": {"declined", "error", "refused"}}),
		}, left, "the widgets naming a renamed dimension are dropped; a declined or error filter keeps what it meant")
		var layouts int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM `+table("dashboard_configs")+` WHERE merchant_id = $1`, emptied).Scan(&layouts))
		require.Zero(t, layouts, "a layout left empty is deleted")
		for step, want := range map[string]struct{ outcome, cause, reason, action string }{
			"approved":        {"approved", "", "", ""},
			"nmi-461":         {"refused", "merchant", "brand_not_accepted", "fix_payment_method"},
			"stripe-new-code": {"declined", "customer", "update_cardholder_data", "retry"},
			"issuer-51":       {"declined", "customer", "insufficient_funds", "retry"},
			"issuer-05":       {"declined", "issuer", "do_not_honor", "retry"},
			"processing":      {"error", "processing", "processing_error", "retry"},
			"zip-issuer-51":   {"declined", "customer", "insufficient_funds", "fix_payment_method"},
			"zip-issuer-05":   {"declined", "customer", "incorrect_zip", "fix_payment_method"},
		} {
			var outcome, cause, reason, action string
			require.NoError(t, pool.QueryRow(ctx, `SELECT outcome, COALESCE(decline_cause, ''), COALESCE(reason, ''), COALESCE(action, '') FROM `+table("payment_attempts")+` WHERE id = $1`,
				ids[step]).Scan(&outcome, &cause, &reason, &action), step)
			require.Equal(t, want.outcome, outcome, step)
			require.Equal(t, want.cause, cause, step)
			require.Equal(t, want.reason, reason, step)
			require.Equal(t, want.action, action, "%s: the action dunning took then is kept", step)
		}
		history := map[string]int{}
		got, err := pool.Query(ctx, `SELECT kind || '/' || reason || '/' || outcome || '/' || COALESCE(decline_cause, ''), authorizations FROM `+table("nmi_history_months"))
		require.NoError(t, err)
		for got.Next() {
			var key string
			var n int
			require.NoError(t, got.Scan(&key, &n))
			history[key] = n
		}
		require.NoError(t, got.Err())
		require.Equal(t, map[string]int{
			"one_off_sale//approved/":                              5,
			"one_off_sale/generic_decline/declined/issuer":         5, // two categories, one reason: one count
			"scheduled_rebill/brand_not_accepted/refused/merchant": 1,
		}, history)
	})
	t.Run("OR-J45-S10/the-system-errors-finding-counts-the-rails-own-errors-not-the-issuers", func(t *testing.T) {
		for _, tc := range []struct {
			name, code, issuer string
			raised             bool
		}{
			// NMI's 200 behind which the issuer's own system failed (96).
			{"an-issuers-malfunction-is-not-ours", "200", "96", false},
			{"a-processor-error-is", "400", "", true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				a, _ := newNMIWorld(t)
				psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
				require.NoError(t, err)
				require.NotEmpty(t, psps.Items)
				merchant, customer := a.alpha.MerchantID().UUID(), uuid.New()
				_, err = a.pool.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{a.schema, "customers"}.Sanitize()+" (id, merchant_id) VALUES ($1, $2)", customer, merchant)
				require.NoError(t, err)
				attempt := func(outcome, cause, reason, action, code, issuer string) {
					t.Helper()
					_, err := a.pool.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{a.schema, "payment_attempts"}.Sanitize()+
						` (merchant_id, customer_id, psp_id, rail, kind, owner, card_entry, source, observed_via, outcome, decline_cause, reason, action, response_code, issuer_code, amount, currency, attempted_at, step)
						VALUES ($1, $2, $3, 'nmi', 'initial', 'none', 'new', 'openrails', 'response', $4, NULLIF($5, ''), NULLIF($6, ''), NULLIF($7, ''), NULLIF($8, ''), NULLIF($9, ''), 1000000, 'USD', $10, '')`,
						merchant, customer, psps.Items[0].ID.UUID(), outcome, cause, reason, action, code, issuer, a.clock.Now().Add(-10*time.Minute))
					require.NoError(t, err)
				}
				for range 19 {
					attempt("approved", "", "", "", "100", "00")
				}
				attempt("error", "processing", "processing_error", "retry", tc.code, tc.issuer)
				fleet := hostFleet(t, a.world)
				fleet.PeriodicJobs().Clear()
				require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
				require.NoError(t, fleet.Start(t.Context()))
				runRegistered(t, fleet, "openrails.merchant_health", 1)
				found, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.payments.system_errors"})
				require.NoError(t, err)
				if tc.raised {
					require.Len(t, found.Items, 1, "one error in twenty attempts is past 2%")
					return
				}
				require.Empty(t, found.Items, "the issuer's failure is not ours")
			})
		}
	})
	t.Run("OR-J45-S03/a-stripe-renewal-radar-blocked-is-the-merchants-refusal-not-the-issuers-fraud", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			block  func(*providerLedger)
			code   string
			reason billing.DeclineReason
			action string
		}{
			// Radar stops the charge before the network; its error still reads
			// as a fraudulent decline.
			{"radar-block-waits-for-a-new-card", func(p *providerLedger) { p.core.SetRadarBlock("4242", "highest_risk_level") }, "highest_risk_level", billing.DeclineBlockedByPSP, "fix_payment_method"},
			// A block reason no table knows is a block all the same.
			{"any-other-block-is-the-merchants", func(p *providerLedger) { p.core.SetRadarBlock("4242", "a_new_radar_reason") }, "blocked", billing.DeclineBlockedByPSP, "fix_payment_method"},
			{"issuer-fraud-is-the-issuers", func(p *providerLedger) { p.core.SetDecline("4242", "fraudulent") }, "fraudulent", billing.DeclineFraudulent, "non_recoverable"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				a, p, sub := purchasedMembership(t, 7*24)
				tc.block(p)
				a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
				fleet := startFleet(t, a.world)
				runRegistered(t, fleet, "openrails.billing_pass", 1)
				rebill := awaitAttempt(t, a, sub.ID, "rebill")
				require.Equal(t, tc.reason, rebill.Reason)
				require.Equal(t, tc.action, rebill.Action)
				require.Equal(t, tc.code, rebill.ResponseCode)
				if tc.reason == billing.DeclineBlockedByPSP {
					waiting := subscriptionState(t, a, sub.ID, "awaiting_method")
					require.NotNil(t, waiting.Dunning)
					require.True(t, waiting.Dunning.WaitingForNewCard, "a block never cancels the subscription")
				}
			})
		}
	})
	t.Run("OR-J45-S04/newly-mapped-codes-decline-with-their-cause", func(t *testing.T) {
		for _, tc := range []struct {
			code   string
			reason billing.DeclineReason
			action string
		}{
			{"do_not_try_again", billing.DeclineGeneric, "fix_payment_method"},
			{"new_account_information_available", billing.DeclineUpdateCardholderData, "fix_payment_method"},
			{"service_not_allowed", billing.DeclineTransactionNotAllowed, "fix_payment_method"},
			{"mobile_device_authentication_required", billing.DeclineAuthenticationRequired, "fix_payment_method"},
			{"invalid_amount", billing.DeclineInvalidRequest, "retry"},
			{"testmode_decline", billing.DeclineInvalidRequest, "retry"},
		} {
			t.Run("stripe-"+tc.code, func(t *testing.T) {
				a, p, sub := purchasedMembership(t, 7*24)
				p.core.SetDecline("4242", tc.code)
				a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
				fleet := startFleet(t, a.world)
				runRegistered(t, fleet, "openrails.billing_pass", 1)
				rebill := awaitAttempt(t, a, sub.ID, "rebill")
				require.Equal(t, tc.reason, rebill.Reason)
				require.Equal(t, tc.action, rebill.Action)
				state := "past_due"
				if tc.action == "fix_payment_method" {
					state = "awaiting_method"
				}
				subscriptionState(t, a, sub.ID, state)
			})
		}
		t.Run("nmi-461-the-merchants-account-takes-no-such-brand", func(t *testing.T) {
			a, p, sub := nmiMembership(t)
			due := *sub.CurrentPeriodEndsAt
			sale := p.core.AddScheduleSale(*sub.RailSubscriptionID, due)
			p.core.EditSale(sale.TransactionID, func(s *nmimock.Sale) { s.Declined = "461" })
			a.clock.Advance(due.Sub(a.clock.Now()) + time.Hour)
			fleet := hostFleet(t, a.world)
			fleet.PeriodicJobs().Clear()
			require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
			require.NoError(t, fleet.Start(t.Context()))
			convergeNMISubscription(t, a, fleet, sub)
			rebill := awaitAttempt(t, a, sub.ID, "rebill")
			require.Equal(t, billing.DeclineBrandNotAccepted, rebill.Reason)
			require.Equal(t, "fix_payment_method", rebill.Action)
			subscriptionState(t, a, sub.ID, "awaiting_method")
		})
	})
	t.Run("OR-J45-S05/mastercard-retry-after-advice-delays-dunning-even-past-the-window", func(t *testing.T) {
		t.Run("mac-26-retries-two-days-after-the-decline", func(t *testing.T) {
			a, p, sub := purchasedMembership(t, 7*24)
			p.core.SetDecline("4242", "insufficient_funds")
			p.core.SetAdvice("4242", stripemock.Advice{NetworkAdviceCode: "26", NetworkDeclineCode: "51"})
			a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
			fleet := startFleet(t, a.world)
			runRegistered(t, fleet, "openrails.billing_pass", 1)
			rebill := awaitAttempt(t, a, sub.ID, "rebill")
			require.Equal(t, billing.DeclineInsufficientFunds, rebill.Reason)
			dunned := subscriptionState(t, a, sub.ID, "past_due")
			require.NotNil(t, dunned.Dunning)
			require.NotNil(t, dunned.Dunning.NextRetryAt)
			// The weekly schedule's first retry is a day out; the advice says two.
			require.WithinDuration(t, rebill.AttemptedAt.Add(48*time.Hour), *dunned.Dunning.NextRetryAt, time.Minute)
		})
		t.Run("mac-27-past-the-window-keeps-the-retry-and-recovers", func(t *testing.T) {
			a, p, sub := purchasedMembership(t, 7*24)
			p.core.SetDecline("4242", "insufficient_funds")
			p.core.SetAdvice("4242", stripemock.Advice{NetworkAdviceCode: "27", NetworkDeclineCode: "51"})
			a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
			fleet := startFleet(t, a.world)
			runRegistered(t, fleet, "openrails.billing_pass", 1)
			rebill := awaitAttempt(t, a, sub.ID, "rebill")
			require.Equal(t, "retry", rebill.Action)
			// Four days is past the weekly case's window (two days and a day's
			// slack): the retry runs at the advised time all the same.
			dunned := subscriptionState(t, a, sub.ID, "past_due")
			require.NotNil(t, dunned.Dunning)
			require.NotNil(t, dunned.Dunning.NextRetryAt)
			require.WithinDuration(t, rebill.AttemptedAt.Add(96*time.Hour), *dunned.Dunning.NextRetryAt, time.Minute)
			require.False(t, dunned.Dunning.WaitingForNewCard, "the card is fine: nobody is asked for another")
			a.clock.Advance(rebill.AttemptedAt.Add(84 * time.Hour).Sub(a.clock.Now()))
			runRegistered(t, fleet, "openrails.billing_pass", 1)
			subscriptionState(t, a, sub.ID, "past_due")
			require.Zero(t, noticeCount(t, a, "payment_method_update_required"))
			// The advised retry is paid.
			p.core.SetDecline("4242", "")
			a.clock.Advance(dunned.Dunning.NextRetryAt.Sub(a.clock.Now()) + time.Minute)
			runRegistered(t, fleet, "openrails.billing_pass", 1)
			require.Eventually(t, func() bool {
				current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
				return err == nil && current.Status == billing.SubscriptionActive && current.CurrentPeriodEndsAt.After(*sub.CurrentPeriodEndsAt)
			}, 20*time.Second, 20*time.Millisecond, "the advised retry collects the renewal")
		})
	})
	t.Run("OR-J45-S12/an-issuer-that-keeps-deferring-ends-the-case-at-the-next-renewal", func(t *testing.T) {
		// A month further back, so every retry is dated in the database's past.
		a, p, sub := purchasedMembershipWith(t, 30*24, func(w *world) { w.clock.Advance(-30 * 24 * time.Hour) })
		enableLifecycleMaintenance(t, a)
		due := *sub.CurrentPeriodEndsAt
		renewal := due.Add(30 * 24 * time.Hour)
		p.core.SetDecline("4242", "insufficient_funds")
		p.core.SetAdvice("4242", stripemock.Advice{NetworkAdviceCode: "30", NetworkDeclineCode: "51"})
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		awaitAttempt(t, a, sub.ID, "rebill")
		// Each decline says retry in ten days: days 10 and 20 run, and the
		// retry day 30 would need is the next renewal's.
		for retries := 1; retries <= 2; retries++ {
			dunned := subscriptionState(t, a, sub.ID, "past_due")
			require.NotNil(t, dunned.Dunning)
			require.NotNil(t, dunned.Dunning.NextRetryAt)
			require.True(t, dunned.Dunning.NextRetryAt.Before(renewal))
			a.clock.Advance(dunned.Dunning.NextRetryAt.Sub(a.clock.Now()) + time.Minute)
			runRegistered(t, fleet, "openrails.billing_pass", 1)
			require.Eventually(t, func() bool {
				page, err := a.alpha.ListPaymentAttempts(t.Context(), billing.PaymentAttemptListParams{SubscriptionID: sub.ID, Kind: []string{"dunning_retry"}})
				require.NoError(t, err)
				return len(page.Items) == retries
			}, 20*time.Second, 20*time.Millisecond)
		}
		awaitDeferredToRenewal(t, a, fleet, sub.ID, renewal, 2)
	})
	t.Run("OR-J45-S13/a-late-retry-without-advice-keeps-dunning", func(t *testing.T) {
		// Two months back, so the late retry is dated in the database's past.
		a, p, sub := purchasedMembershipWith(t, 30*24, func(w *world) { w.clock.Advance(-60 * 24 * time.Hour) })
		enableLifecycleMaintenance(t, a)
		due := *sub.CurrentPeriodEndsAt
		renewal := due.Add(30 * 24 * time.Hour)
		p.core.SetDecline("4242", "insufficient_funds")
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		awaitAttempt(t, a, sub.ID, "rebill")
		dunned := subscriptionState(t, a, sub.ID, "past_due")
		require.NotNil(t, dunned.Dunning)
		require.NotNil(t, dunned.Dunning.NextRetryAt)
		// Nothing ran for a month: the retry runs after the next renewal and
		// declines with no advice. The case goes on with the attempts left.
		a.clock.Advance(renewal.Add(24 * time.Hour).Sub(a.clock.Now()))
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		retry := awaitAttempt(t, a, sub.ID, "dunning_retry")
		require.True(t, retry.AttemptedAt.After(renewal))
		late := subscriptionState(t, a, sub.ID, "past_due")
		require.NotNil(t, late.Dunning)
		require.NotNil(t, late.Dunning.NextRetryAt, "the policy's next retry stands")
		require.True(t, late.Dunning.NextRetryAt.After(renewal))
		require.NotNil(t, late.Dunning.RetriesLeft)
		require.Positive(t, *late.Dunning.RetriesLeft)
	})
	t.Run("OR-J45-S14/advice-past-a-weekly-renewal-keeps-the-case-past-due-until-then", func(t *testing.T) {
		a, p, sub := purchasedMembership(t, 7*24)
		enableLifecycleMaintenance(t, a)
		due := *sub.CurrentPeriodEndsAt
		renewal := due.Add(7 * 24 * time.Hour)
		p.core.SetDecline("4242", "insufficient_funds")
		// Ten days, past the weekly renewal: no retry may run before it.
		p.core.SetAdvice("4242", stripemock.Advice{NetworkAdviceCode: "30", NetworkDeclineCode: "51"})
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		awaitAttempt(t, a, sub.ID, "rebill")
		awaitDeferredToRenewal(t, a, fleet, sub.ID, renewal, 0)
	})
	t.Run("OR-J45-S15/a-new-card-is-not-held-to-the-old-cards-advice", func(t *testing.T) {
		a, p, sub := purchasedMembership(t, 7*24)
		enableLifecycleMaintenance(t, a)
		due := *sub.CurrentPeriodEndsAt
		renewal := due.Add(7 * 24 * time.Hour)
		p.core.SetDecline("4242", "insufficient_funds")
		p.core.SetAdvice("4242", stripemock.Advice{NetworkAdviceCode: "30", NetworkDeclineCode: "51"})
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		awaitAttempt(t, a, sub.ID, "rebill")
		deferred := subscriptionState(t, a, sub.ID, "past_due")
		require.NotNil(t, deferred.Dunning)
		require.Nil(t, deferred.Dunning.NextRetryAt, "the old card's advice defers the case to the renewal")
		// A new card, declined plainly: the case goes on under its policy.
		method := saveMethod(t, a, p, a.aliceToken, "5556")
		status, raw := a.request(t, http.MethodPut, "/v1/me/subscriptions/"+sub.ID.String()+"/payment-method", a.aliceToken, billing.SetSubscriptionPaymentMethodParams{PaymentMethodID: &method.ID})
		require.Equal(t, http.StatusOK, status, string(raw))
		p.core.SetDecline("5556", "insufficient_funds")
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		retry := awaitAttempt(t, a, sub.ID, "dunning_retry")
		require.NotNil(t, retry.Card)
		require.Equal(t, "5556", *retry.Card.Last4)
		dunned := subscriptionState(t, a, sub.ID, "past_due")
		require.NotNil(t, dunned.Dunning)
		require.NotNil(t, dunned.Dunning.NextRetryAt, "the new card's decline keeps dunning")
		require.True(t, dunned.Dunning.NextRetryAt.Before(renewal))
		// Its last retry declines too: the case ends as the card failed, not
		// as the issuer deferred.
		a.clock.Advance(dunned.Dunning.NextRetryAt.Sub(a.clock.Now()) + time.Minute)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		ended := subscriptionState(t, a, sub.ID, "canceled")
		require.NotNil(t, ended.CancelFeedback)
		require.NotEqual(t, "issuer_deferred", *ended.CancelFeedback)
		require.True(t, a.clock.Now().Before(renewal))
	})
	t.Run("OR-J45-S06/new-card-data-and-cards-that-wont-recur-wait-for-a-new-card", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			advice stripemock.Advice
			reason billing.DeclineReason
		}{
			{"mastercard-01-new-account-information", stripemock.Advice{NetworkAdviceCode: "01"}, billing.DeclineUpdateCardholderData},
			{"stripe-confirm-card-data", stripemock.Advice{Code: "confirm_card_data"}, billing.DeclineUpdateCardholderData},
			{"mastercard-40-non-reloadable-prepaid", stripemock.Advice{NetworkAdviceCode: "40"}, billing.DeclineGeneric},
			{"mastercard-41-single-use-virtual-card", stripemock.Advice{NetworkAdviceCode: "41"}, billing.DeclineGeneric},
		} {
			t.Run(tc.name, func(t *testing.T) {
				a, p, sub := purchasedMembership(t, 7*24)
				p.core.SetDecline("4242", "generic_decline")
				p.core.SetAdvice("4242", tc.advice)
				a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
				fleet := startFleet(t, a.world)
				runRegistered(t, fleet, "openrails.billing_pass", 1)
				rebill := awaitAttempt(t, a, sub.ID, "rebill")
				require.Equal(t, tc.reason, rebill.Reason)
				require.Equal(t, "fix_payment_method", rebill.Action)
				waiting := subscriptionState(t, a, sub.ID, "awaiting_method")
				require.NotNil(t, waiting.Dunning)
				require.True(t, waiting.Dunning.WaitingForNewCard)
			})
		}
	})
	t.Run("OR-J45-S11/an-engine-nmi-renewal-reads-the-issuers-code", func(t *testing.T) {
		renew := func(t *testing.T, issuer string, fault func(*nmiJournal)) (*authorityWorld, *billing.Subscription, billing.PaymentAttempt) {
			t.Helper()
			a, p, sub := nmiEngineMembership(t)
			p.core.SetDecline("4242", "200")
			p.core.SetIssuerCode("4242", issuer)
			fault(p)
			a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
			fleet := hostFleet(t, a.world)
			fleet.PeriodicJobs().Clear()
			require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
			require.NoError(t, fleet.Start(t.Context()))
			runRegistered(t, fleet, "openrails.billing_pass", 1)
			var rebill []billing.PaymentAttempt
			require.Eventually(t, func() bool {
				a.clock.Advance(6 * time.Minute)
				wakeJobs(t, fleet, "openrails.provider_operation")
				page, err := a.alpha.ListPaymentAttempts(t.Context(), billing.PaymentAttemptListParams{SubscriptionID: sub.ID, Kind: []string{"rebill"}})
				require.NoError(t, err)
				rebill = page.Items
				return len(rebill) > 0
			}, 30*time.Second, 50*time.Millisecond)
			require.Len(t, rebill, 1)
			return a, sub, rebill[0]
		}
		t.Run("a-never-retry-code-behind-200-waits-for-a-new-card", func(t *testing.T) {
			a, sub, rebill := renew(t, "04", func(*nmiJournal) {})
			require.Equal(t, billing.DeclinePickupCard, rebill.Reason)
			require.Equal(t, "fix_payment_method", rebill.Action)
			require.Equal(t, "04", rebill.IssuerCode)
			waiting := subscriptionState(t, a, sub.ID, "awaiting_method")
			require.NotNil(t, waiting.Dunning)
			require.True(t, waiting.Dunning.WaitingForNewCard)
		})
		t.Run("a-lost-answer-takes-the-issuers-code-from-the-order-read", func(t *testing.T) {
			_, _, rebill := renew(t, "51", func(p *nmiJournal) { p.core.DropSaleResponses(1) })
			require.Equal(t, billing.DeclineInsufficientFunds, rebill.Reason, "the order read carries the issuer's code")
			require.Equal(t, "51", rebill.IssuerCode)
			require.Equal(t, "retry", rebill.Action)
		})
		t.Run("a-failed-issuer-read-keeps-nmis-own-code", func(t *testing.T) {
			_, _, rebill := renew(t, "04", func(p *nmiJournal) { p.core.FailRequests(transactionLookup, http.StatusServiceUnavailable, 1_000_000) })
			require.Equal(t, billing.DeclineGeneric, rebill.Reason)
			require.Equal(t, "retry", rebill.Action)
			require.Empty(t, rebill.IssuerCode)
		})
	})
	t.Run("OR-J45-S02/an-nmi-catch-all-decline-is-classified-by-the-issuers-code", func(t *testing.T) {
		for _, tc := range []struct {
			name, issuer string
			reason       billing.DeclineReason
			action       string
			waits        bool
		}{
			// Visa category 1: the card is never charged again.
			{"pick-up-card-waits-for-a-new-card", "04", billing.DeclinePickupCard, "fix_payment_method", true},
			// A stop order behind NMI's catch-all waits; it never cancels.
			{"stop-order-waits-and-never-cancels", "R1", billing.DeclineStopRecurring, "fix_payment_method", true},
			{"insufficient-funds-is-retried", "51", billing.DeclineInsufficientFunds, "retry", false},
			{"suspected-fraud-is-retried", "59", billing.DeclineSuspectedFraud, "retry", false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				a, p, sub := nmiMembership(t)
				due := *sub.CurrentPeriodEndsAt
				sale := p.core.AddScheduleSale(*sub.RailSubscriptionID, due)
				p.core.EditSale(sale.TransactionID, func(s *nmimock.Sale) { s.Declined = "202" })
				a.clock.Advance(due.Sub(a.clock.Now()) + time.Hour)
				fleet := hostFleet(t, a.world)
				fleet.PeriodicJobs().Clear()
				require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
				require.NoError(t, fleet.Start(t.Context()))
				convergeNMISubscription(t, a, fleet, sub)
				dunned := subscriptionState(t, a, sub.ID, "past_due")
				require.NotNil(t, dunned.Dunning)
				require.NotNil(t, dunned.Dunning.NextRetryAt)

				// OpenRails' retry: Direct Post answers NMI's catch-all 200, and
				// only the Query API names the issuer's code.
				p.core.SetDecline("4242", "200")
				p.core.SetIssuerCode("4242", tc.issuer)
				a.clock.Advance(dunned.Dunning.NextRetryAt.Sub(a.clock.Now()) + time.Minute)
				runRegistered(t, fleet, "openrails.billing_pass", 1)
				retry := awaitAttempt(t, a, sub.ID, "dunning_retry")
				require.Equal(t, tc.reason, retry.Reason)
				require.Equal(t, tc.action, retry.Action)
				require.Equal(t, tc.issuer, retry.IssuerCode)
				require.Equal(t, "200", retry.ResponseCode, "NMI's own code is kept")
				if !tc.waits {
					retried := subscriptionState(t, a, sub.ID, "past_due")
					require.NotNil(t, retried.Dunning)
					require.NotNil(t, retried.Dunning.NextRetryAt, "the schedule goes on")
					return
				}
				waiting := subscriptionState(t, a, sub.ID, "awaiting_method")
				require.NotNil(t, waiting.Dunning)
				require.True(t, waiting.Dunning.WaitingForNewCard)
				require.Nil(t, waiting.Dunning.NextRetryAt, "no retry is scheduled on the card")
			})
		}
	})
	t.Run("OR-J45-S07/authorization-rates-the-decline-mix-and-their-cuts-read-the-stored-answers", func(t *testing.T) {
		ctx := t.Context()
		a, _, sub := nmiMembership(t)
		table := func(name string) string { return pgx.Identifier{a.schema, name}.Sanitize() }
		listed, err := a.alpha.ListPSPs(ctx, billing.PSPListParams{})
		require.NoError(t, err)
		require.Len(t, listed.Items, 1)
		merchant, customer, psp := a.alpha.MerchantID().UUID(), sub.CustomerID.UUID(), listed.Items[0].ID.UUID()
		// The stored credential the rebills cite.
		mandate := uuid.New()
		_, err = a.pool.Exec(ctx, `INSERT INTO `+table("mandates")+` (id, merchant_id, customer_id, psp_id, rail, kind, status, end_reason, ended_at, accepted_at)
			VALUES ($1, $2, $3, $4, 'nmi', 'card_on_file', 'ended', 'replaced', now(), now())`, mandate, merchant, customer, psp)
		require.NoError(t, err)
		// Two days before the membership began, where nothing else happened.
		from := a.clock.Now().Add(-72 * time.Hour)
		window := &billing.MetricsRange{From: from.Format(time.RFC3339), To: from.Add(48 * time.Hour).Format(time.RFC3339)}
		at := from
		type answer struct{ outcome, cause, reason, brand, bin, token, issuer, cited string }
		var (
			visa       = answer{brand: "visa", bin: "411111", token: "psp_token"}
			mastercard = answer{brand: "mastercard", bin: "555555", token: "network_token"}
		)
		with := func(card answer, outcome, cause, reason, issuer, cited string) answer {
			card.outcome, card.cause, card.reason, card.issuer, card.cited = outcome, cause, reason, issuer, cited
			return card
		}
		record := func(kind string, cycle *uuid.UUID, r answer) {
			t.Helper()
			at = at.Add(time.Minute)
			entry, action := "new", ""
			if cycle != nil {
				entry = "saved"
			}
			if r.outcome != "approved" {
				action = "retry"
			}
			var cites *uuid.UUID
			if r.cited != "" {
				cites = &mandate
			}
			sent := ""
			if r.cited == "sent" {
				sent = "storing-" + uuid.NewString()
			}
			_, err := a.pool.Exec(ctx, `INSERT INTO `+table("payment_attempts")+` (merchant_id, customer_id, psp_id, rail, kind, owner, card_entry, source, observed_via,
				outcome, decline_cause, reason, action, card_brand, card_bin, token_type, issuer_code, cycle_id, subscription_id, mandate_id, sent_initial_transaction_id,
				amount, currency, attempted_at, step)
				VALUES ($1, $2, $3, 'nmi', $4, 'nmi_schedule', $5, 'openrails', 'response', $6, NULLIF($7, ''), NULLIF($8, ''), NULLIF($9, ''), $10, $11, $12, NULLIF($13, ''),
				$14, $15, $16, NULLIF($17, ''), 10000000, 'USD', $18, '')`,
				merchant, customer, psp, kind, entry, r.outcome, r.cause, r.reason, action, r.brand, r.bin, r.token, r.issuer, cycle, sub.ID.UUID(), cites, sent, at)
			require.NoError(t, err)
		}
		cycle := func(missed bool) *uuid.UUID {
			t.Helper()
			at = at.Add(time.Minute)
			id := uuid.New()
			var missedAt *time.Time
			var reason *string
			if missed {
				missedAt, reason = &at, ptr("not_attempted")
			}
			_, err := a.pool.Exec(ctx, `INSERT INTO `+table("rebill_cycles")+` (id, merchant_id, subscription_id, customer_id, psp_id, rail, owner, due_at, amount, currency, missed_at, miss_reason)
				VALUES ($1, $2, $3, $4, $5, 'nmi', 'nmi_schedule', $6, 10000000, 'USD', $7, $8)`, id, merchant, sub.ID.UUID(), customer, psp, at, missedAt, reason)
			require.NoError(t, err)
			return &id
		}
		// New-card sales: six approved, four declined by the customer, the
		// issuer or fraud, one refused by the gateway, one that errored.
		for range 4 {
			record("initial", nil, with(visa, "approved", "", "", "", ""))
		}
		for range 2 {
			record("initial", nil, with(mastercard, "approved", "", "", "", ""))
		}
		for range 2 {
			record("initial", nil, with(visa, "declined", "customer", "insufficient_funds", "51", ""))
		}
		record("initial", nil, with(visa, "declined", "issuer", "do_not_honor", "05", ""))
		record("initial", nil, with(visa, "declined", "fraud", "stolen_card", "43", ""))
		record("initial", nil, with(visa, "refused", "merchant", "blocked_by_psp", "", ""))
		record("initial", nil, with(visa, "error", "processing", "processing_error", "", ""))
		// Renewals: three approved at the first try, one declined then
		// recovered, one refused, one missed. Those that sent the storing
		// transaction's id cite it; the others cite the credential without it.
		record("rebill", cycle(false), with(visa, "approved", "", "", "", "sent"))
		record("rebill", cycle(false), with(visa, "approved", "", "", "", "sent"))
		record("rebill", cycle(false), with(mastercard, "approved", "", "", "", "missing"))
		recovered := cycle(false)
		record("rebill", recovered, with(visa, "declined", "issuer", "do_not_honor", "05", "sent"))
		record("dunning_retry", recovered, with(visa, "approved", "", "", "", "sent"))
		record("rebill", cycle(false), with(visa, "refused", "merchant", "blocked_by_psp", "", "missing"))
		cycle(true)

		query := func(measures []string, filters map[string][]string, by ...string) []map[string]any {
			t.Helper()
			return metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: measures, By: by, Filters: filters, Range: window})
		}
		rates := func(rows []map[string]any, dim, measure string) map[string]float64 {
			t.Helper()
			out := map[string]float64{}
			for _, row := range rows {
				if row[measure] != nil {
					out[fmt.Sprint(row[dim])] = fraction(t, row[measure])
				}
			}
			return out
		}
		near := func(want, got map[string]float64) {
			t.Helper()
			require.Len(t, got, len(want), "%v", got)
			for k, v := range want {
				require.InDelta(t, v, got[k], 1e-12, k)
			}
		}
		// Of the fifteen authorizations the issuer answered (refusals and
		// errors are not its answers), ten approved and three were declined
		// at its discretion: do not honor twice and a stolen card.
		require.Equal(t, map[string]int64{"approved": 10, "declined": 5, "refused": 2, "error": 1},
			split(t, query([]string{"attempts"}, nil, "outcome"), "outcome", "attempts"))
		all := only(t, query([]string{"authorization_rate", "issuer_discretion_decline_rate"}, nil))
		require.InDelta(t, 10.0/15, fraction(t, all["authorization_rate"]), 1e-12)
		require.InDelta(t, 3.0/15, fraction(t, all["issuer_discretion_decline_rate"]), 1e-12)
		byKind := query([]string{"authorization_rate", "issuer_discretion_decline_rate"}, nil, "kind")
		near(map[string]float64{"initial": 0.6, "rebill": 0.75, "dunning_retry": 1}, rates(byKind, "kind", "authorization_rate"))
		near(map[string]float64{"initial": 0.2, "rebill": 0.25, "dunning_retry": 0}, rates(byKind, "kind", "issuer_discretion_decline_rate"))
		// The decline mix, and Visa's category of each declined Visa card.
		declined := map[string][]string{"outcome": {"declined"}}
		require.Equal(t, map[string]int64{"customer": 2, "issuer": 2, "fraud": 1},
			split(t, query([]string{"attempts"}, declined, "decline_cause"), "decline_cause", "attempts"))
		require.Equal(t, map[string]int64{"1": 1, "2": 2, "4": 2},
			split(t, query([]string{"attempts"}, declined, "visa_category"), "visa_category", "attempts"))
		// The cuts: by card, BIN, token type and whether the stored
		// credential's transaction id was sent.
		byBrand := query([]string{"authorization_rate", "issuer_discretion_decline_rate"}, nil, "card_brand")
		near(map[string]float64{"visa": 7.0 / 12, "mastercard": 1}, rates(byBrand, "card_brand", "authorization_rate"))
		near(map[string]float64{"visa": 0.25, "mastercard": 0}, rates(byBrand, "card_brand", "issuer_discretion_decline_rate"))
		near(map[string]float64{"411111": 7.0 / 12, "555555": 1}, rates(query([]string{"authorization_rate"}, nil, "card_bin"), "card_bin", "authorization_rate"))
		near(map[string]float64{"psp_token": 7.0 / 12, "network_token": 1}, rates(query([]string{"authorization_rate"}, nil, "token_type"), "token_type", "authorization_rate"))
		require.Equal(t, map[string]int64{"none": 12, "sent": 4, "missing": 2},
			split(t, query([]string{"attempts"}, nil, "network_transaction_id"), "network_transaction_id", "attempts"))

		// First-try rebill approval: three of the five renewals attempted; the
		// missed one had no first attempt.
		require.Equal(t, map[string]int64{"approved": 3, "declined": 1, "refused": 1, "missed": 1},
			split(t, query([]string{"rebills"}, nil, "first_outcome"), "first_outcome", "rebills"))
		require.InDelta(t, 0.6, fraction(t, only(t, query([]string{"first_try_rebill_approval_rate"}, nil))["first_try_rebill_approval_rate"]), 1e-12)
		near(map[string]float64{"visa": 0.5, "mastercard": 1},
			rates(query([]string{"first_try_rebill_approval_rate"}, nil, "card_brand"), "card_brand", "first_try_rebill_approval_rate"))
		near(map[string]float64{"sent": 2.0 / 3, "missing": 0.5},
			rates(query([]string{"first_try_rebill_approval_rate"}, nil, "network_transaction_id"), "network_transaction_id", "first_try_rebill_approval_rate"))

		// The dimensions decline_cause renamed are refused with it.
		_, err = a.alpha.QueryMetrics(ctx, billing.QueryMetricsParams{Measures: []string{"rebills"}, By: []string{"first_failure_category"}, Range: window})
		require.Equal(t, "first_decline_cause", metricRefusals(t, err)["by[0]"]["did_you_mean"])
		_, err = a.alpha.QueryMetrics(ctx, billing.QueryMetricsParams{Measures: []string{"attempts"}, Filters: map[string][]string{"category": {"issuer_soft"}}, Range: window})
		require.Equal(t, "decline_cause", metricRefusals(t, err)["filters.category"]["did_you_mean"])

		// The overview shows them: the rates as tiles, the decline mix and the
		// cuts in Payment health, every one answering.
		widgets := dashboard.DefaultWidgets(billing.DashboardProfile{Subscriptions: true, CardPayments: true})
		shown := map[string]billing.DashboardWidget{}
		for _, w := range widgets {
			shown[w.ID] = w
		}
		for id, measure := range map[string]string{"kpi-authorization-rate": "authorization_rate", "kpi-issuer-discretion": "issuer_discretion_decline_rate", "kpi-first-try-rebill": "first_try_rebill_approval_rate"} {
			require.Equal(t, "kpi", shown[id].Viz, id)
			require.Equal(t, []string{measure}, shown[id].Query.Measures, id)
		}
		answers := overviewAnswers(t, a.alpha, widgets, "")
		require.Equal(t, map[string]int64{"customer": 2, "issuer": 2, "fraud": 1},
			split(t, answers["declines-by-cause"], "decline_cause", "attempts"), "the stacked mix is the declines by cause")
		for _, id := range []string{"authorization-by-psp", "authorization-by-brand", "authorization-by-bin", "authorization-by-token"} {
			require.NotEmpty(t, answers[id], id)
		}
	})
	t.Run("OR-J45-S08/nmi-history-months-carry-each-answers-outcome-and-cause", func(t *testing.T) {
		a, p := newNMIWorld(t)
		vault := p.core.AddVault(nmimock.Card{Brand: "visa", Last4: "4242"})
		at := a.clock.Now().Add(-48 * time.Hour)
		for _, sale := range []nmimock.Sale{
			{},
			{},
			{Declined: "200", IssuerCode: "51"},
			{Declined: "200", IssuerCode: "05"},
			{Declined: "200", IssuerCode: "59"},
			{Declined: "461"},
		} {
			sale.Vault, sale.Amount, sale.At = vault, "10.00", at
			p.core.AddSale(sale)
			at = at.Add(time.Minute)
		}
		require.NoError(t, p.persist(t.Context()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		rows := metricTable(t, a.alpha, billing.QueryMetricsParams{Measures: []string{"nmi_history_authorizations"},
			By: []string{"outcome", "decline_cause", "reason"}, Range: &billing.MetricsRange{Last: "90d"}})
		got := map[string]int64{}
		for _, row := range rows {
			got[fmt.Sprintf("%v/%v/%v", row["outcome"], row["decline_cause"], row["reason"])] += whole(t, row["nmi_history_authorizations"])
		}
		require.Equal(t, map[string]int64{
			"approved//":                           2,
			"declined/customer/insufficient_funds": 1,
			"declined/issuer/do_not_honor":         1,
			"declined/fraud/suspected_fraud":       1,
			"refused/merchant/brand_not_accepted":  1,
		}, got, "NMI's catch-all 200 is the issuer's code; its 461 is the merchant account's")
	})
}

// awaitDeferredToRenewal checks a case the issuer's advice deferred past the
// next renewal: past due with no retry until renewal, no attempt in between,
// then ended there as issuer_deferred.
func awaitDeferredToRenewal(t *testing.T, a *authorityWorld, fleet *river.Client[pgx.Tx], id billing.SubscriptionID, renewal time.Time, retries int) {
	t.Helper()
	deferred := subscriptionState(t, a, id, "past_due")
	require.NotNil(t, deferred.Dunning)
	require.Nil(t, deferred.Dunning.NextRetryAt, "the advice allows no retry before the renewal")
	require.NotNil(t, deferred.Dunning.RetriesLeft)
	require.Zero(t, *deferred.Dunning.RetriesLeft)
	require.NotNil(t, deferred.Dunning.FinalRetryAt)
	require.True(t, deferred.Dunning.FinalRetryAt.Equal(renewal), "the case ends at the renewal")
	a.clock.Advance(renewal.Add(-time.Hour).Sub(a.clock.Now()))
	runRegistered(t, fleet, "openrails.billing_pass", 1)
	subscriptionState(t, a, id, "past_due")
	page, err := a.alpha.ListPaymentAttempts(t.Context(), billing.PaymentAttemptListParams{SubscriptionID: id, Kind: []string{"dunning_retry"}})
	require.NoError(t, err)
	require.Len(t, page.Items, retries, "nothing is charged while the issuer defers")
	a.clock.Advance(renewal.Add(time.Minute).Sub(a.clock.Now()))
	runRegistered(t, fleet, "openrails.billing_pass", 1)
	ended := subscriptionState(t, a, id, "canceled")
	require.NotNil(t, ended.CancelFeedback)
	require.Equal(t, "issuer_deferred", *ended.CancelFeedback, "the issuer deferred: not a card that failed")
}

// nmiEngineMembership buys a weekly OpenRails membership on NMI with a new
// card the gateway vaults: OpenRails collects its renewals.
func nmiEngineMembership(t *testing.T) (*authorityWorld, *nmiJournal, *billing.Subscription) {
	t.Helper()
	a, p := newNMIWorld(t)
	product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "nmi-engine-membership", DisplayName: "NMI engine membership", Entitlements: []string{"nmi:engine"}})
	require.NoError(t, err)
	price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "nmi-engine-weekly", UnitAmount: 10_000_000, Currency: "USD", BillingIntervalHours: ptr(7 * 24)})
	require.NoError(t, err)
	token := p.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242"})
	input := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price.ID}}, ExpectedTotal: ptr(int64(10_000_000)), Reusable: ptr(true), Payment: &billing.OrderPaymentParams{Token: token}}
	status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "nmi-engine-membership"})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var order billing.Order
	require.NoError(t, json.Unmarshal(raw, &order))
	require.Equal(t, billing.OrderComplete, order.Status, string(raw))
	require.NotNil(t, order.Lines[0].SubscriptionID)
	sub, err := a.alpha.GetSubscription(t.Context(), *order.Lines[0].SubscriptionID)
	require.NoError(t, err)
	require.Equal(t, "engine", string(sub.CollectionPolicy))
	return a, p, sub
}

// transactionLookup matches the Query API's read of one transaction by id.
func transactionLookup(r *http.Request) bool {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	form, _ := url.ParseQuery(string(body))
	query := strings.HasSuffix(r.URL.Path, "/query.php") || form.Has("report_type")
	return query && (form.Get("transaction_id") != "" || r.URL.Query().Get("transaction_id") != "")
}

// convergeNMISubscription runs the convergence a provider notice queues for
// an NMI subscription and waits for it to complete.
func convergeNMISubscription(t *testing.T, a *authorityWorld, fleet *river.Client[pgx.Tx], sub *billing.Subscription) {
	t.Helper()
	events, unsubscribe := fleet.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
	defer unsubscribe()
	job, err := fleet.Insert(t.Context(), subscriptionConvergeJob{PSPID: uuid.UUID(sub.PSPID), MerchantID: a.alpha.MerchantID().UUID(), Rail: "nmi", SubscriptionReference: *sub.RailSubscriptionID}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(t, err)
	deadline := time.After(20 * time.Second)
	for {
		select {
		case event := <-events:
			if event != nil && event.Job.ID == job.Job.ID {
				require.Equal(t, river.EventKindJobCompleted, event.Kind, "%+v", event.Job.Errors)
				return
			}
		case <-deadline:
			t.Fatal("the subscription's convergence did not finish")
		}
	}
}

// awaitAttempt waits for the subscription's one attempt of kind.
func awaitAttempt(t *testing.T, a *authorityWorld, id billing.SubscriptionID, kind string) billing.PaymentAttempt {
	t.Helper()
	var found []billing.PaymentAttempt
	require.Eventually(t, func() bool {
		page, err := a.alpha.ListPaymentAttempts(t.Context(), billing.PaymentAttemptListParams{SubscriptionID: id, Kind: []string{kind}})
		require.NoError(t, err)
		found = page.Items
		return len(found) > 0
	}, 20*time.Second, 20*time.Millisecond)
	require.Len(t, found, 1)
	return found[0]
}
