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

func TestCustomerPaymentKeyScansCallerTextBeforeHashing(t *testing.T) {
	for _, key := range []string{"archive-key-1461", "4111111111111111", "opaque-4111 1111 1111 1111"} {
		response := httptest.NewRecorder()
		wire := httptest.NewRequest("POST", "/v1/me/invoices/id/pay-now", nil)
		wire.Header.Set("Idempotency-Key", key)
		accepted, ok := paymentActionKey(httprequest.NewHTTP(response, wire, nil))
		if key == "archive-key-1461" {
			require.True(t, ok)
			require.Equal(t, key, accepted)
		} else {
			require.False(t, ok)
			require.Equal(t, 400, response.Code)
			require.Contains(t, response.Body.String(), "invalid_param")
		}
	}
}
