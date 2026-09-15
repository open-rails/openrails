//go:build integration

package spendgate_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
)

type fixture struct {
	d      *db.DB
	money  *money.MoneyService
	gate   *spendgate.Gate
	clock  *clockwork.FakeClock
	ctx    context.Context
	payer  identity.CustomerID
	policy spendgate.Policy
}

func newFixture(t *testing.T, balance int64) *fixture {
	t.Helper()
	d := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	ctx := dbtest.WithTestMerchant(context.Background())
	dbtest.EnsureTestMerchant(ctx, t, d.Pool())
	clk := clockwork.NewFakeClockAt(time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC))
	f := &fixture{d: d, ctx: ctx, payer: identity.CustomerID(uuid.New()), clock: clk}
	f.money = money.NewMoneyService(d, clk)
	f.gate = spendgate.New(d)
	f.gate.SetClock(clk.Now)
	f.policy = spendgate.Policy{Scopes: []spendgate.ScopedWindows{{Scope: spendgate.ScopePayer, Windows: []spendgate.Window{{Scope: spendgate.ScopePayer, Key: "5h", Duration: 5 * time.Hour, Limit: 600}}}}}
	usage, err := f.gate.WindowUsage(ctx, f.payer.UUID(), "USD", f.policy, spendgate.Request{})
	require.NoError(t, err)
	clk.Advance(usage[0].ResetsAt.Add(-time.Hour).Sub(clk.Now()))
	if balance > 0 {
		_, err = f.money.Deposit(ctx, money.DepositParams{CustomerID: &f.payer, Currency: "USD", Amount: balance, Source: "test"})
		require.NoError(t, err)
	}
	return f
}

func (f *fixture) input(id string, amount int64) spendgate.AdmitInput {
	return spendgate.AdmitInput{Customer: f.payer.UUID(), Currency: "USD", RequestID: id, Cost: amount,
		ExpiresAt: f.clock.Now().Add(time.Second), Policy: f.policy,
		Terms: spendgate.Terms{Invoker: "original", InvokerType: "payer", Roles: []string{"a", "b"}, Resource: "model", Source: "test"}}
}

func (f *fixture) admit(in spendgate.AdmitInput) (spendgate.Decision, error) {
	var decision spendgate.Decision
	err := f.money.WithLockedAdmissionCapacity(f.ctx, f.payer, in.Currency, func(ctx context.Context, d *db.DB, capacity money.AdmissionCapacity) error {
		in.AccountBalance = capacity.Balance - capacity.Held
		var err error
		decision, err = f.gate.Admit(ctx, d.Gen(ctx), in)
		return err
	})
	return decision, err
}

func (f *fixture) used(t *testing.T) int64 {
	t.Helper()
	usage, err := f.gate.WindowUsage(f.ctx, f.payer.UUID(), "USD", f.policy, spendgate.Request{})
	require.NoError(t, err)
	return usage[0].Used
}

func TestDurableAdmissionIdentityCaptureAndWindows(t *testing.T) {
	f := newFixture(t, 1000)
	in := f.input("first", 200)
	decision, err := f.admit(in)
	require.NoError(t, err)
	require.True(t, decision.Allowed)
	for _, change := range []func(*spendgate.AdmitInput){
		func(v *spendgate.AdmitInput) { v.Customer = uuid.New() },
		func(v *spendgate.AdmitInput) { v.Cost++ },
		func(v *spendgate.AdmitInput) { v.Currency = "EUR" },
		func(v *spendgate.AdmitInput) { v.ExpiresAt = time.Time{} },
		func(v *spendgate.AdmitInput) { v.Terms.Invoker = "other" },
		func(v *spendgate.AdmitInput) { v.Terms.Resource = "other" },
		func(v *spendgate.AdmitInput) { v.Terms.AccrualRateDeltaPerHour = 1 },
	} {
		changed := in
		change(&changed)
		_, err := f.admit(changed)
		var conflict *spendgate.Conflict
		require.ErrorAs(t, err, &conflict)
	}
	in.Terms.Roles = []string{"b", "a", "a"}
	decision, err = f.admit(in)
	require.NoError(t, err)
	require.True(t, decision.Replayed)
	balance, err := f.money.GetBalanceForCustomer(f.ctx, f.payer, "USD")
	require.NoError(t, err)
	require.EqualValues(t, 200, balance.HeldBalance)

	// A new gate/process needs no Redis identity or counters to settle and enforce.
	f.gate = spendgate.New(f.d)
	f.gate.SetClock(f.clock.Now)
	captured, err := f.money.CaptureAdmission(f.ctx, "first", 500)
	require.NoError(t, err)
	require.NotNil(t, captured.LedgerTransferID)
	require.EqualValues(t, 500, f.used(t))
	replay, err := f.money.CaptureAdmission(f.ctx, "first", 500)
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	require.Equal(t, captured.LedgerTransferID, replay.LedgerTransferID)
	_, err = f.money.CaptureAdmission(f.ctx, "first", 501)
	require.ErrorIs(t, err, money.ErrIdempotencyKeyReused)
	decision, err = f.admit(f.input("too-large", 101))
	require.NoError(t, err)
	require.False(t, decision.Allowed)

	decision, err = f.admit(f.input("late", 100))
	require.NoError(t, err)
	require.True(t, decision.Allowed)
	f.clock.Advance(2 * time.Second)
	balance, err = f.money.GetBalanceForCustomer(f.ctx, f.payer, "USD")
	require.NoError(t, err)
	require.Zero(t, balance.HeldBalance)
	require.EqualValues(t, 600, f.used(t), "expiry does not erase unreported work risk")
	require.NoError(t, f.gate.Release(f.ctx, "late"))
	require.EqualValues(t, 500, f.used(t))
	_, err = f.money.CaptureAdmission(f.ctx, "late", 50)
	require.NoError(t, err)
	require.EqualValues(t, 550, f.used(t))
	row, err := f.gate.Get(f.ctx, "late")
	require.NoError(t, err)
	require.NotNil(t, row.ReleasedAt)
	require.Equal(t, "captured", row.State)
	require.ErrorIs(t, f.gate.Release(f.ctx, "late"), spendgate.ErrCaptured)

	zero := f.input("zero-estimate", 0)
	zero.ExpiresAt = time.Time{}
	decision, err = f.admit(zero)
	require.NoError(t, err)
	require.True(t, decision.Allowed)
	_, err = f.money.CaptureAdmission(f.ctx, zero.RequestID, 20)
	require.NoError(t, err)
	require.EqualValues(t, 570, f.used(t))
	zero.RequestID = "free-result"
	decision, err = f.admit(zero)
	require.NoError(t, err)
	require.True(t, decision.Allowed)
	free, err := f.money.CaptureAdmission(f.ctx, zero.RequestID, 0)
	require.NoError(t, err)
	require.Nil(t, free.LedgerTransferID)
	require.Zero(t, free.Amount)
	require.EqualValues(t, 570, f.used(t))
	_, err = f.money.CaptureAdmission(f.ctx, "never-admitted", 10)
	require.ErrorIs(t, err, spendgate.ErrNotFound)
}

func TestConcurrentAdmissionsPromiseCapacityOnce(t *testing.T) {
	f := newFixture(t, 100)
	f.policy = spendgate.Policy{}
	var wg sync.WaitGroup
	allowed := make(chan bool, 8)
	errors := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, e := f.admit(f.input(uuid.NewString(), 60))
			allowed <- d.Allowed
			errors <- e
		}()
	}
	wg.Wait()
	close(allowed)
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	count := 0
	for ok := range allowed {
		if ok {
			count++
		}
	}
	require.Equal(t, 1, count)
	bal, err := f.money.GetBalanceForCustomer(f.ctx, f.payer, "USD")
	require.NoError(t, err)
	require.EqualValues(t, 60, bal.HeldBalance)
}

func TestConcurrentZeroAdmissionsOnColdPayer(t *testing.T) {
	f := newFixture(t, 0)
	f.policy = spendgate.Policy{}
	var wg sync.WaitGroup
	errors := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := f.admit(f.input(uuid.NewString(), 0)); errors <- err }()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err, "cold account creation must occur after the payer lock")
	}
}
