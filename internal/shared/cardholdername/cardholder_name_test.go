package cardholdername

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFullNameIsCanonicalAndPartsAreALossyProjection(t *testing.T) {
	require.Equal(t, "María  José Carreño Quiñones", Canonical("  María  José Carreño Quiñones  ", "ignored", "legacy"), "internal spacing is the caller's")
	require.Equal(t, "Ada Lovelace", Canonical("", " Ada ", " Lovelace "))
	require.Equal(t, "Cher", Canonical(" ", "", "Cher"))
	require.Empty(t, Canonical("", " ", ""))

	for _, tc := range []struct{ full, first, last, wantFirst, wantLast string }{
		{"李 小龍", "ignored", "legacy", "李", "小龍"},
		{"Prince", "", "", "Prince", ""}, // never invent a surname
		{" Mary  Ann   de la Vega ", "", "", "Mary", "Ann de la Vega"},
		{"", " María de ", " la Vega ", "María de", "la Vega"}, // legacy parts pass through
	} {
		first, last := Parts(tc.full, tc.first, tc.last)
		require.Equal(t, []string{tc.wantFirst, tc.wantLast}, []string{first, last}, tc.full)
	}
}
