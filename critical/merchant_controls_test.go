//go:build integration

package critical_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/controls"
	"github.com/open-rails/openrails/internal/destructive"
)

// operator runs one controls command for the merchant (or --all-merchants)
// as a trusted operator would.
func operator(t *testing.T, w *world, control, value, merchant, reason string) string {
	t.Helper()
	target := []string{"--merchant", merchant}
	if merchant == "" {
		target = []string{"--all-merchants"}
	}
	return hostCLI(t, w, append(append([]string{"merchant", control, value}, target...), "--by", "critical operator", "--reason", reason)...)
}

func merchantRef(id billing.MerchantID) string { return "id:" + id.String() }

// controlsHistory is the merchant's controls entries, oldest first.
func controlsHistory(t *testing.T, client *openrails.Client) []billing.LogEntry {
	t.Helper()
	page, err := client.ListChanges(t.Context(), billing.LogListParams{ObjectType: billing.LogObjectControls})
	require.NoError(t, err)
	entries := page.Items
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
	return entries
}

// changedField is one field of a controls entry, before and after.
func changedField(t *testing.T, entry billing.LogEntry, field string) (before, after any) {
	t.Helper()
	var changes struct {
		Fields map[string]struct {
			Before any `json:"before"`
			After  any `json:"after"`
		} `json:"fields"`
	}
	require.NoError(t, json.Unmarshal(entry.Changes, &changes))
	change, ok := changes.Fields[field]
	require.True(t, ok, "entry %s changes %s: %s", entry.ID, field, entry.Changes)
	return change.Before, change.After
}

func TestMerchantControls(t *testing.T) {
	t.Run("OR-J06-S74/a-signup-under-a-rebill-hold-charges-and-its-first-renewal-waits-for-the-release", func(t *testing.T) {
		a, p := newPaymentWorld(t, func(w *world) { w.clock.Advance(-30 * 24 * time.Hour) })
		operator(t, a.world, "rebills", "hold", "alpha", "incident: duplicate charges suspected")
		sub := buyMembership(t, a, a.alpha, p, 24)
		require.Equal(t, billing.SubscriptionStatus("active"), sub.Status, "the signup creates its subscription")
		require.Len(t, p.effects(t), 1, "the signup's first charge is the customer's own: it never waits")
		due := *sub.CurrentPeriodEndsAt
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		held, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionStatus("active"), held.Status)
		require.Equal(t, due, *held.CurrentPeriodEndsAt, "the held renewal waits")
		require.Len(t, p.effects(t), 1, "nothing reaches the provider while rebills are held")
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"membership:paid"}})
		require.NoError(t, err)
		require.True(t, access.Entitlements["membership:paid"], "access is kept while the renewal waits")
		operator(t, a.world, "rebills", "release", "alpha", "incident resolved")
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		renewalSettled(t, a, p, sub, due)
	})
	t.Run("OR-J06-S75/a-dunning-retry-waits-under-a-rebill-hold-and-retries-after-the-release", func(t *testing.T) {
		a, p, sub := purchasedMembership(t, 7*24)
		due := *sub.CurrentPeriodEndsAt
		p.core.SetDecline("4242", "insufficient_funds")
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		declined := subscriptionState(t, a, sub.ID, "past_due")
		require.Equal(t, due.Add(24*time.Hour), *declined.Dunning.NextRetryAt)
		attempts, err := a.alpha.ListPaymentAttempts(t.Context(), billing.PaymentAttemptListParams{SubscriptionID: sub.ID})
		require.NoError(t, err)
		operator(t, a.world, "rebills", "hold", "alpha", "incident")
		p.core.SetDecline("4242", "")
		a.clock.Advance(24 * time.Hour)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		waiting, err := a.alpha.ListPaymentAttempts(t.Context(), billing.PaymentAttemptListParams{SubscriptionID: sub.ID})
		require.NoError(t, err)
		require.Len(t, waiting.Items, len(attempts.Items), "the due retry waits while rebills are held")
		subscriptionState(t, a, sub.ID, "past_due")
		require.Len(t, p.effects(t), 1)
		operator(t, a.world, "rebills", "release", "alpha", "incident resolved")
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		renewalSettled(t, a, p, sub, due)
	})
	t.Run("OR-J06-S76/scheduled-usage-collection-waits-under-a-rebill-hold-and-collects-after-the-release", func(t *testing.T) {
		a, p, customer := usageBillingWorld(t, false)
		operator(t, a.world, "rebills", "hold", "alpha", "incident")
		oneUsage(t, a.alpha, usageInput(customer, "held-usage", 3))
		a.clock.Advance(time.Second)
		fleet := startFleet(t, a.world)
		runUsageDue(t, fleet)
		invoices, err := a.alpha.ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Len(t, invoices.Items, 1, "the usage is billed")
		require.NotEqual(t, billing.InvoiceComplete, invoices.Items[0].Status, "and its collection waits")
		p.money(t, 0, 0)
		operator(t, a.world, "rebills", "release", "alpha", "incident resolved")
		runUsageDue(t, fleet)
		require.Eventually(t, func() bool {
			rows, err := a.alpha.ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: customer})
			return err == nil && len(rows.Items) == 1 && rows.Items[0].Status == billing.InvoiceComplete
		}, 15*time.Second, 20*time.Millisecond)
		p.money(t, 1, 3000)
	})
	t.Run("OR-J06-S77/a-rebill-hold-never-holds-what-a-person-asks-for", func(t *testing.T) {
		a, p, sub := purchasedMembership(t)
		operator(t, a.world, "rebills", "hold", "alpha", "incident")
		price := upgradePrice(t, a, sub)
		a.clock.Advance(12 * time.Hour)
		status, raw := a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.ID.String()+"/change", a.aliceToken, map[string]any{"price_id": price.ID},
			map[string]string{"Idempotency-Key": "held-upgrade"})
		require.Equal(t, http.StatusOK, status, string(raw))
		var changed billing.SubscriptionChange
		require.NoError(t, json.Unmarshal(raw, &changed))
		require.Equal(t, "succeeded", changed.Status, "a customer's immediate upgrade charges now: %s", raw)
		require.Len(t, p.effects(t), 2)
		payments, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: sub.CustomerID})
		require.NoError(t, err)
		require.NotEmpty(t, payments.Items)
		refund, err := a.alpha.RefundPayment(t.Context(), "held-staff-refund", payments.Items[len(payments.Items)-1].ID, billing.RefundPaymentParams{Full: true, Reason: "Refund under a rebill hold"})
		require.NoError(t, err, "a staff refund runs under a rebill hold")
		require.Negative(t, refund.Amount)
		require.Eventually(t, func() bool {
			for _, effect := range p.effects(t) {
				if effect.Kind == "refund" {
					return true
				}
			}
			return false
		}, 15*time.Second, 20*time.Millisecond, "the refund reaches the provider")
	})
	t.Run("OR-J06-S78/all-merchants-sets-every-merchant-and-each-change-is-history", func(t *testing.T) {
		w := newWorld(t, fullWrites)
		out := operator(t, w, "rebills", "hold", "", "platform incident")
		for _, slug := range []string{"alpha", "beta"} {
			require.Contains(t, out, "merchant "+slug+": writes full, rebills held, destructive disarmed (changed by critical operator)")
		}
		require.Contains(t, operator(t, w, "rebills", "hold", "", "platform incident"), "merchant alpha: writes full, rebills held, destructive disarmed (unchanged)",
			"a control already set is not changed again")
		operator(t, w, "writes", "limited", "beta", "beta review")
		operator(t, w, "rebills", "release", "", "incident resolved")
		listing := hostCLI(t, w, "merchant", "controls")
		require.Regexp(t, `alpha\s+full\s+released\s+disarmed`, listing)
		require.Regexp(t, `beta\s+limited\s+released\s+disarmed`, listing)
		for _, client := range []*openrails.Client{w.alpha, w.beta} {
			entries := controlsHistory(t, client)
			want := 2
			if client == w.beta {
				want = 3
			}
			require.Len(t, entries, want, "one entry per change, none for an unchanged control")
			before, after := changedField(t, entries[0], "rebills_held")
			require.Equal(t, false, before)
			require.Equal(t, true, after)
			require.Equal(t, billing.LogSourceCLI, entries[0].Source)
			require.Equal(t, client.MerchantID().String(), entries[0].ObjectID)
			require.Equal(t, "platform incident", *entries[0].Reason)
			_, by := changedField(t, entries[0], "changed_by")
			require.Equal(t, "critical operator", by)
			before, after = changedField(t, entries[len(entries)-1], "rebills_held")
			require.Equal(t, true, before)
			require.Equal(t, false, after)
		}
		before, after := changedField(t, controlsHistory(t, w.beta)[1], "writes")
		require.Equal(t, "full", before)
		require.Equal(t, "limited", after)
		_, err := hostCLIResult(t, w, "merchant", "rebills", "hold", "--all-merchants", "--by", "critical operator")
		require.Error(t, err, "a change needs its reason")
	})
	t.Run("OR-J06-S79/destructive-arming-is-each-merchants-own", func(t *testing.T) {
		a, p, sub := purchasedMembership(t, 7*24)
		database := worldDB(t, a.world)
		gate := destructive.New(database)
		for _, id := range []billing.MerchantID{a.alpha.MerchantID(), a.beta.MerchantID()} {
			v := gate.CheckMerchant(t.Context(), id.UUID())
			require.False(t, v.Allowed, "a merchant starts disarmed")
			require.False(t, v.EnforceArmed, "and its pulls advisory")
		}
		operator(t, a.world, "destructive", "arm", "beta", "beta reviewed")
		require.True(t, gate.CheckMerchant(t.Context(), a.beta.MerchantID().UUID()).Allowed)
		require.False(t, gate.CheckMerchant(t.Context(), a.alpha.MerchantID().UUID()).Allowed, "arming beta arms nothing of alpha's")
		due := *sub.CurrentPeriodEndsAt
		p.core.SetDecline("4242", "lost_card")
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := hostFleet(t, a.world)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		waiting := subscriptionState(t, a, sub.ID, "awaiting_method")
		a.clock.Advance(waiting.Dunning.FinalRetryAt.Sub(a.clock.Now()))
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		subscriptionState(t, a, sub.ID, "awaiting_method")
		operator(t, a.world, "destructive", "arm", "alpha", "alpha reviewed")
		v := gate.CheckMerchant(t.Context(), a.alpha.MerchantID().UUID())
		require.True(t, v.Allowed)
		require.True(t, v.EnforceArmed, "one control arms destructive actions and enforcing pulls")
		operator(t, a.world, "destructive", "disarm", "beta", "beta incident")
		require.False(t, gate.CheckMerchant(t.Context(), a.beta.MerchantID().UUID()).Allowed)
		require.True(t, gate.CheckMerchant(t.Context(), a.alpha.MerchantID().UUID()).Allowed, "disarming beta leaves alpha armed")
		a.clock.Advance(time.Hour)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		subscriptionState(t, a, sub.ID, "canceled")
	})
	t.Run("OR-J06-S80/export-and-restore-set-writes-readonly-and-an-armed-destination-is-refused", func(t *testing.T) {
		source := newWorld(t, fullWrites)
		customer := billing.CustomerID(uuid.New())
		_, err := source.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "controls-archive"}})
		require.NoError(t, err)
		archive := archiveBytes(t, source.alpha)
		require.Regexp(t, `alpha\s+readonly\s+released\s+disarmed`, hostCLI(t, source, "merchant", "controls", "--merchant", "alpha"), "export leaves its source readonly")
		target, client := archiveTarget(t, source)
		ref := merchantRef(client.MerchantID())
		operator(t, target, "destructive", "arm", ref, "armed too early")
		_, err = client.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
		require.Error(t, err, "an armed destination is refused")
		grants, err := client.ListCreditGrants(t.Context(), billing.CreditGrantListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Empty(t, grants.Items)
		operator(t, target, "destructive", "disarm", ref, "disarmed for the restore")
		_, err = client.ImportBillingArchive(t.Context(), bytes.NewReader(archive))
		require.NoError(t, err)
		require.Regexp(t, `restored-source\s+readonly\s+released\s+disarmed`, hostCLI(t, target, "merchant", "controls", "--merchant", ref), "a restore lands readonly and disarmed")
		operator(t, target, "writes", "full", ref, "the source is stopped")
		require.Regexp(t, `restored-source\s+full\s+released\s+disarmed`, hostCLI(t, target, "merchant", "controls", "--merchant", ref))
		entries := controlsHistory(t, client)
		require.NotEmpty(t, entries)
		_, after := changedField(t, entries[len(entries)-1], "writes")
		require.Equal(t, "full", after)
	})
	t.Run("OR-J06-S81/unreadable-controls-deny-writes-rebills-and-destructive-actions", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		setControl(t, a.world, "destructive_armed", true, a.alpha.MerchantID())
		database := worldDB(t, a.world)
		view := controls.View{DB: database}
		mid := a.alpha.MerchantID().UUID()
		require.False(t, view.Posture(t.Context(), mid).Limited())
		require.Empty(t, view.RebillHold(t.Context(), mid))
		require.True(t, destructive.New(database).CheckMerchant(t.Context(), mid).Allowed)
		input := saleInput(t, a, p)
		_, err := a.pool.Exec(t.Context(), "ALTER TABLE "+pgx.Identifier{a.schema, "merchant_controls"}.Sanitize()+" RENAME TO merchant_controls_away")
		require.NoError(t, err)
		require.True(t, view.Posture(t.Context(), mid).ReadOnly(), "unreadable writes are readonly")
		require.NotEmpty(t, view.RebillHold(t.Context(), mid), "unreadable rebills are held")
		require.False(t, destructive.New(database).CheckMerchant(t.Context(), mid).Allowed, "unreadable arming is disarmed")
		status, raw := a.request(t, http.MethodPost, "/v1/me/invoices", a.aliceToken, input, map[string]string{"Idempotency-Key": "unreadable-controls"})
		require.NotEqual(t, http.StatusCreated, status, string(raw))
		require.Empty(t, p.effects(t), "nothing reaches the provider")
	})
	t.Run("OR-J06-S82/the-migration-carries-postures-the-switch-and-policies-into-controls", func(t *testing.T) {
		s := newLegacySchemaBefore(t, "_merchant_controls.up.sql")
		d := s.seed(t)
		merchant := func(slug string) uuid.UUID {
			id := uuid.New()
			d.exec("INSERT INTO "+s.table("merchants")+" (id, slug, status) VALUES ($1, $2, 'active')", id, slug+"-"+id.String()[:8])
			return id
		}
		pulled := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
		armed, unreviewed, stopped, plain := d.m, merchant("unreviewed"), merchant("stopped"), merchant("plain")
		d.exec("UPDATE " + s.table("destructive_action_switch") + " SET enabled = true, updated_by = 'op', reason = 'armed'")
		d.exec("INSERT INTO "+s.table("merchant_write_posture")+" (merchant_id, mode, reason, set_by, set_at) VALUES ($1, 'limited', 'operator', 'posture op', $2)", armed, pulled.Add(2*time.Hour))
		d.exec("INSERT INTO "+s.table("merchant_destructive_policy")+" (merchant_id, destructive_actions_enabled, enforce_armed_at, first_pull_completed_at, updated_by, reason, updated_at) VALUES ($1, true, $2, $2, 'policy op', 'reviewed', $2)", armed, pulled)
		d.exec("INSERT INTO "+s.table("merchant_destructive_policy")+" (merchant_id, first_pull_completed_at, updated_at) VALUES ($1, $2, $2)", unreviewed, pulled)
		d.exec("INSERT INTO "+s.table("merchant_destructive_policy")+" (merchant_id, destructive_actions_enabled, enforce_armed_at, updated_by, reason, updated_at) VALUES ($1, false, $2, 'rollback', 'reversing a run', $2)", stopped, pulled)
		d.commit()

		require.NoError(t, s.apply(t.Context(), s.full))
		type row struct {
			writes      string
			held, armed bool
			firstPull   *time.Time
			by, reason  *string
		}
		read := func(id uuid.UUID) (row, bool) {
			var r row
			err := s.pool.QueryRow(t.Context(), "SELECT writes, rebills_held, destructive_armed, first_pull_completed_at, changed_by, reason FROM "+s.table("merchant_controls")+" WHERE merchant_id = $1", id).
				Scan(&r.writes, &r.held, &r.armed, &r.firstPull, &r.by, &r.reason)
			if errors.Is(err, pgx.ErrNoRows) {
				return r, false
			}
			require.NoError(t, err)
			return r, true
		}
		got, ok := read(armed)
		require.True(t, ok)
		require.Equal(t, "limited", got.writes)
		require.False(t, got.held)
		require.True(t, got.armed, "switch on, its stop off and its enforcement armed: armed")
		require.True(t, got.firstPull.Equal(pulled))
		require.Equal(t, "posture op", *got.by, "the newer change is the last")
		require.Equal(t, "write posture", *got.reason)
		got, ok = read(unreviewed)
		require.True(t, ok)
		require.False(t, got.armed, "a merchant never armed for enforcement stays disarmed")
		require.True(t, got.firstPull.Equal(pulled))
		require.Nil(t, got.by, "a pull's stamp is no change")
		got, ok = read(stopped)
		require.True(t, ok)
		require.False(t, got.armed, "a stopped merchant is disarmed")
		require.Equal(t, "rollback", *got.by)
		_, ok = read(plain)
		require.False(t, ok, "a merchant at the defaults keeps no row")
		for _, gone := range []string{"merchant_write_posture", "merchant_destructive_policy", "destructive_action_switch"} {
			var exists bool
			require.NoError(t, s.pool.QueryRow(t.Context(), "SELECT to_regclass($1) IS NOT NULL", s.schema+"."+gone).Scan(&exists))
			require.False(t, exists, gone)
		}
	})
}
