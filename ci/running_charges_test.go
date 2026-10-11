//go:build e2e && integration

package ci_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// chargeWorld is one merchant's engine on a clock the test moves.
type chargeWorld struct {
	t      *testing.T
	f      *fixture
	ctx    context.Context
	clock  *clockwork.FakeClock
	bill   *openrails.Engine
	client *openrails.Client
	db     *db.DB
}

func newChargeWorld(t *testing.T) *chargeWorld {
	t.Helper()
	f := newFixture(t)
	// Mid-day, so no test crosses 00:00 UTC unless it means to.
	clock := clockwork.NewFakeClockAt(time.Now().UTC().Truncate(24 * time.Hour).Add(36 * time.Hour))
	cfg := f.config()
	cfg.Merchants = map[string]openrails.MerchantDeclaration{"charges-" + uuid.NewString()[:8]: {DisplayName: "Charges"}}
	bill, client := f.boot(t, cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool, Clock: clock})
	database, err := db.NewWithPGXPool(f.pool, f.schema)
	require.NoError(t, err)
	return &chargeWorld{t: t, f: f, ctx: merchant.WithID(t.Context(), client.MerchantID()), clock: clock, bill: bill, client: client, db: database}
}

func (w *chargeWorld) now() time.Time { return w.clock.Now().UTC() }

// funded is a new customer holding amount of USD credit that never lapses.
func (w *chargeWorld) funded(amount int64) billing.CustomerID {
	w.t.Helper()
	customer := billing.CustomerID(uuid.New())
	if amount > 0 {
		w.fund(customer, amount, nil)
	}
	return customer
}

func (w *chargeWorld) fund(customer billing.CustomerID, amount int64, expiresAt *time.Time) *billing.CreditGrant {
	w.t.Helper()
	g, err := createCreditGrant(w.ctx, w.client, customer, billing.CreateCreditGrantParams{
		Currency: "USD", Amount: amount, Source: "support", SourceID: uuid.NewString(), ExpiresAt: expiresAt,
	})
	require.NoError(w.t, err)
	return g
}

func (w *chargeWorld) start(customer billing.CustomerID, ratePerHour int64, edit ...func(*billing.StartRunningChargeParams)) *billing.RunningCharge {
	w.t.Helper()
	params := billing.StartRunningChargeParams{Reference: "rental-" + uuid.NewString()[:12], CustomerID: customer, Currency: "USD", RatePerHour: ratePerHour}
	for _, fn := range edit {
		fn(&params)
	}
	charge, err := w.client.StartRunningCharge(w.ctx, params)
	require.NoError(w.t, err)
	return charge
}

func (w *chargeWorld) stop(reference string) *billing.RunningCharge {
	w.t.Helper()
	charge, err := w.client.StopRunningCharge(w.ctx, reference, billing.StopRunningChargeParams{})
	require.NoError(w.t, err)
	return charge
}

func (w *chargeWorld) balance(customer billing.CustomerID) *billing.Balance {
	w.t.Helper()
	bal, err := w.client.GetBalance(w.ctx, customer, "USD")
	require.NoError(w.t, err)
	return bal
}

// postings are the customer's ledger movements a running charge posted,
// oldest first.
func (w *chargeWorld) postings(customer billing.CustomerID) []billing.BalanceTransaction {
	w.t.Helper()
	page, err := w.client.ListBalanceTransactions(w.ctx, customer, billing.BalanceTransactionListParams{Currency: "USD", PageRequest: billing.PageRequest{Limit: billing.MaxPageLimit}})
	require.NoError(w.t, err)
	var out []billing.BalanceTransaction
	for i := len(page.Items) - 1; i >= 0; i-- {
		if page.Items[i].Source == "running_charge" {
			out = append(out, page.Items[i])
		}
	}
	return out
}

// expireCredits runs the credit-expiry sweep at the world's clock.
func (w *chargeWorld) expireCredits() {
	w.t.Helper()
	worker := riverjobs.CreditExpiryWorker{DB: w.db, Clock: w.clock}
	require.NoError(w.t, worker.Work(w.t.Context(), &river.Job[riverjobs.CreditExpiryArgs]{}))
}

// watch runs one pass of the balance watcher at the world's clock, as the
// engine's own worker does.
func (w *chargeWorld) watch() {
	w.t.Helper()
	worker := riverjobs.BalanceWatchWorker{DB: w.db, Money: engine.Graph(w.client).Runtime.MoneyService}
	require.NoError(w.t, worker.Work(w.t.Context(), &river.Job[riverjobs.BalanceWatchArgs]{}))
}

// watched is the customer's balance watch; nil when it has none.
func (w *chargeWorld) watched(customer billing.CustomerID) *gen.BillingBalanceWatch {
	w.t.Helper()
	row, err := w.db.GenDirectory().GetBalanceWatch(w.ctx, gen.GetBalanceWatchParams{MerchantID: w.client.MerchantID().UUID(), CustomerID: customer.UUID(), Currency: "USD"})
	if err == pgx.ErrNoRows {
		return nil
	}
	require.NoError(w.t, err)
	return &row
}

// events are the customer's host events of one type, oldest first.
func (w *chargeWorld) events(eventType billing.HostEventType, customer billing.CustomerID) []billing.BalanceHostEvent {
	w.t.Helper()
	page, err := w.client.ListHostEvents(w.ctx, billing.HostEventListParams{Type: eventType, PageRequest: billing.PageRequest{Limit: billing.MaxPageLimit}})
	require.NoError(w.t, err)
	var out []billing.BalanceHostEvent
	for _, event := range page.Items {
		require.NotNil(w.t, event.Balance)
		if event.Balance.CustomerID == customer {
			require.True(w.t, event.OccurredAt.Equal(event.Balance.AsOf))
			out = append(out, *event.Balance)
		}
	}
	return out
}

// depleted are the references ListRunningCharges names as depleted.
func (w *chargeWorld) depleted(customer billing.CustomerID) []string {
	w.t.Helper()
	page, err := w.client.ListRunningCharges(w.ctx, billing.RunningChargeListParams{CustomerID: customer, Depleted: true})
	require.NoError(w.t, err)
	out := []string{}
	for i := len(page.Items) - 1; i >= 0; i-- {
		require.Equal(w.t, billing.RunningChargeRunning, page.Items[i].State)
		out = append(out, page.Items[i].Reference)
	}
	return out
}

// tables hashes every OpenRails table of the world's schema (River's own are
// not OpenRails').
func (w *chargeWorld) tables() map[string]string {
	w.t.Helper()
	rows, err := w.f.pool.Query(w.t.Context(), `SELECT table_name FROM information_schema.tables
		WHERE table_schema = $1 AND table_type = 'BASE TABLE' AND table_name NOT LIKE 'river%'`, w.f.schema)
	require.NoError(w.t, err)
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(w.t, err)
	require.Contains(w.t, names, "running_charges")
	out := map[string]string{}
	for _, name := range names {
		var hash string
		require.NoError(w.t, w.f.pool.QueryRow(w.t.Context(), fmt.Sprintf(
			"SELECT count(*)::text || ':' || md5(coalesce(string_agg(md5(t::text), '' ORDER BY md5(t::text)), '')) FROM %s t", pgx.Identifier{w.f.schema, name}.Sanitize())).Scan(&hash))
		out[name] = hash
	}
	return out
}

func reused(t *testing.T, err error, param string) {
	t.Helper()
	require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
	var se *billing.StatusError
	require.ErrorAs(t, err, &se)
	require.Equal(t, http.StatusUnprocessableEntity, se.Status, se.Error())
	require.Equal(t, billing.CodeIdempotencyKeyReused, se.Code)
	require.NotNil(t, se.Param)
	require.Equal(t, param, *se.Param)
}

// A charge stopped after N seconds posts round_half_up(rate x N / 3600) once,
// and the balance falls by exactly that. A repeated stop answers the same;
// another instant is refused.
func TestRunningChargeChargesRateTimesSeconds(t *testing.T) {
	w := newChargeWorld(t)
	for _, tc := range []struct {
		name        string
		ratePerHour int64
		seconds     int64
		want        int64
	}{
		{"a fraction rounds down", 1_234_567, 5000, 1_714_676},
		{"half a unit rounds up", 1, 1800, 1},
		{"under half a unit is nothing", 1, 1799, 0},
		{"a tie in the millions rounds up", 1_000_001, 1800, 500_001},
		{"one second", 3_600_000, 1, 1000},
		{"three days and seven seconds", 2_500_000, 3*86_400 + 7, 180_004_861},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const funded = 1_000_000_000
			customer := w.funded(funded)
			startedAt := w.now()
			charge := w.start(customer, tc.ratePerHour, func(p *billing.StartRunningChargeParams) { p.Resource = "gpu:a100" })
			require.Equal(t, billing.RunningChargeRunning, charge.State)
			require.Equal(t, billing.RunningChargeIDPrefix, charge.ID.String()[:len(billing.RunningChargeIDPrefix)])
			require.True(t, charge.StartedAt.Equal(startedAt))
			require.Nil(t, charge.StoppedAt)
			require.Zero(t, charge.ChargedAmount)

			w.clock.Advance(time.Duration(tc.seconds) * time.Second)
			stoppedAt := w.now()
			running, err := w.client.GetRunningCharge(w.ctx, charge.Reference)
			require.NoError(t, err)
			require.Equal(t, tc.want, running.ChargedAmount, "what it has charged so far, before any posting")
			require.Zero(t, running.PostedAmount)
			require.True(t, running.AsOf.Equal(stoppedAt))
			require.Empty(t, w.postings(customer), "nothing is posted while it runs")

			stopped := w.stop(charge.Reference)
			require.Equal(t, billing.RunningChargeStopped, stopped.State)
			require.Equal(t, charge.ID, stopped.ID)
			require.Equal(t, tc.want, stopped.ChargedAmount)
			require.Equal(t, tc.want, stopped.PostedAmount)
			require.True(t, stopped.StoppedAt.Equal(stoppedAt))
			require.True(t, stopped.AsOf.Equal(stoppedAt))

			bal := w.balance(customer)
			require.EqualValues(t, funded-tc.want, bal.BalanceAmount, "the balance falls by exactly the charge")
			require.Zero(t, bal.AccruedAmount)
			require.Zero(t, bal.AccrualRatePerHour)
			require.Nil(t, bal.DepletesAt)
			require.EqualValues(t, funded-tc.want, bal.AvailableAmount)
			require.Zero(t, bal.OwedAmount)

			posted := w.postings(customer)
			if tc.want == 0 {
				require.Empty(t, posted, "a charge of nothing posts nothing")
			} else {
				// One posting for each 00:00 UTC it ran through, and the stop's.
				days := int(stoppedAt.Truncate(24*time.Hour).Sub(startedAt.Truncate(24*time.Hour)) / (24 * time.Hour))
				require.Len(t, posted, days+1)
				var sum int64
				for _, p := range posted {
					require.Equal(t, billing.BalanceTransactionSpend, p.Type)
					require.Equal(t, "gpu:a100", *p.Resource)
					sum += p.Amount
				}
				require.Equal(t, -tc.want, sum)
				require.Equal(t, charge.ID.UUID().String()+"/"+stoppedAt.Format("2006-01-02T15:04:05.000000Z"), posted[days].SourceID)
			}

			// Later, a repeated stop answers the same charge and posts nothing.
			w.clock.Advance(time.Hour)
			for _, again := range []billing.StopRunningChargeParams{{}, {StoppedAt: &stoppedAt}} {
				replayed, err := w.client.StopRunningCharge(w.ctx, charge.Reference, again)
				require.NoError(t, err)
				require.Equal(t, stopped, replayed)
			}
			other := stoppedAt.Add(time.Second)
			_, err = w.client.StopRunningCharge(w.ctx, charge.Reference, billing.StopRunningChargeParams{StoppedAt: &other})
			reused(t, err, "stopped_at")
			require.Len(t, w.postings(customer), len(posted))
			require.EqualValues(t, funded-tc.want, w.balance(customer).BalanceAmount)

			// The reference still names the stopped charge.
			restarted, err := w.client.StartRunningCharge(w.ctx, billing.StartRunningChargeParams{
				Reference: charge.Reference, CustomerID: customer, Currency: "USD", RatePerHour: tc.ratePerHour, Resource: "gpu:a100",
			})
			require.NoError(t, err)
			require.Equal(t, stopped, restarted)
			_, err = w.client.StartRunningCharge(w.ctx, billing.StartRunningChargeParams{
				Reference: charge.Reference, CustomerID: customer, Currency: "USD", RatePerHour: tc.ratePerHour + 1, Resource: "gpu:a100",
			})
			reused(t, err, "rate_per_hour")
		})
	}

	t.Run("the host's instants bound the charge", func(t *testing.T) {
		customer := w.funded(10_000_000)
		now := w.now()
		startedAt := now.Add(-2 * time.Hour)
		charge := w.start(customer, 3_600_000, func(p *billing.StartRunningChargeParams) { p.StartedAt = &startedAt })
		require.EqualValues(t, 7_200_000, charge.ChargedAmount, "a start in the past has charged since then")
		stoppedAt := startedAt.Add(90 * time.Minute)
		stopped, err := w.client.StopRunningCharge(w.ctx, charge.Reference, billing.StopRunningChargeParams{StoppedAt: &stoppedAt})
		require.NoError(t, err)
		require.EqualValues(t, 5_400_000, stopped.ChargedAmount)
		require.EqualValues(t, 4_600_000, w.balance(customer).BalanceAmount)

		invalid := func(err error, param string) {
			t.Helper()
			var se *billing.StatusError
			require.ErrorAs(t, err, &se)
			require.Equal(t, http.StatusBadRequest, se.Status, se.Error())
			require.Equal(t, param, *se.Param)
		}
		for name, at := range map[string]time.Time{"too far ahead": now.Add(61 * time.Second), "too far back": now.Add(-35*24*time.Hour - time.Second)} {
			_, err := w.client.StartRunningCharge(w.ctx, billing.StartRunningChargeParams{Reference: "bad-" + uuid.NewString()[:8], CustomerID: customer, Currency: "USD", RatePerHour: 1, StartedAt: &at})
			invalid(err, "started_at")
			require.NotEmpty(t, name)
		}
		ahead := w.start(customer, 3_600_000)
		before, late := now.Add(-time.Second), now.Add(61*time.Second)
		_, err = w.client.StopRunningCharge(w.ctx, ahead.Reference, billing.StopRunningChargeParams{StoppedAt: &before})
		invalid(err, "stopped_at")
		_, err = w.client.StopRunningCharge(w.ctx, ahead.Reference, billing.StopRunningChargeParams{StoppedAt: &late})
		invalid(err, "stopped_at")
		for _, bad := range []billing.StartRunningChargeParams{
			{Reference: " padded", CustomerID: customer, Currency: "USD", RatePerHour: 1},
			{Reference: "no-rate", CustomerID: customer, Currency: "USD"},
			{Reference: "too-fast", CustomerID: customer, Currency: "USD", RatePerHour: billing.MaxRunningChargeRatePerHour + 1},
			{Reference: "negative", CustomerID: customer, Currency: "USD", RatePerHour: 1, OverdraftAmount: -1},
		} {
			_, err := w.client.StartRunningCharge(w.ctx, bad)
			require.ErrorIs(t, err, billing.ErrInvalid, bad.Reference)
		}
		_, err = w.client.GetRunningCharge(w.ctx, "never-started")
		require.ErrorIs(t, err, billing.ErrNotFound)
		var se *billing.StatusError
		require.ErrorAs(t, err, &se)
		require.Equal(t, "running_charge_not_found", se.Code)

		running := billing.RunningChargeRunning
		page, err := w.client.ListRunningCharges(w.ctx, billing.RunningChargeListParams{CustomerID: customer, State: running})
		require.NoError(t, err)
		require.Len(t, page.Items, 1)
		require.Equal(t, ahead.Reference, page.Items[0].Reference)
		all, err := w.client.ListRunningCharges(w.ctx, billing.RunningChargeListParams{CustomerID: customer, PageRequest: billing.PageRequest{Limit: 1}})
		require.NoError(t, err)
		require.Len(t, all.Items, 1)
		require.Equal(t, ahead.Reference, all.Items[0].Reference, "newest first")
		require.NotEmpty(t, all.Next)
		next, err := w.client.ListRunningCharges(w.ctx, billing.RunningChargeListParams{CustomerID: customer, PageRequest: billing.PageRequest{Limit: 1, Cursor: all.Next}})
		require.NoError(t, err)
		require.Len(t, next.Items, 1)
		require.Equal(t, charge.Reference, next.Items[0].Reference)
		require.Empty(t, next.Next)
	})
}

// Mid-charge the balance is computed, not stored: what has accrued, what is
// available, the rate and the run-out equal the formula at every instant, from
// GetBalance and from GET /v1/me, and the SQL rule agrees with the Go rule.
func TestBalanceMidChargeIsComputed(t *testing.T) {
	w := newChargeWorld(t)
	const funded = 10_000_000
	customer := w.funded(funded)

	auth := &authtest.Fake{}
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, w.bill, openrails.Routes{Auth: auth}))
	token := auth.Person(customer.String())
	me := func() billing.Balance {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var account billing.CustomerAccount
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &account))
		require.Len(t, account.Balances, 1)
		return account.Balances[0]
	}
	q := w.db.GenDirectory()
	type running struct {
		rate      int64
		startedAt time.Time
	}
	var charges []running
	check := func() {
		t.Helper()
		now := w.now()
		var accrued, rate int64
		for _, c := range charges {
			total, err := money.RunningChargeTotal(c.rate, c.startedAt, now)
			require.NoError(t, err)
			inSQL, err := q.GetRunningChargeTotal(w.ctx, gen.GetRunningChargeTotalParams{RatePerHour: c.rate, StartedAt: c.startedAt, At: now})
			require.NoError(t, err)
			require.Equal(t, total, inSQL, "SQL and Go charge the same")
			accrued += total
			rate += c.rate
		}
		held, err := q.GetFinancialHeldAmount(w.ctx, gen.GetFinancialHeldAmountParams{
			MerchantID: w.client.MerchantID().UUID(), CustomerID: customer.UUID(), Currency: "USD", AsOf: now, HeldSince: now.Add(-30 * 24 * time.Hour),
		})
		require.NoError(t, err)
		require.Equal(t, accrued, held.Accrued, "the held total's accrual is the Go sum")
		require.Equal(t, accrued, held.Held)

		for name, bal := range map[string]billing.Balance{"GetBalance": *w.balance(customer), "GET /v1/me": me()} {
			require.EqualValues(t, funded, bal.BalanceAmount, name)
			require.Zero(t, bal.HeldAmount, name)
			require.Equal(t, accrued, bal.AccruedAmount, name)
			require.Equal(t, funded-accrued, bal.AvailableAmount, name)
			require.Equal(t, rate, bal.AccrualRatePerHour, name)
			require.True(t, bal.AsOf.Equal(now), name)
			if rate == 0 {
				require.Nil(t, bal.DepletesAt, name)
				continue
			}
			// The first microsecond the available balance, drawn at the rate, is gone.
			micros := (funded - accrued) * 3_600_000_000
			wait := time.Duration((micros+rate-1)/rate) * time.Microsecond
			require.NotNil(t, bal.DepletesAt, name)
			require.True(t, bal.DepletesAt.Equal(now.Add(wait)), "%s: depletes_at %s, want %s", name, bal.DepletesAt, now.Add(wait))
		}
	}

	check()
	first := w.start(customer, 3_600_000)
	charges = append(charges, running{3_600_000, first.StartedAt})
	check()
	w.clock.Advance(90 * time.Second)
	check()
	second := w.start(customer, 1_800_001)
	charges = append(charges, running{1_800_001, second.StartedAt})
	check()
	for _, step := range []time.Duration{61 * time.Second, 1799 * time.Second, 500 * time.Millisecond, 17 * time.Minute} {
		w.clock.Advance(step)
		check()
	}
	// The rule itself, where rounding and the clock's direction matter.
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		rate    int64
		elapsed time.Duration
		want    int64
	}{
		{1, 1800 * time.Second, 1}, {1, 1799 * time.Second, 0}, {3, 600 * time.Second, 1}, {3, 599 * time.Second, 0},
		{7_200_000, 1999 * time.Millisecond, 2000}, {7_200_000, -5 * time.Second, 0}, {7_200_000, 0, 0},
		{billing.MaxRunningChargeRatePerHour, 400 * 24 * time.Hour, 9_600_000_000_000_000},
	} {
		inGo, err := money.RunningChargeTotal(tc.rate, epoch, epoch.Add(tc.elapsed))
		require.NoError(t, err)
		inSQL, err := q.GetRunningChargeTotal(w.ctx, gen.GetRunningChargeTotalParams{RatePerHour: tc.rate, StartedAt: epoch, At: epoch.Add(tc.elapsed)})
		require.NoError(t, err)
		require.Equal(t, tc.want, inGo, "rate %d over %s", tc.rate, tc.elapsed)
		require.Equal(t, tc.want, inSQL, "rate %d over %s in SQL", tc.rate, tc.elapsed)
	}

	// Stopping one leaves the other's accrual and rate; stopping both leaves none.
	w.stop(first.Reference)
	bal := w.balance(customer)
	require.EqualValues(t, 1_800_001, bal.AccrualRatePerHour)
	secondSoFar, err := money.RunningChargeTotal(1_800_001, second.StartedAt, w.now())
	require.NoError(t, err)
	require.Equal(t, secondSoFar, bal.AccruedAmount)
	w.stop(second.Reference)
	bal = w.balance(customer)
	require.Zero(t, bal.AccruedAmount)
	require.Nil(t, bal.DepletesAt)
}

// An admission that fit when the charge started is refused once accrual has
// used the balance: every spend path sees what running charges have accrued.
func TestAdmissionSubtractsAccrual(t *testing.T) {
	w := newChargeWorld(t)
	customer := w.funded(1_000_000)
	admit := func(amount int64) billing.AdmissionVerdict {
		t.Helper()
		deadline := w.now().Add(time.Hour)
		v, err := w.client.Admit(w.ctx, []billing.AdmitParams{{RequestID: uuid.NewString(), CustomerID: customer, Invoker: customer.String(),
			InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EstimatedAmount: amount, ExpiresAt: &deadline}})
		require.NoError(t, err)
		return v[0]
	}
	release := func(v billing.AdmissionVerdict) {
		t.Helper()
		_, err := releaseAdmission(w.ctx, w.client, v.Admission.RequestID)
		require.NoError(t, err)
	}
	charge := w.start(customer, 3_600_000)
	fits := admit(600_000)
	require.True(t, fits.Allowed())
	release(fits)

	w.clock.Advance(500 * time.Second)
	require.EqualValues(t, 500_000, w.balance(customer).AvailableAmount)
	refused := admit(600_000)
	require.False(t, refused.Allowed())
	require.Equal(t, billing.AdmissionBlockedByMoney, *refused.Admission.BlockedBy)
	rest := admit(500_000)
	require.True(t, rest.Allowed())
	bal := w.balance(customer)
	require.EqualValues(t, 500_000, bal.HeldAmount)
	require.EqualValues(t, 500_000, bal.AccruedAmount)
	require.Zero(t, bal.AvailableAmount)
	require.False(t, admit(1).Allowed(), "the hold and the accrual are both taken")
	release(rest)

	// Once stopped, the charge is spent rather than held.
	w.stop(charge.Reference)
	require.False(t, admit(500_001).Allowed())
	require.True(t, admit(500_000).Allowed())
}

// Accrual up to a credit lot's expiry draws that lot; the rest of it expires.
// The run-out counted the lapse from the start.
func TestRunningChargeDrawsALotBeforeItLapses(t *testing.T) {
	for name, settle := range map[string]func(w *chargeWorld){
		"the expiry sweep settles the lot": func(w *chargeWorld) { w.expireCredits() },
		"a stop settles it first":          nil,
	} {
		t.Run(name, func(t *testing.T) {
			w := newChargeWorld(t)
			customer := billing.CustomerID(uuid.New())
			t0 := w.now()
			lapses := t0.Add(1000 * time.Second)
			lapsing := w.fund(customer, 2_000_000, &lapses)
			lasting := w.fund(customer, 5_000_000, nil)
			charge := w.start(customer, 3_600_000) // 1000 a second

			// 1,000,000 of the lapsing lot is drawn by the time it lapses and the
			// other 1,000,000 is lost; the lasting lot then runs for 5000 seconds.
			runsOut := t0.Add(6000 * time.Second)
			require.True(t, w.balance(customer).DepletesAt.Equal(runsOut), "the run-out counts the lapse: %s", w.balance(customer).DepletesAt)

			w.clock.Advance(1500 * time.Second)
			bal := w.balance(customer)
			require.EqualValues(t, 1_500_000, bal.AccruedAmount)
			require.True(t, bal.DepletesAt.Equal(runsOut), "unchanged before the sweep: %s", bal.DepletesAt)
			require.Empty(t, w.postings(customer))

			if settle != nil {
				settle(w)
				posted := w.postings(customer)
				require.Len(t, posted, 1)
				require.EqualValues(t, -1_000_000, posted[0].Amount)
				require.Equal(t, lapsing.ID, *posted[0].CreditGrantID, "what accrued while the lot lived draws it")
				require.Equal(t, charge.ID.UUID().String()+"/"+lapses.Format("2006-01-02T15:04:05.000000Z"), posted[0].SourceID)
				bal = w.balance(customer)
				require.EqualValues(t, 5_000_000, bal.BalanceAmount, "the rest of the lot expired")
				require.EqualValues(t, 500_000, bal.AccruedAmount)
				require.EqualValues(t, 4_500_000, bal.AvailableAmount)
				require.True(t, bal.DepletesAt.Equal(runsOut), "and after it: %s", bal.DepletesAt)
				running, err := w.client.GetRunningCharge(w.ctx, charge.Reference)
				require.NoError(t, err)
				require.EqualValues(t, 1_000_000, running.PostedAmount)
				require.EqualValues(t, 1_500_000, running.ChargedAmount)
			}

			w.clock.Advance(500 * time.Second)
			stopped := w.stop(charge.Reference)
			require.EqualValues(t, 2_000_000, stopped.ChargedAmount)
			posted := w.postings(customer)
			require.Len(t, posted, 2, "one posting at the lapse and one at the stop")
			require.EqualValues(t, -1_000_000, posted[0].Amount)
			require.Equal(t, lapsing.ID, *posted[0].CreditGrantID)
			require.EqualValues(t, -1_000_000, posted[1].Amount)
			require.Equal(t, lasting.ID, *posted[1].CreditGrantID)

			bal = w.balance(customer)
			require.EqualValues(t, 4_000_000, bal.BalanceAmount)
			require.Zero(t, bal.OwedAmount)
			page, err := w.client.ListBalanceTransactions(w.ctx, customer, billing.BalanceTransactionListParams{Currency: "USD"})
			require.NoError(t, err)
			var expired int64
			for _, tx := range page.Items {
				if tx.Type == billing.BalanceTransactionExpire {
					require.Equal(t, lapsing.ID, *tx.CreditGrantID)
					expired += tx.Amount
				}
			}
			require.EqualValues(t, -1_000_000, expired, "the lot's unspent rest expired, once")
		})
	}
}

// A host commits its own rows with a start or a stop in one transaction: a
// rollback leaves neither, a commit both, and a replay after the commit
// answers the same charge.
func TestRunningChargeTxCommitsWithHostRows(t *testing.T) {
	w := newChargeWorld(t)
	customer := w.funded(10_000_000)
	rentals := pgx.Identifier{w.f.schema, "host_rentals"}.Sanitize()
	_, err := w.f.pool.Exec(t.Context(), "CREATE TABLE "+rentals+" (reference text PRIMARY KEY, state text NOT NULL)")
	require.NoError(t, err)
	state := func(reference string) string {
		t.Helper()
		var s string
		err := w.f.pool.QueryRow(t.Context(), "SELECT state FROM "+rentals+" WHERE reference = $1", reference).Scan(&s)
		if err == pgx.ErrNoRows {
			return ""
		}
		require.NoError(t, err)
		return s
	}
	params := billing.StartRunningChargeParams{Reference: "rental-tx", CustomerID: customer, Currency: "USD", RatePerHour: 3_600_000}
	ready := func(commit bool) *billing.RunningCharge {
		t.Helper()
		tx, err := w.f.pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(context.Background()) }()
		_, err = tx.Exec(t.Context(), "INSERT INTO "+rentals+" (reference, state) VALUES ($1, 'ready') ON CONFLICT (reference) DO NOTHING", params.Reference)
		require.NoError(t, err)
		charge, err := w.client.StartRunningChargeTx(w.ctx, tx, params)
		require.NoError(t, err)
		if commit {
			require.NoError(t, tx.Commit(t.Context()))
		}
		return charge
	}

	rolledBack := ready(false)
	require.Equal(t, billing.RunningChargeRunning, rolledBack.State)
	require.Empty(t, state(params.Reference))
	_, err = w.client.GetRunningCharge(w.ctx, params.Reference)
	require.ErrorIs(t, err, billing.ErrNotFound, "a rolled-back start leaves no charge")
	require.Zero(t, w.balance(customer).AccrualRatePerHour)

	committed := ready(true)
	require.Equal(t, "ready", state(params.Reference))
	stored, err := w.client.GetRunningCharge(w.ctx, params.Reference)
	require.NoError(t, err)
	require.Equal(t, committed.ID, stored.ID)
	require.NotEqual(t, rolledBack.ID, committed.ID)

	w.clock.Advance(100 * time.Second)
	replayed := ready(true)
	require.Equal(t, committed.ID, replayed.ID, "a replay after the commit answers the same charge")
	require.True(t, replayed.StartedAt.Equal(committed.StartedAt))
	require.EqualValues(t, 100_000, replayed.ChargedAmount)
	viaRoute, err := w.client.StartRunningCharge(w.ctx, params)
	require.NoError(t, err)
	require.Equal(t, committed.ID, viaRoute.ID)

	release := func(commit bool) *billing.RunningCharge {
		t.Helper()
		tx, err := w.f.pool.Begin(t.Context())
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(context.Background()) }()
		_, err = tx.Exec(t.Context(), "UPDATE "+rentals+" SET state = 'released' WHERE reference = $1", params.Reference)
		require.NoError(t, err)
		charge, err := w.client.StopRunningChargeTx(w.ctx, tx, params.Reference, billing.StopRunningChargeParams{})
		require.NoError(t, err)
		if commit {
			require.NoError(t, tx.Commit(t.Context()))
		}
		return charge
	}
	require.Equal(t, billing.RunningChargeStopped, release(false).State)
	require.Equal(t, "ready", state(params.Reference))
	require.EqualValues(t, 10_000_000, w.balance(customer).BalanceAmount, "a rolled-back stop posts nothing")
	stored, err = w.client.GetRunningCharge(w.ctx, params.Reference)
	require.NoError(t, err)
	require.Equal(t, billing.RunningChargeRunning, stored.State)

	stopped := release(true)
	require.Equal(t, "released", state(params.Reference))
	require.EqualValues(t, 100_000, stopped.ChargedAmount)
	require.EqualValues(t, 9_900_000, w.balance(customer).BalanceAmount)
	require.Equal(t, stopped, release(true), "a replayed stop answers the same")
	require.Len(t, w.postings(customer), 1)

	// A remote handle has no host transaction to join.
	remote, err := openrails.NewRemoteMerchant("https://openrails.invalid", openrails.RemoteCredential{APIKey: "key"})
	require.NoError(t, err)
	_, err = remote.StartRunningChargeTx(w.ctx, nil, params)
	require.ErrorIs(t, err, openrails.ErrRemoteClient)
	_, err = remote.StopRunningChargeTx(w.ctx, nil, params.Reference, billing.StopRunningChargeParams{})
	require.ErrorIs(t, err, openrails.ErrRemoteClient)
}

// A start that names a minimum is refused unless the customer's capacity
// covers it, and a refusal writes nothing. No minimum never refuses.
func TestStartBelowMinimumIsRefused(t *testing.T) {
	w := newChargeWorld(t)
	customer := w.funded(1_000_000)
	start := func(reference string, minimum, overdraft int64) (*billing.RunningCharge, error) {
		return w.client.StartRunningCharge(w.ctx, billing.StartRunningChargeParams{
			Reference: reference, CustomerID: customer, Currency: "USD", RatePerHour: 3_600_000, MinimumAmount: minimum, OverdraftAmount: overdraft,
		})
	}
	rows := func() (n int) {
		t.Helper()
		for _, table := range []string{"running_charges", "balance_watches"} {
			var count int
			require.NoError(t, w.f.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{w.f.schema, table}.Sanitize()).Scan(&count))
			n += count
		}
		return n
	}

	_, err := start("min-over", 1_000_001, 0)
	require.ErrorIs(t, err, billing.ErrInsufficientCredits)
	var se *billing.StatusError
	require.ErrorAs(t, err, &se)
	require.Equal(t, http.StatusPaymentRequired, se.Status)
	_, err = start("min-over-overdraft", 3_000_001, 2_000_000)
	require.ErrorIs(t, err, billing.ErrInsufficientCredits, "the overdraft reaches 2 USD below zero, no further")
	require.Zero(t, rows(), "a refusal writes nothing")
	_, err = w.client.GetRunningCharge(w.ctx, "min-over")
	require.ErrorIs(t, err, billing.ErrNotFound)

	exact, err := start("min-exact", 1_000_000, 0)
	require.NoError(t, err)
	require.Equal(t, 2, rows(), "the charge and its watch")
	w.clock.Advance(400 * time.Second)
	_, err = start("min-second", 600_001, 0)
	require.ErrorIs(t, err, billing.ErrInsufficientCredits, "what the first charge accrued is taken")
	// The smallest overdraft among the customer's running charges applies.
	_, err = start("min-second", 600_001, 2_000_000)
	require.ErrorIs(t, err, billing.ErrInsufficientCredits)
	_, err = start("min-second", 600_000, 2_000_000)
	require.NoError(t, err)
	replayed, err := start("min-exact", 5_000_000, 0)
	require.NoError(t, err, "a replay is not gated again")
	require.Equal(t, exact.ID, replayed.ID)

	// With the overdraft on every running charge, capacity reaches below zero.
	overdrawn := w.funded(1_000_000)
	customer = overdrawn
	_, err = start("od-first", 3_000_000, 2_000_000)
	require.NoError(t, err)
	w.clock.Advance(1500 * time.Second)
	bal := w.balance(overdrawn)
	require.EqualValues(t, -500_000, bal.AvailableAmount)
	_, err = start("od-over", 1_500_001, 2_000_000)
	require.ErrorIs(t, err, billing.ErrInsufficientCredits)
	_, err = start("od-fit", 1_500_000, 2_000_000)
	require.NoError(t, err)

	// A customer never funded: no minimum never refuses, and a start gives it
	// its balance account.
	fresh := billing.CustomerID(uuid.New())
	customer = fresh
	_, err = start("fresh-min", 1, 0)
	require.ErrorIs(t, err, billing.ErrInsufficientCredits)
	delivered, err := start("fresh", 0, 0)
	require.NoError(t, err)
	w.clock.Advance(10 * time.Second)
	stopped := w.stop(delivered.Reference)
	require.EqualValues(t, 10_000, stopped.ChargedAmount)
	bal = w.balance(fresh)
	require.Zero(t, bal.BalanceAmount)
	require.EqualValues(t, 10_000, bal.OwedAmount, "what was delivered past the balance is owed")
}

// Between a start and a stop, with no other activity and no day boundary,
// watcher passes and reads change no row of any OpenRails table.
func TestRunningChargeWritesNothingWhileItRuns(t *testing.T) {
	w := newChargeWorld(t)
	customer := w.funded(100_000_000)
	charge := w.start(customer, 3_600_000, func(p *billing.StartRunningChargeParams) { p.LowBalanceAmount = 1_000_000 })
	other := w.start(w.funded(50_000_000), 1_800_000)
	before := w.tables()

	for range 10 {
		w.clock.Advance(7 * time.Minute)
		w.watch()
		bal := w.balance(customer)
		require.Positive(t, bal.AccruedAmount)
		running, err := w.client.GetRunningCharge(w.ctx, charge.Reference)
		require.NoError(t, err)
		require.Equal(t, bal.AccruedAmount, running.ChargedAmount)
		_, err = w.client.ListRunningCharges(w.ctx, billing.RunningChargeListParams{State: billing.RunningChargeRunning})
		require.NoError(t, err)
		require.Empty(t, w.depleted(customer))
		_, err = w.client.GetCustomer(w.ctx, customer)
		require.NoError(t, err)
	}
	require.EqualValues(t, 4_200_000, w.balance(customer).AccruedAmount, "seventy minutes accrued")
	require.Equal(t, before, w.tables(), "nothing was written while the charges ran")

	w.stop(charge.Reference)
	w.stop(other.Reference)
	after := w.tables()
	require.NotEqual(t, before["running_charges"], after["running_charges"])
	require.NotEqual(t, before["ledger_transfers"], after["ledger_transfers"])
	require.NotEqual(t, before["balance_watches"], after["balance_watches"])
}

// A deposit moves the run-out later at once. The watch is marked due, and the
// next look re-arms the notices without sending one.
func TestDepositMovesRunOut(t *testing.T) {
	w := newChargeWorld(t)
	customer := w.funded(1_000_000)
	t0 := w.now()
	w.start(customer, 3_600_000, func(p *billing.StartRunningChargeParams) { p.LowBalanceAmount = 500_000 })
	require.True(t, w.balance(customer).DepletesAt.Equal(t0.Add(1000*time.Second)))
	watch := w.watched(customer)
	require.True(t, watch.DepletesAt.Equal(t0.Add(1000*time.Second)))
	require.True(t, watch.NextCheckAt.After(t0), "the low line comes first: %s", watch.NextCheckAt)
	require.True(t, watch.NextCheckAt.Before(t0.Add(1000*time.Second)))

	w.clock.Advance(1200 * time.Second)
	w.watch()
	require.Len(t, w.events(billing.HostEventBalanceLow, customer), 1)
	require.Len(t, w.events(billing.HostEventBalanceDepleted, customer), 1)
	watch = w.watched(customer)
	require.NotNil(t, watch.LowNotifiedAt)
	require.NotNil(t, watch.DepletedAt)
	require.True(t, watch.NextCheckAt.After(w.now()), "nothing to look at until something changes or the day ends")

	// 1.2 of the 1 USD is accrued; 10 USD more leaves 9.8 available: 9800 seconds.
	w.fund(customer, 10_000_000, nil)
	runsOut := w.now().Add(9800 * time.Second)
	require.True(t, w.balance(customer).DepletesAt.Equal(runsOut), "moved by the deposit, before any look: %s", w.balance(customer).DepletesAt)
	watch = w.watched(customer)
	require.False(t, watch.NextCheckAt.After(w.now()), "the deposit marks the watch due")
	require.NotNil(t, watch.DepletedAt, "until the watcher looks")

	w.watch()
	watch = w.watched(customer)
	require.Nil(t, watch.LowNotifiedAt, "re-armed")
	require.Nil(t, watch.DepletedAt, "re-armed")
	require.True(t, watch.DepletesAt.Equal(runsOut))
	require.True(t, watch.NextCheckAt.After(w.now()))
	require.Len(t, w.events(billing.HostEventBalanceLow, customer), 1, "re-arming sends nothing")
	require.Len(t, w.events(billing.HostEventBalanceDepleted, customer), 1)
}

// An admitted request's hold and its capture move the run-out earlier at
// once, and each leaves the watch due.
func TestCaptureMovesRunOut(t *testing.T) {
	w := newChargeWorld(t)
	customer := w.funded(10_000_000)
	t0 := w.now()
	w.start(customer, 3_600_000)
	require.True(t, w.balance(customer).DepletesAt.Equal(t0.Add(10_000*time.Second)))
	require.True(t, w.watched(customer).NextCheckAt.After(t0))

	requestID := uuid.NewString()
	deadline := t0.Add(time.Hour)
	verdicts, err := w.client.Admit(w.ctx, []billing.AdmitParams{{RequestID: requestID, CustomerID: customer, Invoker: customer.String(),
		InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EstimatedAmount: 2_000_000, ExpiresAt: &deadline}})
	require.NoError(t, err)
	require.True(t, verdicts[0].Allowed())
	require.True(t, w.balance(customer).DepletesAt.Equal(t0.Add(8000*time.Second)), "the hold is not there to draw")
	require.False(t, w.watched(customer).NextCheckAt.After(t0), "the admission marks the watch due")
	w.watch()
	watch := w.watched(customer)
	require.True(t, watch.DepletesAt.Equal(t0.Add(8000*time.Second)))
	require.True(t, watch.NextCheckAt.After(t0))

	_, err = w.client.CaptureAdmission(w.ctx, requestID, billing.CaptureAdmissionParams{Amount: 1_500_000})
	require.NoError(t, err)
	require.True(t, w.balance(customer).DepletesAt.Equal(t0.Add(8500*time.Second)), "1.5 USD is spent; the rest of the hold is free")
	require.False(t, w.watched(customer).NextCheckAt.After(t0), "the capture marks the watch due at once")
	require.True(t, w.watched(customer).DepletesAt.Equal(t0.Add(8000*time.Second)), "the stored instant waits for the look")
	w.watch()
	require.True(t, w.watched(customer).DepletesAt.Equal(t0.Add(8500*time.Second)))
	require.Empty(t, w.events(billing.HostEventBalanceDepleted, customer))
}

// Two charges of one customer run its capacity out: one balance.depleted, the
// depleted list names both, another pass adds nothing, a deposit clears it and
// the next crossing fires again. Without the watcher the list still names
// them once the stored instant has passed.
func TestRunOutFiresOnceAndListsBothCharges(t *testing.T) {
	w := newChargeWorld(t)
	customer := w.funded(2_000_000)
	t0 := w.now()
	first := w.start(customer, 3_600_000, func(p *billing.StartRunningChargeParams) { p.Reference = "first-" + uuid.NewString()[:8] })
	second := w.start(customer, 3_600_000, func(p *billing.StartRunningChargeParams) { p.Reference = "second-" + uuid.NewString()[:8] })
	both := []string{first.Reference, second.Reference}
	require.True(t, w.watched(customer).DepletesAt.Equal(t0.Add(1000*time.Second)), "2000 a second from 2 USD")

	w.clock.Advance(999 * time.Second)
	w.watch()
	require.Empty(t, w.depleted(customer))
	require.Empty(t, w.events(billing.HostEventBalanceDepleted, customer))

	w.clock.Advance(time.Second)
	at := w.now()
	w.watch()
	events := w.events(billing.HostEventBalanceDepleted, customer)
	require.Equal(t, []billing.BalanceHostEvent{{
		CustomerID: customer, Currency: "USD", AvailableAmount: 0, OwedAmount: 0, AccrualRatePerHour: 7_200_000, AsOf: at, DepletesAt: at,
	}}, events)
	require.Equal(t, both, w.depleted(customer))
	require.Zero(t, w.balance(customer).AvailableAmount, "the host confirms with the balance")

	w.clock.Advance(40 * time.Second)
	w.watch()
	w.watch()
	require.Len(t, w.events(billing.HostEventBalanceDepleted, customer), 1, "once per crossing")
	require.Equal(t, both, w.depleted(customer))
	require.Empty(t, w.events(billing.HostEventBalanceLow, customer), "no low line was asked for")

	// 80,000 is overshoot, accrued past the line; a deposit clears the state.
	w.fund(customer, 5_080_000, nil)
	w.watch()
	require.Empty(t, w.depleted(customer))
	require.Nil(t, w.watched(customer).DepletedAt)
	require.Len(t, w.events(billing.HostEventBalanceDepleted, customer), 1)

	w.clock.Advance(2500 * time.Second)
	w.watch()
	require.Len(t, w.events(billing.HostEventBalanceDepleted, customer), 2, "the next crossing fires again")
	require.Equal(t, both, w.depleted(customer))

	// Stopping what the charges paid for ends the watch.
	w.stop(first.Reference)
	require.Equal(t, []string{second.Reference}, w.depleted(customer))
	w.stop(second.Reference)
	require.Empty(t, w.depleted(customer))
	require.Nil(t, w.watched(customer))
	w.watch()
	require.Len(t, w.events(billing.HostEventBalanceDepleted, customer), 2)

	t.Run("without the watcher", func(t *testing.T) {
		unwatched := w.funded(2_000_000)
		a := w.start(unwatched, 3_600_000, func(p *billing.StartRunningChargeParams) { p.Reference = "a-" + uuid.NewString()[:8] })
		b := w.start(unwatched, 3_600_000, func(p *billing.StartRunningChargeParams) { p.Reference = "b-" + uuid.NewString()[:8] })
		w.clock.Advance(999 * time.Second)
		require.Empty(t, w.depleted(unwatched))
		w.clock.Advance(time.Second)
		require.Equal(t, []string{a.Reference, b.Reference}, w.depleted(unwatched), "the stored instant has passed")
		require.Empty(t, w.events(billing.HostEventBalanceDepleted, unwatched))
		all, err := w.client.ListRunningCharges(w.ctx, billing.RunningChargeListParams{Depleted: true})
		require.NoError(t, err)
		require.Len(t, all.Items, 2, "only the depleted customer's charges")
		stoppedOnly, err := w.client.ListRunningCharges(w.ctx, billing.RunningChargeListParams{Depleted: true, State: billing.RunningChargeStopped})
		require.NoError(t, err)
		require.Empty(t, stoppedOnly.Items)
	})
}

// A balance under its low line sends balance.low once per crossing: not
// before the line, not again while it stays under, and again after it has
// been back above.
func TestLowBalanceFiresOncePerCrossing(t *testing.T) {
	w := newChargeWorld(t)
	customer := w.funded(2_000_000)
	t0 := w.now()
	// Among a customer's charges the largest low line applies.
	w.start(customer, 1_800_000, func(p *billing.StartRunningChargeParams) { p.LowBalanceAmount = 1_000_000 })
	w.start(customer, 1_800_000, func(p *billing.StartRunningChargeParams) { p.LowBalanceAmount = 250_000 })
	watch := w.watched(customer)
	require.True(t, watch.DepletesAt.Equal(t0.Add(2000*time.Second)))
	require.True(t, watch.NextCheckAt.After(t0.Add(1000*time.Second)) && watch.NextCheckAt.Before(t0.Add(1001*time.Second)),
		"the first instant under 1 USD: %s", watch.NextCheckAt)

	w.clock.Advance(1000 * time.Second)
	w.watch()
	require.Empty(t, w.events(billing.HostEventBalanceLow, customer), "at the line is not under it")

	w.clock.Advance(time.Second)
	at := w.now()
	w.watch()
	require.Equal(t, []billing.BalanceHostEvent{{
		CustomerID: customer, Currency: "USD", AvailableAmount: 999_000, OwedAmount: 0, AccrualRatePerHour: 3_600_000, AsOf: at, DepletesAt: t0.Add(2000 * time.Second),
	}}, w.events(billing.HostEventBalanceLow, customer))
	w.clock.Advance(100 * time.Second)
	w.watch()
	require.Len(t, w.events(billing.HostEventBalanceLow, customer), 1, "once while it stays under")
	require.Empty(t, w.events(billing.HostEventBalanceDepleted, customer))

	w.fund(customer, 500_000, nil)
	w.watch()
	require.Nil(t, w.watched(customer).LowNotifiedAt, "back above the line: re-armed")
	require.Len(t, w.events(billing.HostEventBalanceLow, customer), 1)

	w.clock.Advance(400 * time.Second)
	w.watch()
	require.Len(t, w.events(billing.HostEventBalanceLow, customer), 2, "the next crossing")
}

// A charge that runs through three midnights posts once through each, exactly
// at 00:00 UTC whenever the watcher gets to it, and once at the stop. The
// postings sum to rate x seconds / 3600.
func TestDailyCheckpointPostsThroughMidnight(t *testing.T) {
	w := newChargeWorld(t)
	const funded, rate = 1_000_000_000, 2_500_000
	customer := w.funded(funded)
	t0 := w.now() // 12:00 UTC
	charge := w.start(customer, rate)
	midnight := t0.Truncate(24 * time.Hour).Add(24 * time.Hour)
	require.True(t, w.watched(customer).NextCheckAt.Equal(midnight), "the day's end comes before the run-out")
	total := func(at time.Time) int64 {
		t.Helper()
		amount, err := money.RunningChargeTotal(rate, t0, at)
		require.NoError(t, err)
		return amount
	}
	posting := func(p billing.BalanceTransaction, through time.Time, amount int64) {
		t.Helper()
		require.Equal(t, charge.ID.UUID().String()+"/"+through.Format("2006-01-02T15:04:05.000000Z"), p.SourceID)
		require.Equal(t, -amount, p.Amount)
	}

	// The watcher is a little late; the posting is through the boundary exactly.
	w.clock.Advance(12*time.Hour + 17*time.Second)
	w.watch()
	posted := w.postings(customer)
	require.Len(t, posted, 1)
	posting(posted[0], midnight, total(midnight))
	require.EqualValues(t, 30_000_000, -posted[0].Amount, "twelve hours")
	bal := w.balance(customer)
	require.EqualValues(t, funded-30_000_000, bal.BalanceAmount)
	require.Equal(t, total(w.now())-30_000_000, bal.AccruedAmount, "the seventeen seconds since")
	require.True(t, w.watched(customer).NextCheckAt.Equal(midnight.Add(24*time.Hour)))
	w.watch()
	require.Len(t, w.postings(customer), 1, "once per boundary")

	// It misses a day: the next look posts each boundary on its own.
	w.clock.Advance(48 * time.Hour)
	w.watch()
	posted = w.postings(customer)
	require.Len(t, posted, 3)
	posting(posted[1], midnight.Add(24*time.Hour), 60_000_000)
	posting(posted[2], midnight.Add(48*time.Hour), 60_000_000)

	w.clock.Advance(12*time.Hour - 10*time.Second)
	stopped := w.stop(charge.Reference)
	require.True(t, stopped.StoppedAt.Equal(t0.Add(3*24*time.Hour+7*time.Second)))
	require.EqualValues(t, 180_004_861, stopped.ChargedAmount)
	posted = w.postings(customer)
	require.Len(t, posted, 4, "one line per day and the stop")
	posting(posted[3], *stopped.StoppedAt, 180_004_861-150_000_000)
	var sum int64
	for _, p := range posted {
		sum += p.Amount
	}
	require.EqualValues(t, -180_004_861, sum, "the postings sum to the charge")
	require.EqualValues(t, funded-180_004_861, w.balance(customer).BalanceAmount)
	require.Nil(t, w.watched(customer))

	t.Run("a start in the past is due the day it already crossed", func(t *testing.T) {
		late := w.funded(funded)
		today := w.now().Truncate(24 * time.Hour)
		startedAt := today.Add(-time.Hour)
		back := w.start(late, 3_600_000, func(p *billing.StartRunningChargeParams) { p.StartedAt = &startedAt })
		require.False(t, w.watched(late).NextCheckAt.After(w.now()))
		w.watch()
		posted := w.postings(late)
		require.Len(t, posted, 1)
		require.Equal(t, back.ID.UUID().String()+"/"+today.Format("2006-01-02T15:04:05.000000Z"), posted[0].SourceID)
		require.EqualValues(t, -3_600_000, posted[0].Amount, "the hour before midnight")
		require.True(t, w.watched(late).NextCheckAt.Equal(today.Add(24*time.Hour)))
	})
}

// Started, the engine's own River runs the watcher: a customer already past
// its balance is told without anything calling the worker.
func TestBalanceWatcherRunsOnTheEnginesRiver(t *testing.T) {
	f := newFixture(t)
	bill, client := f.runtimeWithStripe(t, "watcher-"+uuid.NewString()[:8], nil)
	ctx := merchant.WithID(t.Context(), client.MerchantID())
	customer := billing.CustomerID(uuid.New())
	startedAt := time.Now().UTC().Add(-time.Hour)
	_, err := client.StartRunningCharge(ctx, billing.StartRunningChargeParams{
		Reference: "already-delivered", CustomerID: customer, Currency: "USD", RatePerHour: 3_600_000, StartedAt: &startedAt,
	})
	require.NoError(t, err)

	require.NoError(t, bill.Start(t.Context()))
	require.Eventually(t, func() bool {
		page, err := client.ListHostEvents(ctx, billing.HostEventListParams{Type: billing.HostEventBalanceDepleted})
		return err == nil && len(page.Items) == 1 && page.Items[0].Balance.CustomerID == customer
	}, 45*time.Second, 200*time.Millisecond, "the pass at start, or the next one 30 seconds on")
	page, err := client.ListRunningCharges(ctx, billing.RunningChargeListParams{Depleted: true})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
}
