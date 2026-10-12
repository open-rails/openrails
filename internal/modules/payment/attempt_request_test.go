package payment

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/config"
)

const testPriceID = "premium:monthly:v1"

func TestPriceSelector(t *testing.T) {
	for _, tc := range []struct {
		id, key string
		ok      bool
	}{
		{testPriceID, "", true},
		{"", "pro-monthly", true},
		{"", "", false},
		{"  ", "  ", false},
		{testPriceID, "pro-monthly", false},
		{"pro-monthly", "", false},
		{"price_00000000-0000-0000-0000-000000000000", "", false},
	} {
		productKey := ""
		if tc.key != "" {
			productKey = "pro"
		}
		err := validatePriceSelector(tc.id, productKey, tc.key)
		if tc.ok {
			require.NoError(t, err, "%q %q", tc.id, tc.key)
		} else {
			require.ErrorIs(t, err, ErrOrderAttemptValidation, "%q %q", tc.id, tc.key)
		}
	}
}

// Blanks are "not supplied"; everything else must match an allowed origin
// exactly (host policy itself is e2e's).
func TestValidateReturnURLs(t *testing.T) {
	svc := &OrderAttemptService{config: &config.Config{PublicBillingBaseURL: "https://billing.example/api"}}
	require.NoError(t, svc.validateReturnURLs("", "  ", "https://billing.example/done"))
	for _, bad := range []string{"https://user@billing.example/done", "https://billing.example.evil/done", "/relative", "javascript:alert(1)"} {
		require.ErrorIs(t, svc.validateReturnURLs(bad), ErrOrderAttemptValidation, bad)
	}
	require.Error(t, (&OrderAttemptService{}).validateReturnURLs("https://billing.example/done"), "no configured origin refuses all")
	var nilSvc *OrderAttemptService
	require.Error(t, nilSvc.validateReturnURLs("https://billing.example/done"))
}
