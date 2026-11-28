//go:build integration

package critical_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

// legacyNMIBook is a legacy NMI book imported as of asOf: one monthly member per
// due date, paid through it.
type legacyNMIBook struct {
	asOf      time.Time
	customers []billing.CustomerID
	schedules []string
	subs      []billing.SubscriptionID
	dues      []time.Time
	// seeded counts each row's moves to unverified the import made.
	seeded []int
}

// unconnectedNMI declares alpha's NMI PSP without credentials, as an import
// of a legacy book leaves it: declared, never connected.
func unconnectedNMI(w *world) {
	m := cfgMerchant(w.cfg, "alpha")
	m.PSPs = billing.PSPs{"nmi": openrails.NMIPSP{GatewayID: "critical-gateway", Environment: billing.EnvironmentTest}.PSPConfig()}
	setCfgMerchant(&w.cfg, "alpha", m)
}

// unconnectedNMIs declares alpha's NMI PSPs under keys, none with
// credentials.
func unconnectedNMIs(keys ...string) func(*world) {
	return func(w *world) {
		m := cfgMerchant(w.cfg, "alpha")
		m.PSPs = billing.PSPs{}
		for _, key := range keys {
			m.PSPs[key] = openrails.NMIPSP{GatewayID: "critical-" + key, Environment: billing.EnvironmentTest}.PSPConfig()
		}
		setCfgMerchant(&w.cfg, "alpha", m)
	}
}

// importNMIBook imports one monthly NMI member per offset, paid through now
// plus it, with its schedule and first sale at NMI.
func importNMIBook(t *testing.T, a *authorityWorld, p *nmiJournal, offsets ...time.Duration) legacyNMIBook {
	t.Helper()
	return importNMIBookAs(t, a, p, "nmi", offsets...)
}

// importNMIBookAs is importNMIBook into alpha's NMI PSP key.
func importNMIBookAs(t *testing.T, a *authorityWorld, p *nmiJournal, key string, offsets ...time.Duration) legacyNMIBook {
	t.Helper()
	p.core.AddPlan(nmimock.Plan{ID: "monthly", Name: "Monthly", Amount: "10.00", Days: 30})
	product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: key + "-membership", DisplayName: "NMI membership", Entitlements: []string{key + ":member"}})
	require.NoError(t, err)
	// Declared terms stand in while NMI cannot be read.
	price, err := a.alpha.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: key + "-monthly", PSP: key, PlanID: "monthly",
		Currency: "USD", UnitAmount: ptr(int64(10_000_000)), BillingIntervalHours: ptr(720)})
	require.NoError(t, err)
	b := legacyNMIBook{asOf: a.clock.Now().Truncate(time.Second)}
	book := billing.DeclaredBilling{AsOf: b.asOf, DefaultPSP: billing.PSPRef{Key: key}}
	for i, offset := range offsets {
		due := b.asOf.Add(offset)
		started := due.Add(-720 * time.Hour)
		vaultID := p.core.AddVault(nmimock.Card{Brand: "visa", Last4: "4242"})
		vault := p.core.Vault(vaultID)
		source := fmt.Sprintf("legacy-%s-member-%d", key, i)
		schedule := p.core.AddSchedule(nmimock.Schedule{Vault: vaultID, Plan: "monthly", Amount: "10.00", Days: 30, Created: started, NextBilling: due, Order: source})
		initial := p.core.AddScheduleSale(schedule, started)
		customer := billing.CustomerID(uuid.New())
		book.Customers = append(book.Customers, billing.DeclaredCustomer{Customer: customer})
		book.PaymentMethods = append(book.PaymentMethods, billing.DeclaredPaymentMethod{Customer: customer, Rail: "nmi", RailCustomerRef: vaultID, RailMethodRef: vault.BillingID,
			RecurringTransactionID: initial.TransactionID, CreatedAt: started, Card: &billing.CardDetails{Brand: ptr("visa"), Last4: ptr("4242")}})
		book.Subscriptions = append(book.Subscriptions, billing.DeclaredSubscription{SourceID: source, Customer: customer, Price: price.ID, Rail: "nmi", RailSubscriptionID: schedule,
			StartedAt: started, PaidThrough: &due, PaymentMethod: &billing.PaymentMethodRef{Rail: "nmi", RailCustomerRef: vaultID, RailMethodRef: vault.BillingID},
			AcceptedSchedule: &billing.DeclaredProviderSchedule{SourceReference: schedule, Amount: 10_000_000, Currency: "USD", IntervalHours: 720, FirstDueAt: due}})
		book.Transactions = append(book.Transactions, billing.DeclaredTransaction{SourceID: source + "-initial", RailSubscriptionID: schedule, TransactionID: initial.TransactionID,
			Type: "sale", Success: true, Amount: 10_000_000, Currency: "USD", OccurredAt: started, NextBillingAt: &due})
		b.customers, b.schedules, b.dues = append(b.customers, customer), append(b.schedules, schedule), append(b.dues, due)
	}
	require.NoError(t, p.persist(t.Context()))
	imported, err := a.alpha.ImportBilling(t.Context(), book)
	require.NoError(t, err)
	require.Empty(t, imported.Blocked, "%+v", imported.Reasons)
	for _, customer := range b.customers {
		sub := onlySubscription(t, a.alpha, customer)
		require.Equal(t, billing.SubscriptionActive, sub.Status)
		b.subs = append(b.subs, sub.ID)
		b.seeded = append(b.seeded, unverifiedTransitions(t, a.world, sub.ID))
	}
	return b
}

// pspFreshness is the known-through of alpha's PSP key, as ListPSPs serves it.
func pspFreshness(t *testing.T, client *openrails.Client, key string) billing.PSPFreshness {
	t.Helper()
	page, err := client.ListPSPs(t.Context(), billing.PSPListParams{})
	require.NoError(t, err)
	for _, psp := range page.Items {
		if psp.Key == key {
			return psp.Freshness
		}
	}
	t.Fatalf("no PSP %s", key)
	return billing.PSPFreshness{}
}

// pspEvents counts the merchant's host events of kind.
func pspEvents(t *testing.T, client *openrails.Client, kind billing.HostEventType) []billing.Event {
	t.Helper()
	page, err := client.ListEvents(t.Context(), billing.EventListParams{Types: []string{string(kind)}, Limit: 100})
	require.NoError(t, err)
	return page.Events
}

// troubleNotices counts the facts that tell the customers of a payment
// problem or an ending.
func troubleNotices(t *testing.T, w *world, customers []billing.CustomerID) int {
	t.Helper()
	ids := make([]uuid.UUID, 0, len(customers))
	for _, c := range customers {
		ids = append(ids, c.UUID())
	}
	var n int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{w.schema, "notification_events"}.Sanitize()+
		` WHERE customer_id = ANY($1) AND event_type IN ('premium_ended', 'subscription_canceled', 'payment_method_failed', 'payment_method_update_required', 'payment_authentication_required')`, ids).Scan(&n))
	return n
}

// unverifiedTransitions counts the subscription's moves to unverified.
func unverifiedTransitions(t *testing.T, w *world, id billing.SubscriptionID) int {
	t.Helper()
	var n int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{w.schema, "subscription_status_transitions"}.Sanitize()+
		" WHERE subscription_id = $1 AND to_status = 'unverified'", id.UUID()).Scan(&n))
	return n
}

// catchUpsSettled waits until no PSP catch-up is outstanding.
func catchUpsSettled(t *testing.T, fleet *river.Client[pgx.Tx]) {
	t.Helper()
	require.Eventually(t, func() bool {
		jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.psp_catch_up").States(rivertype.JobStateAvailable,
			rivertype.JobStatePending, rivertype.JobStateRetryable, rivertype.JobStateRunning, rivertype.JobStateScheduled))
		return err == nil && len(jobs.Jobs) == 0
	}, 20*time.Second, 20*time.Millisecond, "every PSP catch-up settles")
}

// connectNMI restarts alpha's engine with its NMI PSP's credentials, as an
// operator connecting it does, and starts a fleet on it.
func connectNMI(t *testing.T, a *authorityWorld, fleet *river.Client[pgx.Tx]) *river.Client[pgx.Tx] {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, fleet.StopAndCancel(ctx))
	require.NoError(t, a.engine.Close(t.Context()))
	m := cfgMerchant(a.cfg, "alpha")
	m.PSPs = billing.PSPs{"nmi": openrails.NMIPSP{GatewayID: "critical-gateway", Environment: billing.EnvironmentTest, SecurityKey: "critical-nmi-key", WebhookSigningSecret: "critical-nmi-webhook", TokenizationKey: "critical-collect-key"}.PSPConfig()}
	setCfgMerchant(&a.cfg, "alpha", m)
	var err error
	a.engine, err = openrails.New(t.Context(), a.cfg, a.deps)
	require.NoError(t, err)
	a.applyLimits()
	a.alpha, err = declaredMerchant(a.engine, "alpha")
	require.NoError(t, err)
	return quietFleet(t, a.world)
}

// staleImport is alpha's NMI book imported without NMI credentials and read
// 24 days after its as_of, NMI having renewed every member meanwhile.
func staleImport(t *testing.T) (*authorityWorld, *nmiJournal, legacyNMIBook, *river.Client[pgx.Tx]) {
	t.Helper()
	a, p := newNMIWorld(t, unconnectedNMI)
	b := importNMIBook(t, a, p, 3*24*time.Hour, 10*24*time.Hour)
	fleet := quietFleet(t, a.world)
	require.Eventually(t, func() bool { return pspFreshness(t, a.alpha, "nmi").Cause == billing.PSPImported }, 20*time.Second, 20*time.Millisecond,
		"the import finds its PSP cannot be read")
	a.clock.Advance(24 * 24 * time.Hour)
	p.core.RunDue()
	require.NoError(t, p.persist(t.Context()))
	runRegistered(t, fleet, "openrails.billing_pass", 1)
	catchUpsSettled(t, fleet)
	return a, p, b, fleet
}

func TestKnownThrough(t *testing.T) {
	t.Run("OR-J15-S77/an-imported-book-without-credentials-read-24-days-on-keeps-its-subscriptions-and-reports-its-known-through", func(t *testing.T) {
		a, _, b, _ := staleImport(t)
		for i, id := range b.subs {
			sub, err := a.alpha.GetSubscription(t.Context(), id)
			require.NoError(t, err)
			require.Equal(t, billing.SubscriptionActive, sub.Status, "a renewal after the PSP's known-through is not overdue")
			require.Nil(t, sub.UnverifiedReason)
			require.True(t, sub.CurrentPeriodEndsAt.Equal(b.dues[i]), "its last known state stands")
			require.NotNil(t, sub.MRR)
			require.Positive(t, sub.MRR.Amount)
			require.Nil(t, sub.MRRRisk, "our staleness puts no MRR at risk")
			require.True(t, entitled(t, a.alpha, b.customers[i], "nmi:member"), "access stays")
			require.Equal(t, b.seeded[i], unverifiedTransitions(t, a.world, id), "never unverified after the import")
			renewals, err := a.alpha.ListRenewals(t.Context(), billing.RenewalListParams{SubscriptionID: id})
			require.NoError(t, err)
			for _, renewal := range renewals.Items {
				require.NotEqual(t, "missed", renewal.FirstOutcome, "no rebill is missed for a gap nobody could read")
			}
		}
		missed, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.rebill.missed"})
		require.NoError(t, err)
		require.Empty(t, missed.Items)
		require.Empty(t, atRiskMRR(t, a.alpha, riskWindow(b.asOf, a.clock.Now().Add(time.Second))))
		require.Zero(t, troubleNotices(t, a.world, b.customers), "customers hear nothing of the merchant's integration")

		f := pspFreshness(t, a.alpha, "nmi")
		require.False(t, f.Current)
		require.NotNil(t, f.KnownThrough)
		require.True(t, f.KnownThrough.Equal(b.asOf), "known through the book's as_of, not %s", f.KnownThrough)
		require.Equal(t, billing.KnownThroughImport, f.Source)
		require.Equal(t, billing.PSPImported, f.Cause)
		require.Equal(t, billing.PSPSeverityInfo, f.Severity, "an import being onboarded is information")
		require.NotNil(t, f.StaleSince)
		require.Nil(t, f.LastReadAt)
		require.Empty(t, pspEvents(t, a.alpha, billing.HostEventPSPStale), "onboarding an import sends no notice")
	})

	t.Run("OR-J15-S78/connecting-an-imported-psp-reads-its-gap-before-detection-resumes", func(t *testing.T) {
		a, _, b, fleet := staleImport(t)
		fleet = connectNMI(t, a, fleet)
		// The next hourly pass tries the stale PSP again: now it reads.
		a.clock.Advance(time.Hour)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		require.Eventually(t, func() bool { return pspFreshness(t, a.alpha, "nmi").Current }, 20*time.Second, 20*time.Millisecond,
			"the catch-up reads the gap and the PSP is current")
		catchUpsSettled(t, fleet)
		f := pspFreshness(t, a.alpha, "nmi")
		require.NotNil(t, f.KnownThrough)
		require.True(t, f.KnownThrough.Equal(a.clock.Now().UTC()))
		require.Equal(t, billing.KnownThroughNow, f.Source)
		require.Empty(t, f.Cause)
		require.NotNil(t, f.CatchUp)
		require.True(t, f.CatchUp.From.Equal(b.asOf), "the gap began at the book's as_of")
		require.EqualValues(t, 2, f.CatchUp.Renewed, "the gap hid two renewals")
		require.Zero(t, f.CatchUp.Failed)
		require.Zero(t, f.CatchUp.Canceled)
		require.Zero(t, f.CatchUp.Unconfirmed)
		for i, id := range b.subs {
			sub, err := a.alpha.GetSubscription(t.Context(), id)
			require.NoError(t, err)
			require.Equal(t, billing.SubscriptionActive, sub.Status)
			require.True(t, sub.CurrentPeriodEndsAt.Equal(b.dues[i].Add(720*time.Hour)), "the renewal NMI holds is recorded: %s", sub.CurrentPeriodEndsAt)
			require.Equal(t, b.seeded[i], unverifiedTransitions(t, a.world, id), "read before detection resumed: never unverified")
		}
		require.Empty(t, pspEvents(t, a.alpha, billing.HostEventPSPStale))
		require.Empty(t, pspEvents(t, a.alpha, billing.HostEventPSPRecovered), "an episode never told is not told recovered")

		// Detection resumes: NMI stalls the first member's next renewal.
		a.clock.Advance(b.dues[0].Add(720*time.Hour + 25*time.Hour).Sub(a.clock.Now()))
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		subscriptionState(t, a, b.subs[0], "unverified")
		require.Eventually(t, func() bool {
			sub, err := a.alpha.GetSubscription(t.Context(), b.subs[0])
			return err == nil && sub.UnverifiedReason != nil && *sub.UnverifiedReason == billing.UnverifiedUnconfirmed
		}, 20*time.Second, 20*time.Millisecond, "NMI was read and could not tell")
		require.True(t, entitled(t, a.alpha, b.customers[0], "nmi:member"), "unverified keeps access")
		require.Zero(t, troubleNotices(t, a.world, b.customers))
	})

	t.Run("OR-J15-S79/a-live-psp-whose-reads-fail-for-a-day-is-stale-then-caught-up", func(t *testing.T) {
		mail := &alertMailbox{}
		a, p := newNMIWorld(t, func(w *world) {
			m := cfgMerchant(w.cfg, "alpha")
			m.AlertEmail = ptr("ops@alpha.example")
			setCfgMerchant(&w.cfg, "alpha", m)
			w.deps.Email = mail
		})
		b := importNMIBook(t, a, p, 24*time.Hour, 3*24*time.Hour, 4*24*time.Hour)
		fleet := quietFleet(t, a.world)
		var down atomic.Bool
		p.core.FailRequests(func(r *http.Request) bool { return down.Load() && strings.HasSuffix(r.URL.Path, "/query.php") }, http.StatusServiceUnavailable, 1_000_000)

		// The first renewal is read: NMI renewed it.
		p.core.RenewSchedule(b.schedules[0], true)
		require.NoError(t, p.persist(t.Context()))
		a.clock.Advance(b.dues[0].Add(25 * time.Hour).Sub(a.clock.Now()))
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		require.Eventually(t, func() bool {
			sub, err := a.alpha.GetSubscription(t.Context(), b.subs[0])
			return err == nil && sub.Status == billing.SubscriptionActive && sub.CurrentPeriodEndsAt.Equal(b.dues[0].Add(720*time.Hour))
		}, 20*time.Second, 20*time.Millisecond, "the read records the renewal")
		lastRead := a.clock.Now().UTC()
		require.True(t, pspFreshness(t, a.alpha, "nmi").Current)

		// NMI stops answering; it renews the other two members meanwhile.
		down.Store(true)
		p.core.RenewSchedule(b.schedules[1], true)
		p.core.RenewSchedule(b.schedules[2], true)
		require.NoError(t, p.persist(t.Context()))
		a.clock.Advance(b.dues[1].Add(25 * time.Hour).Sub(a.clock.Now()))
		failedAt := a.clock.Now().UTC()
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		subscriptionState(t, a, b.subs[1], "unverified")
		require.Eventually(t, func() bool { return pspFreshness(t, a.alpha, "nmi").Cause == billing.PSPUnreachable }, 20*time.Second, 20*time.Millisecond,
			"the failed read makes the PSP stale")
		first, err := a.alpha.GetSubscription(t.Context(), b.subs[1])
		require.NoError(t, err)
		require.Equal(t, billing.UnverifiedAwaitingProvider, *first.UnverifiedReason)
		require.Nil(t, first.MRRRisk, "awaiting the provider is not at risk")
		f := pspFreshness(t, a.alpha, "nmi")
		require.False(t, f.Current)
		require.True(t, f.KnownThrough.Equal(lastRead), "known through its last successful read")
		require.Equal(t, billing.KnownThroughRead, f.Source)
		require.True(t, f.StaleSince.Equal(failedAt))
		require.Equal(t, billing.PSPSeverityInfo, f.Severity, "inside NMI's renewal deadline")
		require.Empty(t, pspEvents(t, a.alpha, billing.HostEventPSPStale))

		// The third renewal's deadline passes while the PSP is stale: it keeps
		// its last known state. A day unreadable with live subscriptions is a
		// warning, told once.
		a.clock.Advance(b.dues[2].Add(25 * time.Hour).Sub(a.clock.Now()))
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		catchUpsSettled(t, fleet)
		third, err := a.alpha.GetSubscription(t.Context(), b.subs[2])
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionActive, third.Status)
		require.True(t, third.CurrentPeriodEndsAt.Equal(b.dues[2]))
		require.Eventually(t, func() bool { return len(pspEvents(t, a.alpha, billing.HostEventPSPStale)) == 1 }, 20*time.Second, 20*time.Millisecond)
		require.Equal(t, billing.PSPSeverityWarning, pspFreshness(t, a.alpha, "nmi").Severity)
		a.clock.Advance(time.Hour)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		catchUpsSettled(t, fleet)
		alertsSettled(t, fleet)
		require.Len(t, pspEvents(t, a.alpha, billing.HostEventPSPStale), 1, "never repeated")
		require.Equal(t, 1, mail.count("PSP data is out of date"))

		// NMI answers again: the catch-up reads the gap first.
		down.Store(false)
		a.clock.Advance(time.Hour)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		require.Eventually(t, func() bool { return pspFreshness(t, a.alpha, "nmi").Current }, 20*time.Second, 20*time.Millisecond)
		catchUpsSettled(t, fleet)
		for i := 1; i < 3; i++ {
			sub, err := a.alpha.GetSubscription(t.Context(), b.subs[i])
			require.NoError(t, err)
			require.Equal(t, billing.SubscriptionActive, sub.Status)
			require.True(t, sub.CurrentPeriodEndsAt.Equal(b.dues[i].Add(720*time.Hour)), "the renewal the gap hid is recorded")
		}
		require.Equal(t, b.seeded[2], unverifiedTransitions(t, a.world, b.subs[2]), "the stale PSP's renewal was read, never inferred")
		require.Empty(t, riskCases(t, a.world, a.alpha, b.subs[1]), "its wait for the provider was never at risk")
		f = pspFreshness(t, a.alpha, "nmi")
		require.NotNil(t, f.CatchUp)
		require.True(t, f.CatchUp.From.Equal(lastRead))
		require.EqualValues(t, 2, f.CatchUp.Renewed)
		require.Zero(t, f.CatchUp.Failed)
		require.Zero(t, f.CatchUp.Canceled)
		require.Zero(t, f.CatchUp.Unconfirmed)
		require.Eventually(t, func() bool { return len(pspEvents(t, a.alpha, billing.HostEventPSPRecovered)) == 1 }, 20*time.Second, 20*time.Millisecond)
		alertsSettled(t, fleet)
		require.Equal(t, 1, mail.count("PSP data is up to date"))
		require.Zero(t, troubleNotices(t, a.world, b.customers), "no payment-failed, past-due or cancellation notice")
	})
}
