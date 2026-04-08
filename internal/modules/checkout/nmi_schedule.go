package checkout

import "time"

func buildNMIFutureStartDate(target time.Time, now time.Time) (string, time.Time) {
	targetDay := startOfUTCDate(target)
	minFutureDay := startOfUTCDate(now).AddDate(0, 0, 1)
	if !targetDay.After(startOfUTCDate(now)) {
		targetDay = minFutureDay
	}
	return targetDay.Format("20060102"), targetDay
}

func startOfUTCDate(value time.Time) time.Time {
	utc := value.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
}
