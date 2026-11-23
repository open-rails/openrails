package routes

import (
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/http/handlers"
)

// messagesRoutes is customer messages: a customer's message log, and a
// template checked before it is saved.
var messagesRoutes = []Route{
	{Method: GET, Path: "/v1/admin/customers/{customer_id}/messages", Group: Admin, Auth: AuthMerchant, Name: "ListCustomerMessages", Level: LevelRead,
		Args: []any{billing.CustomerID{}},
		Doc: "ListCustomerMessages lists the messages OpenRails sent a customer about its billing facts, newest first, " +
			"and those it did not send and why: each one's event, status, attempts and recipient. It keeps no body.",
		Query: billing.CustomerMessageListParams{}, Responses: []Reply{{200, billing.ListPage[billing.CustomerMessage]{}}},
		Errors: withCustomer("invalid_cursor"), Handler: h(handlers.ListCustomerMessages)},
	{Method: POST, Path: "/v1/merchant/messages/preview", Group: MerchantConfig, Level: LevelRead, Auth: AuthMerchant, Name: "PreviewMessage", NoConn: true,
		Doc: "PreviewMessage renders an event type's message from the merchant's templates over a sample customer and " +
			"fact, in params.Language or the default language. params.Template, when set, is rendered in place of that " +
			"language's template and not saved: a template is checked before it is saved.",
		Request: billing.PreviewMessageParams{}, Responses: []Reply{{200, billing.MessagePreview{}}},
		Errors: codes("invalid_param", "message_template_invalid", "service_unavailable"), Handler: h(handlers.PreviewMessage)},
	{Method: POST, Path: "/v1/merchant/messages/test", Group: MerchantConfig, Level: LevelUpdate, Auth: AuthMerchant, Name: "CreateTestMessage", NoConn: true,
		Doc: "CreateTestMessage sends one sample message, as PreviewMessage renders it, to the merchant's alert email " +
			"address. It is sent to no other address, at most ten an hour.",
		Request: billing.CreateTestMessageParams{}, Responses: []Reply{{200, billing.TestMessage{}}},
		Errors: codes("alert_email_required", "invalid_param", "message_send_failed", "message_template_invalid", "rate_limit_exceeded", "service_unavailable"), Handler: h(handlers.CreateTestMessage)},
}
