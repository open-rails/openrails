//go:build greenfield && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/embed"
)

// importLegacyEvery lands one NMI-owned membership whose legacy plan bills
// every days, a third of the way through its current period.
func importLegacyEvery(t *testing.T, w *world, tp topology, days int, c *customer) *legacy {
	t.Helper()
	if c == nil {
		c = w.newCustomer()
	}
	l := &legacy{w: w, rail: "nmi", tp: tp, ent: "content:legacy-" + uuid.NewString()[:6], c: c}
	client := w.client[tp]
	plan := "legacy_plan_" + uuid.NewString()[:8]
	w.nmi.legacyPlan(plan, "9.99", days, 0)
	product, err := client.Products.Create(t.Context(), &openrails.ProductCreateParams{Key: "legacy-" + uuid.NewString()[:8], DisplayName: "Legacy", EntitlementsSpec: map[string]*int{l.ent: nil}})
	require.NoError(t, err)
	hours := days * 24
	l.price, err = client.Prices.Create(t.Context(), &openrails.PriceCreateParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 9_990_000, Currency: "USD", AutoRenew: true,
		AccessDurationHours: &hours, PSPLinks: map[string]map[string]string{"nmi": {"plan_id": plan}}})
	require.NoError(t, err)
	cycle := time.Duration(hours) * time.Hour
	end := w.clock.Now().Add(2 * cycle / 3).UTC().Truncate(24 * time.Hour)
	start := end.Add(-cycle)
	l.railCust = w.nmi.legacyVault(visa)
	l.railSub = w.nmi.legacyScheduleEvery(l.railCust, plan, "9.99", days, 0, end)
	paid := w.nmi.scheduleSale(l.railSub, start)
	customerID, err := openrails.ParseCustomerID(c.id)
	require.NoError(t, err)
	priceID, err := openrails.ParsePriceID(l.price.ID)
	require.NoError(t, err)
	method := &openrails.PaymentMethodRef{Rail: "nmi", RailCustomerRef: l.railCust, RailMethodRef: w.nmi.billingOf(l.railCust)}
	result, err := client.ImportBilling(t.Context(), openrails.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: openrails.PSPRef{Key: "nmi"},
		Customers: []openrails.DeclaredCustomer{{Customer: customerID}},
		PaymentMethods: []openrails.DeclaredPaymentMethod{{Customer: customerID, Rail: "nmi", RailCustomerRef: method.RailCustomerRef, RailMethodRef: method.RailMethodRef,
			InitialTransactionID: paid.TransactionID, LastFour: visa.Last4, CardType: visa.Brand, ExpiryDate: "12/35"}},
		Subscriptions: []openrails.DeclaredSubscription{{SourceID: "legacy-" + l.railSub, Customer: customerID, Price: priceID, Rail: "nmi", RailSubscriptionID: l.railSub,
			StartedAt: start, PaidThrough: &end, PaymentMethod: method}},
		Transactions: []openrails.DeclaredTransaction{{RailSubscriptionID: l.railSub, TransactionID: paid.TransactionID, Success: true, AmountCents: 999, Currency: "USD", OccurredAt: start}},
	})
	require.NoError(t, err)
	require.Len(t, result.Imported, 1, "%+v", result)
	w.settle()
	subs, err := client.ListSubscriptions(t.Context(), openrails.SubscriptionFilter{CustomerID: c.id})
	require.NoError(t, err)
	for _, sub := range subs.Data {
		if sub.RailSubscriptionID == l.railSub {
			l.sub = sub.ID
			require.Equal(t, "provider", sub.CollectionPolicy)
		}
	}
	require.False(t, l.sub.IsZero())
	return l
}

// engineWrites is every NMI request that could move money or create a
// charge schedule: sales (including rebill_subscription), subscription
// enrollments and v5 payments.
func (f *nmiFake) engineWrites(vault string) []providerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []providerCall
	for _, c := range f.writes {
		sale := c.Path == "transact.php" && (c.Form.Get("type") == "sale" || c.Form.Get("recurring") == "add_subscription") && (vault == "" || c.Form.Get("customer_vault_id") == vault)
		v5 := vault == "" && strings.HasPrefix(c.Path, "/payments") && !strings.HasSuffix(c.Path, "/auth") && !strings.HasSuffix(c.Path, "/refund") && !strings.HasSuffix(c.Path, "/void")
		if sale || v5 {
			out = append(out, c)
		}
	}
	return out
}

// renewAtNMI is NMI's recurring engine charging the schedule, with or
// without its notification reaching OpenRails.
func (l *legacy) renewAtNMI(paid, notify bool) {
	l.w.t.Helper()
	event := l.providerRenewal(paid)
	if notify {
		require.Equal(l.w.t, http.StatusOK, l.w.deliver("nmi", event))
	}
}

// OpenRails never charges an NMI-owned membership: not in the due pass, not
// in dunning after NMI's own decline, not from convergence or LIFE after
// missing news, not after a crash mid-convergence, and not from a second
// replica. The fake NMI journal holds no OpenRails money write for the
// legacy vault in any row.
func TestLegacyNMIZeroEngineCharges(t *testing.T) {
	t.Parallel()
	type row struct {
		name string
		days int
		// ends: OpenRails may end the NMI schedule (a stale mirror cancels
		// and stops the provider billing a member without access).
		ends bool
		run  func(t *testing.T, w *world, l *legacy)
	}
	cycles := func(t *testing.T, w *world, l *legacy) {
		for i := range 3 {
			w.advanceTo(w.nmi.scheduleState(l.railSub).NextBilling.Add(time.Hour))
			w.runRenewals()
			w.converge()
			// NMI bills on its own clock; the second notice never arrives.
			l.renewAtNMI(true, i != 1)
			w.runRenewals()
			w.converge()
		}
	}
	rows := []row{
		{"due_pass_daily", 1, false, cycles},
		{"due_pass_monthly", 30, false, cycles},
		{"due_pass_yearly", 365, false, cycles},
		{"nmi_dunning", 30, true, func(t *testing.T, w *world, l *legacy) {
			w.advanceTo(l.periodEnd().Add(time.Hour))
			l.renewAtNMI(false, true)
			require.Equal(t, "past_due", w.subscription(l.tp, l.sub).Status)
			for range 10 {
				w.advance(2 * day)
				w.runRenewals()
				w.wake()
			}
			w.converge()
		}},
		{"no_news_life", 30, false, func(t *testing.T, w *world, l *legacy) {
			w.advanceTo(l.periodEnd().Add(5 * day))
			for range 3 {
				w.converge()
				w.runRenewals()
				w.advance(day)
			}
		}},
		{"crash_mid_convergence", 30, false, func(t *testing.T, w *world, l *legacy) {
			w.advanceTo(l.periodEnd().Add(time.Hour))
			sale, _ := w.nmi.providerRenew(l.railSub, true)
			g := w.nmi.hold(newGate(func(r *http.Request) bool {
				return r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/v5/subscriptions/"+l.railSub)
			}, false))
			w.postNMI(nmiEvent("transaction.sale.success", obj{"transaction_id": sale.TransactionID, "transaction_type": "cc", "condition": "pendingsettlement", "amount": sale.Amount,
				"currency": "USD", "customer_vault_id": sale.Vault, "subscription": obj{"subscription_id": l.railSub}, "action": obj{"action_type": "sale", "amount": sale.Amount, "success": "1", "response_code": "100"}}))
			w.promoteUntil(g.arrived, "convergence reads NMI")
			w.kill()
			w.nmi.unhold()
			w.start()
			w.advance(30 * time.Minute)
			w.rescue()
			w.runRenewals()
			w.until(func() bool { return w.subscription(l.tp, l.sub).CurrentPeriodEndsAt.After(sale.At) }, "the NMI renewal is mirrored after the crash")
			require.Len(t, completed(w.payments(l.tp, l.c.id)), 2, "the NMI charge is recorded once")
		}},
		{"two_replicas", 30, false, func(t *testing.T, w *world, l *legacy) {
			e := enroll(t, w, "nmi", embedded)
			engineVault := w.nmi.lastSale().Vault
			second := w.startReplica()
			for i := range 3 {
				w.advanceTo(e.periodEnd().Add(time.Hour))
				w.duePassEverywhere(second)
				l.renewAtNMI(true, i != 1)
				w.duePassEverywhere(second)
				w.converge()
			}
			require.Len(t, w.nmi.ledger(engineVault), 4, "the engine membership charges once per period across replicas")
			require.Len(t, completed(w.payments(embedded, e.c.id)), 4)
		}},
	}
	for i, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.armDestructive()
			tp := []topology{embedded, remote}[i%2]
			l := importLegacyEvery(t, w, tp, r.days, nil)
			w.converge()
			r.run(t, w, l)
			require.Empty(t, w.nmi.engineWrites(l.railCust), "OpenRails never charges the NMI-owned membership")
			if !r.ends {
				require.True(t, w.nmi.scheduleLive(l.railSub), "NMI still owns the schedule")
			}
			require.Equal(t, "provider", w.subscription(tp, l.sub).CollectionPolicy)
			require.Empty(t, w.nmi.unexpected())
		})
	}
}

// One customer holds an NMI-owned legacy membership on one product and an
// engine membership on another, renewing on the same days at the same price.
// Each is billed once per period by its own owner, and the per-period
// duplicate-charge detector reports nothing across ownership modes.
func TestLegacyNMICoexistsWithEngine(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.armDestructive()
			e := enroll(t, w, "nmi", tp)
			engineVault := w.nmi.lastSale().Vault
			l := importLegacyEvery(t, w, tp, 30, e.c)
			w.converge()
			require.NotEqual(t, l.railCust, engineVault)
			for range 3 {
				w.advanceTo(e.periodEnd().Add(time.Hour))
				w.runRenewals()
				l.renewAtNMI(true, true)
				w.runRenewals()
				w.converge()
				require.True(t, e.c.entitled(e.ent) && e.c.entitled(l.ent), "both memberships stay entitled")
			}
			require.Len(t, w.nmi.ledger(engineVault), 4, "one engine charge per engine period")
			require.Empty(t, w.nmi.engineWrites(l.railCust), "no engine charge on the legacy vault")
			var legacyPaid, enginePaid int
			for _, p := range completed(w.payments(tp, e.c.id)) {
				switch p.SubscriptionID {
				case nil:
				default:
					if *p.SubscriptionID == l.sub {
						legacyPaid++
					} else if *p.SubscriptionID == e.sub {
						enginePaid++
					}
				}
			}
			require.Equal(t, 4, enginePaid)
			require.GreaterOrEqual(t, legacyPaid, 4, "every notified NMI renewal is mirrored")
			require.Empty(t, w.openFindings(duplicateCharge), "no duplicate across ownership modes")
			require.Empty(t, w.nmi.unexpected())
		})
	}
}

// postNMI sends a signed NMI notification without waiting for its work.
func (w *world) postNMI(payload any) {
	w.t.Helper()
	body, err := json.Marshal(payload)
	require.NoError(w.t, err)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(whsecNMI))
	mac.Write([]byte(ts + "." + string(body)))
	req, err := http.NewRequestWithContext(w.t.Context(), http.MethodPost, w.server.URL+mountPrefix+"/v1/webhooks/nmi/"+nmiAcct, bytes.NewReader(body))
	require.NoError(w.t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Webhook-Signature", fmt.Sprintf("t=%s,s=%s", ts, hex.EncodeToString(mac.Sum(nil))))
	res, err := http.DefaultClient.Do(req)
	require.NoError(w.t, err)
	_ = res.Body.Close()
	require.Equal(w.t, http.StatusOK, res.StatusCode)
}

// promoteUntil keeps due work promoted until done closes.
func (w *world) promoteUntil(done <-chan struct{}, what string) {
	w.t.Helper()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			w.settleQuiet()
		case <-deadline:
			w.t.Fatalf("timed out: %s", what)
		}
	}
}

// replica is a second process of the same deployment: its own runtime,
// River fleet and HTTP server over the same database and providers.
type replica struct {
	rt     *embed.Runtime
	jobs   *river.Client[pgx.Tx]
	server *httptest.Server
}

func (w *world) startReplica() *replica {
	w.t.Helper()
	rt, jobs, server, client, psp := w.rt, w.jobs, w.server, w.client, w.psp
	w.rt, w.jobs, w.server = nil, nil, nil
	w.start()
	r := &replica{rt: w.rt, jobs: w.jobs, server: w.server}
	w.rt, w.jobs, w.server, w.client, w.psp = rt, jobs, server, client, psp
	w.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.jobs.StopAndCancel(ctx)
		r.server.Close()
		_ = r.rt.Close(context.Background())
	})
	return r
}

// duePassEverywhere runs the scheduled due pass from both processes at once.
func (w *world) duePassEverywhere(other *replica) {
	w.t.Helper()
	var ids []int64
	for _, jobs := range []*river.Client[pgx.Tx]{w.jobs, other.jobs} {
		res, err := jobs.Insert(w.t.Context(), dunningPass{}, &river.InsertOpts{Queue: embed.QueueBilling})
		require.NoError(w.t, err)
		ids = append(ids, res.Job.ID)
	}
	for _, id := range ids {
		require.Eventually(w.t, func() bool {
			job, err := w.jobs.JobGet(w.t.Context(), id)
			return err == nil && (job.State == rivertype.JobStateCompleted || job.State == rivertype.JobStateCancelled || job.State == rivertype.JobStateDiscarded)
		}, 20*time.Second, 20*time.Millisecond, "due pass")
	}
	w.settle()
}

// advanceTo moves engine time forward to at; it never moves it back.
func (w *world) advanceTo(at time.Time) {
	if d := at.Sub(w.clock.Now()); d > 0 {
		w.advance(d)
	}
}
