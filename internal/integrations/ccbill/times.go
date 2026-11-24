package ccbill

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// timeZone is the zone every time CCBill sends is read in: DataLink rows,
// webhook timestamps, subscription-management dates. UNCONFIRMED: CCBill
// documents their shapes but no zone (its billing weeks are MST), so they are
// read as UTC, as they always were, until a live account shows otherwise.
// DataLink's request bounds go out in dataLinkLocation, also unconfirmed.
var timeZone = time.UTC

var (
	instantLayouts = []string{"2006-01-02 15:04:05", "20060102150405", time.RFC3339}
	// dateLayouts are CCBill's date-only fields: DataLink's REBILL and EXPIRE
	// timestamps and its expire, cancel and next rebill dates (YYYY-MM-DD),
	// subscription management's (YYYYMMDD) and the webhook's renewal dates.
	dateLayouts = []string{"2006-01-02", "20060102", "01/02/2006"}
)

// ParseTime reads a time CCBill sent: an instant, or a date as the first
// instant of its day.
func ParseTime(s string) (time.Time, error) {
	if t, err := ParseInstant(s); err == nil {
		return t, nil
	}
	return ParseDate(s)
}

// ParseInstant reads an instant CCBill sent, never a bare date.
func ParseInstant(s string) (time.Time, error) {
	return parseIn(s, instantLayouts)
}

// ParseDate reads a date-only field CCBill sent as the first instant of its
// day.
func ParseDate(s string) (time.Time, error) {
	return parseIn(s, dateLayouts)
}

// DayEnd is the last second of t's day in CCBill's zone: a date-only
// paid-through or renewal date read so that no access gap opens.
func DayEnd(t time.Time) time.Time {
	t = t.In(timeZone)
	return time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, timeZone).UTC()
}

func dayStart(t time.Time) time.Time {
	t = t.In(timeZone)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, timeZone).UTC()
}

func parseIn(s string, layouts []string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, errors.New("ccbill time is empty")
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, s, timeZone); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized ccbill time %q", s)
}
