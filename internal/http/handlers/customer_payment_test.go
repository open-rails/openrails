package handlers

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

func TestCustomerPaymentRequiresVerifiedInteractionClass(t *testing.T) {
	for _, class := range []billingauth.CredentialClass{billingauth.CredentialClassUnknown, billingauth.CredentialClassAutomation, billingauth.CredentialClassUserSession} {
		for _, invoker := range []string{"", "automation-agent"} {
			response := httptest.NewRecorder()
			wire := httptest.NewRequest("POST", "/v1/me/invoices/id/pay-now", strings.NewReader(`{"credential_class":"user_session"}`))
			wire.Header.Set("Credential-Class", "user_session")
			request := httprequest.NewHTTP(response, wire, nil)
			request.SetUserContext(billingauth.UserContext{UserID: uuid.NewString()})
			request.Set(middleware.PrincipalContextKey, &middleware.Principal{CredentialType: middleware.CredentialHostDelegatedUser, CredentialClass: class, Invoker: invoker})
			_, ok := customerActionPayer(request)
			require.Equal(t, class == billingauth.CredentialClassUserSession && invoker == "", ok)
			if !ok {
				require.Equal(t, 403, response.Code)
				require.Contains(t, response.Body.String(), "customer_action_required")
			}
		}
	}
}
