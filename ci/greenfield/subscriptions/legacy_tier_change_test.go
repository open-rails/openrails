//go:build greenfield && integration

package subscriptions_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/nmimock"
)

const tierUpdateStuck = "life.tier_change.provider_update_stuck"

// legacyOnTier imports an NMI-owned membership on price, whose NMI schedule
// bills cents every cycle hours and next bills `left` from now (a date).
func (w *world) legacyOnTier(tp topology, price tier, cents int64, cycle int, left time.Duration) *legacy {
	t := w.t
	t.Helper()
	c := w.newCustomer()
	end := w.clock.Now().Add(left).UTC().Truncate(24 * time.Hour)
	start := end.Add(-time.Duration(cycle) * time.Hour)
	amount := fmt.Sprintf("%d.%02d", cents/100, cents%100)
	vault := w.nmi.AddVault(visa)
	railSub := w.nmi.AddSchedule(nmimock.Schedule{Vault: vault, Plan: price.plan, Amount: amount, Days: cycle / 24, Months: 0, NextBilling: end})
	sale := w.nmi.AddScheduleSale(railSub, start)
	customerID, err := openrails.ParseCustomerID(c.id)
	require.NoError(t, err)
	priceID, err := openrails.ParsePriceID(price.ID)
	require.NoError(t, err)
	method := &openrails.PaymentMethodRef{Rail: "nmi", RailCustomerRef: vault, RailMethodRef: w.nmi.Vault(vault).BillingID}
	result, err := w.client[tp].ImportBilling(t.Context(), openrails.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: openrails.PSPRef{Key: "nmi"},
		Customers: []openrails.DeclaredCustomer{{Customer: customerID}},
		PaymentMethods: []openrails.DeclaredPaymentMethod{{Customer: customerID, Rail: "nmi", RailCustomerRef: vault, RailMethodRef: method.RailMethodRef,
			InitialTransactionID: sale.TransactionID, LastFour: visa.Last4, CardType: visa.Brand, ExpiryDate: "12/35"}},
		Subscriptions: []openrails.DeclaredSubscription{{SourceID: "tier-" + railSub, Customer: customerID, Price: priceID, Rail: "nmi", RailSubscriptionID: railSub,
			StartedAt: start, PaidThrough: &end, PaymentMethod: method}},
		Transactions: []openrails.DeclaredTransaction{{RailSubscriptionID: railSub, TransactionID: sale.TransactionID, Success: true, AmountCents: cents, Currency: "USD", OccurredAt: start}},
	})
	require.NoError(t, err)
	require.Len(t, result.Imported, 1, "%+v", result)
	w.settle()
	subs, err := w.client[tp].ListSubscriptions(t.Context(), openrails.SubscriptionFilter{CustomerID: c.id})
	require.NoError(t, err)
	require.Len(t, subs.Data, 1)
	require.Equal(t, "nmi_schedule", subs.Data[0].CollectionPolicy)
	l := &legacy{w: w, rail: "nmi", tp: tp, c: c, price: price.Price, railSub: railSub, sub: subs.Data[0].ID, ent: price.ent, railCust: vault}
	require.True(t, c.entitled(price.ent))
	return l
}

// scheduleWrites is every request that could create or remove an NMI
// schedule: never sent by an in-place tier change.
func (f *nmiFake) scheduleWrites() int {
	adds := f.CallsTo(http.MethodPost, "transact.php", func(v url.Values) bool { return v.Get("recurring") == "add_subscription" })
	return len(adds) + len(f.CallsTo(http.MethodPost, "/subscriptions", nil)) + len(f.CallsTo(http.MethodDelete, "/subscriptions", nil))
}

func (l *legacy) tierSales() []ledgerEntry { return l.w.nmi.ledger(l.railCust) }

// NMI-owned memberships change tier in place: an upgrade charges the
// preview's prorated amount once and moves the existing NMI schedule to the
// new amount (its next billing date kept); a downgrade moves the amount now and
// the next NMI renewal opens the lower tier. No second schedule, no drift.
func TestLegacyNMITierChange(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run("upgrade/"+string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.armDestructive()
			group := "g" + uuid.NewString()[:8]
			old := w.tierPrice(group, 1, 999, monthHours, true)
			next := w.tierPrice(group, 2, 1999, monthHours, true)
			l := w.legacyOnTier(tp, old, 999, monthHours, 10*day)
			end := l.periodEnd()
			sales := len(l.tierSales())

			preview, err := w.client[tp].PreviewTierChange(t.Context(), l.sub, openrails.ChangeTierRequest{PriceID: next.ID})
			require.NoError(t, err)
			require.Equal(t, "now", preview.Effective)
			require.Positive(t, preview.AmountDueNow)
			require.NotNil(t, preview.NextChargeDate)
			require.True(t, end.Equal(*preview.NextChargeDate), "NMI's next billing date is kept")
			require.Equal(t, time.UTC, preview.NextChargeDate.Location(), "instants are UTC")
			charged := preview.AmountDueNow / 10_000
			require.Contains(t, preview.Message, fmt.Sprintf("$%d.%02d now", charged/100, charged%100), "money in the currency's minor units")
			require.Contains(t, preview.Message, end.UTC().Format("January 2, 2006"))
			key := "up-" + uuid.NewString()
			done, err := w.client[tp].ChangeTier(t.Context(), l.sub, key, openrails.ChangeTierRequest{PriceID: next.ID})
			require.NoError(t, err)
			w.settle()
			require.Equal(t, "succeeded", done.Status, "%+v", done)
			require.Equal(t, preview.AmountDueNow, done.AmountDueNow, "charged equals preview")

			ledger := l.tierSales()
			require.Len(t, ledger, sales+1, "exactly one proration sale")
			require.Equal(t, preview.AmountDueNow/10_000, ledger[len(ledger)-1].Amount, "provider journal carries the quoted amount")
			updates := w.nmi.ScheduleUpdates(l.railSub)
			require.Len(t, updates, 1, "the schedule is updated once")
			require.Equal(t, next.plan, updates[0].Form.Get("plan_id"), "a named-plan schedule switches to the target's linked plan")
			require.Empty(t, updates[0].Form.Get("plan_amount"))
			require.Equal(t, "19.99", w.nmi.Schedule(l.railSub).Amount)
			require.Equal(t, next.plan, w.nmi.Schedule(l.railSub).Plan)
			require.True(t, w.nmi.Schedule(l.railSub).NextBilling.Equal(end), "the next billing date does not move")
			require.Zero(t, w.nmi.scheduleWrites(), "no second schedule, no delete")

			sub := w.subscription(tp, l.sub)
			require.Equal(t, next.ID, sub.PriceID)
			require.True(t, sub.CurrentPeriodEndsAt.Equal(end))
			require.True(t, l.c.entitled(next.ent), "the new tier is granted now")
			require.False(t, l.c.entitled(old.ent), "the old tier ends now")

			// Replay: the same key answers the same result and sends nothing.
			again, err := w.client[tp].ChangeTier(t.Context(), l.sub, key, openrails.ChangeTierRequest{PriceID: next.ID})
			require.NoError(t, err)
			require.Equal(t, done.AmountDueNow, again.AmountDueNow)
			require.Len(t, l.tierSales(), sales+1)
			require.Len(t, w.nmi.ScheduleUpdates(l.railSub), 1)

			w.pull()
			require.Empty(t, w.openFindings("pull.subscription.drift"), "an OpenRails-initiated change is not drift")
			w.converge()
			require.Zero(t, w.accessEndedNotices(l.c.id), "moving up is not access ending")

			// NMI's next renewal bills the new amount and is mirrored once.
			w.advanceTo(end.Add(time.Hour))
			require.Equal(t, http.StatusOK, w.deliver("nmi", l.providerRenewal(true)))
			ledger = l.tierSales()
			require.Equal(t, int64(1999), ledger[len(ledger)-1].Amount)
			require.Equal(t, w.nmiCharges(l.railCust), w.localCharges(tp, l.c.id), "one local payment per NMI sale")
			sub = w.subscription(tp, l.sub)
			require.Equal(t, next.ID, sub.PriceID)
			require.True(t, sub.CurrentPeriodEndsAt.After(end))
			require.True(t, l.c.entitled(next.ent))
			require.Zero(t, w.nmi.scheduleWrites())
			require.Empty(t, w.nmi.Unexpected())
		})

		t.Run("downgrade/"+string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.armDestructive()
			group := "g" + uuid.NewString()[:8]
			old := w.tierPrice(group, 2, 1999, monthHours, true)
			lower := w.tierPrice(group, 1, 499, monthHours, true)
			l := w.legacyOnTier(tp, old, 1999, monthHours, 10*day)
			end := l.periodEnd()
			sales := len(l.tierSales())

			preview, err := w.client[tp].PreviewTierChange(t.Context(), l.sub, openrails.ChangeTierRequest{PriceID: lower.ID})
			require.NoError(t, err)
			require.Equal(t, "period_end", preview.Effective)
			require.Zero(t, preview.AmountDueNow)
			done, err := w.client[tp].ChangeTier(t.Context(), l.sub, "down-"+uuid.NewString(), openrails.ChangeTierRequest{PriceID: lower.ID})
			require.NoError(t, err)
			w.settle()
			require.Equal(t, "succeeded", done.Status, "%+v", done)
			require.Zero(t, done.AmountDueNow)
			require.Len(t, l.tierSales(), sales, "nothing charged now")
			updates := w.nmi.ScheduleUpdates(l.railSub)
			require.Len(t, updates, 1)
			require.Equal(t, lower.plan, updates[0].Form.Get("plan_id"))
			require.Equal(t, "4.99", w.nmi.Schedule(l.railSub).Amount, "NMI bills the lower amount from its next renewal")
			sub := w.subscription(tp, l.sub)
			require.Equal(t, old.ID, sub.PriceID, "the paid period keeps its tier")
			require.NotNil(t, sub.ScheduledPriceID)
			require.Equal(t, lower.ID, *sub.ScheduledPriceID)
			require.True(t, l.c.entitled(old.ent))

			w.pull()
			require.Empty(t, w.openFindings("pull.subscription.drift"), "the scheduled amount is expected")

			w.advanceTo(end.Add(time.Hour))
			require.Equal(t, http.StatusOK, w.deliver("nmi", l.providerRenewal(true)))
			ledger := l.tierSales()
			require.Equal(t, int64(499), ledger[len(ledger)-1].Amount)
			sub = w.subscription(tp, l.sub)
			require.Equal(t, lower.ID, sub.PriceID, "the renewal opens the lower tier")
			require.Nil(t, sub.ScheduledPriceID)
			require.True(t, sub.CurrentPeriodEndsAt.After(end))
			require.True(t, l.c.entitled(lower.ent))
			require.False(t, l.c.entitled(old.ent))
			require.Zero(t, w.nmi.scheduleWrites())
			require.Empty(t, w.nmi.Unexpected())
		})
	}
}

// A declined upgrade changes nothing: no schedule update, same tier.
func TestLegacyNMITierUpgradeDeclined(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			group := "g" + uuid.NewString()[:8]
			old := w.tierPrice(group, 1, 999, monthHours, true)
			next := w.tierPrice(group, 2, 1999, monthHours, true)
			l := w.legacyOnTier(tp, old, 999, monthHours, 10*day)
			sales := len(l.tierSales())
			w.nmi.SetDecline(visa.Last4, "202")
			_, err := w.client[tp].ChangeTier(t.Context(), l.sub, "up-"+uuid.NewString(), openrails.ChangeTierRequest{PriceID: next.ID})
			var status *openrails.StatusError
			require.True(t, errors.As(err, &status), "%v", err)
			require.Equal(t, http.StatusPaymentRequired, status.Status, "%v", err)
			w.settle()
			require.Len(t, l.tierSales(), sales)
			require.Empty(t, w.nmi.ScheduleUpdates(l.railSub))
			require.Equal(t, "9.99", w.nmi.Schedule(l.railSub).Amount)
			sub := w.subscription(tp, l.sub)
			require.Equal(t, old.ID, sub.PriceID)
			require.Nil(t, sub.ScheduledPriceID)
			require.True(t, l.c.entitled(old.ent))
			require.False(t, l.c.entitled(next.ent))
		})
	}
}

// A schedule update that fails after the charge is retried until it lands,
// never charging again; while it cannot converge an operator finding stands.
func TestLegacyNMITierUpgradeScheduleUpdateRetried(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name  string
		fails int
	}{{"transient", 1}, {"stuck", 1000}} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			group := "g" + uuid.NewString()[:8]
			old := w.tierPrice(group, 1, 999, monthHours, true)
			next := w.tierPrice(group, 2, 1999, monthHours, true)
			l := w.legacyOnTier(embedded, old, 999, monthHours, 10*day)
			sales := len(l.tierSales())
			w.nmi.FailScheduleUpdates(row.fails)
			key := "up-" + uuid.NewString()
			_, err := w.client[embedded].ChangeTier(t.Context(), l.sub, key, openrails.ChangeTierRequest{PriceID: next.ID})
			require.NoError(t, err)
			w.settle()
			if row.name == "stuck" {
				w.until(func() bool { return len(w.openFindings(tierUpdateStuck)) > 0 }, "the stuck schedule update raises a finding")
				require.Contains(t, w.openFindings(tierUpdateStuck), l.sub.UUID().String())
				require.Len(t, l.tierSales(), sales+1, "one charge while the update is stuck")
				require.Equal(t, "9.99", w.nmi.Schedule(l.railSub).Amount)
				require.Equal(t, old.ID, w.subscription(embedded, l.sub).PriceID, "the local change waits for NMI")
				w.nmi.FailScheduleUpdates(0)
			}
			w.until(func() bool { return w.subscription(embedded, l.sub).PriceID == next.ID }, "the schedule update converges")
			require.Len(t, l.tierSales(), sales+1, "exactly one charge")
			require.Equal(t, "19.99", w.nmi.Schedule(l.railSub).Amount)
			require.GreaterOrEqual(t, len(w.nmi.ScheduleUpdates(l.railSub)), 2, "the failed update was retried")
			require.Empty(t, w.openFindings(tierUpdateStuck), "the finding closes once NMI converges")
			require.True(t, l.c.entitled(next.ent))
			require.Zero(t, w.nmi.scheduleWrites())
		})
	}
}

// Two replicas race two tier changes (different keys) on one membership: one
// is charged and applied once, the other is refused typed.
func TestLegacyNMITierChangeReplicaRace(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	group := "g" + uuid.NewString()[:8]
	old := w.tierPrice(group, 1, 999, monthHours, true)
	next := w.tierPrice(group, 2, 1999, monthHours, true)
	l := w.legacyOnTier(embedded, old, 999, monthHours, 10*day)
	sales := len(l.tierSales())
	second := w.startReplica()
	staff := w.auth.token(t, "staff")
	other, err := openrails.NewRemote(second.server.URL+mountPrefix, openrails.WithDefaultMerchant(w.slug),
		openrails.WithTokenProvider(func(context.Context) (string, error) { return staff, nil }))
	require.NoError(t, err)
	clients := []*openrails.Client{w.client[embedded], other}
	errs := make([]error, len(clients))
	var wg sync.WaitGroup
	for i, client := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = client.ChangeTier(context.WithoutCancel(t.Context()), l.sub, fmt.Sprintf("race-%d-%s", i, uuid.NewString()), openrails.ChangeTierRequest{PriceID: next.ID})
		}()
	}
	wg.Wait()
	w.settle()
	refused := 0
	for _, err := range errs {
		if err != nil {
			var status *openrails.StatusError
			require.True(t, errors.As(err, &status), "%v", err)
			require.Equal(t, http.StatusConflict, status.Status, "%v", err)
			refused++
		}
	}
	require.LessOrEqual(t, refused, 1)
	w.until(func() bool { return w.subscription(embedded, l.sub).PriceID == next.ID }, "one tier change applies")
	require.Len(t, l.tierSales(), sales+1, "one proration charge across replicas")
	require.Len(t, w.nmi.ScheduleUpdates(l.railSub), 1, "one schedule update")
	require.Zero(t, w.nmi.scheduleWrites())
}

// A target of another billing cycle cannot keep NMI's billing date: refused
// typed on preview and change, both topologies, and nothing is sent.
func TestLegacyNMITierChangeCrossCadenceRefused(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	group := "g" + uuid.NewString()[:8]
	old := w.tierPrice(group, 1, 999, monthHours, true)
	weekly := w.tierPrice(group, 2, 1999, 168, true)
	l := w.legacyOnTier(embedded, old, 999, monthHours, 10*day)
	sales := len(l.tierSales())
	for _, tp := range []topology{embedded, remote} {
		_, err := w.client[tp].PreviewTierChange(t.Context(), l.sub, openrails.ChangeTierRequest{PriceID: weekly.ID})
		requireCode(t, err, http.StatusConflict, openrails.CodeTierChangeCadenceUnsupported)
		_, err = w.client[tp].ChangeTier(t.Context(), l.sub, "x-"+uuid.NewString(), openrails.ChangeTierRequest{PriceID: weekly.ID})
		requireCode(t, err, http.StatusConflict, openrails.CodeTierChangeCadenceUnsupported)
	}
	w.settle()
	require.Len(t, l.tierSales(), sales)
	require.Empty(t, w.nmi.ScheduleUpdates(l.railSub))
	require.Equal(t, old.ID, w.subscription(embedded, l.sub).PriceID)
}

// accessEndedNotices counts premium_ended notifications queued for a customer.
func (w *world) accessEndedNotices(customerID string) int {
	w.t.Helper()
	var n int
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT count(*) FROM openrails.notifications WHERE customer_id = $1::uuid AND event_type = 'premium_ended'`), customerID).Scan(&n))
	return n
}

// A named-plan schedule changes only by switching plans, so a target price
// without a linked NMI plan of its amount and cycle is refused before any
// charge, on preview and change, both topologies.
func TestLegacyNMITierChangeRequiresLinkedPlan(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	group := "g" + uuid.NewString()[:8]
	old := w.tierPrice(group, 2, 999, monthHours, true)
	unlinked := w.tierPrice(group, 3, 1999, monthHours, false)
	lower := w.tierPrice(group, 1, 499, monthHours, false)
	l := w.legacyOnTier(embedded, old, 999, monthHours, 10*day)
	sales := len(l.tierSales())
	for _, target := range []tier{unlinked, lower} {
		for _, tp := range []topology{embedded, remote} {
			_, err := w.client[tp].PreviewTierChange(t.Context(), l.sub, openrails.ChangeTierRequest{PriceID: target.ID})
			requireCode(t, err, http.StatusConflict, openrails.CodeTierChangeRequiresLinkedPlan)
			_, err = w.client[tp].ChangeTier(t.Context(), l.sub, "x-"+uuid.NewString(), openrails.ChangeTierRequest{PriceID: target.ID})
			requireCode(t, err, http.StatusConflict, openrails.CodeTierChangeRequiresLinkedPlan)
		}
	}
	w.settle()
	require.Len(t, l.tierSales(), sales, "nothing charged")
	require.Empty(t, w.nmi.ScheduleUpdates(l.railSub), "nothing sent to NMI")
	require.Equal(t, old.ID, w.subscription(embedded, l.sub).PriceID)
	require.True(t, l.c.entitled(old.ent))
}

// A custom-amount schedule (no named plan) takes the new amount directly,
// whether or not the target price is linked to an NMI plan.
func TestLegacyNMITierChangeCustomSchedule(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			group := "g" + uuid.NewString()[:8]
			old := w.tierPrice(group, 1, 999, monthHours, true)
			next := w.tierPrice(group, 2, 1999, monthHours, false)
			l := w.legacyOnTier(tp, old, 999, monthHours, 10*day)
			w.nmi.customSchedule(l.railSub)
			end := l.periodEnd()
			sales := len(l.tierSales())
			preview, err := w.client[tp].PreviewTierChange(t.Context(), l.sub, openrails.ChangeTierRequest{PriceID: next.ID})
			require.NoError(t, err)
			done, err := w.client[tp].ChangeTier(t.Context(), l.sub, "up-"+uuid.NewString(), openrails.ChangeTierRequest{PriceID: next.ID})
			require.NoError(t, err)
			w.settle()
			require.Equal(t, "succeeded", done.Status, "%+v", done)
			require.Equal(t, preview.AmountDueNow, done.AmountDueNow)
			require.Len(t, l.tierSales(), sales+1)
			updates := w.nmi.ScheduleUpdates(l.railSub)
			require.Len(t, updates, 1)
			require.Equal(t, "19.99", updates[0].Form.Get("plan_amount"))
			require.Empty(t, updates[0].Form.Get("plan_id"))
			state := w.nmi.Schedule(l.railSub)
			require.Equal(t, "19.99", state.Amount)
			require.True(t, state.NextBilling.Equal(end))
			require.Equal(t, next.ID, w.subscription(tp, l.sub).PriceID)
			require.True(t, l.c.entitled(next.ent))
			require.Zero(t, w.nmi.scheduleWrites())
		})
	}
}

// Recovery of a v0.178.0 operation: admitted in amount mode, stuck because
// the schedule is on a named plan whose amount NMI will not change. Each
// retry re-reads the schedule; once it is seen on a named plan the target
// price's linked plan is used and the operation completes, charging once.
func TestLegacyNMITierChangeStuckNamedPlanRecovers(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	group := "g" + uuid.NewString()[:8]
	old := w.tierPrice(group, 1, 999, monthHours, true)
	next := w.tierPrice(group, 2, 1999, monthHours, true)
	l := w.legacyOnTier(embedded, old, 999, monthHours, 10*day)
	w.nmi.customSchedule(l.railSub)
	sales := len(l.tierSales())
	w.nmi.FailScheduleUpdates(1000)
	_, err := w.client[embedded].ChangeTier(t.Context(), l.sub, "up-"+uuid.NewString(), openrails.ChangeTierRequest{PriceID: next.ID})
	require.NoError(t, err)
	w.settle()
	w.until(func() bool { return len(w.openFindings(tierUpdateStuck)) > 0 }, "the stuck update raises a finding")
	require.Len(t, l.tierSales(), sales+1)
	// The schedule turns out to be on a named plan: NMI ignores plan_amount.
	w.nmi.EditSchedule(l.railSub, func(s *nmimock.Schedule) { s.Custom, s.Plan = false, old.plan })
	w.nmi.FailScheduleUpdates(0)
	w.until(func() bool { return w.subscription(embedded, l.sub).PriceID == next.ID }, "the operation converges on the linked plan")
	state := w.nmi.Schedule(l.railSub)
	require.Equal(t, next.plan, state.Plan)
	require.Equal(t, "19.99", state.Amount)
	require.Len(t, l.tierSales(), sales+1, "exactly one charge")
	require.Empty(t, w.openFindings(tierUpdateStuck))
	require.True(t, l.c.entitled(next.ent))
}
