package catalog

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/catalogrules/syntax"
)

func TestBalanceLimitIsReserved(t *testing.T) {
	require.Error(t, syntax.ValidateLimitKey(BalanceLimit))
	require.NoError(t, syntax.ValidateLimitKey("seats"))
}
