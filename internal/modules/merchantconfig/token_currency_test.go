package merchantconfig

import (
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestRoutingUsesRegisteredCurrencyCodes(t *testing.T) {
	for _, code := range []string{"SOL", "USDC"} {
		rules, err := NormalizePSPRouting([]models.PSPRoutingRule{{Match: models.PSPRoutingMatch{Currency: code}, Prefer: []string{"solana"}}})
		require.NoError(t, err)
		require.Equal(t, code, rules[0].Match.Currency)
	}
	_, err := NormalizePSPRouting([]models.PSPRoutingRule{{Match: models.PSPRoutingMatch{Currency: "XYZ"}, Prefer: []string{"stripe"}}})
	require.Error(t, err)
}
