package books

import "time"

// The price changes a lot's reversal and recovery make to the schedules its
// spends funded, as a payment's reversals change its lines' schedules.

// reduce takes x from the schedule's funding, past what its service earned
// so far. Serving, it is a price reduction: recognised revenue falls to what
// the lower price earned by then, the difference the contra. Ended,
// unrecognised revenue goes first, then the contra. It returns both legs.
func (s *schedule) reduce(x int64, serving bool) (contra, deferred int64) {
	if serving {
		s.funded -= x
		was := s.recognized
		s.recognized = s.service.target(s.funded, s.through)
		contra = was - s.recognized
		return contra, x - contra
	}
	deferred = min(x, s.funded-s.recognized)
	contra = x - deferred
	s.funded -= x
	s.recognized -= contra
	return contra, deferred
}

// restore brings y back to the schedule's funding. Serving, it is a price
// restored: recognised revenue rises to what the higher price earned by
// then. Ended, contra of it returns to recognised revenue, and what the
// service earned by end of the rest is recognised too: all of it unless an
// early end's forfeit is still to come.
func (s *schedule) restore(y, contra int64, serving bool, end time.Time) (c, deferred, earned int64) {
	if serving {
		s.funded += y
		was := s.recognized
		s.recognized = s.service.target(s.funded, s.through)
		c = s.recognized - was
		return c, y - c, 0
	}
	pending := s.ended != nil && s.recognized < s.funded
	c = min(contra, y)
	s.funded += y
	s.recognized += c
	earned = max(s.service.target(s.funded, end)-s.recognized, 0)
	if !pending {
		earned = s.funded - s.recognized
	}
	s.recognized += earned
	return c, y - c, earned
}
