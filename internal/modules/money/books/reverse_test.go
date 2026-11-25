package books

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
)

func TestReversalAdjustments(t *testing.T) {
	start := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
	service := Service{Starts: start, Ends: start.Add(720 * time.Hour)}
	quarter := start.Add(180 * time.Hour)

	// Serving a quarter in, 10.00 funded and 2.50 earned: 4.00 refunded is a
	// price reduction to 6.00, which earned 1.50 by then.
	s := schedule{service: service, funded: 10_000_000, recognized: 2_500_000, through: quarter}
	contra, deferred := s.reduce(4_000_000, true)
	require.Equal(t, [2]int64{1_000_000, 3_000_000}, [2]int64{contra, deferred})
	require.Equal(t, [2]int64{6_000_000, 1_500_000}, [2]int64{s.funded, s.recognized})
	// The price restored is where it was.
	c, d, earned := s.restore(4_000_000, 0, true, service.Ends)
	require.Equal(t, [3]int64{1_000_000, 3_000_000, 0}, [3]int64{c, d, earned})
	require.Equal(t, [2]int64{10_000_000, 2_500_000}, [2]int64{s.funded, s.recognized})

	// Ended a quarter in with its forfeit to come: unrecognised revenue goes
	// first, then the contra.
	s = schedule{service: service, funded: 10_000_000, recognized: 2_500_000, through: quarter, ended: &quarter}
	contra, deferred = s.reduce(9_000_000, false)
	require.Equal(t, [2]int64{1_500_000, 7_500_000}, [2]int64{contra, deferred})
	require.Equal(t, [2]int64{1_000_000, 1_000_000}, [2]int64{s.funded, s.recognized})
	// Nothing is left to forfeit, so a won dispute's restored deferral is
	// forfeited at once.
	c, d, earned = s.restore(9_000_000, 1_500_000, false, quarter)
	require.Equal(t, [3]int64{1_500_000, 7_500_000, 7_500_000}, [3]int64{c, d, earned})
	require.Equal(t, [2]int64{10_000_000, 10_000_000}, [2]int64{s.funded, s.recognized})

	// A smaller refund leaves a forfeit to come: the restored deferral waits
	// for it.
	s = schedule{service: service, funded: 10_000_000, recognized: 2_500_000, through: quarter, ended: &quarter}
	contra, deferred = s.reduce(4_000_000, false)
	require.Equal(t, [2]int64{0, 4_000_000}, [2]int64{contra, deferred})
	c, d, earned = s.restore(4_000_000, 0, false, quarter)
	require.Equal(t, [3]int64{0, 4_000_000, 0}, [3]int64{c, d, earned})
	require.Equal(t, [2]int64{10_000_000, 2_500_000}, [2]int64{s.funded, s.recognized})

	// Its service over and all of it recognised: a refund is all contra.
	s = schedule{service: service, funded: 10_000_000, recognized: 10_000_000, through: service.Ends}
	contra, deferred = s.reduce(4_000_000, false)
	require.Equal(t, [2]int64{4_000_000, 0}, [2]int64{contra, deferred})
	require.Equal(t, [2]int64{6_000_000, 6_000_000}, [2]int64{s.funded, s.recognized})
}

func TestReversalDrawsFromItsPayment(t *testing.T) {
	one, two := uuid.New(), uuid.New()
	// 10.00 received: 6.00 to one schedule, 3.00 to another, 1.00 unapplied.
	pb := paymentBooks{received: 10_000_000, applied: map[uuid.UUID]int64{one: 6_000_000, two: 3_000_000}, legs: map[string]*reversalLegs{
		// A refund took the 1.00 and 2.00 of the first schedule; a won
		// dispute brought back half of what a chargeback took of the second.
		"refund":     {returned: 1_000_000, schedules: map[uuid.UUID]taken{one: {deferred: 1_500_000, contra: 500_000}}},
		"chargeback": {chargeback: true, schedules: map[uuid.UUID]taken{two: {deferred: 2_000_000, contra: 1_000_000}}},
		"won":        {schedules: map[uuid.UUID]taken{two: {deferred: -1_000_000, contra: -500_000}}},
	}}
	pb.returned = 1_000_000
	require.Zero(t, pb.unapplied())
	require.Equal(t, map[uuid.UUID]int64{one: 4_000_000, two: 1_500_000}, pb.held())
	require.EqualValues(t, 10_000_000, pb.used())
}

func TestPaymentBooksFromLegs(t *testing.T) {
	payment, schedule := uuid.New(), uuid.New()
	refund, chargeback, won := uuid.NewString(), uuid.NewString(), uuid.NewString()
	row := func(source string, op Operation, typ TransferType, s *uuid.UUID, amount int64) gen.ListPaymentBooksLegsRow {
		return gen.ListPaymentBooksLegsRow{SourceID: source, Operation: string(op), TransferType: string(typ), RevenueScheduleID: s, Amount: amount}
	}
	own := payment.String()
	pb := paymentBooksFrom(payment, []gen.ListPaymentBooksLegsRow{
		row(own, OpCash, CashReceived, nil, 10_000_000),
		row(own, OpApplication, CashApplied, &schedule, 6_000_000),
		// A usage refund's contra takes nothing unapplied or applied.
		row(refund, OpRefund, RefundRecognized, nil, 1_000_000),
		row(refund, OpRefund, CashReturned, nil, 2_000_000),
		row(chargeback, OpChargeback, ChargebackDeferred, &schedule, 3_000_000),
		row(chargeback, OpChargeback, ChargebackRecognized, &schedule, 1_000_000),
		row(chargeback, OpChargeback, UsageReopened, nil, 500_000),
		row(won, OpDisputeWon, DisputeWon, &schedule, 1_000_000),
		row(won, OpDisputeWonDeferred, DisputeWon, &schedule, 3_000_000),
		row(won, OpDisputeWonReopened, DisputeWon, nil, 500_000),
	})
	require.EqualValues(t, 2_000_000, pb.unapplied())
	require.Equal(t, map[uuid.UUID]int64{schedule: 6_000_000}, pb.held())
	require.True(t, pb.legs[chargeback].chargeback)
	require.EqualValues(t, 500_000, pb.legs[chargeback].reopened)
	require.EqualValues(t, -500_000, pb.legs[won].reopened)
	require.Equal(t, taken{deferred: 3_000_000, contra: 1_000_000}, pb.legs[chargeback].schedules[schedule])
}

// An undone end takes back what the end forfeited, never what a refund
// took before it.
func TestUndoneEndReinstatesOnlyItsForfeit(t *testing.T) {
	start := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
	service := Service{Starts: start, Ends: start.Add(720 * time.Hour)}
	ended := start.Add(120 * time.Hour)
	// A refund on the ended schedule took 3.00 of its deferral: 1.50 earned,
	// 6.00 funded, nothing forfeited. The end is then undone.
	s := schedule{service: service, funded: 6_000_000, recognized: 1_500_000, through: ended}
	steps, _, err := s.advance(context.Background(), time.UTC, ended.Add(24*time.Hour))
	require.NoError(t, err)
	require.Empty(t, steps)
	require.EqualValues(t, 1_500_000, s.recognized)

	// A forfeit of 7.50, then 4.00 of the contra reversed by a refund: an
	// undone end takes back what is left of the forfeit above what 5.00
	// earned by then.
	s = schedule{service: service, funded: 5_000_000, recognized: 5_000_000, through: ended, forfeited: 7_500_000}
	steps, _, err = s.advance(context.Background(), time.UTC, ended.Add(24*time.Hour))
	require.NoError(t, err)
	require.Len(t, steps, 1)
	require.Equal(t, OpReinstatement, steps[0].op)
	require.EqualValues(t, 5_000_000-833_333, steps[0].amount)
	require.EqualValues(t, 7_500_000-(5_000_000-833_333), s.forfeited)
}

func TestRecoveryNamingNoChargebackRestoresTheOldestFirst(t *testing.T) {
	one := uuid.New()
	// The older chargeback has the larger id: invoice is by when they occurred.
	older, newer := "0190a000-0000-7000-8000-000000000002", "0190a000-0000-7000-8000-000000000001"
	at := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
	pb := paymentBooks{legs: map[string]*reversalLegs{
		older: {chargeback: true, returned: 2_000_000},
		newer: {chargeback: true, schedules: map[uuid.UUID]taken{one: {deferred: 3_000_000, contra: 1_000_000}}},
		// A recovery naming none brought 1.50 back, of the older; one naming
		// the newer brought back 1.00 of it.
		"unnamed": {returned: -1_500_000},
		"named":   {schedules: map[uuid.UUID]taken{one: {deferred: -750_000, contra: -250_000}}},
	}}
	reversals := []reversalRow{
		{id: older, kind: "chargeback", at: at, recorded: at},
		{id: newer, kind: "chargeback", at: at.Add(time.Hour), recorded: at.Add(time.Hour)},
		{id: "unnamed", kind: "dispute_reversal", at: at.Add(2 * time.Hour), recorded: at.Add(2 * time.Hour)},
		{id: "named", kind: "dispute_reversal", reverses: newer, at: at.Add(3 * time.Hour), recorded: at.Add(3 * time.Hour)},
	}
	// 3.00 more, naming none: the older's last 0.50, then 2.50 of the newer.
	current := reversalRow{id: "current", kind: "dispute_reversal", at: at.Add(4 * time.Hour), recorded: at.Add(4 * time.Hour)}
	left := pb.chargebackLegs(reversals, current, 3_000_000)
	require.EqualValues(t, 500_000, left.returned)
	require.Equal(t, taken{deferred: 1_875_000, contra: 625_000}, left.schedules[one])
}

func TestNamedRecoveryRestoresWhatIsLeft(t *testing.T) {
	one := uuid.New()
	at := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
	pb := paymentBooks{legs: map[string]*reversalLegs{
		"cb": {chargeback: true, returned: 1_000_000, schedules: map[uuid.UUID]taken{one: {deferred: 3_000_000, contra: 1_000_000}}},
		// An earlier recovery of it brought back 2.00 of the schedule.
		"first": {schedules: map[uuid.UUID]taken{one: {deferred: -2_000_000}}},
	}}
	reversals := []reversalRow{
		{id: "cb", kind: "chargeback", at: at, recorded: at},
		{id: "first", kind: "dispute_reversal", reverses: "cb", at: at.Add(time.Hour), recorded: at.Add(time.Hour)},
	}
	left := pb.chargebackLegs(reversals, reversalRow{id: "second", kind: "dispute_reversal", reverses: "cb", recorded: at.Add(2 * time.Hour)}, 3_000_000)
	require.EqualValues(t, 1_000_000, left.returned)
	require.Equal(t, taken{deferred: 1_500_000, contra: 500_000}, left.schedules[one], "2.00 left, in the chargeback's own split")
}

func TestLateObservedChargebackKeepsEarlierRecoveries(t *testing.T) {
	one := uuid.New()
	at := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
	pb := paymentBooks{legs: map[string]*reversalLegs{
		"seen":  {chargeback: true, schedules: map[uuid.UUID]taken{one: {deferred: 2_000_000}}},
		"late":  {chargeback: true, returned: 3_000_000},
		"first": {schedules: map[uuid.UUID]taken{one: {deferred: -2_000_000}}},
	}}
	// "late" occurred first but was stored after the recovery naming none:
	// that recovery restored "seen", the only chargeback stored by then.
	reversals := []reversalRow{
		{id: "late", kind: "chargeback", at: at, recorded: at.Add(3 * time.Hour)},
		{id: "seen", kind: "chargeback", at: at.Add(time.Hour), recorded: at.Add(time.Hour)},
		{id: "first", kind: "dispute_reversal", at: at.Add(2 * time.Hour), recorded: at.Add(2 * time.Hour)},
	}
	left := pb.chargebackLegs(reversals, reversalRow{id: "next", kind: "dispute_reversal", recorded: at.Add(4 * time.Hour)}, 5_000_000)
	require.EqualValues(t, 3_000_000, left.returned, "all of the late one")
	require.Empty(t, left.schedules, "none of the one already restored")
}
