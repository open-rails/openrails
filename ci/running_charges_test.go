//go:build e2e && integration

package ci_test

import (
	"context"
	"encoding/json"
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
	client *openrails.Client
	db     *db.DB
}

func newChargeWorld(t *testing.T) *chargeWorld {
	t.Helper()
	f := newFixture(t)
	// Mid-day, so no test crosses 00:00 UTC unless it means to.
	clock := clockwork.NewFakeClockAt(time.Now().UTC().Truncate(24 * time.Hour).Add(36 * time.Hour))
	cfg := f.config()
	cfg.Merchant = openrails.MerchantDeclaration{Slug: "charges-" + uuid.NewString()[:8], DisplayName: "Charges"}
	client, err := openrails.New(t.Context(), cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool, Clock: clock})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close(context.Background())) })
	database, err := db.NewWithPGXPool(f.pool, f.schema)
	require.NoError(t, err)
	return &chargeWorld{t: t, f: f, ctx: merchant.WithID(t.Context(), client.MerchantID()), clock: clock, client: client, db: database}
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
				require.Len(t, posted, 1, "one posting")
				require.Equal(t, billing.BalanceTransactionSpend, posted[0].Type)
				require.Equal(t, -tc.want, posted[0].Amount)
				require.Equal(t, charge.ID.UUID().String()+"/"+stoppedAt.Format("2006-01-02T15:04:05.000000Z"), posted[0].SourceID)
				require.Equal(t, "gpu:a100", *posted[0].Resource)
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
	require.NoError(t, openrailshttp.Mount(mux, w.client, openrails.Routes{Auth: auth}))
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

	// A remote client has no host transaction to join.
	remote, err := openrails.NewRemote("https://openrails.invalid", openrails.WithMerchantID(w.client.MerchantID()), openrails.WithAPIKey("key"))
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
