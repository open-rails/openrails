package payments

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
)

// The token_type stamp needs rail AND custody; unstated custody stamps nothing.
func TestDefaultTokenType(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ rail, custodian, want string }{
		{"nmi", models.CustodianPSP, charge.TokenTypePSPToken},
		{"nmi", models.CustodianBasisTheory, charge.TokenTypePANViaProxy},
		{" NMI ", models.CustodianHyperSwitch, charge.TokenTypePANViaProxy},
		{"nmi", "", ""},
		{"stripe", models.CustodianPSP, charge.TokenTypePSPToken},
		{"stripe", models.CustodianBasisTheory, ""},
		{"ccbill", models.CustodianPSP, ""},
		{"solana", "", ""},
		{"vaulted_card", models.CustodianBasisTheory, ""},
		{"mobius", models.CustodianPSP, ""},
	} {
		require.Equal(t, tc.want, DefaultTokenType(tc.rail, tc.custodian), "%s/%s", tc.rail, tc.custodian)
	}
}
