package service

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateInvokerSpendLimitInputsRejectsNormalizedDuplicates(t *testing.T) {
	roleID := "22222222-2222-2222-2222-222222222222"
	windows := []SpendLimitWindowInput{{Key: "day", WindowSeconds: 86400, Limit: 1000}}

	_, err := ValidateInvokerSpendLimitInputs([]InvokerSpendLimitInput{
		{Scope: " role ", RoleID: " " + roleID + " ", Windows: windows},
		{Scope: "role", ScopeKey: roleID, Windows: windows},
	})

	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidInvokerSpendLimit))
	require.Contains(t, err.Error(), "duplicate delegation for role\x00"+roleID)
}

func TestValidateInvokerSpendLimitInputsCanonicalizesRoleAliases(t *testing.T) {
	roleID := "22222222-2222-2222-2222-222222222222"
	next, err := ValidateInvokerSpendLimitInputs([]InvokerSpendLimitInput{{
		Scope: " role ", ScopeKey: " " + roleID + " ", RoleID: roleID,
		Windows: []SpendLimitWindowInput{{Key: " day ", WindowSeconds: 86400, Limit: 1000, Currency: " usd "}},
	}})

	require.NoError(t, err)
	require.Equal(t, []InvokerSpendLimitInput{{
		Scope: "role", ScopeKey: roleID, RoleID: roleID,
		Windows: []SpendLimitWindowInput{{Key: "day", WindowSeconds: 86400, Limit: 1000, Currency: "USD"}},
	}}, next)
}
