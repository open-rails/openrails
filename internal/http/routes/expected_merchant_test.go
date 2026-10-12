package routes

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/open-rails/openrails/internal/merchant"
)

// A precondition can reject the credential's merchant, never choose another
// one. The route's operation runs only after a matching, authenticated binding.
func TestExpectedMerchantPrecedesOperation(t *testing.T) {
	rt := gatedRuntime(t)
	a := boundAuth{who: authtest.Application("backend"), scope: authtest.MerchantScope(merchantA)}
	for _, route := range []string{"POST /v1/app/admissions", "POST /v1/admin/payments", "GET /v1/admin/access"} {
		for name, tc := range map[string]struct {
			expected string
			status   int
			code     string
		}{
			"matching":  {merchantA.String(), http.StatusNoContent, ""},
			"other":     {merchantB.String(), http.StatusPreconditionFailed, billing.CodeMerchantMismatch},
			"zero":      {"00000000-0000-0000-0000-000000000000", http.StatusBadRequest, billing.CodeInvalidParam},
			"malformed": {"not-a-merchant", http.StatusBadRequest, billing.CodeInvalidParam},
			"multiple":  {merchantA.String() + ", " + merchantB.String(), http.StatusBadRequest, billing.CodeInvalidParam},
		} {
			t.Run(route+"/"+name, func(t *testing.T) {
				got := runRoute(t, rt, a, route, map[string]string{merchant.ExpectedHeader: tc.expected})
				require.Equal(t, tc.status, got.status)
				require.Equal(t, tc.code, got.code)
				require.Equal(t, merchantA.String(), got.bound, "expectation never rebinds the credential")
				if tc.status != http.StatusNoContent {
					require.Zero(t, got.staff, "operation was never reached")
				}
			})
		}
	}
	unbound := boundAuth{who: authtest.Application("backend"), scope: billingauth.Scope{}}
	got := runRoute(t, runtimeDeclaring(billing.MerchantID{}), unbound, "POST /v1/admin/payments", map[string]string{merchant.ExpectedHeader: merchantA.String()})
	require.Equal(t, http.StatusForbidden, got.status)
	require.Equal(t, billing.CodeMerchantUnresolved, got.code)
	require.Empty(t, got.bound, "an expectation cannot create a binding")
}
