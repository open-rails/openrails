package normalize

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTrim(t *testing.T) {
	require.Equal(t, "value", Trim("  value  "))
}

func TestLower(t *testing.T) {
	require.Equal(t, "mobius", Lower("  MoBiUs  "))
}

func TestFirstNonEmpty(t *testing.T) {
	require.Equal(t, "pi_123", FirstNonEmpty("", "  ", " pi_123 "))
	require.Equal(t, "", FirstNonEmpty("", "  "))
}

func TestOptionalString(t *testing.T) {
	value := OptionalString("  hello ")
	require.NotNil(t, value)
	require.Equal(t, "hello", *value)
	require.Nil(t, OptionalString("   "))
}

func TestFromPtr(t *testing.T) {
	require.Equal(t, "", FromPtr(nil))
	value := "  world "
	require.Equal(t, "world", FromPtr(&value))
}
