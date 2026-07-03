package iputil

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsValidCCBillIPRejectsForwardedHeaderLists(t *testing.T) {
	require.True(t, IsValidCCBillIP("64.38.212.1"))
	require.False(t, IsValidCCBillIP("64.38.212.1, 203.0.113.9"))
}

func TestIsValidCCBillIPDocumentedRanges(t *testing.T) {
	require.True(t, IsValidCCBillIP("64.38.240.10"))
	require.True(t, IsValidCCBillIP("64.38.241.254"))
	require.False(t, IsValidCCBillIP("203.0.113.7"))
	require.False(t, IsValidCCBillIP("198.51.100.4"))
	require.False(t, IsValidCCBillIP(""))
	require.False(t, IsValidCCBillIP("not-an-ip"))
}
