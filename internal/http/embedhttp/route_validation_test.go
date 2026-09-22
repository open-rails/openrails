package embedhttp

import (
	"net/http"
	"testing"

	"github.com/open-rails/openrails/internal/http/router"
	"github.com/stretchr/testify/require"
)

func TestValidateConsoleSubtreeRejectsCustomerDescendants(t *testing.T) {
	handler := http.NotFoundHandler()
	console := router.Entry{Method: http.MethodGet, Path: "/admin/{asset...}", Handler: handler}
	customer := router.Entry{Method: http.MethodGet, Path: "/admin/custom/balance", Handler: handler}
	for _, entries := range [][]router.Entry{{console, customer}, {customer, console}} {
		require.ErrorContains(t, ValidateRouteTable(&router.Table{Entries: entries}), "conflicting native subtree")
	}
	// The console's root redirect and other methods do not compete with its
	// GET subtree. A method-specific guard must preserve these registrations.
	require.NoError(t, ValidateRouteTable(&router.Table{Entries: []router.Entry{
		console,
		{Method: http.MethodGet, Path: "/admin", Handler: handler},
		{Method: http.MethodPost, Path: "/admin/custom/subscriptions/{id}/cancel", Handler: handler},
	}}))
}
