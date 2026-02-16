package services

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestParseCCBillDateUsingTimestamp_EndOfDayUTC(t *testing.T) {
	t.Parallel()

	got, err := parseCCBillDateUsingTimestamp("2026-03-15", "")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, time.Date(2026, 3, 15, 23, 59, 59, 0, time.UTC), *got)
}

func TestParseCCBillDateUsingTimestamp_EmptyDateReturnsNil(t *testing.T) {
	t.Parallel()

	got, err := parseCCBillDateUsingTimestamp("", "")
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestParseCCBillDateUsingTimestamp_InvalidDateFails(t *testing.T) {
	t.Parallel()

	got, err := parseCCBillDateUsingTimestamp("2026-31-99", "")
	require.Error(t, err)
	require.Nil(t, got)
}
