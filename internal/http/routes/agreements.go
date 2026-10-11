package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// The customer's charge agreements: what the merchant may charge its cards
// for without it, each revoked on its own.
var agreementsRoutes = []Route{
	{Method: GET, Path: "/v1/me/agreements", Group: Customer, Auth: AuthCustomer,
		Query: cursorPage, Responses: []Reply{{200, billing.ListPage[billing.Agreement]{}}}, Errors: codes("invalid_cursor"), Handler: h(handlers.ListMyAgreements)},
	{Method: POST, Path: "/v1/me/agreements/{id}/revoke", Group: Customer, Auth: AuthCustomer,
		Responses: []Reply{{200, billing.AgreementRevocation{}}},
		Errors:    codes("cancel_unsupported_on_rail", "customer_action_required", "invalid_param", "payment_in_progress", "provider_cancel_held", "resource_not_found", "service_unavailable", "subscription_not_active"),
		Handler:   h(handlers.RevokeMyAgreement)},
}
