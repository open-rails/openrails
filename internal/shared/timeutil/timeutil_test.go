package timeutil

import (
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"
)

func TestParsersReturnUTC(t *testing.T) {
	day := time.Date(2026, time.March, 9, 0, 0, 0, 0, time.UTC)
	instant := time.Date(2026, time.March, 9, 10, 11, 12, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		parse func(string) (time.Time, error)
		in    string
		want  time.Time
	}{
		{"rfc3339", ParseRFC3339UTC, "2026-03-09T10:11:12Z", instant},
		{"rfc3339 offset normalized to UTC", ParseRFC3339UTC, "2026-03-09T12:11:12+02:00", instant},
		{"date", ParseDateUTC, " 2026-03-09 ", day},
		{"date or rfc3339: date", ParseDateOrRFC3339UTC, "2026-03-09", day},
		{"date or rfc3339: instant", ParseDateOrRFC3339UTC, "2026-03-09T10:11:12Z", instant},
		{"first matching layout", func(v string) (time.Time, error) { return ParseFirstUTC(v, "2006-01-02", "01/02/2006") }, "03/09/2026", day},
	} {
		got, err := tc.parse(tc.in)
		require.NoError(t, err, tc.name)
		require.True(t, got.Equal(tc.want), tc.name)
		require.Equal(t, time.UTC, got.Location(), tc.name)
	}
	for _, bad := range []func() (time.Time, error){
		func() (time.Time, error) { return ParseFirstUTC("", time.RFC3339) },
		func() (time.Time, error) { return ParseFirstUTC("not-a-date", "2006-01-02") },
		func() (time.Time, error) { return ParseDateUTC("  ") },
		func() (time.Time, error) { return ParseRFC3339UTC("2026-03-09") },
	} {
		_, err := bad()
		require.Error(t, err)
	}
}

func TestFirstClockPrefersInjectedClock(t *testing.T) {
	fake := clockwork.NewFakeClock()
	require.Same(t, fake, FirstClock(nil, fake))
	require.NotNil(t, FirstClock())
}
