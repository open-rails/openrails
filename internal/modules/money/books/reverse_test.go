package books

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
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
