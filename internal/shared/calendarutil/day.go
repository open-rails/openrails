package calendarutil

import "time"

// DayStart is when the local date y-m-d begins in loc, resolved as
// PostgreSQL resolves that date's midnight, so Go's period starts agree with
// date_trunc(..., zone): where clocks spring forward over midnight it is the
// transition (the offset before it); where midnight happens twice, the later.
// time.Date gives the previous evening instead. y, m and d normalize as in
// time.Date.
func DayStart(y int, m time.Month, d int, loc *time.Location) time.Time {
	wall := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	at := func(offset int) (time.Time, bool) {
		t := wall.Add(-time.Duration(offset) * time.Second)
		_, in := t.In(loc).Zone()
		return t, in == offset
	}
	_, before := wall.Add(-48 * time.Hour).In(loc).Zone()
	_, after := wall.Add(48 * time.Hour).In(loc).Zone()
	if t, ok := at(after); ok {
		return t
	}
	t, _ := at(before)
	return t
}

// DateStart is DayStart of t's date in loc.
func DateStart(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return DayStart(y, m, d, loc)
}
