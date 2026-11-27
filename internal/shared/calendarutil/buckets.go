package calendarutil

import "time"

// Grains of a calendar bucket on a zone's clock: an hour, or a day, week
// (starting Monday), month, quarter or year from its first date's DayStart.
const (
	Hour    = "hour"
	Day     = "day"
	Week    = "week"
	Month   = "month"
	Quarter = "quarter"
	Year    = "year"
)

// BucketStart is the start of the bucket of grain that t falls in on loc's
// calendar, as a UTC instant. Hours count from their day's first instant, so
// a day of 23 or 25 hours has as many. Other grains start at their first
// date's DayStart, as PostgreSQL's date_trunc(grain, t, zone) does.
func BucketStart(t time.Time, grain string, loc *time.Location) time.Time {
	if grain == Hour {
		day := DateStart(t, loc)
		if day.After(t) { // the first of two midnights: the day before's last hour
			y, m, d := t.In(loc).Date()
			day = DayStart(y, m, d-1, loc)
		}
		return day.Add(t.Sub(day).Truncate(time.Hour)).UTC()
	}
	y, m, d := bucketDate(t.In(loc), grain)
	return DayStart(y, m, d, loc).UTC()
}

// Buckets are the starts of grain's buckets on loc's calendar that overlap
// [from, to), as UTC instants, and the end of the last. Each day starts at its
// DayStart, never a wall clock moved by 24 hours, so a day that starts at a
// transition neither drifts nor repeats. Past max it stops at max+1 starts,
// so a caller can refuse the range.
func Buckets(from, to time.Time, grain string, loc *time.Location, max int) ([]time.Time, time.Time) {
	var starts []time.Time
	cur := BucketStart(from, grain, loc)
	for cur.Before(to) && len(starts) <= max {
		starts = append(starts, cur)
		cur = nextBucket(cur, grain, loc)
	}
	return starts, cur
}

// nextBucket is the start of the bucket after the one starting at start.
func nextBucket(start time.Time, grain string, loc *time.Location) time.Time {
	l := start.In(loc)
	if grain == Hour {
		dayEnd := DayStart(l.Year(), l.Month(), l.Day()+1, loc)
		if next := start.Add(time.Hour); next.Before(dayEnd) {
			return next.UTC()
		}
		return dayEnd.UTC()
	}
	y, m, d := l.Date()
	switch grain {
	case Day:
		d++
	case Week:
		d += 7
	case Month:
		m++
	case Quarter:
		m += 3
	case Year:
		y++
	}
	return DayStart(y, m, d, loc).UTC()
}

// bucketDate is the date t's bucket starts on, in t's location.
func bucketDate(t time.Time, grain string) (int, time.Month, int) {
	y, m, d := t.Date()
	switch grain {
	case Week:
		d -= (int(t.Weekday()) + 6) % 7
	case Month:
		d = 1
	case Quarter:
		m, d = ((m-1)/3)*3+1, 1
	case Year:
		m, d = 1, 1
	}
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Date()
}
