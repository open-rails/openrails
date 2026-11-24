//go:build integration

package critical_test

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
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
				AND conrelid::regclass::text LIKE ANY (ARRAY['%payment_attempts', '%nmi_history_months'])
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

		require.NoError(t, migrate.Apply(ctx, pool, migrate.Options{Schema: schema, Logger: logger}))
		for step, want := range map[string]struct{ outcome, cause, reason, action string }{
			"approved":        {"approved", "", "", ""},
			"nmi-461":         {"refused", "merchant", "brand_not_accepted", "fix_payment_method"},
			"stripe-new-code": {"declined", "customer", "update_cardholder_data", "retry"},
			"issuer-51":       {"declined", "customer", "insufficient_funds", "retry"},
			"issuer-05":       {"declined", "issuer", "do_not_honor", "retry"},
			"processing":      {"error", "processing", "processing_error", "retry"},
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
	t.Run("OR-J45-S05/mastercard-retry-after-advice-delays-dunning-and-past-the-window-waits-for-a-new-card", func(t *testing.T) {
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
		t.Run("mac-27-past-the-window-waits-for-a-new-card", func(t *testing.T) {
			a, p, sub := purchasedMembership(t, 7*24)
			p.core.SetDecline("4242", "insufficient_funds")
			p.core.SetAdvice("4242", stripemock.Advice{NetworkAdviceCode: "27", NetworkDeclineCode: "51"})
			a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
			fleet := startFleet(t, a.world)
			runRegistered(t, fleet, "openrails.billing_pass", 1)
			rebill := awaitAttempt(t, a, sub.ID, "rebill")
			require.Equal(t, "retry", rebill.Action, "the decline itself is retryable")
			waiting := subscriptionState(t, a, sub.ID, "awaiting_method")
			require.NotNil(t, waiting.Dunning)
			require.True(t, waiting.Dunning.WaitingForNewCard, "four days is past the weekly case: it waits, it doesn't cancel")
			require.Nil(t, waiting.Dunning.NextRetryAt)
		})
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
