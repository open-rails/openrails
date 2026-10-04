package api

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// A status's generic code is registered under that status and type, so a
// refusal that names no code still answers one the registry holds.
func TestGenericCodesAreRegistered(t *testing.T) {
	for _, status := range []int{400, 401, 402, 403, 404, 409, 429, 500, 503} {
		body := SimpleErrorResponse(status, "x").Error
		info, ok := billing.LookupErrorCode(body.Code)
		require.True(t, ok, body.Code)
		require.Equal(t, status, info.Status, body.Code)
		require.Equal(t, info.Type, body.Type, body.Code)
	}
}

func TestCodedUsesTheRegistry(t *testing.T) {
	refusal := Coded(billing.CodePermissionRequired, "")
	require.Equal(t, http.StatusForbidden, refusal.HTTPStatus)
	require.Equal(t, ErrorTypeAuthorization, refusal.Type)
	require.NotEmpty(t, refusal.Message)
	require.Equal(t, "why", Coded(billing.CodePermissionRequired, "why").Message)
	require.Empty(t, CodeViolations())

	unknown := Coded("never_registered", "x")
	require.Equal(t, http.StatusInternalServerError, unknown.HTTPStatus)
	require.Equal(t, CodeInternalError, unknown.Code)
	_ = NewAPIError(http.StatusBadRequest, ErrorTypeCard, billing.CodeCardDeclined, "x").ToResponse()
	require.Equal(t, []string{
		"error code card_declined answered 400; billing.ErrorCodes registers 402",
		"error code never_registered is not registered in billing.ErrorCodes",
	}, CodeViolations())
}
