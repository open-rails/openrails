package riverjobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

// jobs_solana_crank_statemachine_test.go is the FAST, network-free,
// CI-runnable companion to the real on-chain devnet rebill test (#275). It
// exercises SolanaCrankWorker.crankOne — the dunning + rebill STATE MACHINE —
// by varying the on-chain pull RESULT (the INPUT) and asserting the resulting
// STATE TRANSITIONS across simulated time.
//
// Nothing here mocks on-chain behaviour dishonestly: the only thing stubbed is
// the network pull (the fakeCranker returns a chosen outcome — success, a real
// on-chain error string, or an RPC error). The classifier
// (recurring.ClassifyCrankError) and the scheduling/dunning logic
// (subscriptions.DunningRetryOffsets, #359 cadence-relative) under test are the
// REAL production code paths. The fakeLifecycle models exactly what the real
// SubscriptionLifecycleService does on FailMembership: escalate RetryAttempts
// and cancel at the schedule's max failures.

// ---- fakes -----------------------------------------------------------------

// fakeSolanaStore is an in-memory stand-in for *solanasubs.SolanaSubscriptionRepo.
// It records the persisted next_pull_at / status so the test can assert
// scheduling across cranks.
type fakeSolanaStore struct {
	status      map[uuid.UUID]string
	nextPullAt  map[uuid.UUID]time.Time
	lastSig     map[uuid.UUID]string
	periodStart map[uuid.UUID]time.Time
}

func newFakeSolanaStore() *fakeSolanaStore {
	return &fakeSolanaStore{
		status:      map[uuid.UUID]string{},
		nextPullAt:  map[uuid.UUID]time.Time{},
		lastSig:     map[uuid.UUID]string{},
		periodStart: map[uuid.UUID]time.Time{},
	}
}

func (s *fakeSolanaStore) SetStatus(_ context.Context, id uuid.UUID, status string) error {
	s.status[id] = status
	return nil
}

func (s *fakeSolanaStore) SetNextPullAt(_ context.Context, id uuid.UUID, nextPullAt time.Time) error {
	s.nextPullAt[id] = nextPullAt.UTC()
	return nil
}

func (s *fakeSolanaStore) AdvanceAfterPull(_ context.Context, id uuid.UUID, periodStart time.Time, signature string, nextPullAt time.Time) error {
	s.periodStart[id] = periodStart.UTC()
	s.lastSig[id] = signature
	s.nextPullAt[id] = nextPullAt.UTC()
	return nil
}

// fakeCranker returns a scripted on-chain pull outcome (the INPUT being
// varied). sig/err are whatever the next pull should yield; the test rewires
// them between cranks to simulate the on-chain result changing over time. It
// implements the real solanaCranker interface (the ONLY stubbed surface — the
// network pull).
type fakeCranker struct {
	sig   string
	err   error
	calls int
}

func (c *fakeCranker) Crank(_ context.Context, _ merchant.ID, _ *models.SolanaSubscription, _ uint64) (string, error) {
	c.calls++
	return c.sig, c.err
}

// fakeLifecycle captures the lifecycle calls crankOne makes AND models the real
// dunning escalation: FailMembership increments RetryAttempts and, on reaching
// the schedule's max failures, cancels (exactly as
// SubscriptionLifecycleService.FailMembership does — see lifecycle_service.go
// RetryAttempts >= DunningMaxFailures).
type fakeLifecycle struct {
	renewals  int
	failures  int
	cancels   int
	cancelled bool // set when escalation or terminal cancel fires

	lastRenew  *subscriptions.RenewMembershipParams
	lastFail   *subscriptions.FailMembershipParams
	lastCancel *subscriptions.CancelMembershipParams
}

func (l *fakeLifecycle) RenewMembership(_ context.Context, p *subscriptions.RenewMembershipParams) error {
	l.renewals++
	l.lastRenew = p
	return nil
}

func (l *fakeLifecycle) FailMembership(_ context.Context, p *subscriptions.FailMembershipParams) error {
	l.failures++
	l.lastFail = p
	// Mirror the production lifecycle: cancel once RetryAttempts reaches the cap.
	if l.failures >= subscriptions.DunningMaxFailures(smCycleHours) {
		l.cancelled = true
	}
	return nil
}

func (l *fakeLifecycle) CancelMembership(_ context.Context, p *subscriptions.CancelMembershipParams) error {
	l.cancels++
	l.cancelled = true
	l.lastCancel = p
	return nil
}

// ---- harness ---------------------------------------------------------------

const (
	smPeriodHours = uint64(1)
	// smCycleHours pins the harness to the MONTHLY dunning tier (#359) so the
	// recoverable-decline path exercises the progressive 5-failure schedule
	// (+2d/+5d/+9d/+13d); the 1h on-chain period alone would otherwise
	// resolve to the 0-retry tier.
	smCycleHours   = 30 * 24
	smAmountBase   = uint64(1_000_000) // 1 USDC (6 decimals)
	smFiatAmount   = int64(100)        // $1.00
	smFiatCurrency = "usd"
	smFingerprint  = int64(0) // 0 => ghost-plan guard disabled
	smSuccessSig   = "SIG_OK"
)

type crankHarness struct {
	w     *SolanaCrankWorker
	clock *clockwork.FakeClock
	store *fakeSolanaStore
	crank *fakeCranker
	life  *fakeLifecycle
	row   *models.SolanaSubscription
}

func newCrankHarness(t *testing.T) *crankHarness {
	t.Helper()
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := clockwork.NewFakeClockAt(start)
	store := newFakeSolanaStore()
	crank := &fakeCranker{sig: smSuccessSig}
	life := &fakeLifecycle{}

	id := uuid.New()
	row := &models.SolanaSubscription{
		ID:                       id,
		MerchantID:               uuid.New(),
		SubscriptionID:           uuid.New(),
		SubscriptionPDA:          "SUB_PDA",
		PlanCreatedAtFingerprint: smFingerprint,
		NextPullAt:               start,
		Status:                   models.SolanaSubscriptionActive,
	}
	store.status[id] = models.SolanaSubscriptionActive
	store.nextPullAt[id] = start

	w := &SolanaCrankWorker{
		Clock:     clock,
		Cranker:   crank,
		Lifecycle: life,
		resolvePlanFn: func(_ context.Context, _ *models.SolanaSubscription) (resolvedPlan, error) {
			return resolvedPlan{
				amountBaseUnits: smAmountBase,
				periodHours:     smPeriodHours,
				fingerprint:     smFingerprint,
				fiatAmount:      smFiatAmount,
				currency:        smFiatCurrency,
				cycleHours:      smCycleHours,
				// mirror production resolvePlan: the failure count BEFORE this
				// crank, as the real subscription row would carry it.
				retryAttempts: life.failures,
			}, nil
		},
	}
	return &crankHarness{w: w, clock: clock, store: store, crank: crank, life: life, row: row}
}

// run performs one crankOne against the harness fakes.
func (h *crankHarness) run(t *testing.T) error {
	t.Helper()
	_, err := h.w.crankOne(context.Background(), h.store, h.row, nil)
	return err
}

// ---- tests -----------------------------------------------------------------

// success across several periods: each confirmed pull renews + advances
// next_pull_at by exactly one period. This is the multi-rebill happy path the
// devnet test (#275-B) proves on real chain — here over simulated time.
func TestCrankStateMachine_SuccessMultiRebill(t *testing.T) {
	h := newCrankHarness(t)

	const cycles = 4
	for i := 0; i < cycles; i++ {
		now := h.w.now()
		h.crank.sig = smSuccessSig
		h.crank.err = nil
		require.NoError(t, h.run(t))

		// next_pull_at advanced exactly one period from "now".
		wantNext := now.Add(time.Duration(smPeriodHours) * time.Hour)
		require.Equal(t, wantNext, h.store.nextPullAt[h.row.ID], "cycle %d next_pull_at", i)
		require.Equal(t, smSuccessSig, h.store.lastSig[h.row.ID])

		// advance the fake clock to the period boundary for the next pull.
		h.clock.Advance(time.Duration(smPeriodHours) * time.Hour)
	}

	require.Equal(t, cycles, h.life.renewals, "one renewal per successful cycle")
	require.Equal(t, 0, h.life.failures, "no dunning on success")
	require.Equal(t, 0, h.life.cancels, "no cancel on success")
	require.False(t, h.life.cancelled)

	// the recorded renewal carried the fiat amount/currency.
	require.NotNil(t, h.life.lastRenew)
	require.Equal(t, smFiatAmount, h.life.lastRenew.Amount)
	require.Equal(t, smFiatCurrency, h.life.lastRenew.Currency)
	require.Equal(t, models.RailSolana, h.life.lastRenew.Rail)
}

// recoverable decline (insufficient USDC, SPL token Custom:1): route to dunning
// and reschedule next_pull_at by the cadence-relative dunning interval (NOT
// one period). Escalating the same decline across retries reaches the
// schedule's max failures -> cancel.
func TestCrankStateMachine_RecoverableDunningEscalatesToCancel(t *testing.T) {
	h := newCrankHarness(t)
	// SPL token InsufficientFunds -> recoverable per the real classifier.
	insufficient := errors.New(`Transaction failed: custom program error: 0x1 {"InstructionError":[0,{"Custom":1}]}`)

	maxFailures := subscriptions.DunningMaxFailures(smCycleHours)
	for attempt := 1; attempt <= maxFailures; attempt++ {
		now := h.w.now()
		h.crank.err = insufficient
		require.NoError(t, h.run(t))

		require.Equal(t, attempt, h.life.failures, "FailMembership called once per attempt")
		require.Equal(t, 0, h.life.renewals, "no renewal while dunning")

		// rescheduled by the schedule's gap for this failure count, not by one
		// period. The terminal failure has no gap: the crank then advances one
		// period so the cancelled record doesn't hot-loop.
		gap := subscriptions.DunningNextRetryIn(smCycleHours, attempt)
		if gap == 0 {
			gap = time.Duration(smPeriodHours) * time.Hour
		}
		wantNext := now.Add(gap)
		require.Equal(t, wantNext, h.store.nextPullAt[h.row.ID], "attempt %d reschedule", attempt)

		// the recorded decline code is the shared InsufficientFunds vocabulary.
		require.NotNil(t, h.life.lastFail)
		require.Equal(t, models.RailSolana, h.life.lastFail.Rail)

		h.clock.Advance(gap)
	}

	// after the schedule's max failures the lifecycle escalates to cancel
	// (modelled here exactly as the real FailMembership does).
	require.True(t, h.life.cancelled, "escalated to cancel at the schedule's max failures")
	require.Equal(t, maxFailures, h.life.failures)
}

// terminal decline (subscriber revoked the SPL delegate, token Custom:4
// OwnerMismatch): cancel the membership + mark the row cancelled, and NEVER dun.
func TestCrankStateMachine_TerminalCancelsNoDunning(t *testing.T) {
	h := newCrankHarness(t)
	revoked := errors.New(`Transaction failed: {"InstructionError":[0,{"Custom":4}]}`)
	h.crank.err = revoked

	require.NoError(t, h.run(t))

	require.Equal(t, 1, h.life.cancels, "CancelMembership called once")
	require.Equal(t, 0, h.life.failures, "terminal never duns")
	require.Equal(t, 0, h.life.renewals)
	require.Equal(t, models.SolanaSubscriptionCancelled, h.store.status[h.row.ID], "row marked cancelled")

	require.NotNil(t, h.life.lastCancel)
	require.Equal(t, models.CancelTypeUser, h.life.lastCancel.CancelType)
	require.True(t, h.life.lastCancel.RevokeAccess)
}

// operational failure (RPC/transport/gas, no program code): retry next run, do
// NOT dun, and leave next_pull_at UNCHANGED so the row stays due.
func TestCrankStateMachine_OperationalRetriesNoDunNoReschedule(t *testing.T) {
	h := newCrankHarness(t)
	before := h.store.nextPullAt[h.row.ID]
	rpcErr := errors.New("rpc error: connection refused: failed to send transaction")
	h.crank.err = rpcErr

	// crankOne returns the underlying error so River retries the job.
	err := h.run(t)
	require.Error(t, err)

	require.Equal(t, 0, h.life.failures, "operational never duns")
	require.Equal(t, 0, h.life.renewals)
	require.Equal(t, 0, h.life.cancels)
	require.Equal(t, before, h.store.nextPullAt[h.row.ID], "next_pull_at unchanged -> stays due")
}

// already-paid (subscriptions program Custom:400 cap reached): the period was
// pulled on-chain but our DB missed it. Advance past this period (no double
// charge, no dun); the reconcile worker repairs the ledger.
func TestCrankStateMachine_AlreadyPaidAdvancesNoDunNoDoubleCharge(t *testing.T) {
	h := newCrankHarness(t)
	now := h.w.now()
	alreadyPaid := errors.New(`Transaction failed: {"InstructionError":[0,{"Custom":400}]}`)
	h.crank.err = alreadyPaid

	require.NoError(t, h.run(t))

	require.Equal(t, 0, h.life.failures, "already-paid never duns")
	require.Equal(t, 0, h.life.renewals, "no double-charge renewal")
	require.Equal(t, 0, h.life.cancels)

	// advanced by exactly one period (not the dunning interval).
	wantNext := now.Add(time.Duration(smPeriodHours) * time.Hour)
	require.Equal(t, wantNext, h.store.nextPullAt[h.row.ID])
}

// ghost-plan guard: a fingerprint mismatch expires the row before any pull is
// attempted (the on-chain pull would fail PlanTermsMismatch).
func TestCrankStateMachine_GhostPlanExpiresBeforePull(t *testing.T) {
	h := newCrankHarness(t)
	h.row.PlanCreatedAtFingerprint = 111
	h.w.resolvePlanFn = func(_ context.Context, _ *models.SolanaSubscription) (resolvedPlan, error) {
		return resolvedPlan{
			amountBaseUnits: smAmountBase,
			periodHours:     smPeriodHours,
			fingerprint:     222, // mismatch
			fiatAmount:      smFiatAmount,
			currency:        smFiatCurrency,
		}, nil
	}

	require.NoError(t, h.run(t))
	require.Equal(t, models.SolanaSubscriptionExpired, h.store.status[h.row.ID])
	require.Equal(t, 0, h.crank.calls, "no on-chain pull attempted for a ghost plan")
	require.Equal(t, 0, h.life.renewals)
	require.Equal(t, 0, h.life.failures)
}
