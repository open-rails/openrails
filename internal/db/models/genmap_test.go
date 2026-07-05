package models

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIntPtrTo32(t *testing.T) {
	require.Nil(t, IntPtrTo32(nil))

	n := 5
	got := IntPtrTo32(&n)
	require.NotNil(t, got)
	require.Equal(t, int32(5), *got)

	// Clamp rather than wrap on out-of-range values.
	over := math.MaxInt32 + 1
	got = IntPtrTo32(&over)
	require.Equal(t, int32(math.MaxInt32), *got)

	under := math.MinInt32 - 1
	got = IntPtrTo32(&under)
	require.Equal(t, int32(math.MinInt32), *got)
}
