package timeutil

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseRFC3339UTC(t *testing.T) {
	parsed, err := ParseRFC3339UTC("2026-03-09T10:11:12Z")
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, time.March, 9, 10, 11, 12, 0, time.UTC), parsed)
}

func TestParseDateUTC(t *testing.T) {
	parsed, err := ParseDateUTC(" 2026-03-09 ")
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, time.March, 9, 0, 0, 0, 0, time.UTC), parsed)
}

func TestParseDateOrRFC3339UTC(t *testing.T) {
	t.Run("accepts date", func(t *testing.T) {
		parsed, err := ParseDateOrRFC3339UTC("2026-03-09")
		require.NoError(t, err)
		require.Equal(t, time.Date(2026, time.March, 9, 0, 0, 0, 0, time.UTC), parsed)
	})

	t.Run("accepts rfc3339", func(t *testing.T) {
		parsed, err := ParseDateOrRFC3339UTC("2026-03-09T10:11:12Z")
		require.NoError(t, err)
		require.Equal(t, time.Date(2026, time.March, 9, 10, 11, 12, 0, time.UTC), parsed)
	})
}

func TestParseFirstUTC(t *testing.T) {
	parsed, err := ParseFirstUTC("03/09/2026", "2006-01-02", "01/02/2006")
	require.NoError(t, err)
	require.Equal(t, time.Date(2026, time.March, 9, 0, 0, 0, 0, time.UTC), parsed)

	_, err = ParseFirstUTC("", time.RFC3339)
	require.Error(t, err)

	_, err = ParseFirstUTC("not-a-date", "2006-01-02")
	require.Error(t, err)
}
