package lifecycle

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var (
	t0   = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1   = t0.AddDate(0, 1, 0) // paid through
	t2   = t1.AddDate(0, 1, 0)
	half = t0.Add(15 * 24 * time.Hour)
)

func snap(status Status, owner Owner) Snapshot {
	return Snapshot{Status: status, Owner: owner, PaidThrough: t1}
}

func TestTransitions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		from    Snapshot
		event   Event
		want    Status
		effects []Effect
		err     error
		through time.Time
	}{
		{name: "initial payment starts", from: Snapshot{Status: Pending, Owner: Engine}, event: InitialPaid{t0, t1}, want: Active, through: t1,
			effects: []Effect{GrantPeriod{t0, t1}, Notify{NoticeStarted}}},
		{name: "replayed initial payment", from: snap(Active, Engine), event: InitialPaid{t0, t1}, want: Active, through: t1},
		{name: "initial failure abandons", from: Snapshot{Status: Pending, Owner: Engine}, event: InitialFailed{}, want: Cancelled},

		{name: "renewal extends", from: snap(Active, Engine), event: RenewalPaid{t1, t2}, want: Active, through: t2,
			effects: []Effect{GrantPeriod{t1, t2}, Notify{NoticeRenewed}}},
		{name: "renewal recovers dunning", from: snap(PastDue, NMISchedule), event: RenewalPaid{t1, t2}, want: Active, through: t2,
			effects: []Effect{GrantPeriod{t1, t2}, CloseDunning{}, Notify{NoticeRenewed}}},
		{name: "renewal settles unverified", from: snap(Unverified, Provider), event: RenewalPaid{t1, t2}, want: Active, through: t2,
			effects: []Effect{GrantPeriod{t1, t2}, CloseDunning{}, Notify{NoticeRenewed}}},
		{name: "renewal after a gap buys the paid period", from: snap(PastDue, Engine), event: RenewalPaid{half.AddDate(0, 1, 0), t2.AddDate(0, 0, 15)}, want: Active, through: t2.AddDate(0, 0, 15),
			effects: []Effect{GrantPeriod{half.AddDate(0, 1, 0), t2.AddDate(0, 0, 15)}, CloseDunning{}, Notify{NoticeRenewed}}},
		{name: "replayed renewal changes nothing", from: snap(Active, Engine), event: RenewalPaid{t0, t1}, want: Active, through: t1},
		{name: "overlapping renewal refused", from: snap(Active, Engine), event: RenewalPaid{half, t2}, want: Active, through: t1, err: ErrInvalid},
		{name: "payment on a cancelled row is refund review", from: snap(Cancelled, NMISchedule), event: RenewalPaid{t1, t2}, want: Cancelled, through: t1, err: ErrTerminal},

		{name: "retryable decline opens dunning", from: snap(Active, Engine), event: RenewalDeclined{t1, Retry}, want: PastDue, through: t1,
			effects: []Effect{OpenDunning{t1}, Notify{NoticePaymentFailed}}},
		{name: "second decline keeps the case", from: snap(PastDue, NMISchedule), event: RenewalDeclined{t1, Retry}, want: PastDue, through: t1,
			effects: []Effect{Notify{NoticePaymentFailed}}},
		{name: "decline of unverified row opens dunning", from: snap(Unverified, NMISchedule), event: RenewalDeclined{t1, Retry}, want: PastDue, through: t1,
			effects: []Effect{OpenDunning{t1}, Notify{NoticePaymentFailed}}},
		{name: "fix-method decline waits for the customer", from: snap(PastDue, Engine), event: RenewalDeclined{t1, FixMethod}, want: AwaitingMethod, through: t1,
			effects: []Effect{CloseDunning{}, Notify{NoticeUpdateMethod}}},
		{name: "non-recoverable decline cancels at paid-through", from: snap(Active, NMISchedule), event: RenewalDeclined{t1, NonRecoverable}, want: Cancelled, through: t1,
			effects: []Effect{CloseDunning{}, EndAccess{t1}, QueueProviderCancel{}, Notify{NoticeEnded}}},
		{name: "provider-owned decline is mirrored only", from: snap(Active, Provider), event: RenewalDeclined{t1, NonRecoverable}, want: PastDue, through: t1},
		{name: "stale decline of a paid period", from: snap(Active, NMISchedule), event: RenewalDeclined{t0, Retry}, want: Active, through: t1},
		{name: "decline after cancellation ignored", from: snap(Cancelled, Engine), event: RenewalDeclined{t1, Retry}, want: Cancelled, through: t1},

		{name: "new card resumes dunning", from: snap(AwaitingMethod, Engine), event: MethodReplaced{}, want: PastDue, through: t1,
			effects: []Effect{OpenDunning{t1}}},
		{name: "new card on an active row", from: snap(Active, Engine), event: MethodReplaced{}, want: Active, through: t1},

		{name: "exhausted dunning cancels", from: snap(PastDue, NMISchedule), event: DunningExhausted{t2}, want: Cancelled, through: t1,
			effects: []Effect{CloseDunning{}, EndAccess{t2}, QueueProviderCancel{}, Notify{NoticeEnded}}},
		{name: "exhaustion needs dunning", from: snap(Unverified, NMISchedule), event: DunningExhausted{t2}, want: Unverified, through: t1, err: ErrIllegal},

		{name: "overdue asks the provider", from: snap(Active, NMISchedule), event: RenewalOverdue{}, want: Unverified, through: t1,
			effects: []Effect{ProbeProvider{}}},
		{name: "overdue mirror asks the provider", from: snap(Active, Provider), event: RenewalOverdue{}, want: Unverified, through: t1,
			effects: []Effect{ProbeProvider{}}},
		{name: "engine renewals are not overdue here", from: snap(Active, Engine), event: RenewalOverdue{}, want: Active, through: t1},
		{name: "overdue never touches dunning", from: snap(PastDue, NMISchedule), event: RenewalOverdue{}, want: PastDue, through: t1},

		{name: "provider cancel keeps paid access", from: snap(Unverified, Provider), event: ProviderCancelled{half}, want: Cancelled, through: t1,
			effects: []Effect{CloseDunning{}, EndAccess{t1}, Notify{NoticeEnded}}},
		{name: "provider cancel after paid-through", from: snap(PastDue, Provider), event: ProviderCancelled{t2}, want: Cancelled, through: t1,
			effects: []Effect{CloseDunning{}, EndAccess{t2}, Notify{NoticeEnded}}},

		{name: "user cancel runs to period end", from: snap(Active, Engine), event: Cancel{Kind: CancelUser, At: half}, want: Active, through: t1,
			effects: []Effect{CloseDunning{}, QueueProviderCancel{}}},
		{name: "immediate cancel ends access", from: snap(Active, NMISchedule), event: Cancel{Kind: CancelMerchant, Immediate: true, At: half}, want: Cancelled, through: t1,
			effects: []Effect{CloseDunning{}, EndAccess{half}, QueueProviderCancel{}, Notify{NoticeEnded}}},
		{name: "chargeback is immediate", from: snap(Active, Provider), event: Cancel{Kind: CancelChargeback, At: half}, want: Cancelled, through: t1,
			effects: []Effect{CloseDunning{}, EndAccess{half}, QueueProviderCancel{}, Notify{NoticeEnded}}},
		{name: "cancel after paid-through is immediate", from: snap(PastDue, Engine), event: Cancel{Kind: CancelUser, At: t2}, want: Cancelled, through: t1,
			effects: []Effect{CloseDunning{}, EndAccess{t2}, QueueProviderCancel{}, Notify{NoticeEnded}}},
		{name: "cancel without a kind", from: snap(Active, Engine), event: Cancel{At: half}, want: Active, through: t1, err: ErrInvalid},

		{name: "period end completes a scheduled cancel", from: Snapshot{Status: Active, Owner: Engine, PaidThrough: t1, CancelAtPeriodEnd: true}, event: PeriodEnded{t1}, want: Cancelled, through: t1,
			effects: []Effect{CloseDunning{}, EndAccess{t1}, Notify{NoticeEnded}}},
		{name: "period end alone renews nothing", from: snap(Active, Engine), event: PeriodEnded{t1}, want: Active, through: t1},
		{name: "resume inside the paid period", from: Snapshot{Status: Active, Owner: Engine, PaidThrough: t1, CancelAtPeriodEnd: true}, event: Resume{half}, want: Active, through: t1},
		{name: "resume after the period", from: Snapshot{Status: Active, Owner: Engine, PaidThrough: t1, CancelAtPeriodEnd: true}, event: Resume{t2}, want: Active, through: t1, err: ErrIllegal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			next, effects, err := Apply(tc.from, tc.event)
			if tc.err != nil {
				require.True(t, errors.Is(err, tc.err), "want %v, got %v", tc.err, err)
				require.Equal(t, tc.from, next, "a refused event changes nothing")
				require.Empty(t, effects)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, next.Status)
			require.Equal(t, tc.through, next.PaidThrough, "paid-through moves only on a payment")
			require.Equal(t, tc.effects, effects)
		})
	}
}

// A chargeback on an already cancelled subscription still ends paid access now.
func TestChargebackAfterCancel(t *testing.T) {
	t.Parallel()
	from := Snapshot{Status: Cancelled, Owner: Engine, PaidThrough: t1, EndedAt: t1, CancelKind: CancelUser}
	next, effects, err := Apply(from, Cancel{Kind: CancelChargeback, At: half})
	require.NoError(t, err)
	require.Equal(t, CancelChargeback, next.CancelKind)
	require.Equal(t, []Effect{EndAccess{half}}, effects)
}

// Paid-through never moves without a payment fact, and access is never ended
// by uncertainty: every non-payment event sequence keeps PaidThrough, and only
// the confirmed outcomes produce EndAccess.
func TestNoEvidenceNoChange(t *testing.T) {
	t.Parallel()
	events := []Event{
		RenewalDeclined{t1, Retry}, RenewalDeclined{t1, FixMethod}, MethodReplaced{}, RenewalOverdue{},
		PeriodEnded{t2}, Resume{half}, RenewalDeclined{t0, Retry},
	}
	for _, owner := range []Owner{Engine, NMISchedule, Provider} {
		s := snap(Active, owner)
		for i := 0; i < 50; i++ {
			next, effects, err := Apply(s, events[i%len(events)])
			if err != nil {
				continue
			}
			require.Equal(t, t1, next.PaidThrough)
			for _, e := range effects {
				_, ended := e.(EndAccess)
				require.False(t, ended, "%s: %T ended access without a confirmed outcome", owner, events[i%len(events)])
				_, granted := e.(GrantPeriod)
				require.False(t, granted)
			}
			s = next
		}
	}
}

func TestRenewalDue(t *testing.T) {
	t.Parallel()
	require.True(t, RenewalDue(snap(Active, Engine), t1))
	require.False(t, RenewalDue(snap(Active, Engine), half))
	require.False(t, RenewalDue(snap(Active, NMISchedule), t1), "NMI's schedule charges its own renewals")
	require.False(t, RenewalDue(snap(PastDue, Engine), t2), "dunning owns a declined renewal")
	require.False(t, RenewalDue(Snapshot{Status: Active, Owner: Engine, PaidThrough: t1, CancelAtPeriodEnd: true}, t1))
}
